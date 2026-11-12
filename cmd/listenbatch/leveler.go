package main

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

// measuredWAV decodes a product WAV using the production codec and returns the
// exact facts used by the rating sheet. Products are short by construction, so
// only one item's audio is resident at any time.
type measuredWAV struct {
	SampleRate int
	Channels   int
	FrameCount int64
	PeakDBFS   float64
	RMSDBFS    float64
}

func measureWAV(wav []byte) (measuredWAV, error) {
	clip, info, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return measuredWAV{}, err
	}
	metrics := audiocodec.Measure(clip.Samples)
	return measuredWAV{
		SampleRate: info.SampleRate,
		Channels:   info.Channels,
		FrameCount: info.FrameCount,
		PeakDBFS:   metrics.PeakDBFS,
		RMSDBFS:    metrics.RMSDBFS,
	}, nil
}

// levelPair loudness-organizes one review pair for level-matched blind
// listening. The quieter member defines the reference; only the louder member
// is attenuated by a single constant gain. Nothing is resampled, remixed,
// re-channeled or peak normalized, and no raw file is modified.
//
// The returned LevelAlignment records every preservation guarantee so a
// reviewer can confirm channel, sample-rate and normalization differences are
// still audible in the review copies.
func levelPair(wavA, wavB []byte, factsA, factsB measuredWAV) (reviewA, reviewB []byte, alignment LevelAlignment, err error) {
	alignment = LevelAlignment{
		Method:         "constant_gain_rms_match_negative_only",
		RMSBeforeA:     factsA.RMSDBFS,
		RMSBeforeB:     factsB.RMSDBFS,
		ChannelsKept:   factsA.Channels == factsB.Channels,
		SampleRateKept: factsA.SampleRate == factsB.SampleRate,
		BitDepthKept:   true, // production codec always emits 16-bit PCM
		FrameCountKept: factsA.FrameCount == factsB.FrameCount,
	}

	diffDB := factsA.RMSDBFS - factsB.RMSDBFS
	switch {
	case math.Abs(diffDB) <= 0.05:
		// Already level matched within 0.05 dB: copy byte-for-byte.
		alignment.ReferenceMember = "both"
		alignment.AdjustedMember = "none"
		alignment.AppliedGainDB = 0
		reviewA = append([]byte(nil), wavA...)
		reviewB = append([]byte(nil), wavB...)
	case diffDB > 0:
		// A is louder: attenuate A down to B.
		gain := math.Pow(10, -diffDB/20)
		reviewA, err = applyConstantGain(wavA, gain)
		if err != nil {
			return nil, nil, alignment, err
		}
		reviewB = append([]byte(nil), wavB...)
		alignment.ReferenceMember = "B"
		alignment.AdjustedMember = "A"
		alignment.AppliedGainDB = -diffDB
	default:
		// B is louder: attenuate B down to A.
		gain := math.Pow(10, diffDB/20) // diffDB is negative
		reviewB, err = applyConstantGain(wavB, gain)
		if err != nil {
			return nil, nil, alignment, err
		}
		reviewA = append([]byte(nil), wavA...)
		alignment.ReferenceMember = "A"
		alignment.AdjustedMember = "B"
		alignment.AppliedGainDB = diffDB
	}

	afterA, err := measureWAV(reviewA)
	if err != nil {
		return nil, nil, alignment, err
	}
	afterB, err := measureWAV(reviewB)
	if err != nil {
		return nil, nil, alignment, err
	}
	alignment.RMSAfterA = afterA.RMSDBFS
	alignment.RMSAfterB = afterB.RMSDBFS

	// Re-verify the structural invariants on the actual review bytes.
	alignment.ChannelsKept = afterA.Channels == factsA.Channels && afterB.Channels == factsB.Channels
	alignment.SampleRateKept = afterA.SampleRate == factsA.SampleRate && afterB.SampleRate == factsB.SampleRate
	alignment.FrameCountKept = afterA.FrameCount == factsA.FrameCount && afterB.FrameCount == factsB.FrameCount
	return reviewA, reviewB, alignment, nil
}

// applyConstantGain decodes with the production codec, scales every sample by
// one factor and re-encodes with the same production encoder. Sample rate,
// channel count and frame count are therefore identical to the input.
func applyConstantGain(wav []byte, gain float64) ([]byte, error) {
	clip, _, err := audiocodec.DecodeWAV(wav)
	if err != nil {
		return nil, err
	}
	scaled := make([]float32, len(clip.Samples))
	for index, sample := range clip.Samples {
		scaled[index] = float32(float64(sample) * gain)
	}
	return audiocodec.EncodePCM16(clip.SampleRate, clip.Channels, scaled)
}
