// SPDX-License-Identifier: Apache-2.0

package compress

// maxChunkTokens matches the ONNX model's declared max_length used at
// inference (512 — kompress_compressor.py's tokenizer(..., max_length=512)
// call).
const maxChunkTokens = 512

// specialTokenBudget reserves room for ModernBERT's [CLS]/[SEP], which get
// added to every independently-encoded chunk.
const specialTokenBudget = 2

const maxContentTokensPerChunk = maxChunkTokens - specialTokenBudget

// chunkWords splits words into chunks sized against real measured token
// counts, so that each chunk's own later independent re-tokenization
// (specials included) is expected to fit within maxChunkTokens.
//
// Kompress's real chunker instead splits fixed 350-word blocks with no
// token-count check at all; measured against all 7 Sprint 1 fixtures
// during Sprint 3 planning, 100% of its chunks exceeded 512 tokens, so
// words past tokenizer truncation got no word_id and were silently
// deleted — never scored, and never rescued by must-keep (which only sees
// words actually sent to the tokenizer). This chunker exists specifically
// to remove that failure mode: since it sizes against real per-word token
// counts instead of guessing a word count, no chunk it produces should
// ever need truncation.
//
// tokensPerWord[i] is word i's non-special token count, measured by
// Compress (windowed whole-content measurement — see its doc comment).
// Cutting at a word boundary and later re-encoding just that chunk's words
// is expected to reproduce the same per-word token count almost exactly
// for a WordPiece/BPE tokenizer (a word's own sub-tokenization doesn't
// depend on its neighbors), which is why measuring once up front is
// sufficient rather than re-measuring per chunk.
func chunkWords(words []string, tokensPerWord []int) [][]string {
	if len(words) == 0 {
		return nil
	}

	var chunks [][]string
	start := 0
	count := 0
	for i, n := range tokensPerWord {
		if count+n > maxContentTokensPerChunk && i > start {
			chunks = append(chunks, words[start:i])
			start = i
			count = 0
		}
		// A single word that alone exceeds the budget (pathological — a
		// huge unbroken token) still gets isolated into its own
		// one-word chunk rather than looping forever on it; the real
		// tokenizer truncating that one word's own sub-tokens is a
		// bounded, rare degradation, not a silent whole-sentence
		// deletion the way the original bug was.
		count += n
	}
	chunks = append(chunks, words[start:])
	return chunks
}

// scoreChunk runs the Scorer over enc's tokens, splitting into
// maxChunkTokens-sized batches if a single chunk's own encoding exceeds
// that bound. This is a hard, unconditional cap at the native-model
// boundary — not just a guard for the pathological single-word case above.
//
// chunkWords keeps every NORMAL multi-word chunk within budget by
// construction, but the single-word escape hatch (see its comment above)
// has no word boundary to cut on, so that one chunk's real token count is
// unbounded. ModernBERT's self-attention is O(n^2) in sequence length, so
// calling Scorer.Score with an unbounded input is a real memory-exhaustion
// risk, not just a correctness one: a real production incident
// (docs/v5-headroom-replacement.md Sprint 9, 2026-08-20) traced a
// restart-looping, host-threatening OOM on forge-compress@deepseek
// directly to this — DeepSeek's real traffic averages ~262K tokens/request,
// and a single unbroken run of non-whitespace content (minified JSON, a
// long diff line, base64) within that is enough to trigger it.
//
// Splitting into batches here (rather than truncating) still scores every
// token — same keep/drop aggregation multi-chunk already does, just at a
// finer grain — at the cost of the model losing whatever cross-token
// context spans a split boundary. That degradation only ever applies to
// content that already blew the per-word budget, i.e. content chunkWords
// itself already flagged as pathological; it is not a new failure mode for
// ordinary text.
func scoreChunk(scorer Scorer, enc Encoding) ([]float32, error) {
	if len(enc.IDs) <= maxChunkTokens {
		return scorer.Score(enc.IDs, enc.AttentionMask)
	}
	scores := make([]float32, 0, len(enc.IDs))
	for start := 0; start < len(enc.IDs); start += maxChunkTokens {
		end := start + maxChunkTokens
		if end > len(enc.IDs) {
			end = len(enc.IDs)
		}
		batch, err := scorer.Score(enc.IDs[start:end], enc.AttentionMask[start:end])
		if err != nil {
			return nil, err
		}
		scores = append(scores, batch...)
	}
	return scores, nil
}
