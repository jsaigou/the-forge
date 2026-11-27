package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestServerBackendSpeechAndRegister(t *testing.T) {
	var gotSpeech map[string]any
	var gotRegister map[string]any
	var registered bool

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/v1/audio/voices":
			registered = true
			json.Unmarshal(body, &gotRegister)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/audio/speech":
			json.Unmarshal(body, &gotSpeech)
			w.Header().Set("Content-Type", "audio/wav")
			w.Write([]byte("WAVDATA"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	b := staticServerBackend(ts.URL, ts.URL, ts.URL)
	res, err := b.Speech(context.Background(), SynthSpec{
		Mode:     ModeBase,
		ID:       "clone1",
		RefAudio: []byte("refwav"),
		RefText:  "transcript",
		Language: "English",
	}, "hello")
	if err != nil {
		t.Fatalf("speech: %v", err)
	}
	if string(res.Audio) != "WAVDATA" {
		t.Fatalf("audio mismatch: %q", res.Audio)
	}
	if !registered {
		t.Fatalf("clone was not registered with tts-server")
	}
	if gotRegister["name"] != "clone1" || gotRegister["ref_text"] != "transcript" {
		t.Fatalf("register payload wrong: %v", gotRegister)
	}
	if gotSpeech["input"] != "hello" || gotSpeech["voice"] != "clone1" {
		t.Fatalf("speech payload wrong: %v", gotSpeech)
	}
}

func TestServerBackendCustomVoiceNoRegister(t *testing.T) {
	registered := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/audio/voices" {
			registered = true
		}
		w.Write([]byte("WAVDATA"))
	}))
	defer ts.Close()

	b := staticServerBackend(ts.URL, ts.URL, ts.URL)
	_, err := b.Speech(context.Background(), SynthSpec{
		Mode:    ModeCustomVoice,
		ID:      "aiden",
		Speaker: "aiden",
	}, "hi")
	if err != nil {
		t.Fatalf("speech: %v", err)
	}
	if registered {
		t.Fatalf("customvoice should not register a clone")
	}
}

func TestServerBackendHealth(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Health never contacts an engine (asking the daemon for one may start it): it reports what the resolver knows.
	b := staticServerBackend(ts.URL, "", "")
	h, err := b.Health(context.Background())
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if h["status"] != "ok" || h["resolver"] != "static" {
		t.Fatalf("health status: %v", h)
	}
}

func TestServerBackendWithNoEngineIsAnErrorNotASlowFallback(t *testing.T) {
	b := staticServerBackend("", "", "")
	res, err := b.Speech(context.Background(), SynthSpec{Mode: ModeCustomVoice, ID: "aiden", Speaker: "aiden"}, "hi")
	if err == nil || res != nil {
		t.Fatalf("speech with no engine = %v, %v; want an error and no audio", res, err)
	}
}
