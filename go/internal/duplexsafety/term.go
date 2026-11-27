// SPDX-License-Identifier: Apache-2.0

package duplexsafety

import "strings"

var termVerdict = Verdict{Stop: true, Code: CodeTerm, Reason: "blocked term detected"}

// TermPolicy stops a session when an operator-supplied term appears in the
// accumulated text. Matching is case-insensitive, whitespace- and
// punctuation-normalized, and respects ASCII word boundaries so a term does
// not match inside a longer word. A term may span deltas.
//
// The package ships no list: the operator supplies the configuration.
type TermPolicy struct {
	terms   []string
	maxTerm int
	buf     []rune
	fired   bool
}

// NewTermPolicy builds a policy from terms. Empty or duplicate terms are
// ignored; an empty list makes Observe a no-op.
func NewTermPolicy(terms []string) *TermPolicy {
	p := &TermPolicy{}
	seen := make(map[string]struct{}, len(terms))
	for _, raw := range terms {
		t := normalizeTerm(raw)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		p.terms = append(p.terms, t)
		if n := len([]rune(t)); n > p.maxTerm {
			p.maxTerm = n
		}
	}
	return p
}

func normalizeTerm(s string) string {
	r := appendNormalized(nil, s)
	for len(r) > 0 && r[len(r)-1] == ' ' {
		r = r[:len(r)-1]
	}
	return string(r)
}

// Observe appends one text delta and returns a stop verdict on the first
// configured term. It retains only enough text to finish a term that is split
// across deltas.
func (p *TermPolicy) Observe(delta string) Verdict {
	if p == nil || len(p.terms) == 0 {
		return Verdict{}
	}
	if p.fired {
		return termVerdict
	}
	p.buf = appendNormalized(p.buf, delta)
	keep := p.maxTerm*2 + 16
	if keep < 64 {
		keep = 64
	}
	p.buf = trimRunes(p.buf, keep)
	text := string(p.buf)
	for _, term := range p.terms {
		if containsWhole(text, term) {
			p.fired = true
			return termVerdict
		}
	}
	return Verdict{}
}

// containsWhole finds term in text and checks ASCII word boundaries. Only
// ASCII letters and digits are word characters, so a CJK term still matches
// inside an unspaced sentence while an English term does not match inside a
// longer English word.
func containsWhole(text, term string) bool {
	for from := 0; from <= len(text)-len(term); {
		i := strings.Index(text[from:], term)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(term)
		if !asciiWordAt(text, i-1) && !asciiWordAt(text, end) {
			return true
		}
		from = i + 1
	}
	return false
}

func asciiWordAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c < 128 && (c >= 'a' && c <= 'z' || c >= '0' && c <= '9')
}
