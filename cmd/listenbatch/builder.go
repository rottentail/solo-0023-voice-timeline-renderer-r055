package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"voice-timeline-renderer/internal/domain"
)

// sourceMapping is the independently retained record of where every blind
// item's sounds came from. It is written before any render and never derived
// from filenames: each entry binds the blind number to deterministic source
// plans, production source IDs and the exact stored WAV digests.
type sourceMapping struct {
	SchemaVersion int                     `json:"schema_version"`
	BlindID       string                  `json:"blind_id"`
	Index         int                     `json:"index"`
	Sources       []sourceMappingEntry    `json:"sources"`
	Intents       map[string]TimelinePlan `json:"intents"`
}

type sourceMappingEntry struct {
	Slot       int        `json:"slot"`
	SourceID   string     `json:"source_id"`
	Digest     string     `json:"digest"`
	SampleRate int        `json:"sample_rate"`
	Channels   int        `json:"channels"`
	FrameCount int64      `json:"frame_count"`
	Plan       SourcePlan `json:"plan"`
}

// builder carries the shared dependencies for one run.
type builder struct {
	paths  corpusPaths
	client *apiClient
	guard  *rssGuard
}

// buildItem runs the full deterministic pipeline for one blind number. It is
// safe to re-invoke after an interruption: completed stages are detected from
// durable state and skipped, and the production service's own idempotency
// guards the shared data root across processes.
func (b *builder) buildItem(ctx context.Context, plan ItemPlan, shard int) error {
	if err := b.guard.check(); err != nil {
		return err
	}
	lock, err := acquireItemLock(b.paths, plan.BlindID)
	if err != nil {
		return err
	}
	defer lock.release()

	if existing, err := readMarker(b.paths, plan.BlindID); err == nil && existing.Status == statusComplete {
		// A finished blind number is never rebuilt or rebound.
		return nil
	}

	marker := ItemMarker{
		BlindID: plan.BlindID,
		Index:   plan.Index,
		Status:  statusInProgress,
		Shard:   shard,
		Stages:  []string{},
	}
	if err := writeMarker(b.paths, marker); err != nil {
		return err
	}
	fail := func(cause error) error {
		marker.Attempts++
		marker.LastError = cause.Error()
		_ = writeMarker(b.paths, marker)
		return cause
	}

	itemDirectory := b.paths.itemDir(plan.BlindID)
	if err := os.MkdirAll(filepath.Join(itemDirectory, "raw"), 0o750); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Join(itemDirectory, "review"), 0o750); err != nil {
		return fail(err)
	}

	// Stage 1: persist the deterministic plan so the item is rebuildable
	// even if the generator binary changes (the manifest version gates that).
	if err := writeJSONAtomic(b.paths.planPath(plan.BlindID), plan); err != nil {
		return fail(err)
	}
	marker.Stages = appendUnique(marker.Stages, "planned")
	if err := writeMarker(b.paths, marker); err != nil {
		return err
	}

	// Stage 2: synthesize and register the source sounds through the real
	// source entry point.
	sourceIDs := make([]string, len(plan.Sources))
	mappingEntries := make([]sourceMappingEntry, 0, len(plan.Sources))
	for slot, sourcePlan := range plan.Sources {
		sourceID := derivedID(idPrefixSource, plan.BlindID) + fmt.Sprintf("_%d", slot)
		sourceIDs[slot] = sourceID
		wav, synthErr := synthesizeWAV(sourcePlan)
		if synthErr != nil {
			return fail(synthErr)
		}
		asset, putErr := b.client.putSource(ctx, sourceID, sourcePlan.Role+" "+plan.BlindID, wav)
		if putErr != nil {
			return fail(putErr)
		}
		// Integrity: the service must have stored exactly the bytes sent.
		digest := sha256.Sum256(wav)
		if asset.Digest != hex.EncodeToString(digest[:]) {
			return fail(fmt.Errorf("source %s digest mismatch: service=%s local=%s", sourceID, asset.Digest, hex.EncodeToString(digest[:])))
		}
		mappingEntries = append(mappingEntries, sourceMappingEntry{
			Slot:       slot,
			SourceID:   sourceID,
			Digest:     asset.Digest,
			SampleRate: asset.SampleRate,
			Channels:   asset.Channels,
			FrameCount: asset.FrameCount,
			Plan:       sourcePlan,
		})
		if err := b.guard.check(); err != nil {
			return fail(err)
		}
	}
	mapping := sourceMapping{
		SchemaVersion: manifestVersion,
		BlindID:       plan.BlindID,
		Index:         plan.Index,
		Sources:       mappingEntries,
		Intents: map[string]TimelinePlan{
			"A": plan.Primary,
			"B": plan.Alternate,
		},
	}
	if err := writeJSONAtomic(b.paths.sourceMapPath(plan.BlindID), mapping); err != nil {
		return fail(err)
	}
	marker.Stages = appendUnique(marker.Stages, "sources_registered")
	if err := writeMarker(b.paths, marker); err != nil {
		return err
	}

	// Stages 3-4: compose both clip intents and render both products through
	// the real timeline and render entry points.
	memberA, err := b.renderMember(ctx, plan, sourceIDs, plan.Primary, idPrefixTimelineA, idPrefixRenderA, "A", itemDirectory)
	if err != nil {
		return fail(err)
	}
	memberB, err := b.renderMember(ctx, plan, sourceIDs, plan.Alternate, idPrefixTimelineB, idPrefixRenderB, "B", itemDirectory)
	if err != nil {
		return fail(err)
	}
	marker.Stages = appendUnique(marker.Stages, "products_rendered")
	if err := writeMarker(b.paths, marker); err != nil {
		return err
	}

	// Stage 5: loudness-organize review copies without touching the raw
	// products or their structural properties.
	rawA, err := os.ReadFile(memberA.rawPath)
	if err != nil {
		return fail(err)
	}
	rawB, err := os.ReadFile(memberB.rawPath)
	if err != nil {
		return fail(err)
	}
	factsA, err := measureWAV(rawA)
	if err != nil {
		return fail(err)
	}
	factsB, err := measureWAV(rawB)
	if err != nil {
		return fail(err)
	}
	reviewA, reviewB, alignment, err := levelPair(rawA, rawB, factsA, factsB)
	if err != nil {
		return fail(err)
	}
	if err := writeBytesAtomic(memberA.reviewPath, reviewA, 0o640); err != nil {
		return fail(err)
	}
	if err := writeBytesAtomic(memberB.reviewPath, reviewB, 0o640); err != nil {
		return fail(err)
	}
	if !alignment.ChannelsKept || !alignment.SampleRateKept || !alignment.BitDepthKept || !alignment.FrameCountKept {
		return fail(fmt.Errorf("leveling altered a preserved property for %s", plan.BlindID))
	}
	memberA.populateFacts(factsA)
	memberB.populateFacts(factsB)
	marker.Stages = appendUnique(marker.Stages, "review_leveled")
	if err := writeMarker(b.paths, marker); err != nil {
		return err
	}

	// Stage 6: receipts, numerical judgment sheet with blank human fields.
	receiptA, _, err := b.client.getReceipt(ctx, memberA.Member.RenderID)
	if err != nil {
		return fail(err)
	}
	receiptB, _, err := b.client.getReceipt(ctx, memberB.Member.RenderID)
	if err != nil {
		return fail(err)
	}
	if err := writeJSONAtomic(filepath.Join(itemDirectory, "raw", "member_a.receipt.json"), receiptA); err != nil {
		return fail(err)
	}
	if err := writeJSONAtomic(filepath.Join(itemDirectory, "raw", "member_b.receipt.json"), receiptB); err != nil {
		return fail(err)
	}
	memberA.Member.Receipt = filepath.Join("raw", "member_a.receipt.json")
	memberB.Member.Receipt = filepath.Join("raw", "member_b.receipt.json")
	memberA.Member.SourceMap = filepath.Join("..", "..", "mapping", plan.BlindID+".sources.json")
	memberB.Member.SourceMap = memberA.Member.SourceMap

	checks := buildJudgments(factsA, factsB, receiptA, receiptB)
	sheet := RatingSheet{
		SchemaVersion: manifestVersion,
		BlindID:       plan.BlindID,
		Index:         plan.Index,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Members: map[string]Member{
			"A": memberA.Member,
			"B": memberB.Member,
		},
		Alignment: alignment,
		Checks:    checks,
		HumanResponse: HumanRating{
			PreferredMember:  nil,
			TimbreDifference: nil,
			ArtifactAudible:  nil,
			Notes:            nil,
		},
	}
	if err := writeJSONAtomic(filepath.Join(itemDirectory, "rating_sheet.json"), sheet); err != nil {
		return fail(err)
	}
	marker.Stages = appendUnique(marker.Stages, "sheet_written")

	// Stage 7: verify every artifact on disk before declaring completion.
	if err := b.verifyItemArtifacts(plan.BlindID, memberA, memberB, rawA, rawB, reviewA, reviewB); err != nil {
		return fail(err)
	}
	marker.Status = statusComplete
	marker.LastError = ""
	if err := writeMarker(b.paths, marker); err != nil {
		return err
	}
	return b.guard.check()
}

type memberArtifacts struct {
	Member     Member
	rawPath    string
	reviewPath string
}

func (m *memberArtifacts) populateFacts(facts measuredWAV) {
	m.Member.SampleRate = facts.SampleRate
	m.Member.Channels = facts.Channels
	m.Member.BitsPerSample = 16
	m.Member.FrameCount = facts.FrameCount
	m.Member.PeakDBFS = facts.PeakDBFS
	m.Member.RMSDBFS = facts.RMSDBFS
}

// renderMember composes one timeline intent and renders it through the
// production HTTP surface, then archives the raw product byte-for-byte.
func (b *builder) renderMember(
	ctx context.Context,
	plan ItemPlan,
	sourceIDs []string,
	intent TimelinePlan,
	timelinePrefix, renderPrefix, memberLabel, itemDirectory string,
) (memberArtifacts, error) {
	timelineID := derivedID(timelinePrefix, plan.BlindID)
	renderID := derivedID(renderPrefix, plan.BlindID)

	spans := make([]domain.TimelineSpan, len(intent.Spans))
	for index, span := range intent.Spans {
		spans[index] = domain.TimelineSpan{
			SourceID:         sourceIDs[span.SourceSlot],
			SourceStartFrame: span.SourceStartFrame,
			SourceEndFrame:   span.SourceEndFrame,
			GainDB:           span.GainDB,
			CrossfadeFrames:  span.CrossfadeFrames,
		}
	}
	timeline, err := b.client.putTimeline(ctx, timelineID, intent, spans)
	if err != nil {
		return memberArtifacts{}, err
	}
	if timeline.State != "ready" {
		return memberArtifacts{}, fmt.Errorf("timeline %s not ready: %s", timelineID, timeline.State)
	}
	job, err := b.client.putRender(ctx, renderID, timelineID)
	if err != nil {
		return memberArtifacts{}, err
	}
	if job.State != domain.RenderCompleted || job.Output == nil {
		return memberArtifacts{}, fmt.Errorf("render %s state=%s", renderID, job.State)
	}

	output, status, err := b.client.getOutput(ctx, renderID)
	if err != nil {
		return memberArtifacts{}, err
	}
	if status != 200 {
		return memberArtifacts{}, fmt.Errorf("render %s output status %d", renderID, status)
	}
	rawName := "member_" + lowerLabel(memberLabel) + ".raw.wav"
	reviewName := "member_" + lowerLabel(memberLabel) + ".review.wav"
	rawPath := filepath.Join(itemDirectory, "raw", rawName)
	reviewPath := filepath.Join(itemDirectory, "review", reviewName)
	if err := writeBytesAtomic(rawPath, output, 0o640); err != nil {
		return memberArtifacts{}, err
	}
	digest := sha256.Sum256(output)
	return memberArtifacts{
		rawPath:    rawPath,
		reviewPath: reviewPath,
		Member: Member{
			RenderID:   renderID,
			TimelineID: timelineID,
			Variant:    intent.Variant,
			RawWAV:     filepath.Join("raw", rawName),
			ReviewWAV:  filepath.Join("review", reviewName),
			Digest:     hex.EncodeToString(digest[:]),
		},
	}, nil
}

// verifyItemArtifacts confirms the archived raw bytes still match the
// production output digest and that review copies kept their structure.
func (b *builder) verifyItemArtifacts(
	blindID string,
	memberA, memberB memberArtifacts,
	rawA, rawB, reviewA, reviewB []byte,
) error {
	digestA := sha256.Sum256(rawA)
	if hex.EncodeToString(digestA[:]) != memberA.Member.Digest {
		return fmt.Errorf("%s member A raw digest drift", blindID)
	}
	digestB := sha256.Sum256(rawB)
	if hex.EncodeToString(digestB[:]) != memberB.Member.Digest {
		return fmt.Errorf("%s member B raw digest drift", blindID)
	}
	rawFactsA, err := measureWAV(rawA)
	if err != nil {
		return err
	}
	reviewFactsA, err := measureWAV(reviewA)
	if err != nil {
		return err
	}
	if rawFactsA.SampleRate != reviewFactsA.SampleRate ||
		rawFactsA.Channels != reviewFactsA.Channels ||
		rawFactsA.FrameCount != reviewFactsA.FrameCount {
		return fmt.Errorf("%s member A review copy changed structure", blindID)
	}
	rawFactsB, err := measureWAV(rawB)
	if err != nil {
		return err
	}
	reviewFactsB, err := measureWAV(reviewB)
	if err != nil {
		return err
	}
	if rawFactsB.SampleRate != reviewFactsB.SampleRate ||
		rawFactsB.Channels != reviewFactsB.Channels ||
		rawFactsB.FrameCount != reviewFactsB.FrameCount {
		return fmt.Errorf("%s member B review copy changed structure", blindID)
	}
	return nil
}

const (
	statusInProgress = "in_progress"
	statusComplete   = "complete"
)

func appendUnique(stages []string, stage string) []string {
	for _, existing := range stages {
		if existing == stage {
			return stages
		}
	}
	return append(stages, stage)
}

func lowerLabel(label string) string {
	switch label {
	case "A":
		return "a"
	case "B":
		return "b"
	default:
		return label
	}
}

// writeBytesAtomic commits a byte payload through a temp file and rename.
func writeBytesAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(directory, ".bytes-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tempName, mode); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
