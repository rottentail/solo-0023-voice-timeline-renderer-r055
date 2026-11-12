package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const defaultRSSLimitMiB = 256

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	arguments := os.Args[2:]
	var err error
	switch command {
	case "init":
		err = cmdInit(arguments)
	case "generate":
		err = cmdGenerate(arguments)
	case "scan":
		err = cmdScan(arguments)
	case "verify":
		err = cmdVerify(arguments)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "listenbatch %s failed: %v\n", command, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: listenbatch <init|generate|scan|verify> [flags]")
}

type commonFlags struct {
	root       string
	seed       int64
	count      int
	shard      int
	shards     int
	rssLimit   int
	remote     string
	maxSources int64
}

func newFlagSet(name string, config *commonFlags) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.StringVar(&config.root, "root", "var/listening-corpus", "corpus data root")
	flags.Int64Var(&config.seed, "seed", 20261112, "deterministic master seed")
	flags.IntVar(&config.count, "count", 512, "total item count across all shards")
	flags.IntVar(&config.shard, "shard", 0, "this worker's shard index (0-based)")
	flags.IntVar(&config.shards, "shards", 1, "total number of shards")
	flags.IntVar(&config.rssLimit, "rss-limit-mib", defaultRSSLimitMiB, "per-process RSS ceiling in MiB")
	flags.StringVar(&config.remote, "remote", "", "optional base URL of a running voiceservice; default embeds the real HTTP handler")
	flags.Int64Var(&config.maxSources, "max-source-bytes", 8<<20, "maximum source payload bytes accepted by the embedded service")
	return flags
}

func parseCommon(arguments []string) (*flag.FlagSet, *commonFlags) {
	config := &commonFlags{}
	flags := newFlagSet("listenbatch", config)
	_ = flags.Parse(arguments)
	return flags, config
}

func cmdInit(arguments []string) error {
	_, config := parseCommon(arguments)
	paths := newCorpusPaths(config.root)
	if err := paths.mkdirAll(); err != nil {
		return err
	}
	if _, err := os.Stat(paths.manifest); err == nil {
		var existing Manifest
		if readErr := readJSON(paths.manifest, &existing); readErr != nil {
			return readErr
		}
		if existing.Seed != config.seed || existing.ItemCount != config.count || existing.SchemaVersion != manifestVersion {
			return fmt.Errorf("refusing to re-init %s: existing manifest version=%d seed=%d count=%d",
				config.root, existing.SchemaVersion, existing.Seed, existing.ItemCount)
		}
		fmt.Printf("manifest already initialized at %s\n", paths.manifest)
		return nil
	}
	manifest := Manifest{
		SchemaVersion:  manifestVersion,
		Tool:           "cmd/listenbatch",
		Seed:           config.seed,
		ItemCount:      config.count,
		MaxSourceBytes: config.maxSources,
		RSSLimitMiB:    config.rssLimit,
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		Notes: []string{
			"Blind IDs are content-derived from (schema_version, seed, index).",
			"Raw products are retained verbatim; review copies add constant-gain leveling only.",
			"Human rating fields are intentionally null until a listener completes them.",
		},
	}
	if err := writeJSONAtomic(paths.manifest, manifest); err != nil {
		return err
	}
	fmt.Printf("initialized corpus root %s (version=%d seed=%d count=%d rss=%dMiB)\n",
		config.root, manifestVersion, config.seed, config.count, config.rssLimit)
	return nil
}

func loadManifest(paths corpusPaths) (Manifest, error) {
	var manifest Manifest
	if err := readJSON(paths.manifest, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("read manifest (run 'init' first): %w", err)
	}
	if manifest.SchemaVersion != manifestVersion {
		return Manifest{}, fmt.Errorf("manifest version %d does not match builder version %d", manifest.SchemaVersion, manifestVersion)
	}
	return manifest, nil
}

func cmdGenerate(arguments []string) error {
	_, config := parseCommon(arguments)
	if config.shards < 1 || config.shard < 0 || config.shard >= config.shards {
		return fmt.Errorf("invalid shard %d of %d", config.shard, config.shards)
	}
	paths := newCorpusPaths(config.root)
	manifest, err := loadManifest(paths)
	if err != nil {
		return err
	}
	if config.rssLimit > manifest.RSSLimitMiB {
		return fmt.Errorf("requested RSS limit %d MiB exceeds manifest ceiling %d MiB", config.rssLimit, manifest.RSSLimitMiB)
	}
	guard := newRSSGuard(config.rssLimit)

	var client *apiClient
	if config.remote != "" {
		client = newRemoteClient(strings.TrimRight(config.remote, "/"))
	} else {
		client, err = newEmbeddedClient(paths.service, manifest.MaxSourceBytes)
		if err != nil {
			return err
		}
	}
	defer client.close()

	b := &builder{paths: paths, client: client, guard: guard}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logFile, err := os.OpenFile(
		filepath.Join(paths.logs, fmt.Sprintf("generate-shard%d-of-%d.log", config.shard, config.shards)),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640,
	)
	if err != nil {
		return err
	}
	defer logFile.Close()

	processed, skipped, failed := 0, 0, 0
	start := time.Now()
	for index := 0; index < manifest.ItemCount; index++ {
		if index%config.shards != config.shard {
			continue
		}
		if err := guard.check(); err != nil {
			return err
		}
		plan := planItem(manifest.Seed, index)
		// Resume: skip only durably complete numbers.
		if marker, markerErr := readMarker(paths, plan.BlindID); markerErr == nil && marker.Status == statusComplete {
			skipped++
			continue
		}
		if ctx.Err() != nil {
			fmt.Fprintf(logFile, "%s interrupted at index %d; resume with the same command\n", time.Now().UTC().Format(time.RFC3339), index)
			fmt.Printf("interrupted; completed=%d skipped=%d failed=%d peakRSS=%dMiB\n", processed, skipped, failed, guard.peak())
			return ctx.Err()
		}
		buildErr := b.buildItem(ctx, plan, config.shard)
		processed++
		if buildErr != nil {
			failed++
			fmt.Fprintf(logFile, "%s item %d (%s) failed: %v\n", time.Now().UTC().Format(time.RFC3339), index, plan.BlindID, buildErr)
			// Integrity errors must stop the shard rather than silently
			// rebinding a blind number.
			if strings.Contains(buildErr.Error(), "digest") || strings.Contains(buildErr.Error(), "locked") {
				return buildErr
			}
			continue
		}
		fmt.Fprintf(logFile, "%s item %d (%s) complete\n", time.Now().UTC().Format(time.RFC3339), index, plan.BlindID)
		if processed%16 == 0 {
			fmt.Printf("shard %d/%d progress: visited=%d skipped=%d failed=%d peakRSS=%dMiB\n",
				config.shard, config.shards, processed, skipped, failed, guard.peak())
		}
	}
	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("shard %d/%d done: processed=%d skipped=%d failed=%d elapsed=%s peakRSS=%dMiB\n",
		config.shard, config.shards, processed, skipped, failed, elapsed, guard.peak())
	if failed > 0 {
		return fmt.Errorf("%d item(s) failed; re-run the same shard to resume", failed)
	}
	return nil
}

func cmdScan(arguments []string) error {
	_, config := parseCommon(arguments)
	paths := newCorpusPaths(config.root)
	manifest, err := loadManifest(paths)
	if err != nil {
		return err
	}
	summary := RunSummary{
		SchemaVersion: manifestVersion,
		GeneratedAt:   time.Now().UTC(),
		ItemCount:     manifest.ItemCount,
		RSSLimitMiB:   manifest.RSSLimitMiB,
	}
	// Stream marker files one at a time; no audio is loaded for a scan.
	entries, err := os.ReadDir(paths.state)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".marker.json") {
			continue
		}
		var marker ItemMarker
		if err := readJSON(filepath.Join(paths.state, entry.Name()), &marker); err != nil {
			return err
		}
		switch marker.Status {
		case statusComplete:
			summary.Completed++
		case statusInProgress:
			summary.InProgress++
		default:
			summary.NotStarted++
		}
	}
	summary.NotStarted += manifest.ItemCount - summary.Completed - summary.InProgress
	if summary.NotStarted < 0 {
		summary.NotStarted = 0
	}
	guard := newRSSGuard(config.rssLimit)
	_ = guard.check()
	summary.PeakRSSMiB = guard.peak()
	reportPath := filepath.Join(paths.root, "run-summary.json")
	if err := writeJSONAtomic(reportPath, summary); err != nil {
		return err
	}
	fmt.Printf("scan: total=%d complete=%d in_progress=%d not_started=%d -> %s\n",
		summary.ItemCount, summary.Completed, summary.InProgress, summary.NotStarted, reportPath)
	return nil
}

func cmdVerify(arguments []string) error {
	config := &commonFlags{}
	flags := newFlagSet("verify", config)
	only := flags.String("only", "", "verify a single blind ID")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	paths := newCorpusPaths(config.root)
	manifest, err := loadManifest(paths)
	if err != nil {
		return err
	}
	guard := newRSSGuard(config.rssLimit)
	checked, problems := 0, 0
	for index := 0; index < manifest.ItemCount; index++ {
		plan := planItem(manifest.Seed, index)
		if *only != "" && plan.BlindID != *only {
			continue
		}
		// Re-derivation check: the stored plan must equal a fresh derivation.
		var stored ItemPlan
		if err := readJSON(paths.planPath(plan.BlindID), &stored); err != nil {
			problems++
			fmt.Printf("MISSING plan %s: %v\n", plan.BlindID, err)
			continue
		}
		if stored.BlindID != plan.BlindID || stored.Index != plan.Index {
			problems++
			fmt.Printf("REBIND %s: stored plan no longer matches derivation\n", plan.BlindID)
			continue
		}
		marker, err := readMarker(paths, plan.BlindID)
		if err != nil {
			problems++
			fmt.Printf("NO-MARKER %s: %v\n", plan.BlindID, err)
			continue
		}
		if marker.Status != statusComplete {
			problems++
			fmt.Printf("INCOMPLETE %s status=%s\n", plan.BlindID, marker.Status)
			continue
		}
		if err := verifyOneItem(paths, plan, guard); err != nil {
			problems++
			fmt.Printf("FAIL %s: %v\n", plan.BlindID, err)
			continue
		}
		checked++
	}
	fmt.Printf("verify: checked=%d problems=%d peakRSS=%dMiB\n", checked, problems, guard.peak())
	if problems > 0 {
		return fmt.Errorf("%d verification problem(s)", problems)
	}
	return nil
}

// verifyOneItem re-checks a finished item from on-disk bytes: raw digest
// against the sheet, review structure preservation, mapping independence, and
// numerical judgments.
func verifyOneItem(paths corpusPaths, plan ItemPlan, guard *rssGuard) error {
	if err := guard.check(); err != nil {
		return err
	}
	itemDirectory := paths.itemDir(plan.BlindID)
	var sheet RatingSheet
	if err := readJSON(filepath.Join(itemDirectory, "rating_sheet.json"), &sheet); err != nil {
		return err
	}
	var mapping sourceMapping
	if err := readJSON(paths.sourceMapPath(plan.BlindID), &mapping); err != nil {
		return err
	}
	if mapping.BlindID != plan.BlindID || len(mapping.Sources) != len(plan.Sources) {
		return fmt.Errorf("source mapping does not match derived plan")
	}
	for label, member := range sheet.Members {
		rawPath := filepath.Join(itemDirectory, member.RawWAV)
		digest, err := sha256File(rawPath)
		if err != nil {
			return err
		}
		if digest != member.Digest {
			return fmt.Errorf("member %s raw digest mismatch", label)
		}
		rawWAV, err := os.ReadFile(rawPath)
		if err != nil {
			return err
		}
		rawFacts, err := measureWAV(rawWAV)
		if err != nil {
			return err
		}
		reviewWAV, err := os.ReadFile(filepath.Join(itemDirectory, member.ReviewWAV))
		if err != nil {
			return err
		}
		reviewFacts, err := measureWAV(reviewWAV)
		if err != nil {
			return err
		}
		if rawFacts.SampleRate != reviewFacts.SampleRate ||
			rawFacts.Channels != reviewFacts.Channels ||
			rawFacts.FrameCount != reviewFacts.FrameCount {
			return fmt.Errorf("member %s review copy altered structure", label)
		}
	}
	// Human fields must remain null: the builder never fabricates opinions.
	if sheet.HumanResponse.PreferredMember != nil ||
		sheet.HumanResponse.TimbreDifference != nil ||
		sheet.HumanResponse.ArtifactAudible != nil ||
		sheet.HumanResponse.Notes != nil {
		return fmt.Errorf("human response fields were pre-filled")
	}
	// Evaluate the objective checks against the actual bytes.
	rawA, err := os.ReadFile(filepath.Join(itemDirectory, sheet.Members["A"].RawWAV))
	if err != nil {
		return err
	}
	rawB, err := os.ReadFile(filepath.Join(itemDirectory, sheet.Members["B"].RawWAV))
	if err != nil {
		return err
	}
	factsA, err := measureWAV(rawA)
	if err != nil {
		return err
	}
	factsB, err := measureWAV(rawB)
	if err != nil {
		return err
	}
	evaluateJudgments(sheet.Checks, factsA, factsB)
	for _, check := range sheet.Checks {
		if check.Pass == nil || !*check.Pass {
			return fmt.Errorf("numerical check %s failed: %s", check.ID, judgmentSummary(sheet.Checks))
		}
	}
	return nil
}
