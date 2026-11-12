package main

import "time"

// manifestVersion is bumped only when the deterministic generation rules
// change in a way that would alter audio bytes. An existing data root built
// with another version is refused rather than silently mixed.
const manifestVersion = 1

// Manifest is written once when a corpus is initialized. Its fields fully
// determine every generated item, so the corpus is rebuildable from scratch
// without consulting any mutable run state.
type Manifest struct {
	SchemaVersion  int    `json:"schema_version"`
	Tool           string `json:"tool"`
	Seed           int64  `json:"seed"`
	ItemCount      int    `json:"item_count"`
	MaxSourceBytes int64  `json:"max_source_bytes"`
	// RSSLimitMiB is the per-process resident set ceiling the builder
	// enforces on itself while generating and organizing.
	RSSLimitMiB int      `json:"rss_limit_mib"`
	CreatedAt   string   `json:"created_at"`
	Notes       []string `json:"notes"`
}

// SourcePlan describes one deterministic source sound for a blind item.
type SourcePlan struct {
	Role       string  `json:"role"`
	Kind       string  `json:"kind"`
	SampleRate int     `json:"sample_rate"`
	Channels   int     `json:"channels"`
	Frames     int64   `json:"frames"`
	BaseFreq   float64 `json:"base_freq_hz"`
	Amplitude  float64 `json:"amplitude"`
	ToneCount  int     `json:"tone_count"`
	ModRate    float64 `json:"mod_rate_hz"`
	NoiseLevel float64 `json:"noise_level"`
	SweepSpan  float64 `json:"sweep_span_hz"`
	Phase      float64 `json:"stereo_phase_offset"`
}

// SpanPlan is one timeline span inside a clip intent.
type SpanPlan struct {
	SourceSlot       int     `json:"source_slot"`
	SourceStartFrame int64   `json:"source_start_frame"`
	SourceEndFrame   int64   `json:"source_end_frame"`
	GainDB           float64 `json:"gain_db"`
	CrossfadeFrames  int64   `json:"crossfade_frames"`
}

// TimelinePlan is the deterministic clip intent for one product variant.
type TimelinePlan struct {
	Variant        string     `json:"variant"`
	SampleRate     int        `json:"sample_rate"`
	OutputChannels int        `json:"output_channels"`
	TargetPeakDBFS float64    `json:"target_peak_dbfs"`
	Spans          []SpanPlan `json:"spans"`
}

// ItemPlan is the complete deterministic description of one blind number.
type ItemPlan struct {
	Index     int          `json:"index"`
	BlindID   string       `json:"blind_id"`
	Sources   []SourcePlan `json:"sources"`
	Primary   TimelinePlan `json:"primary_intent"`
	Alternate TimelinePlan `json:"alternate_intent"`
}

// LevelAlignment records how review copies were loudness matched. At most one
// member is attenuated by a constant gain; no resampling, channel mapping or
// peak normalization is performed here.
type LevelAlignment struct {
	Method          string  `json:"method"`
	ReferenceMember string  `json:"reference_member"`
	AdjustedMember  string  `json:"adjusted_member"`
	AppliedGainDB   float64 `json:"applied_gain_db"`
	RMSBeforeA      float64 `json:"rms_dbfs_before_a"`
	RMSBeforeB      float64 `json:"rms_dbfs_before_b"`
	RMSAfterA       float64 `json:"rms_dbfs_after_a"`
	RMSAfterB       float64 `json:"rms_dbfs_after_b"`
	ChannelsKept    bool    `json:"channels_kept"`
	SampleRateKept  bool    `json:"sample_rate_kept"`
	BitDepthKept    bool    `json:"bit_depth_kept"`
	FrameCountKept  bool    `json:"frame_count_kept"`
}

// NumericalJudgment is an objective grading entry with an expected value
// measured from the actual rendered bytes. Tolerances are deliberately wide
// enough for 16-bit quantization but narrow enough to catch real regressions.
type NumericalJudgment struct {
	ID        string   `json:"id"`
	Member    string   `json:"member"`
	Question  string   `json:"question"`
	Expected  float64  `json:"expected"`
	Tolerance float64  `json:"tolerance"`
	Unit      string   `json:"unit"`
	Observed  *float64 `json:"observed"`
	Pass      *bool    `json:"pass"`
}

// HumanRating holds blank fields reserved for a real listener. The builder
// never writes a non-null value into these fields.
type HumanRating struct {
	PreferredMember  *string `json:"preferred_member"`
	TimbreDifference *int    `json:"timbre_difference_1_5"`
	ArtifactAudible  *bool   `json:"artifact_audible"`
	Notes            *string `json:"notes"`
}

// RatingSheet is the per-item audit and grading document.
type RatingSheet struct {
	SchemaVersion int                 `json:"schema_version"`
	BlindID       string              `json:"blind_id"`
	Index         int                 `json:"index"`
	GeneratedAt   string              `json:"generated_at"`
	Members       map[string]Member   `json:"members"`
	Alignment     LevelAlignment      `json:"level_alignment"`
	Checks        []NumericalJudgment `json:"numerical_judgments"`
	// HumanResponse stays entirely null until a person fills it in.
	HumanResponse HumanRating `json:"human_response"`
}

// Member describes one side of a listening pair.
type Member struct {
	RenderID      string  `json:"render_id"`
	TimelineID    string  `json:"timeline_id"`
	Variant       string  `json:"variant"`
	RawWAV        string  `json:"raw_wav"`
	ReviewWAV     string  `json:"review_wav"`
	Receipt       string  `json:"receipt"`
	SourceMap     string  `json:"source_map"`
	SampleRate    int     `json:"sample_rate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bits_per_sample"`
	FrameCount    int64   `json:"frame_count"`
	PeakDBFS      float64 `json:"peak_dbfs"`
	RMSDBFS       float64 `json:"rms_dbfs"`
	Digest        string  `json:"sha256"`
}

// ItemMarker is the resumable per-item state file. A marker only reaches
// status "complete" after every artifact is on disk and verified.
type ItemMarker struct {
	BlindID   string    `json:"blind_id"`
	Index     int       `json:"index"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	Stages    []string  `json:"stages"`
	Shard     int       `json:"shard"`
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
}

// RunSummary aggregates a scan of markers without loading audio.
type RunSummary struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	ItemCount     int       `json:"item_count"`
	Completed     int       `json:"completed"`
	InProgress    int       `json:"in_progress"`
	NotStarted    int       `json:"not_started"`
	PeakRSSMiB    int       `json:"peak_rss_mib"`
	RSSLimitMiB   int       `json:"rss_limit_mib"`
}
