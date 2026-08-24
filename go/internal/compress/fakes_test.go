// SPDX-License-Identifier: Apache-2.0

package compress

// fakeTokenizer is a deterministic, cheap stand-in for the real ModernBERT
// tokenizer (hftokenizer, cgo-backed, tested separately against the real
// model on ForgeHost). It doesn't resemble ModernBERT's real subword
// vocabulary — what's under test here is this package's own orchestration
// logic (chunk boundaries, must-keep override, word aggregation,
// determinism), not tokenizer fidelity. Token ids encode their own word
// index (id/1000) so fakeScorer can make keep/drop decisions per word
// without needing a real vocabulary either.
type fakeTokenizer struct {
	// tokensPerWord, when set, returns how many sub-tokens a word
	// produces (default 1). Lets tests construct words that individually
	// blow the chunk budget.
	tokensPerWord func(word string) int
}

const (
	fakeCLS = -1
	fakeSEP = -2
)

func (f fakeTokenizer) EncodeWords(words []string) (Encoding, error) {
	ids := []int64{fakeCLS}
	mask := []int64{1}
	widx := []int{-1}
	for i, w := range words {
		n := 1
		if f.tokensPerWord != nil {
			if got := f.tokensPerWord(w); got > 0 {
				n = got
			}
		}
		for k := 0; k < n; k++ {
			ids = append(ids, int64(i)*1000+int64(k))
			mask = append(mask, 1)
			widx = append(widx, i)
		}
	}
	ids = append(ids, fakeSEP)
	mask = append(mask, 1)
	widx = append(widx, -1)
	return Encoding{IDs: ids, AttentionMask: mask, WordIndex: widx}, nil
}

// fakeScorer keeps a token iff keepWordIndex(word index) says so (default:
// keep everything). The word index is recovered from the id fakeTokenizer
// assigned (id/1000), not from a real model score.
type fakeScorer struct {
	keepWordIndex func(wordIndex int) bool
}

// fakeScorerFunc adapts a plain func to the Scorer interface, for tests
// that need to assert something about each individual call (e.g. the
// batch size scoreChunk actually sends) rather than a keep/drop policy.
type fakeScorerFunc func(inputIDs, attentionMask []int64) ([]float32, error)

func (f fakeScorerFunc) Score(inputIDs, attentionMask []int64) ([]float32, error) {
	return f(inputIDs, attentionMask)
}

func (f fakeScorer) Score(inputIDs, _ []int64) ([]float32, error) {
	scores := make([]float32, len(inputIDs))
	for i, id := range inputIDs {
		if id < 0 { // special token
			continue
		}
		wi := int(id / 1000)
		keep := true
		if f.keepWordIndex != nil {
			keep = f.keepWordIndex(wi)
		}
		if keep {
			scores[i] = 1
		}
	}
	return scores, nil
}

func newTestEngine(tok fakeTokenizer, sc fakeScorer, cfgOverrides ...func(*Config)) *Engine {
	cfg := Config{ScoreThreshold: 0.5, MinWords: 1, ByteThreshold: 0}
	for _, o := range cfgOverrides {
		o(&cfg)
	}
	return &Engine{Tokenizer: tok, Scorer: sc, Config: cfg}
}
