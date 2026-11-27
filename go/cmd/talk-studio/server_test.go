package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, duplexURL, token string) (*Server, *httptest.Server) {
	t.Helper()
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Options{Store: st, DuplexURL: duplexURL, DuplexToken: token})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func call(t *testing.T, ts *httptest.Server, method, path string, body io.Reader, hdr map[string]string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func jsonCall(t *testing.T, ts *httptest.Server, method, path string, v any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(v)
	code, b := call(t, ts, method, path, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json"})
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return code, out
}

func wavOf(seconds float64) []byte {
	n := int(seconds*sampleRate) * 2
	pcm := make([]byte, n)
	for i := range pcm {
		pcm[i] = byte(i * 7)
	}
	return append(WAVHeader(n), pcm...)
}

func upload(t *testing.T, ts *httptest.Server, name string, wav []byte) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("name", name)
	fw, _ := mw.CreateFormFile("file", "v.wav")
	_, _ = fw.Write(wav)
	_ = mw.Close()
	return call(t, ts, "POST", "/api/voices", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
}

func TestConfigListsTheSeededModel(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	code, b := call(t, ts, "GET", "/api/config", nil, nil)
	var cfg struct {
		Models []Model `json:"models"`
		Frame  map[string]int
	}
	if code != 200 || json.Unmarshal(b, &cfg) != nil || len(cfg.Models) != 1 || cfg.Models[0].ID != "haru" || cfg.Models[0].Card != "haru" || cfg.Frame["samples"] != 3200 {
		t.Fatalf("config %d %s", code, b)
	}
}

func TestModelRegistryIsDataAndIsValidated(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	two := []Model{{ID: "haru", Label: "Haru", Tenant: "haru", Card: "haru"}, {ID: "next-s2s", Label: "Next", Tenant: "next", Card: "text", Reference: true}}
	if code, _ := jsonCall(t, ts, "PUT", "/api/models", two); code != 200 {
		t.Fatalf("a second model must be addable as data: %d", code)
	}
	for name, bad := range map[string][]Model{
		"empty":         {},
		"duplicate id":  {{ID: "a", Label: "A", Tenant: "a", Card: "none"}, {ID: "a", Label: "B", Tenant: "b", Card: "none"}},
		"no tenant":     {{ID: "a", Label: "A", Card: "none"}},
		"unknown card":  {{ID: "a", Label: "A", Tenant: "a", Card: "weird"}},
		"uppercase id":  {{ID: "Haru", Label: "A", Tenant: "a", Card: "none"}},
		"no label":      {{ID: "a", Tenant: "a", Card: "none"}},
		"path-like id":  {{ID: "../x", Label: "A", Tenant: "a", Card: "none"}},
		"overlong name": {{ID: "a", Label: "A", Tenant: strings.Repeat("t", 65), Card: "none"}},
	} {
		if code, _ := jsonCall(t, ts, "PUT", "/api/models", bad); code != 422 {
			t.Errorf("%s: %d, want 422", name, code)
		}
	}
	_, b := call(t, ts, "GET", "/api/models", nil, nil)
	if !strings.Contains(string(b), "next-s2s") {
		t.Errorf("a rejected write changed the registry: %s", b)
	}
}

func TestVoicesAreValidatedStoredAndServedBack(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	wav := wavOf(1.5)
	code, b := upload(t, ts, " Reina ", wav)
	var v Voice
	if code != 201 || json.Unmarshal(b, &v) != nil || v.Name != "Reina" || v.Seconds < 1.49 || v.Seconds > 1.51 {
		t.Fatalf("add voice: %d %s", code, b)
	}
	code, got := call(t, ts, "GET", "/api/voices/"+v.ID+"/audio", nil, nil)
	if code != 200 || !bytes.Equal(got, wav) {
		t.Fatalf("the stored voice must come back byte for byte: %d (%d bytes vs %d)", code, len(got), len(wav))
	}
	// Bad uploads are refused with a reason, and nothing is stored.
	bad := map[string][]byte{
		"not a wav":   []byte("hello"),
		"stereo":      func() []byte { w := wavOf(1); w[22] = 2; return w }(),
		"wrong rate":  func() []byte { w := wavOf(1); w[24] = 0x44; w[25] = 0xAC; return w }(),
		"length lies": func() []byte { w := wavOf(1); return w[:len(w)-10] }(),
		"too long":    wavOf(50),
		"no audio":    WAVHeader(0),
	}
	for name, w := range bad {
		if code, _ := upload(t, ts, "x", w); code != 422 && code != 400 {
			t.Errorf("%s: %d, want a refusal", name, code)
		}
	}
	if code, _ := upload(t, ts, "", wav); code != 422 {
		t.Errorf("a voice without a name: %d", code)
	}
	_, list := call(t, ts, "GET", "/api/voices", nil, nil)
	var vs []Voice
	_ = json.Unmarshal(list, &vs)
	if len(vs) != 1 {
		t.Errorf("refused uploads left %d voices", len(vs))
	}
	if code, _ := call(t, ts, "DELETE", "/api/voices/"+v.ID, nil, nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call(t, ts, "GET", "/api/voices/"+v.ID+"/audio", nil, nil); code != 404 {
		t.Errorf("a deleted voice is still served: %d", code)
	}
	if code, _ := call(t, ts, "GET", "/api/voices/..%2F..%2Fmodels.json/audio", nil, nil); code != 404 {
		t.Errorf("a path-shaped voice id was not refused: %d", code)
	}
}

func TestCardPresets(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	code, out := jsonCall(t, ts, "POST", "/api/cards", map[string]any{"name": "Cafe", "model": "haru", "card": map[string]any{"language": "ja", "name": "ハル"}})
	if code != 200 || out["id"] == "" {
		t.Fatalf("save: %d %v", code, out)
	}
	id := out["id"].(string)
	// Saving with the id replaces.
	if code, _ := jsonCall(t, ts, "POST", "/api/cards", map[string]any{"id": id, "name": "Cafe 2", "model": "haru", "card": map[string]any{"language": "en"}}); code != 200 {
		t.Fatalf("replace: %d", code)
	}
	_, b := call(t, ts, "GET", "/api/cards", nil, nil)
	var cs []CardPreset
	_ = json.Unmarshal(b, &cs)
	if len(cs) != 1 || cs[0].Name != "Cafe 2" {
		t.Fatalf("presets %s", b)
	}
	for name, body := range map[string]map[string]any{
		"unknown model": {"name": "x", "model": "nope", "card": map[string]any{}},
		"no name":       {"name": "", "model": "haru", "card": map[string]any{}},
		"too big":       {"name": "x", "model": "haru", "card": strings.Repeat("a", 9000)},
	} {
		if code, _ := jsonCall(t, ts, "POST", "/api/cards", body); code != 422 {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := call(t, ts, "DELETE", "/api/cards/"+id, nil, nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call(t, ts, "DELETE", "/api/cards/"+id, nil, nil); code != 404 {
		t.Errorf("delete twice: %d", code)
	}
}

func put(t *testing.T, ts *httptest.Server, id, stream string, off int64, chunk []byte) (int, map[string]any) {
	t.Helper()
	code, b := call(t, ts, "PUT", "/api/recordings/"+id+"/audio/"+stream+"?offset="+itoa(off), bytes.NewReader(chunk), nil)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return code, out
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// A recording is uploaded in chunks while the conversation runs, so a closed tab loses at most the last chunk; an acknowledgement lost on the
// way must neither duplicate nor skip audio.
func TestRecordingChunksAreResumableAndExportAsWAV(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	code, rec := jsonCall(t, ts, "POST", "/api/recordings", map[string]any{"model": "haru", "voice": "abc", "card": map[string]any{"language": "ja"}})
	if code != 201 {
		t.Fatalf("new recording: %d %v", code, rec)
	}
	id := rec["id"].(string)
	a, b := bytes.Repeat([]byte{1, 2}, 3200), bytes.Repeat([]byte{3, 4}, 1600)
	if code, out := put(t, ts, id, "user", 0, a); code != 200 || out["size"].(float64) != float64(len(a)) {
		t.Fatalf("first chunk: %d %v", code, out)
	}
	// The same chunk again (a retry after a lost reply) is a conflict that reports the real size.
	code, out := put(t, ts, id, "user", 0, a)
	if code != 409 || out["size"].(float64) != float64(len(a)) {
		t.Fatalf("a repeated chunk: %d %v, want 409 with the stored size", code, out)
	}
	if code, _ := put(t, ts, id, "user", int64(len(a)), b); code != 200 {
		t.Fatalf("second chunk: %d", code)
	}
	if code, _ := put(t, ts, id, "model", 0, b); code != 200 {
		t.Fatal("model stream")
	}
	if code, _ := put(t, ts, id, "user", 7, []byte{1}); code != 400 && code != 409 {
		t.Errorf("an odd-length chunk at the wrong offset: %d", code)
	}
	if code, _ := put(t, ts, id, "bogus", 0, a); code != 404 {
		t.Errorf("an unknown stream: %d", code)
	}
	events := `{"t":0.5,"type":"text","text":"こんにちは"}` + "\n" + `{"t":1.0,"type":"state","state":"running"}`
	if code, _ := call(t, ts, "POST", "/api/recordings/"+id+"/events", strings.NewReader(events), nil); code != 204 {
		t.Fatalf("events: %d", code)
	}
	if code, _ := call(t, ts, "POST", "/api/recordings/"+id+"/events", strings.NewReader("not json"), nil); code != 400 {
		t.Errorf("a non-JSON event line: %d", code)
	}
	if code, _ := jsonCall(t, ts, "POST", "/api/recordings/"+id+"/finish", map[string]any{"stats": map[string]any{"frames": 12}}); code != 200 {
		t.Fatalf("finish: %d", code)
	}
	code, wav := call(t, ts, "GET", "/api/recordings/"+id+"/audio/user", nil, nil)
	pcm, err := ParseWAV(wav)
	if code != 200 || err != nil || !bytes.Equal(pcm, append(append([]byte{}, a...), b...)) {
		t.Fatalf("exported user audio: %d %v (%d bytes)", code, err, len(pcm))
	}
	// A range request works, so a player can seek.
	code, part := call(t, ts, "GET", "/api/recordings/"+id+"/audio/model", nil, map[string]string{"Range": "bytes=0-43"})
	if code != 206 || len(part) != 44 {
		t.Errorf("range: %d, %d bytes", code, len(part))
	}
	code, b2 := call(t, ts, "GET", "/api/recordings/"+id, nil, nil)
	var full struct {
		Recording Recording         `json:"recording"`
		Events    []json.RawMessage `json:"events"`
		Seconds   float64           `json:"seconds"`
	}
	if code != 200 || json.Unmarshal(b2, &full) != nil || len(full.Events) != 2 || full.Recording.Finished == nil || full.Recording.UserB != int64(len(a)+len(b)) {
		t.Fatalf("recording detail: %d %s", code, b2)
	}
	if _, lb := call(t, ts, "GET", "/api/recordings", nil, nil); !strings.Contains(string(lb), id) {
		t.Errorf("the recording is not listed: %s", lb)
	}
	if code, _ := call(t, ts, "DELETE", "/api/recordings/"+id, nil, nil); code != 204 {
		t.Errorf("delete: %d", code)
	}
	if code, _ := call(t, ts, "GET", "/api/recordings/"+id, nil, nil); code != 404 {
		t.Errorf("a deleted recording is still there: %d", code)
	}
	if code, _ := call(t, ts, "GET", "/api/recordings/..%2F..%2Fmodels.json", nil, nil); code != 404 {
		t.Errorf("a path-shaped recording id was not refused: %d", code)
	}
}

func TestSessionTicketComesFromForgeDuplexAndTheTokenStaysHere(t *testing.T) {
	var gotAuth, gotPath string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_, _ = w.Write([]byte(`{"ticket":"tkt-123","expires_in":30}`))
	}))
	defer fake.Close()
	_, ts := newTestServer(t, fake.URL, "secret-token-value")
	code, out := jsonCall(t, ts, "POST", "/api/session", map[string]any{"model": "haru"})
	if code != 200 || out["ticket"] != "tkt-123" || out["tenant"] != "haru" || !strings.HasPrefix(out["ws_url"].(string), "ws://") || !strings.HasSuffix(out["ws_url"].(string), "/v1/audio/duplex") {
		t.Fatalf("session: %d %v", code, out)
	}
	if gotPath != "/v1/audio/duplex/ticket" || gotAuth != "Bearer secret-token-value" {
		t.Errorf("forge-duplex saw %q with %q", gotPath, gotAuth)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "secret-token-value") {
		t.Fatal("the long-lived token reached the browser")
	}
	if code, _ := jsonCall(t, ts, "POST", "/api/session", map[string]any{"model": "nope"}); code != 404 {
		t.Errorf("an unknown model: %d", code)
	}
}

func TestSessionFailuresAreSpecific(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	if code, out := jsonCall(t, ts, "POST", "/api/session", map[string]any{"model": "haru"}); code != 503 || out["code"] != "duplex_not_configured" {
		t.Errorf("not configured: %d %v", code, out)
	}
	refuse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "forbidden", 403) }))
	defer refuse.Close()
	_, ts2 := newTestServer(t, refuse.URL, "bad")
	if code, out := jsonCall(t, ts2, "POST", "/api/session", map[string]any{"model": "haru"}); code != 502 || out["code"] != "duplex_refused" {
		t.Errorf("refused: %d %v", code, out)
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	_, ts3 := newTestServer(t, url, "")
	if code, out := jsonCall(t, ts3, "POST", "/api/session", map[string]any{"model": "haru"}); code != 502 || out["code"] != "duplex_unreachable" {
		t.Errorf("unreachable: %d %v", code, out)
	}
}

func TestAStateChangeFromAnotherOriginIsRefused(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	raw := strings.NewReader(`{"model":"haru"}`)
	code, _ := call(t, ts, "POST", "/api/recordings", raw, map[string]string{"Origin": "https://evil.example", "Content-Type": "application/json"})
	if code != 403 {
		t.Errorf("a cross-origin POST: %d, want 403", code)
	}
	// Reads are allowed, and the page's own origin may write.
	if code, _ := call(t, ts, "GET", "/api/config", nil, map[string]string{"Origin": "https://evil.example"}); code != 200 {
		t.Errorf("a read: %d", code)
	}
	code, _ = call(t, ts, "POST", "/api/recordings", strings.NewReader(`{"model":"haru"}`), map[string]string{"Origin": ts.URL, "Content-Type": "application/json"})
	if code != 201 {
		t.Errorf("same-origin POST: %d", code)
	}
}

func TestAddVoiceRejectsSilence(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	silent := append(WAVHeader(48000), make([]byte, 48000)...)
	if _, err := st.AddVoice("silent", "", silent); err == nil {
		t.Fatal("a silent reference voice must be refused")
	}
}

func TestVoiceLanguageIsStoredPatchedAndValidated(t *testing.T) {
	srv, ts := newTestServer(t, "", "")
	st := srv.o.Store
	// A new voice may carry the language spoken in its clip; an old one has none until it is set.
	v, err := st.AddVoice("en voice", "en", wavOf(1.5))
	if err != nil || v.Language != "en" {
		t.Fatalf("add with language: %+v %v", v, err)
	}
	if _, err := st.AddVoice("bad", "English", wavOf(1.5)); err == nil {
		t.Fatal("a language that is not a two-letter code must be refused")
	}
	old, err := st.AddVoice("old voice", "", wavOf(1.5))
	if err != nil || old.Language != "" {
		t.Fatalf("add without language: %+v %v", old, err)
	}
	code, b := jsonCall(t, ts, "PATCH", "/api/voices/"+old.ID, map[string]string{"language": "ja"})
	if code != 200 || b["language"] != "ja" {
		t.Fatalf("patch language: %d %v", code, b)
	}
	got, err := st.Voice(old.ID)
	if err != nil || got.Language != "ja" || got.Name != "old voice" {
		t.Fatalf("the patch must persist and change nothing else: %+v %v", got, err)
	}
	if code, _ := jsonCall(t, ts, "PATCH", "/api/voices/"+old.ID, map[string]string{"language": "Japanese"}); code != 422 {
		t.Errorf("an invalid language: %d, want 422", code)
	}
	if code, _ := jsonCall(t, ts, "PATCH", "/api/voices/nosuchvoice1", map[string]string{"language": "en"}); code != 404 {
		t.Errorf("an unknown voice: %d, want 404", code)
	}
}

func TestScenarioKeepsItsVoiceAndSampleText(t *testing.T) {
	_, ts := newTestServer(t, "", "")
	card := map[string]any{"language": "en", "name": "Haru", "situation": "An evening phone call."}
	code, saved := jsonCall(t, ts, "POST", "/api/cards", map[string]any{"name": "Evening call", "model": "haru", "card": card, "voice": "vs:sana", "sample_text": "Hey Alex, it's Haru."})
	if code != 200 || saved["voice"] != "vs:sana" || saved["sample_text"] != "Hey Alex, it's Haru." {
		t.Fatalf("save scenario: %d %v", code, saved)
	}
	_, b := call(t, ts, "GET", "/api/cards", nil, nil)
	var list []CardPreset
	if json.Unmarshal(b, &list) != nil || len(list) != 1 || list[0].Voice != "vs:sana" || list[0].SampleText != "Hey Alex, it's Haru." {
		t.Fatalf("the voice and the sample text must come back with the scenario: %s", b)
	}
	// An older preset without them still loads.
	if code, _ := jsonCall(t, ts, "POST", "/api/cards", map[string]any{"name": "Old", "model": "haru", "card": card}); code != 200 {
		t.Fatalf("a preset without a voice: %d", code)
	}
}
