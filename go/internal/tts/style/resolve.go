// SPDX-License-Identifier: Apache-2.0

package style

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// BaseTag is a tag applied to the whole line as a default.
type BaseTag struct {
	Tag      string  `json:"tag"`
	Strength float64 `json:"strength"`
}

// Defaults are the settings a client sends with a request (voice-studio merges its global and per-voice settings into one of these). An inline tag overrides them:
// an inline emotion replaces a baseline emotion inside its range, an inline age replaces the default age.
type Defaults struct {
	// Age is the line's default age setting, in the tag's own units (about -1.5 to +1.5).
	Age *float64 `json:"age,omitempty"`
	// Base are tags applied to the whole line.
	Base []BaseTag `json:"base,omitempty"`
	// Mult scales a tag's strength everywhere it is used (a voice that takes anger badly can have 0.5).
	Mult map[string]float64 `json:"mult,omitempty"`
	// Disabled tags are ignored wherever they appear (so a voice that loses itself under age can have age switched off).
	Disabled []string `json:"disabled,omitempty"`
	// Rate is the line's speaking rate in bytes of text per second; nil leaves the engine's setting.
	Rate *float64 `json:"rate,omitempty"`
}

// Span is a range of the spoken text with the hidden-space controls to apply over it. Spans never overlap.
type Span struct {
	Start    int                `json:"start"`
	End      int                `json:"end"`
	Controls map[string]float64 `json:"controls"`
}

// Resolved is the plan for one line.
type Resolved struct {
	Spoken string `json:"spoken"`
	// Global are hidden-space controls over the whole line (baseline tags nobody overrides). Raw are embedding-space controls (age): whole line, embedding mode.
	Global map[string]float64 `json:"global,omitempty"`
	Raw    map[string]float64 `json:"raw,omitempty"`
	Spans  []Span             `json:"spans,omitempty"`
	// Rate is the line's rate from the defaults (nil = the engine's setting). Unpin is set when a tempo tag is present: the pinned rate must then be left off.
	Rate  *float64 `json:"rate,omitempty"`
	Unpin bool     `json:"unpin_rate,omitempty"`
	// Applied describes what was asked for, in words ("happy:3.0@[12,40)"), for response headers and previews.
	Applied  []string `json:"applied,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Active reports whether the plan steers anything.
func (r *Resolved) Active() bool { return len(r.Global) > 0 || len(r.Raw) > 0 || len(r.Spans) > 0 }

type active struct {
	tag      *Tag
	name     string
	strength float64 // multiplied by the default Mult, clamped
	inline   bool
	global   bool // applied over the whole line, not as a span
}

func clampStrength(t *Tag, s, scale float64) float64 {
	max := t.MaxStrength() * scale
	if !t.Bipolar && s < 0 {
		return 0
	}
	return math.Max(-max, math.Min(max, s))
}

func isEmotion(t *Tag) bool { return t.Group == "emotion" }

// spanUse is one inline span tag with its range and its position in Parsed.Uses, which orders
// nested uses of the same tag: the one found later was opened deeper (the parser closes the
// nearest matching tag first).
type spanUse struct {
	a    active
	s, e int
	idx  int
}

// emotionYieldsTo reports whether emotion span i gives way to emotion span j at a point both
// cover. A different emotion strictly nested inside replaces the outer one; the same tag nested
// in itself takes the inner dose (for equal ranges the deeper use is the later one). Emotions on
// identical ranges never yield to each other: they blend, which is the serialiser's stacked form,
// bounded by MaxOverlappingEmotions.
func emotionYieldsTo(i, j spanUse) bool {
	if j.s < i.s || j.e > i.e {
		return false
	}
	if i.a.tag == j.a.tag {
		return j.s > i.s || j.e < i.e || j.idx > i.idx
	}
	return j.s > i.s || j.e < i.e
}

// Resolve plans a line: it applies the defaults, clamps every dose to its ceiling, and flattens nested and adjacent tags into spans that do not overlap, because
// the engine refuses overlapping spans. Doses on one control that meet are added and clamped again, except for emotions: a strictly nested emotion replaces
// (different tag) or overrides (same tag) the outer one inside the inner range, while emotions on identical ranges blend.
func Resolve(p *Parsed, d Defaults, v *Vocabulary) *Resolved {
	r := &Resolved{Spoken: p.Spoken, Rate: d.Rate, Warnings: append([]string(nil), p.Warnings...)}
	disabled := map[string]bool{}
	for _, n := range d.Disabled {
		if t, ok := v.Lookup(n); ok {
			disabled[t.Name] = true
		}
	}
	mult := func(t *Tag) float64 {
		if d.Mult != nil {
			if m, ok := d.Mult[t.Name]; ok {
				return m
			}
		}
		return 1
	}
	warn := func(format string, a ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...)) }

	// ---- line-scope tags (age): the inline one wins over the default
	var age *Use
	for i := range p.Uses {
		u := &p.Uses[i]
		if u.Tag.Scope != "line" {
			continue
		}
		if disabled[u.Tag.Name] {
			warn("<%s> is switched off for this voice and was ignored", u.Name)
			continue
		}
		if age != nil {
			warn("<%s> was given more than once; the last one is used", u.Name)
		}
		age = u
	}
	rawTag, _ := v.Lookup("age")
	if age != nil {
		s := clampStrength(age.Tag, age.Strength*mult(age.Tag), 1)
		if s != age.Strength*mult(age.Tag) {
			warn("<%s:%g> is beyond what was measured safe and was reduced to %g", age.Name, age.Strength, s)
		}
		r.Raw = map[string]float64{}
		for c, w := range age.Tag.Controls {
			r.Raw[c] = round3(w * age.Tag.Default * s)
		}
		r.Applied = append(r.Applied, fmt.Sprintf("%s:%+.2f(line)", age.Tag.Name, age.Tag.Default*s))
	} else if d.Age != nil && rawTag != nil && !disabled[rawTag.Name] && *d.Age != 0 {
		s := clampStrength(rawTag, *d.Age/rawTag.Default, 1)
		r.Raw = map[string]float64{}
		for c, w := range rawTag.Controls {
			r.Raw[c] = round3(w * rawTag.Default * s)
		}
		r.Applied = append(r.Applied, fmt.Sprintf("age:%+.2f(default)", rawTag.Default*s))
	}

	// ---- baseline tags
	var base []active
	for _, b := range d.Base {
		t, ok := v.Lookup(b.Tag)
		if !ok {
			warn("the default style names %q, which is not a tag", b.Tag)
			continue
		}
		if t.Scope == "line" {
			warn("the default style cannot use <%s>; use the age setting", t.Name)
			continue
		}
		if disabled[t.Name] {
			continue
		}
		s := b.Strength
		if s == 0 {
			s = 1
		}
		base = append(base, active{tag: t, name: t.Name, strength: clampStrength(t, s*mult(t), 1), global: true})
	}

	// ---- inline span tags. A tag that covers the whole text is a whole-line control (no span boost, whole-line ceiling): the boost exists because a span inside a
	// longer text is weaker than the same dose over the whole line, and a span that is the whole line is not weaker.
	// A tag's dose never depends on any other tag in the line (direction-editor plan, WP A rule 4): a whole-text tag that also contains inner tags stays a
	// whole-line global control with its own whole-line dose. The emotion replacement below is between span tags only — a whole-text emotion is not nested in
	// the span sense, so an inner span emotion on part of the line adds to the whole-line one instead of replacing it.
	nText := len([]rune(p.Spoken))
	var spans []spanUse
	var whole []active
	for i := range p.Uses {
		u := p.Uses[i]
		if u.Tag.Scope != "span" {
			continue
		}
		if disabled[u.Tag.Name] {
			warn("<%s> is switched off for this voice and was ignored", u.Name)
			continue
		}
		isWhole := u.Start <= 0 && u.End >= nText
		scale := v.SpanScale
		if isWhole {
			scale = 1
		}
		want := u.Strength * mult(u.Tag)
		s := clampStrength(u.Tag, want, scale)
		if s != want {
			warn("<%s:%g> is beyond what was measured safe and was reduced", u.Name, u.Strength)
		}
		if s == 0 {
			warn("<%s:%g> has no effect", u.Name, u.Strength)
			continue
		}
		a := active{tag: u.Tag, name: u.Name, strength: s, inline: true}
		if isWhole {
			a.global = true
			whole = append(whole, a)
			continue
		}
		spans = append(spans, spanUse{a, u.Start, u.End, i})
	}
	// A whole-text emotion replaces the baseline emotion everywhere.
	for _, w := range whole {
		if isEmotion(w.tag) {
			kept := base[:0]
			for _, b := range base {
				if !isEmotion(b.tag) {
					kept = append(kept, b)
				}
			}
			base = kept
			break
		}
	}

	// Which baseline tags does an inline tag take over? An inline emotion replaces a baseline emotion inside its range.
	baseSuppressed := false
	for _, sp := range spans {
		if isEmotion(sp.a.tag) {
			for _, b := range base {
				if isEmotion(b.tag) {
					baseSuppressed = true
				}
			}
		}
	}

	// Nesting warnings, once per pair, before the per-piece flattening: a strictly nested
	// different emotion says what replaces what; the same tag nested in itself says the inner
	// one is used. Emotions on identical ranges (the serialiser's stacked form) replace nothing
	// and get no new warning.
	for i := range spans {
		if !isEmotion(spans[i].a.tag) {
			continue
		}
		for j := range spans {
			if j == i || !isEmotion(spans[j].a.tag) {
				continue
			}
			if spans[j].s < spans[i].s || spans[j].e > spans[i].e {
				continue // disjoint or crossing: no replacement
			}
			strict := spans[j].s > spans[i].s || spans[j].e < spans[i].e
			switch {
			case spans[i].a.tag == spans[j].a.tag && (strict || j > i):
				warn("<%s> is nested inside <%s>; the inner one is used", spans[j].a.name, spans[i].a.name)
			case spans[i].a.tag != spans[j].a.tag && strict:
				warn("<%s> replaces <%s> inside its range", spans[j].a.name, spans[i].a.name)
			}
		}
	}

	doseOf := func(a active, scale float64) map[string]float64 {
		out := map[string]float64{}
		for c, w := range a.tag.Controls {
			out[c] = w * a.tag.Default * a.strength * scale
		}
		return out
	}
	limitOf := func(t *Tag, scale float64) float64 { return t.Ceiling * scale }
	clampControls := func(sum map[string]float64, owners map[string]float64) {
		for c, val := range sum {
			if lim, ok := owners[c]; ok && math.Abs(val) > lim {
				sum[c] = math.Copysign(lim, val)
			}
		}
	}

	// ---- global controls: the baseline when nothing overrides it, and every tag that covers the whole text. Whole-line is stronger and exact, no span needed.
	globals := []active{}
	if !baseSuppressed {
		globals = append(globals, base...)
	}
	globals = append(globals, whole...)
	if len(globals) > 0 {
		r.Global = map[string]float64{}
		lim := map[string]float64{}
		for _, b := range globals {
			for c, val := range doseOf(b, 1) {
				r.Global[c] += val
				if l := limitOf(b.tag, 1) * math.Abs(b.tag.Controls[c]); l > lim[c] {
					lim[c] = l
				}
			}
			if b.inline {
				r.Applied = append(r.Applied, fmt.Sprintf("%s:%.1f(whole line)", b.name, b.tag.Default*b.strength))
			} else {
				r.Applied = append(r.Applied, fmt.Sprintf("%s:%.1f(default)", b.name, b.tag.Default*b.strength))
			}
			r.Unpin = r.Unpin || b.tag.Tempo
		}
		clampControls(r.Global, lim)
		roundAll(r.Global)
	}

	// ---- flatten the spans
	if len(spans) > 0 {
		n := len([]rune(p.Spoken))
		pts := map[int]bool{0: true, n: true}
		for _, sp := range spans {
			pts[sp.s], pts[sp.e] = true, true
		}
		cuts := make([]int, 0, len(pts))
		for c := range pts {
			if c >= 0 && c <= n {
				cuts = append(cuts, c)
			}
		}
		sort.Ints(cuts)
		overlapWarned := false
		var out []Span
		for k := 0; k+1 < len(cuts); k++ {
			a, b := cuts[k], cuts[k+1]
			var here []int // indexes into spans
			for si, sp := range spans {
				if sp.s <= a && sp.e >= b {
					here = append(here, si)
				}
			}
			if len(here) == 0 && !baseSuppressed {
				continue
			}
			var emos []int
			for _, si := range here {
				if isEmotion(spans[si].a.tag) {
					emos = append(emos, si)
				}
			}
			// An emotion gives way to a deeper one inside that one's range: a different
			// nested emotion replaces it, the same tag nested in itself takes the inner
			// dose. Emotions on identical ranges blend; delivery and mood tags add.
			if len(emos) > 1 {
				yielded := map[int]bool{}
				for _, i := range emos {
					for _, j := range emos {
						if j != i && emotionYieldsTo(spans[i], spans[j]) {
							yielded[i] = true
							break
						}
					}
				}
				if len(yielded) > 0 {
					kept := emos[:0]
					for _, i := range emos {
						if !yielded[i] {
							kept = append(kept, i)
						}
					}
					emos = kept
					keptHere := here[:0]
					for _, si := range here {
						if !yielded[si] {
							keptHere = append(keptHere, si)
						}
					}
					here = keptHere
				}
			}
			// at most N emotions at once: keep the strongest
			if len(emos) > v.MaxOverlappingEmotions {
				sort.SliceStable(emos, func(x, y int) bool {
					hx, hy := spans[emos[x]].a, spans[emos[y]].a
					return hx.tag.Default*hx.strength > hy.tag.Default*hy.strength
				})
				drop := map[int]bool{}
				for _, i := range emos[v.MaxOverlappingEmotions:] {
					drop[i] = true
				}
				var keep []int
				for _, si := range here {
					if !drop[si] {
						keep = append(keep, si)
					}
				}
				here = keep
				if !overlapWarned {
					warn("more than %d emotions overlap at one point; the weakest were dropped", v.MaxOverlappingEmotions)
					overlapWarned = true
				}
			}
			hasInlineEmotion := false
			for _, si := range here {
				if isEmotion(spans[si].a.tag) {
					hasInlineEmotion = true
				}
			}
			sum, lim := map[string]float64{}, map[string]float64{}
			add := func(h active, scale float64) {
				for c, val := range doseOf(h, scale) {
					sum[c] += val
					if l := limitOf(h.tag, scale) * math.Abs(h.tag.Controls[c]); l > lim[c] {
						lim[c] = l
					}
				}
			}
			for _, si := range here {
				add(spans[si].a, v.SpanScale)
			}
			if baseSuppressed {
				for _, b := range base {
					if isEmotion(b.tag) && hasInlineEmotion {
						continue // the inline emotion replaces it here
					}
					add(b, v.SpanScale)
				}
			}
			clampControls(sum, lim)
			roundAll(sum)
			for c, val := range sum {
				if val == 0 {
					delete(sum, c)
				}
			}
			if len(sum) == 0 {
				continue
			}
			if m := len(out); m > 0 && out[m-1].End == a && sameControls(out[m-1].Controls, sum) {
				out[m-1].End = b
			} else {
				out = append(out, Span{Start: a, End: b, Controls: sum})
			}
		}
		r.Spans = out
		for _, sp := range spans {
			r.Applied = append(r.Applied, fmt.Sprintf("%s:%.1f[%d,%d)", sp.a.name, sp.a.tag.Default*sp.a.strength, sp.s, sp.e))
			r.Unpin = r.Unpin || sp.a.tag.Tempo
		}
		if baseSuppressed {
			for _, b := range base {
				r.Applied = append(r.Applied, fmt.Sprintf("%s:%.1f(default, replaced where an inline emotion applies)", b.name, b.tag.Default*b.strength))
				r.Unpin = r.Unpin || b.tag.Tempo
			}
		}
	}
	if r.Unpin {
		warn("this line changes pace with a tempo tag, so the speaking rate is not pinned and its length is less predictable")
	}
	return r
}

func sameControls(a, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || math.Abs(v-w) > 1e-9 {
			return false
		}
	}
	return true
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }

func roundAll(m map[string]float64) {
	for k, v := range m {
		m[k] = round3(v)
	}
}

// Describe is the Applied list as one header-safe string.
func (r *Resolved) Describe() string {
	return strings.Join(r.Applied, "; ")
}
