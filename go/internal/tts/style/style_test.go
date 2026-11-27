// SPDX-License-Identifier: Apache-2.0

package style

import (
	"math"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

func vocab(t testing.TB) *Vocabulary { t.Helper(); return Default() }

func TestVocabularyIsValidAndComplete(t *testing.T) {
	v := vocab(t)
	emotions := 0
	for _, tag := range v.Tags {
		if tag.Group == "emotion" {
			emotions++
		}
		if tag.Scope == "span" && tag.Space == "raw" {
			t.Errorf("%s: a raw control cannot be a span", tag.Name)
		}
	}
	if emotions != 23 {
		t.Errorf("%d emotions, want all 23 (the operator asked for all of them)", emotions)
	}
	for _, n := range []string{"happy", "sad", "angry", "whisper", "loud", "age", "calm", "excited", "fast", "slow", "em", "valence", "arousal"} {
		if _, ok := v.Lookup(n); !ok {
			t.Errorf("tag %q is missing", n)
		}
	}
	for _, gone := range []string{"gender", "accent", "british", "scottish", "deep", "raspy"} {
		if _, ok := v.Lookup(gone); ok {
			t.Errorf("%q must not be a tag: gender and accents were dropped on purpose", gone)
		}
	}
	if tag, _ := v.Lookup("calm"); tag == nil || tag.Name != "serene" {
		t.Error("calm is an alias of serene")
	}
	if _, err := Load([]byte(`{"span_scale":1.5,"max_overlapping_emotions":2,"tags":[{"name":"age","scope":"span","space":"raw","controls":{"identity.age":1},"default":1,"ceiling":1.5}]}`)); err == nil {
		t.Error("a raw span tag was accepted")
	}
}

func TestParse_Basics(t *testing.T) {
	v := vocab(t)
	for name, tc := range map[string]struct {
		in, spoken string
		uses       []string // name@start-end
	}{
		"plain":                  {"Hello there.", "Hello there.", nil},
		"one tag":                {"I am <sad>so sorry</sad> today.", "I am so sorry today.", []string{"sad@5-13"}},
		"strength":               {"<happy:2>Great!</happy>", "Great!", []string{"happy@0-6"}},
		"close with </>":         {"<sad>Oh no.</>", "Oh no.", []string{"sad@0-6"}},
		"nested":                 {"<happy>a <whisper>b</whisper> c</happy>", "a b c", []string{"happy@0-5", "whisper@2-3"}},
		"adjacent":               {"<sad>a</sad><happy>b</happy>", "ab", []string{"sad@0-1", "happy@1-2"}},
		"alias":                  {"<calm>Easy.</calm>", "Easy.", []string{"calm@0-5"}},
		"case-insensitive":       {"<SAD>x</SAD>", "x", []string{"SAD@0-1"}},
		"line tag":               {"<age:+1>Hello.", "Hello.", []string{"age@-1--1"}},
		"line tag in the middle": {"Hello <age:-1>there.", "Hello there.", []string{"age@-1--1"}},
		"unknown is literal":     {"a <b>bold</b> move", "a <b>bold</b> move", nil},
		"comparison":             {"if a < b and c > d", "if a < b and c > d", nil},
		"brackets are text":      {"see [1] and [happy]", "see [1] and [happy]", nil},
		"escaped":                {`write \<happy> to start`, "write <happy> to start", nil},
		"a lone <":               {"<", "<", nil},
		"a tag-shaped far >":     {"a < " + strings.Repeat("x", 60) + " > b", "a < " + strings.Repeat("x", 60) + " > b", nil},
		"japanese offsets":       {"こんにちは。<happy>嬉しいです</happy>！", "こんにちは。嬉しいです！", []string{"happy@6-11"}},
		"emoji count once":       {"😀<sad>😢x</sad>", "😀😢x", []string{"sad@1-3"}},
	} {
		p := Parse(tc.in, v)
		if p.Spoken != tc.spoken {
			t.Errorf("%s: spoken %q, want %q", name, p.Spoken, tc.spoken)
		}
		var got []string
		for _, u := range p.Uses {
			got = append(got, u.Name+"@"+itoa(u.Start)+"-"+itoa(u.End))
		}
		if !reflect.DeepEqual(got, tc.uses) {
			t.Errorf("%s: uses %v, want %v (warnings %v)", name, got, tc.uses, p.Warnings)
		}
	}
}

func itoa(n int) string {
	if n < 0 {
		return "-" + itoa(-n)
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}

func TestParse_StrengthIsRead(t *testing.T) {
	v := vocab(t)
	for in, want := range map[string]float64{"<happy>x</happy>": 1, "<happy:2>x</happy>": 2, "<happy:0.5>x</happy>": 0.5, "<happy:.5>x</happy>": 0.5, "<age:+1>": 1, "<age:-1.5>": -1.5} {
		p := Parse(in, v)
		if len(p.Uses) != 1 || p.Uses[0].Strength != want {
			t.Errorf("%q: %+v, want strength %v", in, p.Uses, want)
		}
	}
}

func TestParse_MistakesAreReportedNotFatal(t *testing.T) {
	v := vocab(t)
	for name, tc := range map[string]struct{ in, warning string }{
		"never closed":      {"<sad>no end", "never closed"},
		"closes nothing":    {"oops </sad>", "closes nothing"},
		"</> with nothing":  {"oops </>", "closes nothing"},
		"empty span":        {"a <sad></sad> b", "covers no text"},
		"misnested":         {"<happy>a <sad>b</happy> c</sad>", "was still open"},
		"strength on close": {"<sad>x</sad:2>", "no strength"},
	} {
		p := Parse(tc.in, v)
		found := false
		for _, w := range p.Warnings {
			found = found || strings.Contains(w, tc.warning)
		}
		if !found {
			t.Errorf("%s: warnings %q lack %q", name, p.Warnings, tc.warning)
		}
	}
	if p := Parse("<sad>no end", v); len(p.Uses) != 1 || !p.Uses[0].Open || p.Uses[0].End != len([]rune(p.Spoken)) {
		t.Errorf("an unclosed tag must run to the end: %+v", p.Uses)
	}
}

func TestStripAndLooksTagged(t *testing.T) {
	v := vocab(t)
	if Strip("<excited>Wow!</excited> a < b", v) != "Wow! a < b" {
		t.Error("Strip")
	}
	if LooksTagged("no brackets") || !LooksTagged("a < b") {
		t.Error("LooksTagged")
	}
}

func res(t *testing.T, text string, d Defaults) *Resolved {
	t.Helper()
	v := vocab(t)
	return Resolve(Parse(text, v), d, v)
}

func TestResolve_AnEmotionBecomesASpanWithItsCalibratedDose(t *testing.T) {
	v := vocab(t)
	happy, _ := v.Lookup("happy")
	r := res(t, "ab <happy>cd</happy> ef", Defaults{})
	if len(r.Spans) != 1 || r.Spans[0].Start != 3 || r.Spans[0].End != 5 {
		t.Fatalf("spans %+v", r.Spans)
	}
	want := round3(happy.Default * v.SpanScale) // a span is applied span_scale times stronger than the calibrated whole-line dose
	if got := r.Spans[0].Controls["emotions.happy"]; math.Abs(got-want) > 1e-9 {
		t.Errorf("dose %v, want %v", got, want)
	}
	if r.Global != nil || r.Raw != nil {
		t.Errorf("an inline emotion must not turn into a global control: %+v %+v", r.Global, r.Raw)
	}
}

func TestResolve_StrengthScalesAndIsClamped(t *testing.T) {
	v := vocab(t)
	happy, _ := v.Lookup("happy")
	r := res(t, "x <happy:0.5>a</happy> y", Defaults{})
	if got, want := r.Spans[0].Controls["emotions.happy"], round3(happy.Default*0.5*v.SpanScale); got != want {
		t.Errorf("half strength: %v, want %v", got, want)
	}
	r = res(t, "x <happy:100>a</happy> y", Defaults{})
	limit := round3(happy.Ceiling * v.SpanScale)
	if got := r.Spans[0].Controls["emotions.happy"]; got > limit+1e-9 {
		t.Errorf("a huge strength gave %v, over the ceiling %v", got, limit)
	}
	found := false
	for _, w := range r.Warnings {
		found = found || strings.Contains(w, "reduced")
	}
	if !found {
		t.Errorf("clamping must be reported: %v", r.Warnings)
	}
}

func TestResolve_NestingIsFlattenedToDisjointSpans(t *testing.T) {
	r := res(t, "x <happy>aa <whisper>bb</whisper> cc</happy> y", Defaults{})
	if len(r.Spans) != 3 {
		t.Fatalf("%d spans, want 3 (happy, happy+whisper, happy): %+v", len(r.Spans), r.Spans)
	}
	if r.Spans[1].Controls["delivery.whisper"] == 0 || r.Spans[1].Controls["emotions.happy"] == 0 {
		t.Errorf("the inner range must carry both controls: %+v", r.Spans[1])
	}
	if r.Spans[0].Controls["delivery.whisper"] != 0 || r.Spans[2].Controls["delivery.whisper"] != 0 {
		t.Errorf("whisper leaked outside its tag: %+v", r.Spans)
	}
	assertDisjoint(t, r)
}

// Rule 3: the same tag nested in itself takes the inner dose; nothing stacks.
func TestResolve_TheSameTagNestedInItselfTakesTheInnerDoseWithoutStacking(t *testing.T) {
	v := vocab(t)
	happy, _ := v.Lookup("happy")
	r := res(t, "x <happy:0.5>a <happy:1.4>b</happy></happy> y", Defaults{})
	assertDisjoint(t, r)
	if len(r.Spans) != 2 {
		t.Fatalf("%d spans, want 2 (outer on \"a \", inner on \"b\" — no third value from stacking): %+v", len(r.Spans), r.Spans)
	}
	outer := round3(happy.Default * 0.5 * v.SpanScale)
	inner := round3(happy.Default * 1.4 * v.SpanScale)
	if got := r.Spans[0].Controls["emotions.happy"]; math.Abs(got-outer) > 1e-9 {
		t.Errorf("outside the inner tag %v, want the outer dose %v", got, outer)
	}
	if got := r.Spans[1].Controls["emotions.happy"]; math.Abs(got-inner) > 1e-9 {
		t.Errorf("inside %v, want the inner dose %v, not the sum of both", got, inner)
	}
	if !contains(r.Warnings, "<happy> is nested inside <happy>; the inner one is used") {
		t.Errorf("warnings %q must say which one is used", r.Warnings)
	}
	// identical ranges of the same tag: still the inner one, never a doubled dose
	r = res(t, "q <happy><happy>x</happy></happy> q", Defaults{})
	if len(r.Spans) != 1 {
		t.Fatalf("%+v", r.Spans)
	}
	if got, want := r.Spans[0].Controls["emotions.happy"], round3(happy.Default*v.SpanScale); math.Abs(got-want) > 1e-9 {
		t.Errorf("dose %v, want %v: nesting the same tag does not double it", got, want)
	}
	if !contains(r.Warnings, "is nested inside") {
		t.Errorf("warnings %q lack the nesting message", r.Warnings)
	}
}

// Rule 1: a strictly nested different emotion replaces the outer one inside its range;
// delivery and mood tags still add.
func TestResolve_NestedDifferentEmotionReplacesTheOuterInsideItsRange(t *testing.T) {
	v := vocab(t)
	happy, _ := v.Lookup("happy")
	r := res(t, "x <happy>a <sad>b</sad> c</happy> y", Defaults{})
	assertDisjoint(t, r)
	if len(r.Spans) != 3 {
		t.Fatalf("%d spans, want 3 (happy | sad | happy, no blend): %+v", len(r.Spans), r.Spans)
	}
	want := round3(happy.Default * v.SpanScale)
	if got := r.Spans[0].Controls["emotions.happy"]; math.Abs(got-want) > 1e-9 {
		t.Errorf("before the inner tag %v, want the outer dose %v", got, want)
	}
	if r.Spans[1].Controls["emotions.happy"] != 0 {
		t.Errorf("inside the inner range the outer emotion must be gone: %+v", r.Spans[1].Controls)
	}
	if r.Spans[1].Controls["emotions.sad"] == 0 {
		t.Errorf("the inner emotion must be applied: %+v", r.Spans[1].Controls)
	}
	if got := r.Spans[2].Controls["emotions.happy"]; math.Abs(got-want) > 1e-9 {
		t.Errorf("after the inner tag %v, want the outer dose %v again", got, want)
	}
	if r.Spans[0].Controls["emotions.sad"] != 0 || r.Spans[2].Controls["emotions.sad"] != 0 {
		t.Errorf("sad leaked outside its range: %+v", r.Spans)
	}
	if !contains(r.Warnings, "<sad> replaces <happy> inside its range") {
		t.Errorf("warnings %q must say what replaced what", r.Warnings)
	}
}

func TestResolve_AReplacingEmotionStillAddsDeliveryTags(t *testing.T) {
	r := res(t, "x <happy>aa <sad>b <whisper>cc</whisper> d</sad> ee</happy> y", Defaults{})
	assertDisjoint(t, r)
	var mid *Span
	for i := range r.Spans {
		if r.Spans[i].Start == 7 && r.Spans[i].End == 9 {
			mid = &r.Spans[i]
		}
	}
	if mid == nil {
		t.Fatalf("no span for the whisper range: %+v", r.Spans)
	}
	if mid.Controls["delivery.whisper"] == 0 {
		t.Errorf("delivery adds to the replacing emotion: %+v", mid.Controls)
	}
	if mid.Controls["emotions.sad"] == 0 || mid.Controls["emotions.happy"] != 0 {
		t.Errorf("inside <sad> only sad applies: %+v", mid.Controls)
	}
	if !contains(r.Warnings, "<sad> replaces <happy> inside its range") {
		t.Errorf("warnings %q", r.Warnings)
	}
}

// Rule 2: emotions on the identical range (the serialiser's stacked form) blend; there is no
// replacement warning because nothing was replaced.
func TestResolve_EmotionsOnTheIdenticalRangeBlendWithoutAWarning(t *testing.T) {
	r := res(t, "q <happy><sad>x</sad></happy> q", Defaults{})
	if len(r.Spans) != 1 {
		t.Fatalf("%+v", r.Spans)
	}
	if r.Spans[0].Controls["emotions.happy"] == 0 || r.Spans[0].Controls["emotions.sad"] == 0 {
		t.Errorf("both emotions must apply on the identical range: %+v", r.Spans[0].Controls)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "replaces") || strings.Contains(w, "nested inside") {
			t.Errorf("a stacked identical range replaces nothing: %q", w)
		}
	}
}

// Rule 2 again: a third emotion on the identical range is dropped with the existing warning, and
// no new warning appears.
func TestResolve_AThirdEmotionOnTheIdenticalRangeKeepsTheOldWarning(t *testing.T) {
	r := res(t, "z <sad:0.5><angry:2><happy>x</happy></></> z", Defaults{})
	if len(r.Spans) != 1 {
		t.Fatalf("%+v", r.Spans)
	}
	emos := 0
	for c := range r.Spans[0].Controls {
		if strings.HasPrefix(c, "emotions.") {
			emos++
		}
	}
	if emos > 2 {
		t.Errorf("%d emotions at one point, at most 2 allowed: %+v", emos, r.Spans[0].Controls)
	}
	if r.Spans[0].Controls["emotions.sad"] != 0 {
		t.Errorf("the weakest emotion (sad ×0.5) should have been the one dropped: %+v", r.Spans[0].Controls)
	}
	if !contains(r.Warnings, "the weakest were dropped") {
		t.Errorf("the existing warning must stay: %v", r.Warnings)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "replaces") || strings.Contains(w, "nested inside") {
			t.Errorf("identical ranges replace nothing: %q", w)
		}
	}
}

// Rule 4: a tag's dose does not depend on other tags in the line. A whole-text tag that also
// contains an inner tag keeps the whole-line dose it would have alone.
func TestResolve_AWholeTextTagKeepsItsWholeLineDoseWhenItAlsoContainsAnInnerTag(t *testing.T) {
	v := vocab(t)
	happy, _ := v.Lookup("happy")
	with := res(t, "<happy>start <sad>mid</sad> end</happy>", Defaults{})
	alone := res(t, "<happy>start mid end</happy>", Defaults{})
	if with.Global["emotions.happy"] != alone.Global["emotions.happy"] {
		t.Errorf("the inner tag changed the outer tag's dose: %v vs %v", with.Global, alone.Global)
	}
	if got, want := with.Global["emotions.happy"], round3(happy.Default); math.Abs(got-want) > 1e-9 {
		t.Errorf("dose %v, want the whole-line default %v", got, want)
	}
	assertDisjoint(t, with)
	if len(with.Spans) != 1 || with.Spans[0].Controls["emotions.sad"] == 0 {
		t.Errorf("the inner tag is still its own span: %+v", with.Spans)
	}
}

// Regression: disjoint tags are not affected by the nesting rules — no replacement warnings,
// each range keeps its own tag.
func TestResolve_DisjointTagsNestUnaffected(t *testing.T) {
	r := res(t, "<sad>a</sad> b <happy>c</happy>", Defaults{})
	assertDisjoint(t, r)
	if len(r.Spans) != 2 {
		t.Fatalf("%+v", r.Spans)
	}
	if r.Spans[0].Controls["emotions.happy"] != 0 || r.Spans[1].Controls["emotions.sad"] != 0 {
		t.Errorf("disjoint tags mixed: %+v", r.Spans)
	}
	for _, w := range r.Warnings {
		if strings.Contains(w, "replaces") || strings.Contains(w, "nested inside") {
			t.Errorf("nothing is nested here: %q", w)
		}
	}
}

// Three emotions stacked on the identical range (no nesting, so nothing replaces anything): the
// existing keep-the-strongest rule still bounds them.
func TestResolve_TooManyOverlappingEmotionsKeepsTheStrongest(t *testing.T) {
	r := res(t, "z <sad:0.5><angry:2><happy>x</happy></></> z", Defaults{})
	if len(r.Spans) != 1 {
		t.Fatalf("%+v", r.Spans)
	}
	emos := 0
	for c := range r.Spans[0].Controls {
		if strings.HasPrefix(c, "emotions.") {
			emos++
		}
	}
	if emos > 2 {
		t.Errorf("%d emotions at one point, at most 2 allowed: %+v", emos, r.Spans[0].Controls)
	}
	if r.Spans[0].Controls["emotions.sad"] != 0 {
		t.Errorf("the weakest emotion (sad ×0.5) should have been the one dropped: %+v", r.Spans[0].Controls)
	}
}

func TestResolve_BaselineIsGlobalUntilAnInlineEmotionReplacesIt(t *testing.T) {
	d := Defaults{Base: []BaseTag{{Tag: "serene", Strength: 1}}}
	r := res(t, "one two three", d)
	if r.Global["emotions.serene"] == 0 || len(r.Spans) != 0 {
		t.Fatalf("an unopposed baseline is a whole-line control: global %+v spans %+v", r.Global, r.Spans)
	}
	r = res(t, "one <angry>two</angry> three", d)
	if r.Global != nil {
		t.Errorf("once an inline emotion overrides the baseline it cannot be global: %+v", r.Global)
	}
	if len(r.Spans) != 3 {
		t.Fatalf("%+v", r.Spans)
	}
	if r.Spans[1].Controls["emotions.angry"] == 0 || r.Spans[1].Controls["emotions.serene"] != 0 {
		t.Errorf("inside the inline emotion the baseline emotion is replaced, not added: %+v", r.Spans[1])
	}
	if r.Spans[0].Controls["emotions.serene"] == 0 || r.Spans[2].Controls["emotions.serene"] == 0 {
		t.Errorf("outside it the baseline continues: %+v", r.Spans)
	}
}

func TestResolve_ABaselineOfAnotherGroupAddsToAnInlineEmotion(t *testing.T) {
	r := res(t, "one <angry>two</angry> three", Defaults{Base: []BaseTag{{Tag: "lively", Strength: 1}}})
	if r.Global["expressive.expressive"] == 0 {
		t.Errorf("a non-emotion baseline stays global: %+v", r.Global)
	}
}

func TestResolve_AgeIsAWholeLineRawControlAndInlineBeatsDefault(t *testing.T) {
	one := 1.0
	r := res(t, "Hello.", Defaults{Age: &one})
	if r.Raw["identity.age"] != 1 {
		t.Errorf("default age: %+v", r.Raw)
	}
	r = res(t, "<age:-1>Hello.", Defaults{Age: &one})
	if r.Raw["identity.age"] != -1 {
		t.Errorf("an inline age replaces the default (it is not added to it): %+v", r.Raw)
	}
	r = res(t, "<age:+5>Hello.", Defaults{})
	if r.Raw["identity.age"] > 1.5+1e-9 {
		t.Errorf("age beyond the range was not clamped: %+v", r.Raw)
	}
	r = res(t, "<age:1>a</age> and <age:-1>b", Defaults{})
	if r.Raw["identity.age"] != -1 || !contains(r.Warnings, "more than once") {
		t.Errorf("repeated age: %+v %v", r.Raw, r.Warnings)
	}
	if len(r.Spans) != 0 {
		t.Errorf("age must never become a span (the engine refuses it): %+v", r.Spans)
	}
}

func contains(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestResolve_DisabledAndMultipliers(t *testing.T) {
	v := vocab(t)
	angry, _ := v.Lookup("angry")
	r := res(t, "z <angry>x</angry> z", Defaults{Disabled: []string{"angry"}})
	if len(r.Spans) != 0 || !contains(r.Warnings, "switched off") {
		t.Errorf("disabled: %+v %v", r.Spans, r.Warnings)
	}
	r = res(t, "z <angry>x</angry> z", Defaults{Mult: map[string]float64{"angry": 0.5}})
	if got, want := r.Spans[0].Controls["emotions.angry"], round3(angry.Default*0.5*v.SpanScale); got != want {
		t.Errorf("multiplier: %v, want %v", got, want)
	}
	one := 1.0
	r = res(t, "x", Defaults{Age: &one, Disabled: []string{"age"}})
	if r.Raw != nil {
		t.Errorf("a voice with age switched off must not get the default age: %+v", r.Raw)
	}
}

func TestResolve_TempoUnpinsTheRate(t *testing.T) {
	r := res(t, "a <fast>b</fast>", Defaults{})
	if !r.Unpin || !contains(r.Warnings, "not pinned") {
		t.Errorf("a tempo tag must unpin the rate and say so: %+v %v", r.Unpin, r.Warnings)
	}
	if r.Spans[0].Controls["delivery.tempo"] <= 0 {
		t.Errorf("fast is positive tempo: %+v", r.Spans)
	}
	r = res(t, "a <slow>b</slow>", Defaults{})
	if r.Spans[0].Controls["delivery.tempo"] >= 0 {
		t.Errorf("slow is negative tempo: %+v", r.Spans)
	}
	if res(t, "a <happy>b</happy>", Defaults{}).Unpin {
		t.Error("an emotion must not unpin the rate")
	}
	rate := 15.0
	if got := res(t, "x", Defaults{Rate: &rate}).Rate; got == nil || *got != 15 {
		t.Errorf("rate: %v", got)
	}
}

func TestResolve_NegativeStrengthOnAUnipolarTagDoesNothing(t *testing.T) {
	r := res(t, "z <happy:-1>x</happy> z", Defaults{})
	if len(r.Spans) != 0 || !contains(r.Warnings, "no effect") {
		t.Errorf("%+v %v", r.Spans, r.Warnings)
	}
	r = res(t, "z <arousal:-2>x</arousal> z", Defaults{})
	if r.Spans[0].Controls["mood.arousal"] >= 0 {
		t.Errorf("arousal is bipolar: %+v", r.Spans)
	}
}

func TestResolve_PlainTextIsUntouchedAndInactive(t *testing.T) {
	r := res(t, "Nothing special here.", Defaults{})
	if r.Active() || r.Spoken != "Nothing special here." || len(r.Warnings) != 0 {
		t.Errorf("%+v", r)
	}
}

func assertDisjoint(t testing.TB, r *Resolved) {
	t.Helper()
	n := utf8.RuneCountInString(r.Spoken)
	spans := append([]Span(nil), r.Spans...)
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	prev := 0
	for _, s := range spans {
		if s.Start < prev || s.End <= s.Start || s.End > n || s.Start < 0 {
			t.Fatalf("span %+v is outside the text (0..%d) or overlaps the previous one (ends at %d): %+v", s, n, prev, spans)
		}
		prev = s.End
		if len(s.Controls) == 0 {
			t.Fatalf("an empty span was emitted: %+v", s)
		}
	}
}

// Whatever the tag soup, the plan is sane: spans never overlap (the engine would answer 400), stay inside the text, no dose passes its ceiling, and the spoken text
// is the input with the recognised tags removed.
func TestResolve_RandomTagSoup(t *testing.T) {
	v := vocab(t)
	rng := rand.New(rand.NewSource(7))
	names := v.Names()
	words := []string{"hello", "world", "こんにちは", "😀", "a", "<", ">", "[1]", "x<y", ".", ",", " ", "\n", `\<`}
	for iter := 0; iter < 3000; iter++ {
		var b strings.Builder
		var open []string
		for k := rng.Intn(14); k >= 0; k-- {
			switch rng.Intn(6) {
			case 0:
				n := names[rng.Intn(len(names))]
				open = append(open, n)
				b.WriteString("<" + n)
				if rng.Intn(3) == 0 {
					b.WriteString(":" + []string{"0.5", "2", "-1", "+1", "9", "0"}[rng.Intn(6)])
				}
				b.WriteString(">")
			case 1:
				if len(open) > 0 && rng.Intn(2) == 0 {
					i := rng.Intn(len(open))
					b.WriteString("</" + open[i] + ">")
					open = append(open[:i], open[i+1:]...)
				} else {
					b.WriteString("</>")
				}
			default:
				b.WriteString(words[rng.Intn(len(words))])
			}
		}
		text := b.String()
		d := Defaults{}
		if rng.Intn(2) == 0 {
			d.Base = []BaseTag{{Tag: "serene", Strength: 1}, {Tag: "lively", Strength: 0.5}}
		}
		p := Parse(text, v)
		r := Resolve(p, d, v)
		assertDisjoint(t, r)
		for _, s := range r.Spans {
			for c, val := range s.Controls {
				if math.IsNaN(val) || math.IsInf(val, 0) {
					t.Fatalf("%q: %s = %v", text, c, val)
				}
			}
		}
		for c, val := range r.Global {
			if math.IsNaN(val) {
				t.Fatalf("%q: global %s = %v", text, c, val)
			}
		}
		for _, u := range p.Uses {
			if u.Tag.Scope == "span" && (u.Start < 0 || u.End > utf8.RuneCountInString(p.Spoken) || u.End < u.Start) {
				t.Fatalf("%q: use %+v is outside the spoken text %q", text, u, p.Spoken)
			}
		}
		// a second parse of the spoken text finds no tag, unless the text carried an escaped "<" that became a real one
		if !strings.Contains(text, `\<`) && Parse(p.Spoken, v).HasTags() {
			t.Fatalf("%q: tags survived into the spoken text %q", text, p.Spoken)
		}
	}
}

// A tag that covers the whole text is a whole-line control: the span boost exists because a span inside a longer text is weaker than the same dose over the whole line,
// which is not true of a span that IS the whole line (found live: a batch wraps every sentence in its tag, and <happy:2> raised pitch by 10 semitones).
func TestResolve_ATagCoveringTheWholeTextIsAWholeLineControl(t *testing.T) {
	v := vocab(t)
	happy, _ := v.Lookup("happy")
	r := res(t, "<happy>The whole line.</happy>", Defaults{})
	if len(r.Spans) != 0 {
		t.Fatalf("a whole-text tag must not be a span: %+v", r.Spans)
	}
	if got, want := r.Global["emotions.happy"], round3(happy.Default); got != want {
		t.Errorf("dose %v, want exactly the calibrated default %v (no span boost)", got, want)
	}
	r = res(t, "<happy:100>Everything.</happy>", Defaults{})
	if got := r.Global["emotions.happy"]; got > happy.Ceiling+1e-9 {
		t.Errorf("clamped to %v, want at most the whole-line ceiling %v", got, happy.Ceiling)
	}
	// an open-ended tag runs to the end: that is the whole text too
	if r := res(t, "<happy>Runs to the end.", Defaults{}); len(r.Spans) != 0 || r.Global["emotions.happy"] == 0 {
		t.Errorf("open-ended whole-text tag: %+v", r)
	}
	// a tag that starts later is still a span, with the boost
	if r := res(t, "Not <happy>all</happy> of it.", Defaults{}); len(r.Spans) != 1 || r.Global != nil {
		t.Errorf("a partial tag must stay a span: %+v %+v", r.Spans, r.Global)
	}
}

func TestResolve_AWholeTextEmotionReplacesTheBaselineEmotionEverywhere(t *testing.T) {
	d := Defaults{Base: []BaseTag{{Tag: "serene", Strength: 1}, {Tag: "lively", Strength: 1}}}
	r := res(t, "<angry>All of it.</angry>", d)
	if r.Global["emotions.angry"] == 0 || r.Global["emotions.serene"] != 0 {
		t.Errorf("a whole-text emotion replaces the baseline emotion: %+v", r.Global)
	}
	if r.Global["expressive.expressive"] == 0 {
		t.Errorf("a baseline of another group stays: %+v", r.Global)
	}
	if len(r.Spans) != 0 {
		t.Errorf("%+v", r.Spans)
	}
}

func TestResolve_AWholeTextTagAndAPartialOneOfAnotherGroupCombine(t *testing.T) {
	r := res(t, "<lively>one <whisper>two</whisper> three</lively>", Defaults{})
	if r.Global["expressive.expressive"] == 0 {
		t.Errorf("the whole-text tag is global: %+v", r.Global)
	}
	if len(r.Spans) != 1 || r.Spans[0].Controls["delivery.whisper"] == 0 || r.Spans[0].Controls["expressive.expressive"] != 0 {
		t.Errorf("only the inner tag is a span, and it does not repeat the global one: %+v", r.Spans)
	}
	assertDisjoint(t, r)
}
