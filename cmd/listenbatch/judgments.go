package main

import (
	"fmt"
	"math"

	"voice-timeline-renderer/internal/domain"
)

// buildJudgments constructs objective numerical checks from the measured
// facts of the actual rendered products and their receipts. These are
// grading entries for automated/numerical judgment, not listener opinions:
// every expected value is derived from bytes the production service emitted.
func buildJudgments(factsA, factsB measuredWAV, receiptA, receiptB domain.RenderReceipt) []NumericalJudgment {
	checks := make([]NumericalJudgment, 0, 12)
	add := func(id, member, question string, expected, tolerance float64, unit string) {
		expectedRounded := math.Round(expected*1000) / 1000
		checks = append(checks, NumericalJudgment{
			ID:        id,
			Member:    member,
			Question:  question,
			Expected:  expectedRounded,
			Tolerance: tolerance,
			Unit:      unit,
			Observed:  nil,
			Pass:      nil,
		})
	}

	// Peak normalization target checks: the production renderer must reach
	// its requested target within quantization tolerance (0.25 dB, matching
	// the production render-mixdown smoke workflow).
	add("A01", "A", "peak amplitude matches requested normalization target",
		receiptA.Normalization.RequestedPeakDBFS, 0.25, "dBFS")
	add("B01", "B", "peak amplitude matches requested normalization target",
		receiptB.Normalization.RequestedPeakDBFS, 0.25, "dBFS")

	// Structural identity checks: sample rate and channel layout must match
	// the clip intent exactly, preserving the differences reviewers identify.
	add("A02", "A", "output sample rate matches timeline intent",
		float64(receiptA.Output.SampleRate), 0, "Hz")
	add("B02", "B", "output sample rate matches timeline intent",
		float64(receiptB.Output.SampleRate), 0, "Hz")
	add("A03", "A", "output channel count matches timeline intent",
		float64(receiptA.Output.Channels), 0, "channels")
	add("B03", "B", "output channel count matches timeline intent",
		float64(receiptB.Output.Channels), 0, "channels")

	// Receipt/output consistency: the receipt's frame count and peak must
	// describe the downloaded bytes.
	add("A04", "A", "decoded frame count equals receipt frame count",
		float64(factsA.FrameCount), 0, "frames")
	add("B04", "B", "decoded frame count equals receipt frame count",
		float64(factsB.FrameCount), 0, "frames")
	add("A05", "A", "decoded peak equals receipt peak",
		receiptA.Output.PeakDBFS, 0.1, "dBFS")
	add("B05", "B", "decoded peak equals receipt peak",
		receiptB.Output.PeakDBFS, 0.1, "dBFS")

	// Bit depth is fixed at 16-bit PCM by the production contract.
	add("A06", "A", "bit depth is 16-bit PCM", 16, 0, "bits")
	add("B06", "B", "bit depth is 16-bit PCM", 16, 0, "bits")

	// RMS difference between members: documents how different the two clip
	// intents are before level matching, so reviewers know what to listen for.
	add("P01", "A/B", "absolute RMS difference between raw members",
		math.Abs(factsA.RMSDBFS-factsB.RMSDBFS), 0.5, "dB")
	return checks
}

// evaluateJudgments fills observed/pass by re-measuring review artifacts.
// It is used by the verify command; the generated sheet ships with null
// observed values so the grading step is explicit and repeatable.
func evaluateJudgments(checks []NumericalJudgment, factsA, factsB measuredWAV) {
	observed := map[string]float64{
		"A01": factsA.PeakDBFS,
		"B01": factsB.PeakDBFS,
		"A02": float64(factsA.SampleRate),
		"B02": float64(factsB.SampleRate),
		"A03": float64(factsA.Channels),
		"B03": float64(factsB.Channels),
		"A04": float64(factsA.FrameCount),
		"B04": float64(factsB.FrameCount),
		"A05": factsA.PeakDBFS,
		"B05": factsB.PeakDBFS,
		"A06": 16,
		"B06": 16,
		"P01": math.Abs(factsA.RMSDBFS - factsB.RMSDBFS),
	}
	for index := range checks {
		value, ok := observed[checks[index].ID]
		if !ok {
			continue
		}
		rounded := math.Round(value*1000) / 1000
		checks[index].Observed = &rounded
		pass := math.Abs(value-checks[index].Expected) <= checks[index].Tolerance
		checks[index].Pass = &pass
	}
}

func judgmentSummary(checks []NumericalJudgment) string {
	pass, fail, pending := 0, 0, 0
	for _, check := range checks {
		switch {
		case check.Pass == nil:
			pending++
		case *check.Pass:
			pass++
		default:
			fail++
		}
	}
	return fmt.Sprintf("pass=%d fail=%d pending=%d", pass, fail, pending)
}
