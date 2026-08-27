package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKokoroBackendSpeechAndList(t *testing.T) {
	var gotSpeech map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/speech":
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &gotSpeech)
			w.Header().Set("Content-Type", "audio/wav")
			w.Write([]byte("KOKOROWAV"))
		case "/v1/audio/voices":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
				{"id": "af_heart"}, {"id": "am_michael"},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	b := NewKokoroBackend(ts.URL, "")

	res, err := b.Speech(context.Background(), SynthSpec{Mode: ModeKokoro, ID: "af_heart", Speed: 1.0}, "hello")
	if err != nil {
		t.Fatalf("speech: %v", err)
	}
	if string(res.Audio) != "KOKOROWAV" {
		t.Fatalf("audio mismatch: %q", res.Audio)
	}
	if gotSpeech["voice"] != "af_heart" || gotSpeech["input"] != "hello" {
		t.Fatalf("speech payload wrong: %v", gotSpeech)
	}

	voices, err := b.ListVoices(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(voices) != 2 || voices[0].Tier != "fast" || voices[0].Type != "kokoro" {
		t.Fatalf("unexpected voices: %+v", voices)
	}
}

func TestIsKokoroVoice(t *testing.T) {
	koko := []string{"af_heart", "am_michael", "bf_emma", "zf_xiaobei", "jm_kumo"}
	for _, v := range koko {
		if !IsKokoroVoice(v) {
			t.Fatalf("%q should be a Kokoro voice", v)
		}
	}
	not := []string{"billie", "samantha", "bmo", "anthony", "me"}
	for _, v := range not {
		if IsKokoroVoice(v) {
			t.Fatalf("%q should NOT be a Kokoro voice", v)
		}
	}
}

type fakeBackend struct {
	lastSpec SynthSpec
	lastText string
}

func (f *fakeBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	f.lastSpec = spec
	f.lastText = text
	return &SpeechResult{Audio: []byte("QWENWAV"), Format: "wav"}, nil
}
func (f *fakeBackend) Health(ctx context.Context) (map[string]any, error) {
	return map[string]any{"status": "ok"}, nil
}
func (f *fakeBackend) Close() error { return nil }

func TestDualEngineRoutesByNamespace(t *testing.T) {
	kts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		w.Write([]byte("KOKOROWAV"))
	}))
	defer kts.Close()
	kokoro := NewKokoroBackend(kts.URL, "")

	fb := &fakeBackend{}
	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	qwen := NewQwenTTS(fb, reg, nil)

	dual := NewDualEngine(qwen, kokoro, "billie", "af_heart")

	// fast tier -> kokoro
	r1, err := dual.Speech(context.Background(), SpeechRequest{Voice: "af_heart", Input: "hi"})
	if err != nil {
		t.Fatalf("kokoro speech: %v", err)
	}
	if string(r1.Audio) != "KOKOROWAV" {
		t.Fatalf("expected kokoro audio, got %q", r1.Audio)
	}
	if fb.lastText != "" {
		t.Fatalf("premium backend should not have been called for kokoro voice")
	}

	// premium tier -> qwen
	r2, err := dual.Speech(context.Background(), SpeechRequest{Voice: "billie", Input: "hi"})
	if err != nil {
		t.Fatalf("qwen speech: %v", err)
	}
	if string(r2.Audio) != "QWENWAV" {
		t.Fatalf("expected qwen audio, got %q", r2.Audio)
	}

	// explicit model=kokoro overrides
	r3, err := dual.Speech(context.Background(), SpeechRequest{Model: "kokoro", Voice: "billie", Input: "hi"})
	if err != nil {
		t.Fatalf("model=kokoro speech: %v", err)
	}
	if string(r3.Audio) != "KOKOROWAV" {
		t.Fatalf("expected kokoro audio via model override, got %q", r3.Audio)
	}
}

func TestDualEngineListMerge(t *testing.T) {
	kts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": "af_heart"}, {"id": "am_michael"},
		}})
	}))
	defer kts.Close()
	kokoro := NewKokoroBackend(kts.URL, "")

	reg := NewRegistry(t.TempDir())
	reg.Put(VoiceEntry{ID: "billie", Name: "Billie", Type: "design", Design: &DesignSpec{Instruct: "warm"}})
	qwen := NewQwenTTS(&fakeBackend{}, reg, nil)

	dual := NewDualEngine(qwen, kokoro, "billie", "af_heart")
	voices, err := dual.ListVoices(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var fast, premium int
	for _, v := range voices {
		if v.Tier == "fast" {
			fast++
		} else if v.Tier == "premium" {
			premium++
		}
	}
	if fast != 2 {
		t.Fatalf("expected 2 fast voices, got %d", fast)
	}
	if premium != 1 {
		t.Fatalf("expected 1 premium voice, got %d", premium)
	}
}
