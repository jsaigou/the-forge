package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// floatWAV builds a float32 WAV with a chunk (LIST) between fmt and data, as TTS engines and ffmpeg write them.
func floatWAV(rate, channels int, seconds float64) []byte {
	n := int(seconds * float64(rate))
	data := make([]byte, 0, n*channels*4)
	for i := 0; i < n; i++ {
		v := float32(0.3 * math.Sin(2*math.Pi*220*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			data = binary.LittleEndian.AppendUint32(data, math.Float32bits(v))
		}
	}
	var b bytes.Buffer
	le := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.WriteString("RIFF")
	le(uint32(4 + 8 + 16 + 8 + 4 + 8 + len(data)))
	b.WriteString("WAVEfmt ")
	le(uint32(16))
	le(uint16(3))
	le(uint16(channels))
	le(uint32(rate))
	le(uint32(rate * channels * 4))
	le(uint16(channels * 4))
	le(uint16(32))
	b.WriteString("LIST")
	le(uint32(4))
	b.WriteString("INFO")
	b.WriteString("data")
	le(uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}

func TestCanonicalWAVConvertsToWhatTheEngineReads(t *testing.T) {
	out, err := canonicalWAV(floatWAV(48000, 2, 4))
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := ParseWAV(out)
	if err != nil {
		t.Fatalf("the result must be a canonical WAV: %v", err)
	}
	if got := float64(len(pcm)) / (sampleRate * 2); math.Abs(got-4) > 0.01 {
		t.Errorf("duration %.3f s, want 4", got)
	}
	if pcmRMS(pcm) < 0.05 {
		t.Errorf("the audio was lost in conversion (level %.4f)", pcmRMS(pcm))
	}
	// Already canonical audio passes through with the same length.
	again, err := canonicalWAV(out)
	if err != nil || len(again) != len(out) {
		t.Errorf("a canonical WAV must come back unchanged in size: %d vs %d (%v)", len(again), len(out), err)
	}
	for name, bad := range map[string][]byte{"not a wav": []byte("hello world, not audio"), "no data": floatWAV(24000, 1, 0)[:36]} {
		if _, err := canonicalWAV(bad); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

// fakeVoiceStudio speaks a fixed WAV for any text and records how it was called.
type fakeVoiceStudio struct {
	mu        sync.Mutex
	generated int
	deleted   []string
	seconds   float64
	failJob   bool
	srv       *httptest.Server
}

func newFakeVoiceStudio(t *testing.T, seconds float64) *fakeVoiceStudio {
	f := &fakeVoiceStudio{seconds: seconds}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/voices", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"voices": []map[string]any{
			{"id": "sana", "name": "Sana", "language": "English", "builtin": false},
			{"id": "bad id!", "name": "x"},
		}})
	})
	mux.HandleFunc("POST /api/generate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.generated++
		f.mu.Unlock()
		// The real voice-studio field is response_format (default mp3); a request that does not say wav gets mp3 there, which the engine cannot read.
		var req struct {
			Input          string `json:"input"`
			Voice          string `json:"voice"`
			ResponseFormat string `json:"response_format"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Voice != "sana" || req.ResponseFormat != "wav" || req.Input == "" {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		writeJSON(w, 202, map[string]any{"job_id": "job1", "state": "queued"})
	})
	mux.HandleFunc("GET /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.failJob {
			writeJSON(w, 200, map[string]any{"state": "failed", "error": "engine down"})
			return
		}
		writeJSON(w, 200, map[string]any{"state": "complete"})
	})
	mux.HandleFunc("GET /api/jobs/{id}/audio", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(floatWAV(48000, 1, f.seconds))
	})
	mux.HandleFunc("DELETE /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.deleted = append(f.deleted, r.PathValue("id"))
		f.mu.Unlock()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newSampleServer(t *testing.T, vs string) *httptest.Server {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(Options{Store: st, VoiceStudioURL: vs}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestSampleIsMadeByVoiceStudioStoredCachedAndTheJobRemoved(t *testing.T) {
	vs := newFakeVoiceStudio(t, 6)
	ts := newSampleServer(t, vs.srv.URL)
	body := map[string]string{"voice": "sana", "text": "Hey Alex, it's Haru. How has your day been?"}
	code, got := jsonCall(t, ts, "POST", "/api/sample", body)
	id, _ := got["id"].(string)
	if code != 200 || id == "" || got["cached"] != false || got["seconds"].(float64) < 5.9 {
		t.Fatalf("make sample: %d %v", code, got)
	}
	code, wav := call(t, ts, "GET", "/api/samples/"+id+"/audio", nil, nil)
	if _, err := ParseWAV(wav); code != 200 || err != nil {
		t.Fatalf("the stored sample must be a canonical WAV the engine can read: %d %v", code, err)
	}
	vs.mu.Lock()
	deleted := append([]string(nil), vs.deleted...)
	vs.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "job1" {
		t.Errorf("the voice-studio job must be removed after the sample is stored: %v", deleted)
	}
	// The same voice and text again is served from the cache without asking voice-studio.
	code, again := jsonCall(t, ts, "POST", "/api/sample", body)
	if code != 200 || again["id"] != id || again["cached"] != true {
		t.Fatalf("second request: %d %v", code, again)
	}
	if vs.generated != 1 {
		t.Errorf("voice-studio was asked %d times, want 1", vs.generated)
	}
}

func TestSampleRefusalsAndFailuresAreReportedAndCleanedUp(t *testing.T) {
	vs := newFakeVoiceStudio(t, 1.5) // shorter than the 3 s Haru needs
	ts := newSampleServer(t, vs.srv.URL)
	if code, b := jsonCall(t, ts, "POST", "/api/sample", map[string]string{"voice": "sana", "text": "Hi."}); code != 422 || !strings.Contains(b["error"].(string), "at least 3") {
		t.Errorf("a too short sample: %d %v", code, b)
	}
	if len(vs.deleted) != 1 {
		t.Errorf("a refused sample must still remove its job: %v", vs.deleted)
	}
	for name, body := range map[string]map[string]string{
		"no voice":   {"text": "Hello there"},
		"bad voice":  {"voice": "../x", "text": "Hello there"},
		"empty text": {"voice": "sana", "text": "  "},
		"long text":  {"voice": "sana", "text": strings.Repeat("a", sampleMaxText+1)},
	} {
		if code, _ := jsonCall(t, ts, "POST", "/api/sample", body); code != 422 {
			t.Errorf("%s: %d, want 422", name, code)
		}
	}
	vs.failJob = true
	if code, b := jsonCall(t, ts, "POST", "/api/sample", map[string]string{"voice": "sana", "text": "A longer line of speech"}); code != 502 || !strings.Contains(b["error"].(string), "engine down") {
		t.Errorf("a failed job: %d %v", code, b)
	}
}

func TestSampleFeatureIsOffWithoutAVoiceStudio(t *testing.T) {
	ts := newSampleServer(t, "")
	if code, _ := jsonCall(t, ts, "POST", "/api/sample", map[string]string{"voice": "sana", "text": "Hello there"}); code != 404 {
		t.Errorf("sample without voice-studio: %d, want 404", code)
	}
	if code, _ := call(t, ts, "GET", "/api/vs/voices", nil, nil); code != 404 {
		t.Errorf("voice list without voice-studio: %d, want 404", code)
	}
	_, b := call(t, ts, "GET", "/api/config", nil, nil)
	if !strings.Contains(string(b), `"voice_studio_configured":false`) {
		t.Errorf("config must say the feature is off: %s", b)
	}
}

func TestVoiceStudioVoicesAreListedWithoutUnsafeIDs(t *testing.T) {
	vs := newFakeVoiceStudio(t, 5)
	ts := newSampleServer(t, vs.srv.URL)
	code, b := call(t, ts, "GET", "/api/vs/voices", nil, nil)
	var list []struct{ ID, Name, Language string }
	if code != 200 || json.Unmarshal(b, &list) != nil || len(list) != 1 || list[0].ID != "sana" || list[0].Language != "English" {
		t.Fatalf("voices: %d %s", code, b)
	}
}
