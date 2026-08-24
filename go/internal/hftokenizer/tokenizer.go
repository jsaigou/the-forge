// SPDX-License-Identifier: Apache-2.0

// Package hftokenizer wraps daulet/tokenizers (a cgo binding to Hugging
// Face's Rust `tokenizers` crate) to satisfy compress.Tokenizer with the
// real ModernBERT tokenizer (answerdotai/ModernBERT-base's tokenizer.json
// — the tokenizer chopratejas/kompress-v2-base was trained against).
//
// Isolated in its own package specifically so forge's own pure-Go build
// never imports it (import "C" only triggers cgo for packages that use it)
// — see docs/v5-headroom-replacement.md Sprint 3's "Build & deploy"
// section for why that isolation matters.
//
// daulet/tokenizers only exposes single-string encoding with byte offsets
// (WithReturnOffsets), not Python's is_split_into_words=True + word_ids()
// API the original Kompress implementation depends on. EncodeWords
// reconstructs the same word alignment by joining words with a single
// space (mirroring Kompress's own words = content.split() round trip) and
// mapping each token's byte offset back to the word span that contains it.
// Verified live (this sprint) against the real tokenizer.json that
// Offsets are byte offsets into the original (pre-normalization) input —
// e.g. encoding "hello 104件 world" gives the CJK token "件" offset
// [9,12), the correct 3-byte UTF-8 span — so plain Go byte-slicing is
// sufficient with no rune-index translation.
package hftokenizer

import (
	"fmt"
	"strings"

	"github.com/daulet/tokenizers"

	"github.com/jsaigou/the-forge/internal/compress"
)

// Tokenizer wraps one loaded HF fast tokenizer.
type Tokenizer struct {
	tk *tokenizers.Tokenizer
}

var _ compress.Tokenizer = (*Tokenizer)(nil)

// New loads a tokenizer.json from disk (e.g. ModernBERT-base's).
func New(path string) (*Tokenizer, error) {
	tk, err := tokenizers.FromFile(path)
	if err != nil {
		return nil, fmt.Errorf("hftokenizer: load %s: %w", path, err)
	}
	return &Tokenizer{tk: tk}, nil
}

// Close releases the underlying Rust tokenizer.
func (t *Tokenizer) Close() error {
	return t.tk.Close()
}

// EncodeWords implements compress.Tokenizer. See the package doc for the
// offset-based word-alignment reconstruction this depends on.
func (t *Tokenizer) EncodeWords(words []string) (compress.Encoding, error) {
	if len(words) == 0 {
		return compress.Encoding{}, nil
	}

	spans := wordSpans(words)
	joined := strings.Join(words, " ")

	enc, err := t.tk.EncodeWithOptionsErr(joined, true,
		tokenizers.WithReturnOffsets(),
		tokenizers.WithReturnSpecialTokensMask(),
	)
	if err != nil {
		return compress.Encoding{}, fmt.Errorf("hftokenizer: encode: %w", err)
	}

	out := compress.Encoding{
		IDs:           make([]int64, len(enc.IDs)),
		AttentionMask: make([]int64, len(enc.IDs)),
		WordIndex:     make([]int, len(enc.IDs)),
	}

	wi := 0
	for i, id := range enc.IDs {
		out.IDs[i] = int64(id)
		// This engine never pads (one chunk = one batch-of-one forward
		// pass — see compress.Encoding's doc comment), so every real
		// token's mask is 1 regardless of what the library itself reports.
		out.AttentionMask[i] = 1

		isSpecial := i < len(enc.SpecialTokensMask) && enc.SpecialTokensMask[i] != 0
		off := enc.Offsets[i]
		if isSpecial || off[1] <= off[0] {
			out.WordIndex[i] = -1
			continue
		}
		// The token's last byte (off[1]-1) always falls inside its own
		// word's span, even when off[0] reaches back into the preceding
		// separator space (byte-level BPE's "Ġ" leading-space prefix
		// convention) — see the package doc's worked example.
		last := int(off[1]) - 1
		for wi < len(spans)-1 && last >= spans[wi].end {
			wi++
		}
		out.WordIndex[i] = wi
	}
	return out, nil
}

type span struct{ start, end int }

// wordSpans returns each word's byte range within strings.Join(words, " ")
// — a plain cumulative sum, since a single ASCII space always separates
// words in that join regardless of any word's own byte width.
func wordSpans(words []string) []span {
	spans := make([]span, len(words))
	pos := 0
	for i, w := range words {
		spans[i] = span{start: pos, end: pos + len(w)}
		pos += len(w) + 1 // +1 for the joining space
	}
	return spans
}
