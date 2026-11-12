package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
)

// Allowed signal shapes. Keeping discrete choices makes plans auditable and
// keeps every generated WAV inside the service's validation envelope.
var (
	allowedRates         = []int{44100, 48000, 32000, 22050}
	allowedChannels      = []int{1, 2}
	allowedKinds         = []string{"sine", "chord", "am_tone", "sweep", "noisy_tone"}
	allowedTargets       = []float64{-1.0, -3.0, -6.0, -9.0}
	allowedOutChans      = []int{1, 2}
	allowedGains         = []float64{-6.0, -3.0, -1.5, 0.0, 1.5, 3.0}
	allowedTimelineRates = []int{44100, 48000}
)

const (
	idPrefixSource    = "src"
	idPrefixTimelineA = "tla"
	idPrefixTimelineB = "tlb"
	idPrefixRenderA   = "rpa"
	idPrefixRenderB   = "rpb"
)

// blindNumber derives the stable blind ID for an item. The hash binds the
// manifest version and seed to the index, so the number cannot be rebound to
// different audio by sorting, sharding or restarting the run.
func blindNumber(seed int64, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("listenbatch-v%d-seed%d-item%06d", manifestVersion, seed, index)))
	return "LB" + hex.EncodeToString(sum[:])[:14]
}

// derivedID builds a production resource ID that is unique per item and role
// and stable across restarts. Resource IDs must be 3-64 chars of
// [A-Za-z0-9_-]; "LB..." plus a prefix satisfies that.
func derivedID(prefix, blindID string) string {
	return prefix + "_" + blindID
}

// itemRNG returns the private random stream for one item. Every random draw
// for item N comes from this stream only, so items are independent and
// individually reproducible.
func itemRNG(seed int64, index int) *rand.Rand {
	return rand.New(rand.NewSource(seed*1_000_003 + int64(index)*7919 + 17))
}

func pickInt(rng *rand.Rand, options []int) int {
	return options[rng.Intn(len(options))]
}

func pickFloat(rng *rand.Rand, options []float64) float64 {
	return options[rng.Intn(len(options))]
}

func pickString(rng *rand.Rand, options []string) string {
	return options[rng.Intn(len(options))]
}

// roundToFrames snaps a frame count to a small block so plans stay compact.
func roundToFrames(rng *rand.Rand, sampleRate int, seconds float64) int64 {
	block := int64(sampleRate / 100) // 10 ms blocks
	frames := int64(seconds * float64(sampleRate))
	frames = ((frames + block - 1) / block) * block
	if frames < block {
		frames = block
	}
	return frames
}

// planItem deterministically constructs the full description of one blind
// item: its two source sounds, the primary clip intent and the alternate clip
// intent. Nothing here touches the network or disk.
func planItem(seed int64, index int) ItemPlan {
	rng := itemRNG(seed, index)
	blindID := blindNumber(seed, index)

	sources := make([]SourcePlan, 2)
	for slot := range sources {
		rate := pickInt(rng, allowedRates)
		// 1.2 - 3.0 s sources keep every in-memory clip small.
		duration := 1.2 + rng.Float64()*1.8
		frames := roundToFrames(rng, rate, duration)
		sources[slot] = SourcePlan{
			Role:       fmt.Sprintf("source_%d", slot),
			Kind:       pickString(rng, allowedKinds),
			SampleRate: rate,
			Channels:   pickInt(rng, allowedChannels),
			Frames:     frames,
			BaseFreq:   180 + rng.Float64()*520,
			Amplitude:  0.18 + rng.Float64()*0.45,
			ToneCount:  2 + rng.Intn(3),
			ModRate:    3 + rng.Float64()*7,
			NoiseLevel: 0.01 + rng.Float64()*0.05,
			SweepSpan:  120 + rng.Float64()*480,
			Phase:      0.2 + rng.Float64()*0.6,
		}
	}

	primary := planTimeline(rng, blindID, sources, "primary", 11)
	alternate := planTimeline(rng, blindID, sources, "alternate", 23)

	return ItemPlan{
		Index:     index,
		BlindID:   blindID,
		Sources:   sources,
		Primary:   primary,
		Alternate: alternate,
	}
}

// planTimeline builds one valid clip intent over the two sources. The salt
// gives primary and alternate independent random draws while keeping both
// deterministic for the item.
func planTimeline(rng *rand.Rand, blindID string, sources []SourcePlan, variant string, salt int64) TimelinePlan {
	sub := rand.New(rand.NewSource(rng.Int63() + salt))
	timelineRate := pickInt(sub, allowedTimelineRates)
	outChannels := pickInt(sub, allowedOutChans)
	target := pickFloat(sub, allowedTargets)

	spans := make([]SpanPlan, 2)
	for slot := range spans {
		source := sources[slot]
		// Use between 55% and 90% of the source as the clip interval.
		usable := float64(source.Frames)
		end := int64(usable * (0.55 + sub.Float64()*0.35))
		endBlock := int64(timelineRate / 100)
		end = (end / endBlock) * endBlock
		if end < endBlock {
			end = endBlock
		}
		if end > source.Frames {
			end = source.Frames
		}
		spans[slot] = SpanPlan{
			SourceSlot:       slot,
			SourceStartFrame: 0,
			SourceEndFrame:   end,
			GainDB:           pickFloat(sub, allowedGains),
			CrossfadeFrames:  0,
		}
	}

	// Crossfade in output frames: 40-120 ms, bounded by both scaled spans.
	firstScaled := scaleFrames(spans[0].SourceEndFrame, sources[0].SampleRate, timelineRate)
	secondScaled := scaleFrames(spans[1].SourceEndFrame, sources[1].SampleRate, timelineRate)
	maxCross := firstScaled
	if secondScaled < maxCross {
		maxCross = secondScaled
	}
	crossBlock := int64(timelineRate / 100)
	cross := crossBlock * (4 + sub.Int63n(9)) // 400-1200 ms in 10 ms blocks -> 40-120 blocks
	if cross >= maxCross {
		cross = maxCross / 2
		cross = (cross / crossBlock) * crossBlock
	}
	if cross < 0 {
		cross = 0
	}
	spans[1].CrossfadeFrames = cross

	return TimelinePlan{
		Variant:        variant,
		SampleRate:     timelineRate,
		OutputChannels: outChannels,
		TargetPeakDBFS: target,
		Spans:          spans,
	}
}

func scaleFrames(frameCount int64, sourceRate, targetRate int) int64 {
	if frameCount <= 0 {
		return 0
	}
	scaled := frameCount * int64(targetRate) / int64(sourceRate)
	if scaled < 1 {
		scaled = 1
	}
	return scaled
}
