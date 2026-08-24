// SPDX-License-Identifier: Apache-2.0

package compress

import (
	"fmt"
	"testing"
)

// countsFromEnc mirrors Compress's windowed measurement: tokensPerWord from
// a whole-content encoding. Test-local convenience for building the input
// chunkWords has taken since S3's windowed-measurement change.
func countsFromEnc(words []string, enc Encoding) []int {
	tpw := make([]int, len(words))
	for _, wi := range enc.WordIndex {
		if wi >= 0 && wi < len(words) {
			tpw[wi]++
		}
	}
	return tpw
}

func TestChunkWords_RespectsTokenBudget(t *testing.T) {
	// 2000 words, each producing 1 token — should split into chunks of at
	// most maxContentTokensPerChunk words each, never exceeding budget.
	words := make([]string, 2000)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	tok := fakeTokenizer{}
	enc, err := tok.EncodeWords(words)
	if err != nil {
		t.Fatal(err)
	}
	chunks := chunkWords(words, countsFromEnc(words, enc))

	total := 0
	for _, c := range chunks {
		if len(c) > maxContentTokensPerChunk {
			t.Errorf("chunk of %d words exceeds budget %d", len(c), maxContentTokensPerChunk)
		}
		total += len(c)
	}
	if total != len(words) {
		t.Errorf("chunks cover %d words, want %d (words lost or duplicated)", total, len(words))
	}
}

func TestChunkWords_NeverTruncatesLikeTheOriginalBug(t *testing.T) {
	// Reproduces the exact shape of the bug found during Sprint 3
	// planning: words dense enough that a fixed-350-word chunker would
	// exceed 512 tokens per chunk (e.g. CJK-heavy content where many
	// "words" are multi-token). Each word here produces 3 tokens, so 350
	// words would be 1050 tokens — well past the 512 budget under the old
	// scheme. Assert the token-budget chunker never produces an
	// over-budget chunk regardless of words-per-token density.
	words := make([]string, 400)
	for i := range words {
		words[i] = fmt.Sprintf("w%d", i)
	}
	tok := fakeTokenizer{tokensPerWord: func(string) int { return 3 }}
	enc, err := tok.EncodeWords(words)
	if err != nil {
		t.Fatal(err)
	}
	chunks := chunkWords(words, countsFromEnc(words, enc))
	if len(chunks) < 3 {
		t.Errorf("expected at least 3 chunks for 400 words at 3 tokens/word (1200 content tokens / 510 budget), got %d", len(chunks))
	}
	seen := 0
	for _, c := range chunks {
		tokens := len(c) * 3
		if tokens > maxContentTokensPerChunk {
			t.Errorf("chunk of %d words = %d tokens exceeds budget %d", len(c), tokens, maxContentTokensPerChunk)
		}
		seen += len(c)
	}
	if seen != len(words) {
		t.Errorf("chunks cover %d words, want %d", seen, len(words))
	}
}

func TestChunkWords_PathologicalSingleWordDoesNotHang(t *testing.T) {
	// A single word whose own token count exceeds the whole budget must
	// still terminate, isolated in its own chunk, rather than looping
	// forever or panicking.
	words := []string{"normal", "hugeword", "normal2"}
	tok := fakeTokenizer{tokensPerWord: func(w string) int {
		if w == "hugeword" {
			return maxContentTokensPerChunk + 100
		}
		return 1
	}}
	enc, err := tok.EncodeWords(words)
	if err != nil {
		t.Fatal(err)
	}
	chunks := chunkWords(words, countsFromEnc(words, enc))
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != len(words) {
		t.Errorf("chunks cover %d words, want %d", total, len(words))
	}
	foundHuge := false
	for _, c := range chunks {
		for _, w := range c {
			if w == "hugeword" {
				foundHuge = true
			}
		}
	}
	if !foundHuge {
		t.Error("the oversized word was lost entirely, not just left over-budget")
	}
}

func TestScoreChunk_SplitsOversizedEncoding(t *testing.T) {
	// Reproduces the 2026-08-20 deepseek OOM incident's exact shape: an
	// encoding whose token count blows past maxChunkTokens (the pathological
	// single-word case chunkWords can't itself bound — see its comment).
	// The Scorer must never see more than maxChunkTokens tokens in one call.
	n := maxChunkTokens*3 + 17
	ids := make([]int64, n)
	mask := make([]int64, n)
	for i := range ids {
		ids[i] = int64(i)
		mask[i] = 1
	}
	enc := Encoding{IDs: ids, AttentionMask: mask}

	var maxSeen int
	var calls int
	sc := fakeScorerFunc(func(inputIDs, attentionMask []int64) ([]float32, error) {
		calls++
		if len(inputIDs) != len(attentionMask) {
			t.Fatalf("batch %d: ids/mask length mismatch: %d vs %d", calls, len(inputIDs), len(attentionMask))
		}
		if len(inputIDs) > maxSeen {
			maxSeen = len(inputIDs)
		}
		if len(inputIDs) > maxChunkTokens {
			t.Fatalf("batch %d: scorer received %d tokens, want <= %d (this is the OOM-triggering shape)", calls, len(inputIDs), maxChunkTokens)
		}
		return make([]float32, len(inputIDs)), nil
	})

	scores, err := scoreChunk(sc, enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != n {
		t.Errorf("scoreChunk returned %d scores, want %d (must cover every token, none dropped)", len(scores), n)
	}
	wantCalls := (n + maxChunkTokens - 1) / maxChunkTokens
	if calls != wantCalls {
		t.Errorf("scorer called %d times, want %d", calls, wantCalls)
	}
	if maxSeen > maxChunkTokens {
		t.Errorf("largest single batch was %d tokens, want <= %d", maxSeen, maxChunkTokens)
	}
}

func TestScoreChunk_UnderBudgetIsOneCall(t *testing.T) {
	enc := Encoding{IDs: []int64{1, 2, 3}, AttentionMask: []int64{1, 1, 1}}
	calls := 0
	sc := fakeScorerFunc(func(inputIDs, _ []int64) ([]float32, error) {
		calls++
		return make([]float32, len(inputIDs)), nil
	})
	if _, err := scoreChunk(sc, enc); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("scorer called %d times for an under-budget chunk, want exactly 1 (no unnecessary splitting)", calls)
	}
}

func TestChunkWords_Empty(t *testing.T) {
	if got := chunkWords(nil, nil); got != nil {
		t.Errorf("chunkWords(nil, ...) = %v, want nil", got)
	}
}

func TestChunkWords_SingleSmallChunk(t *testing.T) {
	words := []string{"a", "b", "c"}
	tok := fakeTokenizer{}
	enc, _ := tok.EncodeWords(words)
	chunks := chunkWords(words, countsFromEnc(words, enc))
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	if len(chunks[0]) != 3 {
		t.Errorf("got %d words in the one chunk, want 3", len(chunks[0]))
	}
}
