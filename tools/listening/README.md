# Listening-batch material builder

`cmd/listenbatch` builds a reproducible, resumable corpus of blind listening
pairs and numerical judgment materials by driving the production Voice
Timeline Renderer through its **real HTTP surface**. It adds no production
code path and changes no production behavior; every source registration,
timeline composition and render goes through the same `httpapi` handler stack
that `cmd/voiceservice` exposes (embedded over `httptest` by default, or
against a separately running service via `-remote`).

## What one blind item contains

```text
items/<blind-id>/
  raw/
    member_a.raw.wav            original product, verbatim render output
    member_b.raw.wav            alternate product, verbatim render output
    member_a.receipt.json       production receipt for A
    member_b.receipt.json       production receipt for B
  review/
    member_a.review.wav         level-matched review copy (constant gain only)
    member_b.review.wav
  rating_sheet.json             numerical judgments + blank human response
mapping/<blind-id>.sources.json independent source->sound mapping (digests, plans)
planning/<blind-id>.plan.json   full deterministic derivation record
state/<blind-id>.marker.json    resumable per-item state
```

The production service keeps its own independent copy under
`service-data/` (sources, timelines, jobs, renders, tmp). The archived
`raw/` WAVs are byte-for-byte copies of the render output, verified by
SHA-256 against the rating sheet.

## Determinism and stable blind numbers

- The master seed and item count live in `manifest.json` (written once by
  `init`).
- A blind ID is `LB` + the first 14 hex chars of
  `sha256("listenbatch-v<version>-seed<seed>-item<index:06d>")`. It binds the
  manifest version, seed and item index, so:
  - sorting, sharding or restarting a run can never rebind a finished blind
    number to another group of sources, clip intent or product;
  - two corpora built with the same seed align the same indices to the same
    blind IDs;
  - changing the seed produces an entirely disjoint ID space.
- All signal synthesis parameters, timeline intents and resource IDs are
  pure functions of `(version, seed, index)`. Deleting an item and rebuilding
  it yields byte-identical raw products (verified by digest in tests).

## Resume and multi-process generation

- `generate -shard S -shards N` processes indices where `index % N == S`.
  Shards are disjoint and may run in parallel against one data root; an
  advisory `flock` per blind ID plus the service's own idempotent/conflict
  semantics prevent cross-process collisions.
- On restart, markers with status `complete` are skipped; only unfinished
  numbers are rebuilt. A `SIGTERM`/`SIGINT` is caught between items, so an
  interrupted item simply stays `in_progress` and is retried on the next run.
- A source/timeline/render ID reused with *different* content is rejected by
  the production service as a conflict and treated as a fatal integrity error
  rather than silently swapping audio.

## Loudness organization preserves identifiable differences

Review copies are level matched for blind listening with a **single constant
gain** applied to at most one member (the louder one is attenuated to the
quieter one's RMS). The tool never resamples, remaps channels, re-encodes at a
different bit depth, peak-normalizes or renames files to fake a new sample.
Each `rating_sheet.json` records the applied gain and asserts
`channels_kept`, `sample_rate_kept`, `bit_depth_kept` and `frame_count_kept`,
so mono/stereo, 44.1k/48k and normalization-target differences between the
two members remain present and measurable in the review files. The raw
products are never modified.

## No fabricated listener data

`rating_sheet.json` carries objective numerical judgments (normalization
target, sample rate, channel count, frame count, receipt consistency, bit
depth, raw RMS spread) whose expected values are measured from the actual
rendered bytes. The `human_response` object ships entirely `null`; the
`verify` command fails if any human field has been pre-filled.

## Memory ceiling

The corpus can be far larger than memory: one item's short clips are held at a
time and everything else is streamed to disk. A `/proc/self/statm` RSS guard
runs before and after each item and fails the process if resident memory
exceeds `-rss-limit-mib` (default 256, matching the manifest ceiling). The
guard refuses to run above the manifest ceiling, and observed peak RSS in the
reference runs stays well under the budget (roughly 15-30 MiB).

## Commands

```bash
go run ./cmd/listenbatch init     -root var/listening-corpus -count 600
go run ./cmd/listenbatch generate -root var/listening-corpus -count 600 \
    -shard 0 -shards 4
go run ./cmd/listenbatch scan     -root var/listening-corpus -count 600
go run ./cmd/listenbatch verify   -root var/listening-corpus -count 600
# or one shot:
tools/listening/run-shards.sh var/listening-corpus 600 4
```

`verify` re-derives every plan, re-checks raw digests, review-copy structural
preservation, mapping independence and all numerical judgments from on-disk
bytes. Use `-only <blind-id>` to verify a single item.
