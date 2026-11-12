package main

import (
	"math"

	"voice-timeline-renderer/internal/audiocodec"
)

// synthesizeWAV deterministically renders the described source to a 16-bit
// PCM WAV. Synthesis is a pure function of the SourcePlan, so restarting a
// run produces byte-identical source payloads. Only short clips are held in
// memory; everything is streamed to disk immediately afterwards.
func synthesizeWAV(plan SourcePlan) ([]byte, error) {
	samples := make([]float32, int(plan.Frames)*plan.Channels)
	for frame := int64(0); frame < plan.Frames; frame++ {
		t := float64(frame) / float64(plan.SampleRate)
		first := frameValue(plan, t, 0)
		if plan.Channels == 1 {
			samples[frame] = clampFloat(first)
			continue
		}
		second := frameValue(plan, t, plan.Phase)
		samples[int(frame)*2] = clampFloat(first)
		samples[int(frame)*2+1] = clampFloat(second * 0.85)
	}
	return audiocodec.EncodePCM16(plan.SampleRate, plan.Channels, samples)
}

// frameValue renders one mono sample at time t. phaseOffset is used only for
// the second stereo channel.
func frameValue(plan SourcePlan, t, phaseOffset float64) float64 {
	base := plan.BaseFreq
	amp := plan.Amplitude
	switch plan.Kind {
	case "sine":
		return amp * math.Sin(2*math.Pi*base*t+phaseOffset)
	case "chord":
		value := 0.0
		for harmonic := 1; harmonic <= plan.ToneCount; harmonic++ {
			value += math.Sin(2*math.Pi*base*float64(harmonic)*t + phaseOffset)
		}
		return amp * value / float64(plan.ToneCount)
	case "am_tone":
		carrier := math.Sin(2*math.Pi*base*t + phaseOffset)
		modulator := 0.5 + 0.5*math.Sin(2*math.Pi*plan.ModRate*t)
		return amp * carrier * modulator
	case "sweep":
		// Linear upward sweep over base .. base+span, looping each second.
		cycle := math.Mod(t, 1.0)
		instant := base + plan.SweepSpan*cycle
		return amp * math.Sin(2*math.Pi*instant*t+phaseOffset)
	case "noisy_tone":
		tone := math.Sin(2*math.Pi*base*t + phaseOffset)
		// Deterministic pseudo-noise derived from time position; no global
		// PRNG state is read here, keeping synthesis a pure function.
		h := math.Sin(t*127.1+phaseOffset*311.7) * 43758.5453
		noise := h - math.Floor(h)
		return amp*tone + plan.NoiseLevel*(noise*2-1)
	default:
		return amp * math.Sin(2*math.Pi*base*t+phaseOffset)
	}
}

func clampFloat(value float64) float32 {
	if value > 1 {
		value = 1
	}
	if value < -1 {
		value = -1
	}
	return float32(value)
}
