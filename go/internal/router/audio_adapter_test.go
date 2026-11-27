// SPDX-License-Identifier: Apache-2.0

package router

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// audioEnv is a d1Env-style router whose single fake slot answers chat (ASR) and /tts, over this catalog:
//
//	omni-test   visible, audio in + audio out, text out
//	text-test   visible, text only
//	omni-hidden hidden,  audio in + audio out
type audioEnv struct{ *d1Env }

const asrAnswer = `{"choices":[{"message":{"role":"assistant","content":" hello forge \n"}}],"usage":{"prompt_tokens":30,"completion_tokens":3}}`

func newAudioEnv(t *testing.T) (*audioEnv, *httptest.Server) {
	t.Helper()
	e := newD1Env(t)
	cat := e.db.Catalog()
	both := []string{store.OutputText, store.OutputAudio}
	audioIn := seedConfigOpts{modelOutputs: both, modelModalities: []string{"text", "audio"}, withMMProj: true}
	seedCatalogConfig(t, cat, "omni-test", 4096, "visible", audioIn)
	seedCatalogConfig(t, cat, "omni-hidden", 4096, "hidden", audioIn)
	seedCatalogConfig(t, cat, "text-test", 4096, "visible")
	e.respBody = asrAnswer
	// forge-tts stand-in
	tts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("FORGE-TTS"))
	}))
	t.Cleanup(tts.Close)
	e.srv.cfg().TTSURL = tts.URL + "/v1"
	return &audioEnv{e}, tts
}

func (e *audioEnv) transcribe(t *testing.T, fields map[string]string, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	if filename != "" {
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write(data)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/v1/audio/transcriptions", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTranscriptions_BuildsChatRequestAndShapesResponse(t *testing.T) {
	e, _ := newAudioEnv(t)
	rec := e.transcribe(t, map[string]string{"model": "omni-test", "language": "ja"}, "clip.wav", []byte("RIFFxx"))
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["text"] != "hello forge" {
		t.Errorf("text = %q", out["text"])
	}
	if e.gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q", e.gotPath)
	}
	var sent struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(e.gotBody, &sent)
	if sent.Messages[0].Content != "Perform ASR in Japanese." {
		t.Errorf("system prompt = %v", sent.Messages[0].Content)
	}
	part := sent.Messages[1].Content.([]any)[0].(map[string]any)["input_audio"].(map[string]any)
	if part["format"] != "wav" || part["data"] != "UklGRnh4" {
		t.Errorf("input_audio = %v", part)
	}
	if atomic.LoadInt32(e.loads) != 1 {
		t.Errorf("loads = %d, want 1 (on-demand)", *e.loads)
	}
}

func TestTranscriptions_TextFormatAndDetectedLanguage(t *testing.T) {
	e, _ := newAudioEnv(t)
	rec := e.transcribe(t, map[string]string{"model": "omni-test", "response_format": "text"}, "c.mp3", []byte("x"))
	if rec.Code != 200 || rec.Body.String() != "hello forge\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("got %d %q %q", rec.Code, rec.Body, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(string(e.gotBody), `"Perform ASR."`) || !strings.Contains(string(e.gotBody), `"mp3"`) {
		t.Errorf("body = %s", e.gotBody)
	}
}

func TestTranscriptions_Rejections(t *testing.T) {
	e, _ := newAudioEnv(t)
	cases := []struct {
		name   string
		fields map[string]string
		file   string
		code   int
		errKey string
	}{
		{"no model", map[string]string{}, "a.wav", 422, "validation_failed"},
		{"no file", map[string]string{"model": "omni-test"}, "", 422, "validation_failed"},
		{"flac", map[string]string{"model": "omni-test"}, "a.flac", 422, "validation_failed"},
		{"bad language", map[string]string{"model": "omni-test", "language": "xx"}, "a.wav", 422, "validation_failed"},
		{"verbose_json", map[string]string{"model": "omni-test", "response_format": "verbose_json"}, "a.wav", 422, "validation_failed"},
		{"unknown model is a 404, not forge-stt", map[string]string{"model": "whisper-1"}, "a.wav", 404, "model_not_found"},
		{"hidden", map[string]string{"model": "omni-hidden"}, "a.wav", 404, "model_not_routable"},
		{"text model has no audio input", map[string]string{"model": "text-test"}, "a.wav", 400, "unsupported_modality"},
	}
	for _, c := range cases {
		rec := e.transcribe(t, c.fields, c.file, []byte("x"))
		if rec.Code != c.code || errCode(t, rec)["error"] != c.errKey {
			t.Errorf("%s: got %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if e.calls() != 0 {
		t.Errorf("a rejected request reached the slot %d times", e.calls())
	}
}

func TestTranscriptions_UpstreamErrorRelayed(t *testing.T) {
	e, _ := newAudioEnv(t)
	e.status, e.respBody = 500, `{"error":{"message":"audio too long"}}`
	rec := e.transcribe(t, map[string]string{"model": "omni-test"}, "a.wav", []byte("x"))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "audio too long") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func TestTranscriptions_UnshapeableSuccessIs502(t *testing.T) {
	e, _ := newAudioEnv(t)
	e.respBody = `{"choices":[]}`
	rec := e.transcribe(t, map[string]string{"model": "omni-test"}, "a.wav", []byte("x"))
	if rec.Code != 502 || errCode(t, rec)["error"] != "bad_upstream_response" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}

func (e *audioEnv) speech(body string) *httptest.ResponseRecorder {
	return e.post("/v1/audio/speech", body)
}

func TestSpeech_CatalogModelUsesSlotTTSAtRoot(t *testing.T) {
	e, _ := newAudioEnv(t)
	e.respBody = "SLOTWAV"
	rec := e.speech(`{"model":"omni-test","input":"hi there","voice":"male","language":"en","response_format":"wav"}`)
	if rec.Code != 200 || rec.Body.String() != "SLOTWAV" {
		t.Fatalf("got %d %q", rec.Code, rec.Body)
	}
	if e.gotPath != "/tts" {
		t.Errorf("upstream path = %q, want /tts (server root, not /v1)", e.gotPath)
	}
	var sent map[string]string
	_ = json.Unmarshal(e.gotBody, &sent)
	if sent["input"] != "hi there" || sent["voice"] != "male" || sent["lang"] != "en" || len(sent) != 3 {
		t.Errorf("tts body = %s", e.gotBody)
	}
}

func TestSpeech_OtherNamesStayOnForgeTTS(t *testing.T) {
	e, _ := newAudioEnv(t)
	for _, body := range []string{
		`{"model":"tts-1","input":"hi"}`,
		`{"model":"qwen3-tts","voice":"billie","input":"hi"}`,
		`{"input":"hi"}`,
		`not json`,
	} {
		rec := e.speech(body)
		if rec.Code != 200 || rec.Body.String() != "FORGE-TTS" {
			t.Errorf("%s: got %d %q, want forge-tts", body, rec.Code, rec.Body)
		}
	}
	if e.calls() != 0 {
		t.Errorf("slot was called %d times", e.calls())
	}
}

func TestSpeech_CatalogRejections(t *testing.T) {
	e, _ := newAudioEnv(t)
	cases := []struct {
		name, body string
		code       int
		errKey     string
	}{
		{"text-only model", `{"model":"text-test","input":"hi"}`, 400, "unsupported_capability"},
		{"hidden", `{"model":"omni-hidden","input":"hi"}`, 404, "model_not_routable"},
		{"empty input", `{"model":"omni-test","input":" "}`, 422, "validation_failed"},
		{"mp3", `{"model":"omni-test","input":"hi","response_format":"mp3"}`, 422, "validation_failed"},
		{"speed", `{"model":"omni-test","input":"hi","speed":1.5}`, 422, "validation_failed"},
	}
	for _, c := range cases {
		rec := e.speech(c.body)
		if rec.Code != c.code || errCode(t, rec)["error"] != c.errKey {
			t.Errorf("%s: got %d %s", c.name, rec.Code, rec.Body)
		}
	}
	if e.calls() != 0 {
		t.Errorf("a rejected request reached the slot %d times", e.calls())
	}
}

func TestAudio_RequiresAuth(t *testing.T) {
	e, _ := newAudioEnv(t)
	for _, p := range []string{"/v1/audio/speech", "/v1/audio/transcriptions"} {
		req := httptest.NewRequest("POST", p, strings.NewReader(`{}`))
		req.RemoteAddr = "203.0.113.9:1234" // off-tailnet, no bearer
		rec := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s: status %d, want 401", p, rec.Code)
		}
	}
}

// A success row is written from a callback after the response is sent; it must still carry the verb's own action
// (it was once filed as chat_completion) and the token usage.
func TestAudio_AuditActionOnSuccess(t *testing.T) {
	e, _ := newAudioEnv(t)
	if rec := e.transcribe(t, map[string]string{"model": "omni-test"}, "a.wav", []byte("x")); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	e.respBody = d1Answer
	if rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`}`); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	e.audit.mu.Lock()
	defer e.audit.mu.Unlock()
	want := map[string]string{"transcription": "input_tokens=30,output_tokens=3", "systemone": "input_tokens=43"}
	for action, detail := range want {
		found := false
		for _, en := range e.audit.entries {
			if en.Action == action && strings.Contains(en.Detail, detail) {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q audit row containing %q; rows: %+v", action, detail, e.audit.entries)
		}
	}
}
