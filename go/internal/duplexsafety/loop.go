// SPDX-License-Identifier: Apache-2.0

package duplexsafety

// Config controls LoopDetector. Its zero value is completed by Default's
// values in NewLoopDetector; callers that care should start from Default.
type Config struct {
	// WindowRunes bounds the normalized stream the detector remembers.
	WindowRunes int
	// MinPhraseRunes is the shortest unit that can be called a loop. It is a
	// floor against normal short repetitions such as "no no no", "yes yes"
	// and backchannels.
	MinPhraseRunes int
	// MaxPhraseRunes caps the repeated unit considered.
	MaxPhraseRunes int
	// MinRepeats is how many back-to-back copies make a loop.
	MinRepeats int
	// MaxMismatches is the absolute cap on runes that may differ from the
	// first copy in each later copy. The zero value uses the default (2); a
	// negative value means exact matching.
	MaxMismatches int
	// MaxMismatchRatio also caps differences as a fraction of the phrase
	// length, so a short unit may never drift by a large share of its runes.
	// The zero value uses the default (0.08).
	MaxMismatchRatio float64
}

// Default is deliberately conservative. It fires only when a unit of at least
// 8 runes (lower-case, whitespace-collapsed, punctuation removed) repeats 4
// times back to back in a 2048-rune window, allowing at most 2 differing runes
// per repeat and never more than 8% of the unit.
//
// The floor keeps normal speech out: "no no no" (3-rune unit), "yes yes"
// (4), "mm-hm ... mm-hm" (4-5 after normalization) and "really, really" are
// below 8 runes; a phrase said twice or three times for emphasis is below 4
// repeats; counting and one-off lists do not repeat. The 2-rune tolerance
// catches a nearly identical loop without accepting arbitrary paraphrases. The
// bundled eval set (testdata/transcripts.json) is the regression proof: 14
// loops stop, 24 normal transcripts (including Japanese without spaces) do
// not.
func Default() Config {
	return Config{
		WindowRunes:      2048,
		MinPhraseRunes:   8,
		MaxPhraseRunes:   200,
		MinRepeats:       4,
		MaxMismatches:    2,
		MaxMismatchRatio: 0.08,
	}
}

func (c Config) withDefaults() Config {
	if c.WindowRunes <= 0 {
		c.WindowRunes = 2048
	}
	if c.MinPhraseRunes <= 0 {
		c.MinPhraseRunes = 8
	}
	if c.MaxPhraseRunes <= 0 {
		c.MaxPhraseRunes = 200
	}
	if c.MaxPhraseRunes < c.MinPhraseRunes {
		c.MaxPhraseRunes = c.MinPhraseRunes
	}
	if c.MinRepeats <= 0 {
		c.MinRepeats = 4
	}
	if c.MaxMismatches == 0 {
		c.MaxMismatches = 2
	}
	if c.MaxMismatches < 0 {
		c.MaxMismatches = 0
	}
	if c.MaxMismatchRatio <= 0 {
		c.MaxMismatchRatio = 0.08
	}
	return c
}

var loopVerdict = Verdict{Stop: true, Code: CodeLoop, Reason: "repetition loop detected"}

// LoopDetector detects a phrase repeating back to back in the accumulated text
// stream. Deltas are tiny and may split words, so it works on the normalized
// stream, never on a delta in isolation. It is intended for one session's
// goroutine.
type LoopDetector struct {
	cfg   Config
	buf   []rune
	fired bool
}

// NewLoopDetector returns a detector for one session. Pass Default() for the
// documented configuration.
func NewLoopDetector(cfg Config) *LoopDetector {
	return &LoopDetector{cfg: cfg.withDefaults()}
}

// Observe appends one text delta and returns a stop verdict when the current
// tail contains a repeated phrase. Work is bounded by Config.WindowRunes and
// the configured phrase/repeat limits, so a long session does not grow the
// cost per delta.
func (d *LoopDetector) Observe(delta string) Verdict {
	if d == nil {
		return Verdict{}
	}
	if d.fired {
		return loopVerdict
	}
	d.buf = appendNormalized(d.buf, delta)
	d.buf = trimRunes(d.buf, d.cfg.WindowRunes)
	if len(d.buf) < d.cfg.MinPhraseRunes*d.cfg.MinRepeats {
		return Verdict{}
	}
	if d.repeated(analysisView(d.buf)) {
		d.fired = true
		return loopVerdict
	}
	return Verdict{}
}

func (d *LoopDetector) repeated(s []rune) bool {
	// analysisView appends one space so space-separated units align. A script
	// with no spaces (Japanese) then ends one rune past its repeat boundary, so
	// try the view with and without that padding.
	for _, end := range []int{len(s), len(s) - 1} {
		if d.repeatedAt(s, end) {
			return true
		}
	}
	return false
}

func (d *LoopDetector) repeatedAt(s []rune, end int) bool {
	if end < d.cfg.MinPhraseRunes*d.cfg.MinRepeats {
		return false
	}
	maxL := d.cfg.MaxPhraseRunes
	if byRepeats := end / d.cfg.MinRepeats; maxL > byRepeats {
		maxL = byRepeats
	}
	for l := d.cfg.MinPhraseRunes; l <= maxL; l++ {
		allowed := d.cfg.MaxMismatches
		if byRatio := int(float64(l) * d.cfg.MaxMismatchRatio); byRatio < allowed {
			allowed = byRatio
		}
		if repeatedBlocks(s, end, l, d.cfg.MinRepeats, allowed) {
			return true
		}
	}
	return false
}

// repeatedBlocks reports whether the last k blocks of length l ending at end
// all match the first block within maxMismatches differing runes.
func repeatedBlocks(s []rune, end, l, k, maxMismatches int) bool {
	if l <= 0 || k < 2 || end < l*k {
		return false
	}
	base := end - l*k
	for repeat := 1; repeat < k; repeat++ {
		off := base + repeat*l
		mismatches := 0
		for i := 0; i < l; i++ {
			if s[base+i] != s[off+i] {
				mismatches++
				if mismatches > maxMismatches {
					return false
				}
			}
		}
	}
	return true
}
