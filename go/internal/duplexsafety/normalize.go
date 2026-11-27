// SPDX-License-Identifier: Apache-2.0

package duplexsafety

import (
	"strings"
	"unicode"
)

// fillerTokens are hesitation sounds removed before loop analysis so that a
// loop with an occasional "um" or "uh" still lines up. Only standalone tokens
// are removed; the set is deliberately small because a broad list would erase
// ordinary words. Backchannels therefore also sit below the detector's phrase
// floor rather than becoming false repetitions.
var fillerTokens = map[string]struct{}{
	"ah":  {},
	"er":  {},
	"erm": {},
	"hm":  {},
	"hmm": {},
	"mm":  {},
	"mhm": {},
	"uh":  {},
	"uhm": {},
	"um":  {},
}

// appendNormalized lower-cases delta, maps punctuation, symbols and whitespace
// to single spaces, and appends the result to buf. It never creates a leading
// space or runs of spaces, but preserves one trailing space so a later delta
// that starts a word is separated correctly.
func appendNormalized(buf []rune, delta string) []rune {
	for _, r := range delta {
		r = unicode.ToLower(r)
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			if len(buf) > 0 && buf[len(buf)-1] != ' ' {
				buf = append(buf, ' ')
			}
			continue
		}
		buf = append(buf, r)
	}
	return buf
}

// trimRunes keeps at most the last max runes, copying so the discarded prefix
// can be collected. It drops a leading partial space from the new window.
func trimRunes(buf []rune, max int) []rune {
	if max <= 0 || len(buf) <= max {
		return buf
	}
	out := make([]rune, max)
	copy(out, buf[len(buf)-max:])
	if len(out) > 0 && out[0] == ' ' {
		out = out[1:]
	}
	return out
}

// analysisView returns the filler-stripped words of buf joined by one space,
// with a trailing space. The trailing space makes every repetition a
// fixed-length unit: "a b " repeated is "a b a b a b a b ", so block
// comparison does not drift at the last repeat.
func analysisView(buf []rune) []rune {
	words := strings.Fields(string(buf))
	out := make([]rune, 0, len(buf)+1)
	for _, w := range words {
		if _, ok := fillerTokens[w]; ok {
			continue
		}
		if len(out) > 0 {
			out = append(out, ' ')
		}
		out = append(out, []rune(w)...)
	}
	if len(out) > 0 {
		out = append(out, ' ')
	}
	return out
}
