package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// corpusPaths centralizes the on-disk layout. Raw production artifacts live
// under the service data root and are additionally copied into raw/; the
// source mapping is independent of the raw products and kept under mapping/.
type corpusPaths struct {
	root     string
	manifest string
	service  string // production data root (sources/timelines/jobs/renders/tmp)
	planning string
	mapping  string
	items    string
	state    string
	logs     string
}

func newCorpusPaths(root string) corpusPaths {
	return corpusPaths{
		root:     root,
		manifest: filepath.Join(root, "manifest.json"),
		service:  filepath.Join(root, "service-data"),
		planning: filepath.Join(root, "planning"),
		mapping:  filepath.Join(root, "mapping"),
		items:    filepath.Join(root, "items"),
		state:    filepath.Join(root, "state"),
		logs:     filepath.Join(root, "logs"),
	}
}

func (paths corpusPaths) mkdirAll() error {
	for _, directory := range []string{
		paths.root, paths.service, paths.planning, paths.mapping,
		paths.items, paths.state, paths.logs,
		filepath.Join(paths.state, "locks"),
	} {
		if err := os.MkdirAll(directory, 0o750); err != nil {
			return err
		}
	}
	return nil
}

func (paths corpusPaths) itemDir(blindID string) string {
	return filepath.Join(paths.items, blindID)
}

func (paths corpusPaths) markerPath(blindID string) string {
	return filepath.Join(paths.state, blindID+".marker.json")
}

func (paths corpusPaths) lockPath(blindID string) string {
	return filepath.Join(paths.state, "locks", blindID+".lock")
}

func (paths corpusPaths) planPath(blindID string) string {
	return filepath.Join(paths.planning, blindID+".plan.json")
}

func (paths corpusPaths) sourceMapPath(blindID string) string {
	return filepath.Join(paths.mapping, blindID+".sources.json")
}

// writeJSONAtomic writes via a same-directory temp file and rename so readers
// never observe a half-written document.
func writeJSONAtomic(path string, value any) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		_ = temp.Close()
		return err
	}
	encoded = append(encoded, '\n')
	if _, err := temp.Write(encoded); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func readJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

// copyFile copies a file byte-for-byte through a small buffered stream, so
// even large artifacts never need to be fully resident.
func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	input, err := os.Open(src)
	if err != nil {
		return err
	}
	defer input.Close()
	temp, err := os.CreateTemp(filepath.Dir(dst), ".copy-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	buffer := make([]byte, 64*1024)
	for {
		n, readErr := input.Read(buffer)
		if n > 0 {
			if _, writeErr := temp.Write(buffer[:n]); writeErr != nil {
				_ = temp.Close()
				return writeErr
			}
		}
		if readErr != nil {
			_ = temp.Close()
			if errors.Is(readErr, io.EOF) {
				break
			}
			return readErr
		}
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, mode); err != nil {
		return err
	}
	return os.Rename(tempName, dst)
}

// sha256File streams a digest without loading the whole file.
func sha256File(path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	hasher := sha256.New()
	buffer := make([]byte, 64*1024)
	for {
		n, err := input.Read(buffer)
		if n > 0 {
			hasher.Write(buffer[:n])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// writeMarker records resumable state. A marker is only marked complete by
// the builder after all item artifacts exist and verify.
func writeMarker(paths corpusPaths, marker ItemMarker) error {
	marker.UpdatedAt = time.Now().UTC()
	return writeJSONAtomic(paths.markerPath(marker.BlindID), marker)
}

func readMarker(paths corpusPaths, blindID string) (ItemMarker, error) {
	var marker ItemMarker
	err := readJSON(paths.markerPath(blindID), &marker)
	return marker, err
}

// itemLock is an advisory cross-process lock around one blind item, so
// parallel shard workers can never organize the same number concurrently.
type itemLock struct {
	file *os.File
}

func acquireItemLock(paths corpusPaths, blindID string) (*itemLock, error) {
	if err := os.MkdirAll(filepath.Dir(paths.lockPath(blindID)), 0o750); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(paths.lockPath(blindID), os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("item %s is locked by another worker: %w", blindID, err)
	}
	return &itemLock{file: file}, nil
}

func (lock *itemLock) release() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
}

// rssGuard enforces the per-process resident set ceiling. It samples the
// kernel's RSS accounting and fails fast (rather than swapping or growing
// past the budget) while the corpus itself can be far larger than memory.
type rssGuard struct {
	limitMiB int
	peakMiB  int
	mu       sync.Mutex
}

func newRSSGuard(limitMiB int) *rssGuard {
	if limitMiB <= 0 {
		limitMiB = 256
	}
	return &rssGuard{limitMiB: limitMiB}
}

func (guard *rssGuard) check() error {
	// /proc/self/statm field 2 is resident size in pages; page size is 4096
	// on the supported Linux target. Reading a tiny proc file is O(1).
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		// Non-Linux or restricted proc: do not fail the build, but do not
		// claim enforcement either.
		return nil
	}
	fields := splitStatm(data)
	if len(fields) < 2 {
		return nil
	}
	rssPages := parseUint64(fields[1])
	rssMiB := int(rssPages * 4096 / (1024 * 1024))
	guard.mu.Lock()
	if rssMiB > guard.peakMiB {
		guard.peakMiB = rssMiB
	}
	guard.mu.Unlock()
	if rssMiB > guard.limitMiB {
		return fmt.Errorf("rss %d MiB exceeds configured ceiling %d MiB", rssMiB, guard.limitMiB)
	}
	return nil
}

func (guard *rssGuard) peak() int {
	guard.mu.Lock()
	defer guard.mu.Unlock()
	return guard.peakMiB
}
