// SPDX-License-Identifier: Apache-2.0

// Package compress implements the Sprint 3 stateless replacement for
// headroom-ai's Kompress transform (docs/v5-headroom-replacement.md Sprint
// 3): a faithful, corrected reimplementation of KompressCompressor.compress
// (headroom-ai's kompress_compressor.py), not a wrapper around the Python
// package. It reproduces Kompress's real algorithm — split on whitespace,
// score each token via the same kompress-v2-base ONNX model
// (chopratejas/kompress-v2-base, ModernBERT-based), keep a word if any of
// its sub-tokens scores above threshold or it matches the must-keep safety
// net, drop everything else — with two corrections found during Sprints
// 1-3 of the replacement effort:
//
//  1. The must-keep regex's "standalone number" pattern used a \w-boundary
//     lookaround, and Python's \w is Unicode-aware, so it silently failed
//     to protect a number glued to CJK text with no separating whitespace
//     ("104件", "2,100円", "2024年8月22日" — this fleet's primary content
//     shape). Fixed to a digit-only boundary. See mustkeep.go and
//     .sweep/headroom-bakeoff-2026-08-19.md's "Follow-up" section for the
//     original discovery.
//  2. Kompress's real chunker splits fixed 350-word blocks and tokenizes
//     each with max_length=512, truncation=True. Measured against all 7
//     Sprint 1 fixtures during Sprint 3 planning, 100% of chunks exceeded
//     512 tokens — words past the truncation point get no word_id and are
//     silently deleted (never scored, never protected by must-keep, which
//     only runs must-keep against the *unsent* tail, not skipped). This is
//     almost certainly the shape of Sprint 1's "kakaku release-date
//     sentence entirely absent" miss. Fixed by chunking against a real
//     measured token budget instead of a fixed word count. See chunk.go.
//
// This package is pure logic with no I/O of its own: the real ONNX model
// and HF tokenizer are injected via the Tokenizer and Scorer interfaces so
// the engine's own logic (chunk boundaries, must-keep override, word
// aggregation, determinism) is fully unit-testable without cgo or native
// libraries. The real cgo-backed implementations live in sibling packages
// (onnxscorer, hftokenizer) and are wired together only by
// cmd/forge-compress, which forge's own build never imports — see
// docs/v5-headroom-replacement.md Sprint 3's "Build & deploy" section for
// why that isolation matters (forge's cross-compile pipeline must stay
// pure-Go).
package compress

import "strings"

// Config controls the engine's behavior. DefaultConfig returns the values
// Sprints 1 and 2 measured.
type Config struct {
	// ScoreThreshold is the keep/drop cutoff on the model's per-token
	// score. Mirrors KompressConfig.score_threshold (default 0.5) —
	// _OnnxModel.get_keep_mask in the original: score > 0.5.
	ScoreThreshold float32
	// MinWords: content with fewer words than this passes through
	// unscored. Mirrors Kompress's own early return
	// (kompress_compressor.py: "if n_words < 10: return self._passthrough").
	MinWords int
	// ByteThreshold: content shorter than this (in bytes) passes through
	// untouched rather than paying compression latency for a token saving
	// smaller than the latency cost. Sprint 2's measured net-positive
	// crossover recommended 2KB as the default (docs/v5-headroom-replacement.md
	// Sprint 2 result, Probe A).
	ByteThreshold int
}

// DefaultConfig returns the values this initiative's own research settled
// on: ScoreThreshold 0.5 (Kompress's own default), MinWords 10 (Kompress's
// own early-return), ByteThreshold 2048 (Sprint 2 Probe A).
func DefaultConfig() Config {
	return Config{ScoreThreshold: 0.5, MinWords: 10, ByteThreshold: 2048}
}

// Encoding is one Tokenizer result: token ids plus, for every entry, the
// index into the original words slice it came from (or -1 for a
// non-word/special token such as [CLS]/[SEP]). Mirrors what Python's
// tokenizers library gives you via is_split_into_words=True +
// encoding.word_ids() — the API the original Kompress implementation
// depends on and that Go's daulet/tokenizers doesn't expose directly (it
// offers per-token character Offsets instead); hftokenizer reconstructs
// this shape from those offsets.
type Encoding struct {
	// IDs are the token ids fed to the model, special tokens included, in
	// the exact order the model expects.
	IDs []int64
	// AttentionMask is 1 for every real token (including specials), 0 for
	// padding. This engine never pads (each chunk is its own batch-of-one
	// forward pass), so every entry is 1 — kept explicit because the ONNX
	// model declares attention_mask as a real input.
	AttentionMask []int64
	// WordIndex[i] is the index into the words slice passed to EncodeWords
	// that produced IDs[i], or -1 for a special/non-word token.
	WordIndex []int
}

// Tokenizer turns pre-split words into model input tokens with word
// alignment. The real implementation (hftokenizer) wraps the ModernBERT
// tokenizer via cgo; tests use a fake — see fakes_test.go.
type Tokenizer interface {
	// EncodeWords tokenizes words (already split on whitespace, matching
	// Kompress's own content.split()) and returns their model-ready
	// encoding, including specials ([CLS]/[SEP] for ModernBERT).
	EncodeWords(words []string) (Encoding, error)
}

// Scorer runs the Kompress ONNX model for one chunk (batch size 1 — this
// engine never batches; "one forward pass per request" is a deliberate v1
// scope decision, docs/v5-headroom-replacement.md decision 1), returning
// one score per input position. The real implementation (onnxscorer) wraps
// onnxruntime_go via cgo; tests use a fake.
type Scorer interface {
	Score(inputIDs, attentionMask []int64) ([]float32, error)
}

// Engine is the stateless compressor itself: no cache, no lineage, no CCR
// (decision 1). It holds no per-request state — Compress is a pure
// function of its content argument (decision 5, the frozen-prefix
// contract; see TestCompress_FrozenPrefix).
type Engine struct {
	Tokenizer Tokenizer
	Scorer    Scorer
	Config    Config
}

// Result is the outcome of one Compress call.
type Result struct {
	Compressed      string
	OriginalWords   int
	CompressedWords int
	// OriginalTokens/CompressedTokens are real ModernBERT-tokenizer content
	// token counts (specials excluded), not a word-count or length
	// estimate — this is what the proxy binary's tokens_saved metric
	// reports (docs/v5-headroom-replacement.md Sprint 3 decision 4: never
	// a length/word-count estimate, matching an earlier honesty fix to
	// this same metric name — see this repo's headroom_persistent_savings_*
	// history). Both are 0 for a MinWords/ByteThreshold passthrough (the
	// whole point of that gate is to skip tokenization's own latency cost,
	// so no real count exists to report). For every other Passthrough
	// case (the all-dropped fallback), CompressedTokens == OriginalTokens
	// — the output is genuinely unchanged, so reported savings must be
	// genuinely zero.
	OriginalTokens   int
	CompressedTokens int
	// Passthrough is true when content was returned unchanged: below
	// MinWords/ByteThreshold, or every word ended up dropped by the model
	// (Kompress's own "if not kept_ids: passthrough" fallback — a
	// degenerate all-drop result is far more likely a bad chunk than a
	// legitimate "compress everything away," so verbatim beats empty).
	// Compress itself never fails open on error/timeout — that is the
	// caller's responsibility (the proxy binary wraps this with a
	// wall-clock budget and panic recovery); see the package doc.
	Passthrough bool
}

// Compress reproduces Kompress's real algorithm (see package doc for the
// two corrections). It is a pure function of content — same input always
// produces the same output, with no dependency on call order, timing, or
// any process state. This determinism is what lets a caller fail open on a
// slow/erroring call without ever risking a different compression decision
// for content that already made it through on a prior turn — the exact
// property a provider's prompt-cache prefix depends on staying frozen (see
// TestCompress_FrozenPrefix).
func (e *Engine) Compress(content string) (Result, error) {
	if len(content) < e.Config.ByteThreshold {
		return passthroughResult(content), nil
	}
	words := strings.Fields(content)
	if len(words) < e.Config.MinWords {
		return passthroughResult(content), nil
	}

	// Whole-content token measurement for chunkWords — computed in bounded
	// word windows rather than one giant EncodeWords call. A word's own
	// sub-tokenization is context-free (WordPiece/BPE — the same property
	// chunk.go's doc comment already relies on for per-chunk re-encoding),
	// so windowed counting yields byte-identical tokensPerWord to a single
	// whole-content call; what changes is peak allocation, which now scales
	// with the window instead of the request. DeepSeek's real traffic
	// averages ~262K tokens/request and few-and-huge requests are exactly
	// this proxy's shape (Sprint 9's OOM), so the tokenizer-side balloon on
	// a many-giant-requests pile-up is worth capping even though no single
	// request OOM'd on it.
	const measureWindowWords = 32768
	tokensPerWord := make([]int, len(words))
	for start := 0; start < len(words); start += measureWindowWords {
		end := start + measureWindowWords
		if end > len(words) {
			end = len(words)
		}
		win, err := e.Tokenizer.EncodeWords(words[start:end])
		if err != nil {
			return Result{}, err
		}
		for _, wi := range win.WordIndex {
			if wi >= 0 {
				tokensPerWord[start+wi]++
			}
		}
	}
	chunks := chunkWords(words, tokensPerWord)

	kept := make([]bool, len(words))
	wordOffset := 0
	for _, chunk := range chunks {
		enc, err := e.Tokenizer.EncodeWords(chunk)
		if err != nil {
			return Result{}, err
		}
		scores, err := scoreChunk(e.Scorer, enc)
		if err != nil {
			return Result{}, err
		}
		// Word kept if ANY of its sub-tokens scores above threshold —
		// mirrors the original's word_ids/mask_list aggregation
		// (kompress_compressor.py: "for idx, wid in enumerate(word_ids):
		// if bool(mask_list[idx]): kept_ids.add(wid + chunk_start)").
		for i, wi := range enc.WordIndex {
			if wi < 0 || i >= len(scores) {
				continue
			}
			if scores[i] > e.Config.ScoreThreshold {
				kept[wordOffset+wi] = true
			}
		}
		// Hard override: must-keep words survive regardless of model
		// score, applied per already-whitespace-split word (mirrors
		// _add_kompress_must_keep_words).
		for i, w := range chunk {
			mk, err := mustKeep(w)
			if err != nil {
				return Result{}, err
			}
			if mk {
				kept[wordOffset+i] = true
			}
		}
		wordOffset += len(chunk)
	}

	// Real content-token count of the whole input, specials excluded —
	// derived from the windowed per-word token counts already computed for
	// chunking, not a second tokenizer call.
	originalTokens := 0
	for _, n := range tokensPerWord {
		originalTokens += n
	}

	out := make([]string, 0, len(words))
	compressedTokens := 0
	for i, w := range words {
		if kept[i] {
			out = append(out, w)
			compressedTokens += tokensPerWord[i]
		}
	}
	if len(out) == 0 {
		res := passthroughResult(content)
		res.OriginalTokens = originalTokens
		res.CompressedTokens = originalTokens
		return res, nil
	}
	return Result{
		Compressed:       strings.Join(out, " "),
		OriginalWords:    len(words),
		CompressedWords:  len(out),
		OriginalTokens:   originalTokens,
		CompressedTokens: compressedTokens,
	}, nil
}

func passthroughResult(content string) Result {
	n := len(strings.Fields(content))
	return Result{Compressed: content, OriginalWords: n, CompressedWords: n, Passthrough: true}
}
