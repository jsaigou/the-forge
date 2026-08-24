package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

type fakeEngine struct {
	voices   []VoiceEntry
	samples  map[string][]byte
	speech   *SpeechResult
	speechErr error
	models   []ModelInfo
}

func (f *fakeEngine) Health(ctx context.Context) (map[string]any, error) {
	return map[string]any{"status": "ok"}, nil
}
func (f *fakeEngine) Models() []ModelInfo {
	if f.models != nil {
		return f.models
	}
	return []ModelInfo{{ID: "tts-1", Object: "model", OwnedBy: "forge"}}
}
func (f *fakeEngine) Speech(ctx context.Context, req SpeechRequest) (*SpeechResult, error) {
	return f.speech, f.speechErr
}
func (f *fakeEngine) ListVoices(ctx context.Context) ([]VoiceEntry, error) { return f.voices, nil }
func (f *fakeEngine) GetVoice(ctx context.Context, id string) (VoiceEntry, bool, error) {
	for _, v := range f.voices {
		if v.ID == id {
			return v, true, nil
		}
	}
	return VoiceEntry{}, false, nil
}
func (f *fakeEngine) SaveVoice(ctx context.Context, v VoiceEntry) error {
	f.voices = append(f.voices, v)
	return nil
}
func (f *fakeEngine) Preview(ctx context.Context, p PreviewRequest) (*SpeechResult, error) {
	return f.speech, f.speechErr
}
func (f *fakeEngine) GenerateSample(ctx context.Context, id string) error { return nil }
func (f *fakeEngine) DeleteVoice(ctx context.Context, id string) error { return nil }
func (f *fakeEngine) GetSample(ctx context.Context, id string) ([]byte, error) {
	if d, ok := f.samples[id]; ok {
		return d, nil
	}
	return nil, errNotFound
}
func (f *fakeEngine) SetSample(ctx context.Context, id string, wav []byte) error {
	if f.samples == nil {
		f.samples = map[string][]byte{}
	}
	f.samples[id] = wav
	return nil
}

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

const errNotFound = sentinelError("not found")

func newTestServer(token string) (*Server, *fakeEngine) {
	fe := &fakeEngine{samples: map[string][]byte{}}
	audioDir, _ := os.MkdirTemp("", "tts-audio-")
	srv := NewServer(fe, token, audioDir)
	return srv, fe
}

func TestHealthAndModels(t *testing.T) {
	srv, _ := newTestServer("")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var h map[string]any
	json.NewDecoder(resp.Body).Decode(&h)
	if h["model_ready"] != true {
		t.Fatalf("model_ready missing: %v", h)
	}

	resp2, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("models status %d", resp2.StatusCode)
	}
}

func TestSpeech(t *testing.T) {
	srv, fe := newTestServer("")
	fe.speech = &SpeechResult{Audio: []byte("RIFF...."), Format: "wav"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(SpeechRequest{Input: "hello", Voice: "aiden"})
	resp, err := http.Post(ts.URL+"/v1/audio/speech", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("speech status %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "RIFF...." {
		t.Fatalf("audio mismatch")
	}
}

func TestVoicesListAndAuth(t *testing.T) {
	srv, fe := newTestServer("secret")
	fe.voices = []VoiceEntry{{ID: "x", Name: "X", Type: "design", Design: &DesignSpec{Instruct: "secret-instruction"}}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// no token -> 200, sensitive fields stripped
	resp, _ := http.Get(ts.URL + "/v1/voices")
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 without token, got %d", resp.StatusCode)
	}
	var anon struct {
		Voices []voiceView `json:"voices"`
	}
	json.NewDecoder(resp.Body).Decode(&anon)
	resp.Body.Close()
	if len(anon.Voices) == 0 || anon.Voices[0].ID != "x" {
		t.Fatalf("expected voice list without token: %+v", anon.Voices)
	}
	if anon.Voices[0].Instruct != "" {
		t.Fatalf("expected instruct stripped without token, got %q", anon.Voices[0].Instruct)
	}

	// with token -> 200, full detail
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/voices", nil)
	req.Header.Set("X-Forge-Internal-Token", "secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200 with token, got %d", resp2.StatusCode)
	}
	var out struct {
		Voices []voiceView `json:"voices"`
	}
	json.NewDecoder(resp2.Body).Decode(&out)
	if len(out.Voices) == 0 || out.Voices[0].Instruct != "secret-instruction" {
		t.Fatalf("expected instruct with token: %+v", out.Voices)
	}
}

func TestImportMultipartAndDelete(t *testing.T) {
	srv, fe := newTestServer("secret")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("id", "v2")
	_ = mw.WriteField("name", "V2")
	_ = mw.WriteField("type", "clone")
	_ = mw.WriteField("ref_text", "transcript")
	w, _ := mw.CreateFormFile("ref_audio", "ref.wav")
	w.Write([]byte("refwav"))
	mw.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/voices/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Forge-Internal-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import status %d: %s", resp.StatusCode, string(b))
	}
	if len(fe.voices) != 1 || fe.voices[0].Clone == nil {
		t.Fatalf("import did not save clone: %+v", fe.voices)
	}

	// delete
	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/voices/v2", nil)
	delReq.Header.Set("X-Forge-Internal-Token", "secret")
	resp3, _ := http.DefaultClient.Do(delReq)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("delete status %d", resp3.StatusCode)
	}
}

func TestSampleGetSet(t *testing.T) {
	srv, fe := newTestServer("secret")
	fe.samples["v3"] = []byte("samplewav")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/voices/v3/sample", nil)
	req.Header.Set("X-Forge-Internal-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("sample get status %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "samplewav" {
		t.Fatalf("sample data mismatch")
	}

	// range request -> 206
	reqR, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/voices/v3/sample", nil)
	reqR.Header.Set("X-Forge-Internal-Token", "secret")
	reqR.Header.Set("Range", "bytes=0-3")
	respR, _ := http.DefaultClient.Do(reqR)
	if respR.StatusCode != http.StatusPartialContent {
		t.Fatalf("expected 206 for range, got %d", respR.StatusCode)
	}
	respR.Body.Close()

	// missing sample -> 404
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/voices/nope/sample", nil)
	req2.Header.Set("X-Forge-Internal-Token", "secret")
	resp2, _ := http.DefaultClient.Do(req2)
	resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Fatalf("expected 404 for missing sample, got %d", resp2.StatusCode)
	}
}

func TestVoicesListModelsWarm(t *testing.T) {
	srv, _ := newTestServer("secret")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/voices", nil)
	req.Header.Set("X-Forge-Internal-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Voices      []voiceView `json:"voices"`
		ModelsWarm  map[string]bool `json:"models_warm"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	if out.ModelsWarm == nil {
		t.Fatalf("models_warm missing")
	}
	for _, v := range out.Voices {
		if v.ID == "aiden" && !v.Builtin {
			t.Fatalf("aiden should be builtin")
		}
	}
}

func TestPreview(t *testing.T) {
	srv, fe := newTestServer("secret")
	fe.speech = &SpeechResult{Audio: []byte("PREVIEWWAV"), Format: "wav"}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(PreviewRequest{Mode: "design", Instruct: "warm", Text: "hi"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/voices/preview", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Internal-Token", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("preview status %d", resp.StatusCode)
	}
	d, _ := io.ReadAll(resp.Body)
	if string(d) != "PREVIEWWAV" {
		t.Fatalf("preview audio mismatch")
	}
}

func TestCreateConflict(t *testing.T) {
	srv, fe := newTestServer("secret")
	fe.voices = []VoiceEntry{{ID: "exists", Name: "E", Type: "design"}}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{"id": "exists", "name": "E", "type": "design", "instruct": "x"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/voices", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Internal-Token", "secret")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for existing id, got %d", resp.StatusCode)
	}
}
