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

// recordingBatchScorer records every ScoreBatch call's batch size and
// returns one all-zero-score slice per input sequence, matching each
// sequence's own length. Score is never expected to be called by these
// tests (scoreEncodings only falls back to it for an oversized single
// encoding) — it panics if it is, so a wiring mistake fails loudly instead
// of silently passing.
type recordingBatchScorer struct {
	batchSizes []int
}

func (r *recordingBatchScorer) Score(inputIDs, _ []int64) ([]float32, error) {
	panic("recordingBatchScorer.Score called unexpectedly — scoreEncodings should batch this input")
}

func (r *recordingBatchScorer) ScoreBatch(inputIDs, attentionMask [][]int64) ([][]float32, error) {
	if len(inputIDs) != len(attentionMask) {
		panic("ids/mask batch length mismatch")
	}
	r.batchSizes = append(r.batchSizes, len(inputIDs))
	out := make([][]float32, len(inputIDs))
	for i, ids := range inputIDs {
		// Score is the sequence's own index+1 (never 0), so tests can
		// verify per-sequence results land back at the right position.
		s := make([]float32, len(ids))
		for j := range s {
			s[j] = float32(i + 1)
		}
		out[i] = s
	}
	return out, nil
}

func TestScoreEncodings_GroupsUpToBatchSize(t *testing.T) {
	// 10 small encodings, batchSize 4 -> batches of 4, 4, 2.
	encs := make([]Encoding, 10)
	for i := range encs {
		encs[i] = Encoding{IDs: []int64{1, 2, 3}, AttentionMask: []int64{1, 1, 1}}
	}
	sc := &recordingBatchScorer{}
	if _, err := scoreEncodings(sc, encs, 4); err != nil {
		t.Fatal(err)
	}
	want := []int{4, 4, 2}
	if len(sc.batchSizes) != len(want) {
		t.Fatalf("ScoreBatch called %d times with sizes %v, want %d calls sized %v", len(sc.batchSizes), sc.batchSizes, len(want), want)
	}
	for i := range want {
		if sc.batchSizes[i] != want[i] {
			t.Errorf("batch %d size = %d, want %d (sizes: %v)", i, sc.batchSizes[i], want[i], sc.batchSizes)
		}
	}
}

func TestScoreEncodings_PreservesOrderAcrossBatches(t *testing.T) {
	// 5 encodings, batchSize 2 -> 3 batches. Each result must land back at
	// its ORIGINAL index regardless of which batch it was scored in.
	encs := make([]Encoding, 5)
	for i := range encs {
		encs[i] = Encoding{IDs: []int64{1}, AttentionMask: []int64{1}}
	}
	sc := &recordingBatchScorer{}
	scores, err := scoreEncodings(sc, encs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 5 {
		t.Fatalf("got %d results, want 5", len(scores))
	}
	// Batches are [0,1], [2,3], [4] — within-batch sequence index+1 gives
	// scores 1,2 / 1,2 / 1 respectively, NOT a monotonically increasing
	// global sequence — this pins down that scoreEncodings doesn't
	// accidentally assume batch-local index == global index.
	want := []float32{1, 2, 1, 2, 1}
	for i, w := range want {
		if len(scores[i]) != 1 || scores[i][0] != w {
			t.Errorf("scores[%d] = %v, want [%v]", i, scores[i], w)
		}
	}
}

func TestScoreEncodings_OversizedEncodingBypassesBatchingWithoutDisruptingNeighbors(t *testing.T) {
	// A middle encoding exceeds maxChunkTokens (the pathological
	// single-huge-word case) — it must go through scoreChunk's existing
	// single-item sub-batch path (via Score, not ScoreBatch), and must not
	// get silently folded into a surrounding ScoreBatch call, nor prevent
	// the normal encodings before/after it from still being batched
	// together with each other.
	normal := Encoding{IDs: []int64{1, 2}, AttentionMask: []int64{1, 1}}
	oversized := Encoding{
		IDs:           make([]int64, maxChunkTokens+10),
		AttentionMask: make([]int64, maxChunkTokens+10),
	}
	for i := range oversized.IDs {
		oversized.IDs[i] = int64(i)
		oversized.AttentionMask[i] = 1
	}
	encs := []Encoding{normal, normal, oversized, normal, normal}

	var scoreCalls, scoreBatchCalls int
	var maxScoreBatchLen int
	sc := fakeScorerFunc(func(inputIDs, attentionMask []int64) ([]float32, error) {
		scoreCalls++
		if len(inputIDs) > maxChunkTokens {
			t.Fatalf("Score received %d tokens, want <= %d", len(inputIDs), maxChunkTokens)
		}
		return make([]float32, len(inputIDs)), nil
	})
	// Wrap to also count/inspect ScoreBatch calls, since fakeScorerFunc's
	// own ScoreBatch just loops Score — swap in a small local wrapper that
	// delegates but records.
	wrapped := recordingWrapper{inner: sc, onBatch: func(n int) {
		scoreBatchCalls++
		if n > maxScoreBatchLen {
			maxScoreBatchLen = n
		}
	}}

	scores, err := scoreEncodings(wrapped, encs, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != len(encs) {
		t.Fatalf("got %d results, want %d", len(scores), len(encs))
	}
	if len(scores[2]) != len(oversized.IDs) {
		t.Errorf("oversized encoding's score length = %d, want %d (must cover every token)", len(scores[2]), len(oversized.IDs))
	}
	// The oversized encoding forces a flush before/after it, so the 4
	// normal encodings split into two ScoreBatch calls of 2 (indices 0,1
	// then 3,4) rather than one call of 4 spanning across it.
	if scoreBatchCalls != 2 {
		t.Errorf("ScoreBatch called %d times, want 2 (flushed around the oversized encoding)", scoreBatchCalls)
	}
	if maxScoreBatchLen != 2 {
		t.Errorf("largest ScoreBatch call = %d items, want 2 (never spans the oversized encoding)", maxScoreBatchLen)
	}
	// scoreChunk splits the oversized encoding into ceil((maxChunkTokens+10)/maxChunkTokens) = 2 Score calls.
	if scoreCalls != 2 {
		t.Errorf("Score called %d times, want 2 (scoreChunk's sub-batching of the oversized encoding)", scoreCalls)
	}
}

// recordingWrapper adapts a fakeScorerFunc into a Scorer whose ScoreBatch
// calls onBatch(len) before delegating per-item to the wrapped Score func —
// lets a test both assert ScoreBatch call shape AND reuse fakeScorerFunc's
// existing Score-call assertions.
type recordingWrapper struct {
	inner   fakeScorerFunc
	onBatch func(n int)
}

func (r recordingWrapper) Score(inputIDs, attentionMask []int64) ([]float32, error) {
	return r.inner(inputIDs, attentionMask)
}

func (r recordingWrapper) ScoreBatch(inputIDs, attentionMask [][]int64) ([][]float32, error) {
	r.onBatch(len(inputIDs))
	// Deliberately does NOT delegate to r.inner (Score) — that's reserved
	// for scoreChunk's oversized-encoding sub-batching, so a test can count
	// Score calls and ScoreBatch calls as two independent signals.
	out := make([][]float32, len(inputIDs))
	for i, ids := range inputIDs {
		out[i] = make([]float32, len(ids))
	}
	return out, nil
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
