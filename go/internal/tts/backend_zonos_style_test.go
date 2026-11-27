// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/tts/style"
)

// styledSpec builds a clone request the way the dual engine would: tags taken out of the text, the defaults carried, the voice's own language known.
func styledSpec(text, requestLang string, d *style.Defaults) (SynthSpec, string) {
	req := applyTags(SpeechRequest{Input: text, Style: d})
	sp := cloneSpec("v")
	sp.VoiceLanguage, sp.Language = "English", requestLang
	sp.Style = req.styled
	return sp, req.Input
}

func lastGen(f *fakeZonos) map[string]any { return f.gens[len(f.gens)-1] }

func spansOf(t *testing.T, g map[string]any) []map[string]any {
	t.Helper()
	raw, _ := g["control_spans"].([]any)
	var out []map[string]any
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func TestZonosStyle_TagsBecomeSpansPlacedByReadingWhenTheEngineHasHeads(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.laci = []string{"en_us"}
	b, _ := zonosBackend(f)
	sp, text := styledSpec("ab <happy>cd</happy> ef", "English", nil)
	res, err := b.Speech(context.Background(), sp, text)
	if err != nil {
		t.Fatal(err)
	}
	g := lastGen(f)
	if g["text"] != "ab cd ef" {
		t.Errorf("the engine was sent %q: the tags must never reach it", g["text"])
	}
	spans := spansOf(t, g)
	if len(spans) != 1 || spans[0]["start"] != float64(3) || spans[0]["end"] != float64(5) {
		t.Fatalf("spans %v, want one over code points 3-5", spans)
	}
	if spans[0]["controls"].(map[string]any)["emotions.happy"].(float64) <= 0 {
		t.Errorf("controls %v", spans[0]["controls"])
	}
	if g["control_span_follow"] != "reading" {
		t.Errorf("follow = %v: the engine has heads for en_us, so the spans must follow the reading position", g["control_span_follow"])
	}
	if !strings.Contains(res.Notes[headerStyleApplied], "happy") || !strings.Contains(res.Notes[headerStyleApplied], "embedding") {
		t.Errorf("the response must say what was applied: %v", res.Notes)
	}
}

func TestZonosStyle_WithoutHeadsPlacementFallsBackToTheRateScheduleAndSaysSo(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	sp, text := styledSpec("ab <happy>cd</happy> ef", "English", nil)
	res, err := b.Speech(context.Background(), sp, text)
	if err != nil {
		t.Fatal(err)
	}
	if lastGen(f)["control_span_follow"] != "rate" || !strings.Contains(res.Notes[headerStyleWarnings], "approximate") {
		t.Errorf("follow %v, notes %v", lastGen(f)["control_span_follow"], res.Notes)
	}
}

func TestZonosStyle_AgeIsWholeLineAndOnlyInEmbeddingMode(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	sp, text := styledSpec("<age:+1>Hello there.", "English", nil)
	if _, err := b.Speech(context.Background(), sp, text); err != nil {
		t.Fatal(err)
	}
	g := lastGen(f)
	if g["speaker_mode"] != "embedding" || g["controls"].(map[string]any)["identity.age"] != float64(1) {
		t.Errorf("same-language age: %v", g)
	}
	if len(spansOf(t, g)) != 0 {
		t.Errorf("age must never be a span: %v", g["control_spans"])
	}
	sp, text = styledSpec("<age:+1>Hola.", "Spanish", nil)
	res, err := b.Speech(context.Background(), sp, text)
	if err != nil {
		t.Fatal(err)
	}
	g = lastGen(f)
	if g["speaker_mode"] != "reference" || g["controls"] != nil {
		t.Errorf("cross-language runs in reference mode, where age cannot apply: %v", g)
	}
	if !strings.Contains(res.Notes[headerStyleWarnings], "age was ignored") {
		t.Errorf("the dropped age must be reported: %v", res.Notes)
	}
}

func TestZonosStyle_ADefaultAgeAndABaselineTravelWithTheRequest(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	age := -1.0
	sp, text := styledSpec("Plain text, no tags at all.", "English", &style.Defaults{Age: &age, Base: []style.BaseTag{{Tag: "serene", Strength: 1}}})
	if _, err := b.Speech(context.Background(), sp, text); err != nil {
		t.Fatal(err)
	}
	c := lastGen(f)["controls"].(map[string]any)
	if c["identity.age"] != float64(-1) || c["emotions.serene"].(float64) <= 0 {
		t.Errorf("defaults without any tag must still steer: %v", c)
	}
	if len(spansOf(t, lastGen(f))) != 0 {
		t.Errorf("an unopposed baseline is a whole-line control, not a span: %v", lastGen(f)["control_spans"])
	}
}

func TestZonosStyle_RateComesFromTheDefaultsAndATempoTagUnpinsIt(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	rate := 15.0
	sp, text := styledSpec("A plain line.", "English", &style.Defaults{Rate: &rate})
	if _, err := b.Speech(context.Background(), sp, text); err != nil {
		t.Fatal(err)
	}
	if cv := lastGen(f)["condition_values"].(map[string]any); cv["speaking_rate"] != float64(15) {
		t.Errorf("the client's own rate was not used: %v", cv)
	}
	sp, text = styledSpec("a <fast>quick</fast> bit", "English", &style.Defaults{Rate: &rate})
	res, err := b.Speech(context.Background(), sp, text)
	if err != nil {
		t.Fatal(err)
	}
	if cv := lastGen(f)["condition_values"].(map[string]any); cv["speaking_rate"] != nil || cv["loudness"] == nil {
		t.Errorf("a tempo tag leaves the pace unpinned but keeps the loudness: %v", cv)
	}
	if !strings.Contains(res.Notes[headerStyleWarnings], "not pinned") {
		t.Errorf("unpinning must be said: %v", res.Notes)
	}
}

func TestZonosStyle_WithoutASteeringBundleNothingIsSteeredAndItIsSaid(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.noSteer = true
	b, _ := zonosBackend(f)
	sp, text := styledSpec("ab <happy>cd</happy> ef", "English", nil)
	res, err := b.Speech(context.Background(), sp, text)
	if err != nil {
		t.Fatal(err)
	}
	g := lastGen(f)
	if g["control_spans"] != nil || g["controls"] != nil || g["text"] != "ab cd ef" {
		t.Errorf("an engine with no bundle must get plain text and no controls: %v", g)
	}
	if !strings.Contains(res.Notes[headerStyleWarnings], "no steering bundle") {
		t.Errorf("%v", res.Notes)
	}
}

func TestZonosStyle_APlainRequestSendsNoSteeringFields(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	b, _ := zonosBackend(f)
	if _, err := b.Speech(context.Background(), cloneSpec("v"), "no style here"); err != nil {
		t.Fatal(err)
	}
	g := lastGen(f)
	for _, k := range []string{"controls", "control_spans", "control_span_follow"} {
		if g[k] != nil {
			t.Errorf("%s was sent for an unstyled request: %v", k, g[k])
		}
	}
}

// A steered request whose stream breaks off fails, and says what steering was active: no quiet retry.
func TestZonosStyle_ACutStreamFailsAndNamesTheSteering(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.cutSteered = true
	b, _ := zonosBackend(f)
	sp, text := styledSpec("ab <sad:2>cd</sad> ef", "English", nil)
	if _, err := b.Speech(context.Background(), sp, text); err == nil || !strings.Contains(err.Error(), "steering was active") || !strings.Contains(err.Error(), "sad") {
		t.Errorf("buffered: %v", err)
	}
	gens := len(f.gens)
	s, err := b.SpeechStream(context.Background(), sp, text)
	if err != nil {
		t.Fatal(err)
	}
	_, rerr := io.ReadAll(s.Body)
	s.Body.Close()
	if rerr == nil || !strings.Contains(rerr.Error(), "steering was active") {
		t.Errorf("streamed: %v", rerr)
	}
	for _, g := range f.gens[gens:] {
		if g["control_spans"] == nil {
			t.Error("a failed steered request was quietly retried without its steering")
		}
	}
	// and an unsteered request on the same engine is fine
	if _, err := b.Speech(context.Background(), cloneSpec("v"), "plain"); err != nil {
		t.Errorf("an unsteered request failed: %v", err)
	}
}

func TestZonosStyle_TheBodyOfTheRequestIsValidJSONWithDisjointSpans(t *testing.T) {
	f := newFakeZonos(t, 0.1)
	f.laci = []string{"en_us"}
	b, _ := zonosBackend(f)
	sp, text := styledSpec("<happy>aa <whisper>bb</whisper> cc</happy> dd <sad>ee</sad>", "English", nil)
	if _, err := b.Speech(context.Background(), sp, text); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(lastGen(f))
	var back map[string]any
	if json.Unmarshal(raw, &back) != nil {
		t.Fatal("not JSON")
	}
	prev := -1.0
	for _, s := range spansOf(t, back) {
		a, e := s["start"].(float64), s["end"].(float64)
		if a < prev || e <= a {
			t.Errorf("spans overlap or are empty: %v", spansOf(t, back))
		}
		prev = e
	}
}

// Level is the loudness condition's job (measured: -17 gave -17.3 dB, -29 gave -31.4) and the steering tags shape voicing, not level (loud x2.25 moved it by 0.4 dB).
// So a tagged line is levelled like any other, in a stream and in a buffered WAV: an exemption would only have let its level wander.
func TestZonosStyle_ATaggedLineIsLevelledLikeAnyOther(t *testing.T) {
	f := newFakeZonos(t, 0.3)
	f.secs = 4
	b, _ := zonosBackend(f)
	margin := DefaultZonosOptions().Edge
	for _, tag := range []string{"whisper", "loud", "happy"} {
		sp, text := styledSpec("<"+tag+">a line of words that goes on for a while</"+tag+">", "English", nil)
		s, err := b.SpeechStream(context.Background(), sp, text)
		if err != nil {
			t.Fatal(err)
		}
		pcm, _ := io.ReadAll(s.Body)
		s.Body.Close()
		if got := rmsDB(pcm[2*24*margin.LeadMS : len(pcm)-2*24*margin.TailMS]); math.Abs(got-(-24)) > 3 {
			t.Errorf("<%s> streamed: %.1f dBFS, want about -24", tag, got)
		}
		res, err := b.Speech(context.Background(), sp, text)
		if err != nil {
			t.Fatal(err)
		}
		pcmW := res.Audio[44:]
		n, tail := 2*44100*margin.LeadMS/1000, 2*44100*margin.TailMS/1000
		smp := make([]float32, (len(pcmW)-n-tail)/2)
		for i := range smp {
			smp[i] = float32(int16(uint16(pcmW[n+2*i])|uint16(pcmW[n+2*i+1])<<8)) / 32768
		}
		if db, _ := ActiveRMSdB(smp, zonosNativeRate); math.Abs(db-(-24)) > 0.5 {
			t.Errorf("<%s> buffered: %.1f dBFS, want -24", tag, db)
		}
	}
}
