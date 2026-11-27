// SPDX-License-Identifier: Apache-2.0

// Package style turns inline tags such as <sad>I'm sorry</sad> into the steering controls of the ZONOS3-Mini engine (docs/zonos-style-tags-plan.md).
//
// It is pure: text in, a plan out. The engine rules it has to respect were measured, not assumed: control spans must not overlap, age moves the speaker embedding
// and so applies to a whole line only, a span is about half as strong as the same dose over the whole line, and each emotion has its own narrow usable range. Those
// numbers live in vocabulary.json, which is data and can be recalibrated without touching the code.
package style

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed vocabulary.json
var vocabularyJSON []byte

// Tag is one word of the tag language.
type Tag struct {
	Name        string   `json:"name"`
	Aliases     []string `json:"aliases,omitempty"`
	Label       string   `json:"label"`
	Group       string   `json:"group"` // emotion, mood, delivery, style, line
	Description string   `json:"description"`
	// Scope is "span" (a range of the text) or "line" (the whole line; written once, anywhere).
	Scope string `json:"scope"`
	// Space is "hidden" (the default) or "raw". Raw controls move the speaker embedding: whole line, embedding mode only.
	Space string `json:"space,omitempty"`
	// Controls maps engine control ids to a weight; the dose of a control is weight × Default × strength.
	Controls map[string]float64 `json:"controls"`
	// Default is the dose that strength 1 stands for, in the control's own unit (calibrated). Ceiling is the largest dose that was measured to be safe when the
	// control is applied to the whole line; a span may go SpanScale times higher because a span is weaker.
	Default float64 `json:"default"`
	Ceiling float64 `json:"ceiling"`
	// Bipolar tags accept a negative strength (younger, flatter, negative valence).
	Bipolar bool `json:"bipolar,omitempty"`
	// Requires names a mode the tag needs ("embedding").
	Requires string `json:"requires,omitempty"`
	// Tempo marks a tag that changes pace; a line that uses one is sent without the pinned speaking rate or the steering would have nothing to act on.
	Tempo bool `json:"tempo,omitempty"`
	// Provisional marks a default or ceiling that has not been calibrated yet.
	Provisional bool `json:"provisional,omitempty"`
}

// Vocabulary is the set of tags and the rules that apply to all of them.
type Vocabulary struct {
	Version int `json:"version"`
	// SpanScale is how much stronger a dose must be as a span than over the whole line for the same audible effect (measured: about 0.5 of the effect).
	SpanScale float64 `json:"span_scale"`
	// MaxOverlappingEmotions bounds how many emotions may be active at one point; more blur into mush.
	MaxOverlappingEmotions int   `json:"max_overlapping_emotions"`
	Tags                   []Tag `json:"tags"`

	byName map[string]*Tag
}

var (
	defaultOnce  sync.Once
	defaultVocab *Vocabulary
	defaultErr   error
)

// Default is the vocabulary shipped with this build.
func Default() *Vocabulary {
	defaultOnce.Do(func() { defaultVocab, defaultErr = Load(vocabularyJSON) })
	if defaultErr != nil {
		panic("style: the embedded vocabulary is invalid: " + defaultErr.Error()) // a build error, caught by the package tests
	}
	return defaultVocab
}

// Load parses and validates a vocabulary.
func Load(data []byte) (*Vocabulary, error) {
	var v Vocabulary
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	if v.SpanScale < 1 {
		return nil, fmt.Errorf("span_scale %v: a span cannot be stronger than the whole line, so the scale is at least 1", v.SpanScale)
	}
	if v.MaxOverlappingEmotions < 1 {
		return nil, fmt.Errorf("max_overlapping_emotions must be at least 1")
	}
	v.byName = map[string]*Tag{}
	for i := range v.Tags {
		t := &v.Tags[i]
		if t.Name == "" || t.Name != strings.ToLower(t.Name) {
			return nil, fmt.Errorf("tag %q: names are lower case and not empty", t.Name)
		}
		if t.Scope != "span" && t.Scope != "line" {
			return nil, fmt.Errorf("tag %s: scope %q is not span or line", t.Name, t.Scope)
		}
		if t.Space != "" && t.Space != "hidden" && t.Space != "raw" {
			return nil, fmt.Errorf("tag %s: space %q", t.Name, t.Space)
		}
		if t.Space == "raw" && t.Scope != "line" {
			return nil, fmt.Errorf("tag %s: a raw (embedding-space) control cannot be a span: the engine refuses it", t.Name)
		}
		if len(t.Controls) == 0 || t.Default <= 0 || t.Ceiling < t.Default {
			return nil, fmt.Errorf("tag %s: needs controls, a positive default and a ceiling of at least the default", t.Name)
		}
		for _, n := range append([]string{t.Name}, t.Aliases...) {
			if _, dup := v.byName[n]; dup {
				return nil, fmt.Errorf("tag name %q is used twice", n)
			}
			v.byName[n] = t
		}
	}
	return &v, nil
}

// Lookup finds a tag by name or alias, case-insensitively.
func (v *Vocabulary) Lookup(name string) (*Tag, bool) {
	t, ok := v.byName[strings.ToLower(name)]
	return t, ok
}

// Names lists every tag name and alias, sorted (for messages and tests).
func (v *Vocabulary) Names() []string {
	out := make([]string, 0, len(v.byName))
	for n := range v.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// MaxStrength is the largest strength a tag accepts on a whole line: the ceiling expressed in multiples of its default.
func (t *Tag) MaxStrength() float64 { return t.Ceiling / t.Default }
