// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"fmt"
	"strings"
	"testing"
)

func TestCompress_ByteThresholdPassthrough(t *testing.T) {
	e := newTestEngine(fakeTokenizer{}, fakeScorer{}, func(c *Config) {
		c.ByteThreshold = 2048
		c.MinWords = 1
	})
	content := "short content well under the byte threshold"
	res, err := e.Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passthrough {
		t.Error("want Passthrough=true for content under ByteThreshold")
	}
	if res.Compressed != content {
		t.Errorf("Compressed = %q, want unchanged %q", res.Compressed, content)
	}
}

func TestCompress_MinWordsPassthrough(t *testing.T) {
	e := newTestEngine(fakeTokenizer{}, fakeScorer{}, func(c *Config) {
		c.ByteThreshold = 0
		c.MinWords = 10
	})
	content := "only four words here"
	res, err := e.Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passthrough {
		t.Error("want Passthrough=true for content under MinWords")
	}
}

func TestCompress_DropsLowScoringWords(t *testing.T) {
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	content := strings.Join(words, " ")
	// Drop every odd-indexed word.
	e := newTestEngine(fakeTokenizer{}, fakeScorer{keepWordIndex: func(wi int) bool { return wi%2 == 0 }},
		func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 })
	res, err := e.Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passthrough {
		t.Fatal("want a real compression, not passthrough")
	}
	got := strings.Fields(res.Compressed)
	want := []string{"alpha", "gamma", "epsilon", "eta", "iota"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Compressed words = %v, want %v", got, want)
	}
}

func TestCompress_MustKeepSurvivesLowScore(t *testing.T) {
	// "104件" is must-keep shaped (CJK-glued number) — even if the model
	// scores it below threshold, it must survive.
	content := strings.Repeat("filler ", 12) + "104件"
	e := newTestEngine(fakeTokenizer{}, fakeScorer{keepWordIndex: func(int) bool { return false }},
		func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 })
	res, err := e.Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passthrough {
		t.Fatal("want a real compression, not passthrough (must-keep should have rescued one word)")
	}
	if !strings.Contains(res.Compressed, "104件") {
		t.Errorf("Compressed = %q, must contain the must-keep word 104件", res.Compressed)
	}
}

func TestCompress_TokenCounts(t *testing.T) {
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	content := strings.Join(words, " ")

	t.Run("real compression reports real kept-token counts", func(t *testing.T) {
		e := newTestEngine(fakeTokenizer{}, fakeScorer{keepWordIndex: func(wi int) bool { return wi%2 == 0 }},
			func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 })
		res, err := e.Compress(content)
		if err != nil {
			t.Fatal(err)
		}
		if res.OriginalTokens != len(words) {
			t.Errorf("OriginalTokens = %d, want %d", res.OriginalTokens, len(words))
		}
		if res.CompressedTokens != 5 {
			t.Errorf("CompressedTokens = %d, want 5 (words at even indices)", res.CompressedTokens)
		}
	})

	t.Run("all-dropped passthrough reports zero savings, not a fake token count", func(t *testing.T) {
		e := newTestEngine(fakeTokenizer{}, fakeScorer{keepWordIndex: func(int) bool { return false }},
			func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 })
		res, err := e.Compress(content)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Passthrough {
			t.Fatal("expected passthrough")
		}
		if res.OriginalTokens != len(words) {
			t.Errorf("OriginalTokens = %d, want %d", res.OriginalTokens, len(words))
		}
		if res.CompressedTokens != res.OriginalTokens {
			t.Errorf("CompressedTokens = %d, want == OriginalTokens (%d) for a genuinely-unchanged passthrough", res.CompressedTokens, res.OriginalTokens)
		}
	})

	t.Run("byte/word-threshold passthrough never tokenizes, reports zero not a guess", func(t *testing.T) {
		e := newTestEngine(fakeTokenizer{}, fakeScorer{}, func(c *Config) {
			c.ByteThreshold = 1 << 20
			c.MinWords = 1
		})
		res, err := e.Compress(content)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Passthrough {
			t.Fatal("expected passthrough")
		}
		if res.OriginalTokens != 0 || res.CompressedTokens != 0 {
			t.Errorf("expected zero token counts for an untokenized byte-threshold passthrough, got Original=%d Compressed=%d", res.OriginalTokens, res.CompressedTokens)
		}
	})
}

func TestCompress_AllDroppedFallsBackToPassthrough(t *testing.T) {
	words := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta", "iota", "kappa"}
	content := strings.Join(words, " ")
	e := newTestEngine(fakeTokenizer{}, fakeScorer{keepWordIndex: func(int) bool { return false }},
		func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 })
	res, err := e.Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passthrough {
		t.Error("want Passthrough=true when every word is dropped and none is must-keep")
	}
	if res.Compressed != content {
		t.Errorf("Compressed = %q, want unchanged %q", res.Compressed, content)
	}
}

func TestCompress_MultiChunkAggregation(t *testing.T) {
	// Force multiple chunks (3 tokens/word, 400 words -> several chunks,
	// see chunk_test.go) and verify a word's fate is determined by its
	// CONTENT, not by which chunk (and therefore which chunk-local token
	// id) it happened to land in. The scorer here drops every word
	// (score-based keep never fires), so only must-keep-shaped words can
	// survive — three of them are planted at global positions 0, 150, and
	// 399, deliberately spread across what chunk.go will split into
	// different chunks, so this also exercises the wordOffset+i mapping
	// back to the correct global word.
	//
	// This test originally tried to key survival off a per-chunk
	// positional token id, which doesn't work: fakeTokenizer (like the
	// real tokenizer) assigns ids fresh per independently-tokenized
	// chunk, so the same "id/1000 == 150" condition matched a different
	// word in every chunk rather than the one intended global word —
	// caught by this test actually failing with 5 survivors instead of
	// 3, not a bug in compress.go itself. Using must-keep content
	// (ALLCAPS, which the model score can't override) instead of a
	// model-score position sidesteps that entirely.
	words := make([]string, 400)
	for i := range words {
		words[i] = base26Word(i)
	}
	words[0] = "TARGETA"
	words[150] = "TARGETB"
	words[399] = "TARGETC"
	content := strings.Join(words, " ")
	tok := fakeTokenizer{tokensPerWord: func(string) int { return 3 }}
	sc := fakeScorer{keepWordIndex: func(int) bool { return false }}
	e := newTestEngine(tok, sc, func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 })

	res, err := e.Compress(content)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(res.Compressed)
	want := []string{"TARGETA", "TARGETB", "TARGETC"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Compressed words = %v, want %v (chunk aggregation must map back to global word index)", got, want)
	}
}

// TestCompress_PathologicalSingleWordNeverOversizesScorerCall reproduces
// the 2026-08-20 deepseek OOM incident end to end: a single word (no
// whitespace boundary chunkWords can cut on) whose own token count blows
// past maxChunkTokens, mixed into otherwise-ordinary content. Before the
// scoreChunk fix, this word's full, unbounded encoding went straight to
// Scorer.Score — a real production host briefly exhausted 123GB of RAM in
// well under a minute because of it. See chunk.go's scoreChunk doc comment.
func TestCompress_PathologicalSingleWordNeverOversizesScorerCall(t *testing.T) {
	words := make([]string, 20)
	for i := range words {
		words[i] = base26Word(i)
	}
	words[10] = "hugeword"
	content := strings.Join(words, " ")

	tok := fakeTokenizer{tokensPerWord: func(w string) int {
		if w == "hugeword" {
			return maxChunkTokens*5 + 3 // e.g. a giant unbroken base64/JSON blob
		}
		return 1
	}}
	sc := fakeScorerFunc(func(inputIDs, _ []int64) ([]float32, error) {
		if len(inputIDs) > maxChunkTokens {
			t.Fatalf("scorer received %d tokens in one call, want <= %d — this is exactly the shape that OOM'd production", len(inputIDs), maxChunkTokens)
		}
		return make([]float32, len(inputIDs)), nil
	})
	e := &Engine{Tokenizer: tok, Scorer: sc, Config: Config{ScoreThreshold: 0.5, ByteThreshold: 0, MinWords: 1}}

	if _, err := e.Compress(content); err != nil {
		t.Fatal(err)
	}
}

// base26Word turns i into a digit-free, spreadsheet-column-style
// identifier (0->"a", 1->"b", ..., 25->"z", 26->"aa", ...) — used by tests
// that need many distinct fake words guaranteed not to accidentally match
// the must-keep "standalone number" pattern.
func base26Word(i int) string {
	if i < 0 {
		panic("base26Word: negative index")
	}
	var b []byte
	for {
		b = append([]byte{byte('a' + i%26)}, b...)
		i = i/26 - 1
		if i < 0 {
			break
		}
	}
	return string(b)
}

// TestCompress_FrozenPrefix asserts decision 5's frozen-prefix contract:
// Compress is a pure function of its content argument, with no dependency
// on call order, prior calls, or process state. This is what lets a
// caller fail open on a slow/erroring compression without ever risking a
// *different* compression decision for content that already made it
// through on an earlier turn — the property a provider's prompt-cache
// prefix depends on staying byte-identical across retries.
func TestCompress_FrozenPrefix(t *testing.T) {
	e := newTestEngine(
		fakeTokenizer{tokensPerWord: func(w string) int { return 1 + len(w)%3 }},
		fakeScorer{keepWordIndex: func(wi int) bool { return wi%3 != 0 }},
		func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 },
	)

	frozen := strings.Repeat("the quick brown fox jumps over the lazy dog 104件 ", 40)

	first, err := e.Compress(frozen)
	if err != nil {
		t.Fatal(err)
	}

	// Interleave a burst of unrelated calls on the SAME engine instance,
	// simulating concurrent/later requests against the same
	// process-resident, cache-free engine — nothing about them should be
	// able to change the frozen content's own result.
	for i := 0; i < 25; i++ {
		other := fmt.Sprintf("unrelated content number %d with some words 42 and more", i)
		if _, err := e.Compress(other); err != nil {
			t.Fatal(err)
		}
	}

	second, err := e.Compress(frozen)
	if err != nil {
		t.Fatal(err)
	}

	if first.Compressed != second.Compressed {
		t.Errorf("frozen-prefix violation: same content produced different output across calls\nfirst:  %q\nsecond: %q", first.Compressed, second.Compressed)
	}
	if first.Passthrough != second.Passthrough {
		t.Errorf("frozen-prefix violation: Passthrough flag differed (%v vs %v)", first.Passthrough, second.Passthrough)
	}
}

// TestCompress_FrozenPrefix_IndependentOfSurroundingMessages guards
// against a specific real trap: a design that decided what to compress
// based on message *position* (e.g. "only compress if not among the last N
// messages") would recompress the same tool output differently as the
// conversation grows around it, silently busting a provider's cached
// prefix. This engine has no such notion — Compress only ever sees the
// one message's content, never the surrounding conversation — but the test
// exists so a future change that threads position/context into Compress's
// signature has to consciously break this guarantee, not do it by
// accident.
func TestCompress_FrozenPrefix_IndependentOfSurroundingMessages(t *testing.T) {
	e := newTestEngine(
		fakeTokenizer{},
		fakeScorer{keepWordIndex: func(wi int) bool { return wi%2 == 0 }},
		func(c *Config) { c.ByteThreshold = 0; c.MinWords = 1 },
	)
	msg := strings.Repeat("alpha beta gamma delta ", 10)

	asFirstMessage, err := e.Compress(msg)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the same message arriving as, e.g., the 21st message in a
	// long-running conversation — Compress's signature has no way to
	// express that, which is the point.
	for i := 0; i < 20; i++ {
		if _, err := e.Compress(fmt.Sprintf("prior turn %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	asLaterMessage, err := e.Compress(msg)
	if err != nil {
		t.Fatal(err)
	}
	if asFirstMessage.Compressed != asLaterMessage.Compressed {
		t.Errorf("same message content compressed differently depending on conversation position: %q vs %q",
			asFirstMessage.Compressed, asLaterMessage.Compressed)
	}
}
