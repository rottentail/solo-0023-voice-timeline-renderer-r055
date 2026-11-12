// Package main implements the listenbatch material builder.
//
// listenbatch produces a reproducible, resumable corpus of blind listening
// pairs and numerical judgment sheets by driving the production Voice
// Timeline Renderer through its real HTTP surface (the same handler stack
// used by cmd/voiceservice and the checkflow smoke workflows). It never
// imports the render pipeline directly and never modifies production code.
//
// Guarantees:
//   - Blind numbers are content-derived and stable: every blind ID is a hash
//     of the manifest version, seed and item index, so restarting, sharding or
//     re-sorting the run never rebinds a finished blind number to other audio.
//   - All generated signals, timelines and IDs are pure functions of the
//     (version, seed, index) tuple. The same tuple always yields the same
//     source sounds, clip intents and original products.
//   - Resume is state based: a blind number whose marker is complete is
//     skipped; only unfinished numbers are filled in. The production service
//     itself is idempotent on identical IDs and rejects ID reuse with
//     different content, which is treated as a fatal integrity error.
//   - Raw products (rendered output WAVs and receipts) are kept verbatim in
//     the service data root and copied byte-for-byte into raw/; loudness
//     organization only writes additional leveled review copies and never
//     rewrites, resamples or re-normalizes the originals.
//   - Level matching applies a constant gain to at most one member of a pair.
//     Sample rate, channel count, bit depth and frame alignment are preserved,
//     so channel/sample/normalization differences remain identifiable.
//   - No listener opinions are fabricated: rating sheets ship with null
//     response fields for humans to fill in.
package main
