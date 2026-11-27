// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/tts/style"
)

func TestApplyTags(t *testing.T) {
	no := false
	yes := style.Defaults{}
	for name, tc := range map[string]struct {
		req    SpeechRequest
		input  string
		styled bool
	}{
		"plain":                  {SpeechRequest{Input: "Hello there."}, "Hello there.", false},
		"a bracket that is text": {SpeechRequest{Input: "if a < b then"}, "if a < b then", false},
		"a tag":                  {SpeechRequest{Input: "I am <sad>sorry</sad>."}, "I am sorry.", true},
		"tags switched off":      {SpeechRequest{Input: "I am <sad>sorry</sad>.", Tags: &no}, "I am <sad>sorry</sad>.", false},
		"defaults only":          {SpeechRequest{Input: "Hello.", Style: &yes}, "Hello.", true},
		"unknown tag is text":    {SpeechRequest{Input: "a <b>bold</b> move"}, "a <b>bold</b> move", false},
	} {
		got := applyTags(tc.req)
		if got.Input != tc.input || (got.styled != nil) != tc.styled {
			t.Errorf("%s: input %q styled=%v, want %q styled=%v", name, got.Input, got.styled != nil, tc.input, tc.styled)
		}
	}
}

// Kokoro cannot steer, and must never read a tag aloud: it gets the text without tags and the response says that was all that happened.
func TestDualEngine_KokoroGetsPlainTextAndAWarning(t *testing.T) {
	var sent string
	kts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.Header().Set("Content-Type", "audio/wav")
		w.Write([]byte("KOKOROWAV"))
	}))
	defer kts.Close()
	d := NewDualEngine(NewQwenTTS(&fakeBackend{}, NewRegistry(t.TempDir())), staticKokoroBackend(kts.URL, ""), "billie", "af_heart")
	res, err := d.Speech(context.Background(), SpeechRequest{Voice: "af_heart", Input: "Hello <happy>there</happy>, friend."})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sent, "<happy>") || strings.Contains(sent, "happy") || !strings.Contains(sent, "Hello there, friend.") {
		t.Errorf("Kokoro was sent %s", sent)
	}
	if !strings.Contains(res.Notes[headerStyleWarnings], "Kokoro") {
		t.Errorf("notes %v", res.Notes)
	}
}

// A request with no tags is exactly what it was: no notes, nothing changed.
func TestDualEngine_AnUntaggedRequestIsUntouched(t *testing.T) {
	fb := &fakeBackend{}
	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	d := NewDualEngine(NewQwenTTS(fb, reg), nil, "billie", "af_heart")
	res, err := d.Speech(context.Background(), SpeechRequest{Voice: "billie", Input: "if a < b then c > d"})
	if err != nil {
		t.Fatal(err)
	}
	if fb.lastText != "if a < b then c > d" || res.Notes != nil || fb.lastSpec.Style != nil {
		t.Errorf("text %q notes %v style %v", fb.lastText, res.Notes, fb.lastSpec.Style)
	}
}

// Qwen voice design cannot steer either: the tags come out of the text, the engine never sees them, and the response says so.
func TestDualEngine_QwenDesignGetsPlainTextAndAWarning(t *testing.T) {
	var sent map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&sent)
		w.Write([]byte("WAVDATA"))
	}))
	defer ts.Close()
	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	d := NewDualEngine(NewQwenTTS(staticServerBackend("", ts.URL, ""), reg), nil, "billie", "af_heart")
	res, err := d.Speech(context.Background(), SpeechRequest{Voice: "billie", Input: "It is <whisper>quiet</whisper> now"})
	if err != nil {
		t.Fatal(err)
	}
	if sent["input"] != "It is quiet now" {
		t.Errorf("the engine was sent %v", sent["input"])
	}
	if !strings.Contains(res.Notes[headerStyleWarnings], "cannot be steered") {
		t.Errorf("notes %v", res.Notes)
	}
}

func TestServer_StyleNotesBecomeResponseHeaders(t *testing.T) {
	srv, fe := newTestServer("")
	fe.speech = &SpeechResult{Audio: []byte("WAV"), Format: "wav", Notes: map[string]string{headerStyleApplied: "happy:3.0[0,5)", headerStyleWarnings: "line one\nline two é"}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	resp, _ := postSpeech(t, ts, "wav")
	if resp.Header.Get(headerStyleApplied) != "happy:3.0[0,5)" {
		t.Errorf("applied header %q", resp.Header.Get(headerStyleApplied))
	}
	if w := resp.Header.Get(headerStyleWarnings); strings.ContainsAny(w, "\n\r") || !strings.Contains(w, "line one") {
		t.Errorf("warnings header %q must be one safe line", w)
	}
}

func TestHeaderSafe(t *testing.T) {
	if got := headerSafe("a\nb\tc é"); got != "a b c ?" {
		t.Errorf("%q", got)
	}
	if long := headerSafe(strings.Repeat("x", 5000)); len(long) > 1600 {
		t.Errorf("not bounded: %d", len(long))
	}
}

func TestServer_StyleEndpoints(t *testing.T) {
	srv, _ := newTestServer("")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/style/vocabulary")
	if err != nil {
		t.Fatal(err)
	}
	var vocab struct {
		Tags []struct{ Name, Group string } `json:"tags"`
	}
	json.NewDecoder(resp.Body).Decode(&vocab)
	resp.Body.Close()
	emotions := 0
	for _, tg := range vocab.Tags {
		if tg.Group == "emotion" {
			emotions++
		}
	}
	if resp.StatusCode != 200 || emotions != 23 {
		t.Errorf("vocabulary: HTTP %d, %d emotions", resp.StatusCode, emotions)
	}

	body := `{"text":"Hello <sad>there</sad> <age:+1>friend","style":{"base":[{"tag":"lively","strength":1}]}}`
	resp, err = http.Post(ts.URL+"/v1/style/parse", "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Spoken  string `json:"spoken"`
		HasTags bool   `json:"has_tags"`
		Uses    []struct {
			Tag        string
			Start, End int
		} `json:"uses"`
		Plan struct {
			Spans []struct{ Start, End int } `json:"spans"`
			Raw   map[string]float64         `json:"raw"`
		} `json:"plan"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.Spoken != "Hello there friend" || !out.HasTags || len(out.Uses) != 2 || len(out.Plan.Spans) != 1 || out.Plan.Spans[0].Start != 6 || out.Plan.Raw["identity.age"] != 1 {
		t.Errorf("parse: %+v", out)
	}
	if r, _ := http.Get(ts.URL + "/v1/style/parse"); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET parse: %d", r.StatusCode)
	}
}
