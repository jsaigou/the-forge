package tts

// Golden contract tests for the forge-tts front door (phase P0 of
// docs/speech-routing-plan.md, sections 2 and 4.4).
//
// The external callers that must not break are voice-studio (a separate app,
// ~/Documents/repo/voice-studio, the source of truth for voices) and a0. The
// facts about what voice-studio sends and expects are taken from
// docs/speech-routing-plan.md section 2 "What must not break" (the voice-studio
// repo itself was not read for this file).
//
//	TestContract_MustKeep_*  shapes that must stay exactly as they are through
//	                         the whole redesign.
//	TestContract_Current_*   today's behavior that the plan intentionally
//	                         changes; each carries a "CHANGES in P2" comment.
//
// The server is wired like cmd/forge-tts/main.go: Registry -> QwenTTS ->
// dualEngine (Kokoro behind an httptest server) -> NewServer(dual, token,
// reg.AudioDir()).Handler().

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const contractToken = "contract-secret"

// contractErrBackend is a Backend whose Speech always fails.
type contractErrBackend struct{ err error }

func (b *contractErrBackend) Speech(ctx context.Context, spec SynthSpec, text string) (*SpeechResult, error) {
	return nil, b.err
}
func (b *contractErrBackend) Health(ctx context.Context) (map[string]any, error) {
	return map[string]any{"status": "ok"}, nil
}
func (b *contractErrBackend) Close() error { return nil }

type contractOpts struct {
	backend Backend
}

type contractStack struct {
	url string
	reg *Registry
	fb  *fakeBackend // nil when opts.backend was supplied
}

// newContractStack builds the production wiring against a temp registry dir.
func newContractStack(t *testing.T, token string, o contractOpts) *contractStack {
	t.Helper()
	kokoroSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/voices":
			w.Write([]byte(`{"data":[{"id":"af_heart"},{"id":"am_michael"}]}`))
		case "/health":
			w.Write([]byte(`{"status":"ok"}`))
		default:
			w.Header().Set("Content-Type", "audio/wav")
			w.Write([]byte("KOKOROWAV"))
		}
	}))
	t.Cleanup(kokoroSrv.Close)

	reg := NewRegistry(t.TempDir())
	st := &contractStack{reg: reg}
	be := o.backend
	if be == nil {
		st.fb = &fakeBackend{}
		be = st.fb
	}
	dual := NewDualEngine(NewQwenTTS(be, reg), staticKokoroBackend(kokoroSrv.URL, ""), "billie", "af_heart")
	ts := httptest.NewServer(NewServer(dual, token, reg.AudioDir()).Handler())
	t.Cleanup(ts.Close)
	st.url = ts.URL
	return st
}

func (s *contractStack) putDesign(t *testing.T, id, instruct string) {
	t.Helper()
	if err := s.reg.Put(VoiceEntry{ID: id, Name: id, Type: "design", Language: "English", Design: &DesignSpec{Instruct: instruct}}); err != nil {
		t.Fatal(err)
	}
}

func (s *contractStack) putClone(t *testing.T, id string) {
	t.Helper()
	if err := s.reg.Put(VoiceEntry{ID: id, Name: id, Type: "clone", Language: "English",
		Clone: &CloneSpec{RefAudio: id + ".wav", RefText: "the transcript", XVectorOnly: true}}); err != nil {
		t.Fatal(err)
	}
}

type contractResp struct {
	code   int
	header http.Header
	body   []byte
}

func (r contractResp) text() string { return string(r.body) }

func contractDo(t *testing.T, req *http.Request) contractResp {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return contractResp{code: resp.StatusCode, header: resp.Header, body: b}
}

func contractGet(t *testing.T, url, token string) contractResp {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("X-Forge-Internal-Token", token)
	}
	return contractDo(t, req)
}

func contractDelete(t *testing.T, url, token string) contractResp {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, url, nil)
	if token != "" {
		req.Header.Set("X-Forge-Internal-Token", token)
	}
	return contractDo(t, req)
}

// contractSpeech posts exactly the four fields voice-studio sends (and only
// those): input, voice, language, response_format.
func contractSpeech(t *testing.T, base, voice string) contractResp {
	t.Helper()
	body := fmt.Sprintf(`{"input":"hello there","voice":%q,"language":"English","response_format":"wav"}`, voice)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/audio/speech", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return contractDo(t, req)
}

func contractImport(t *testing.T, base, token string, fields [][2]string, files map[string][]byte) contractResp {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, f := range fields {
		if err := mw.WriteField(f[0], f[1]); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		w, err := mw.CreateFormFile(name, name+".wav")
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/voices/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.Header.Set("X-Forge-Internal-Token", token)
	}
	return contractDo(t, req)
}

func designFields(id string) [][2]string {
	return [][2]string{{"id", id}, {"name", "Name " + id}, {"type", "design"}, {"language", "English"}, {"instruct", "a warm voice"}}
}

func cloneFields(id string) [][2]string {
	return [][2]string{{"id", id}, {"name", "Name " + id}, {"type", "clone"}, {"language", "English"},
		{"ref_text", "the transcript"}, {"x_vector_only", "true"}}
}

func decodeVoices(t *testing.T, r contractResp) ([]map[string]any, map[string]any) {
	t.Helper()
	var out struct {
		Voices     []map[string]any `json:"voices"`
		ModelsWarm map[string]any   `json:"models_warm"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatalf("decode voices: %v: %s", err, r.text())
	}
	return out.Voices, out.ModelsWarm
}

func findVoice(vs []map[string]any, id string) map[string]any {
	for _, v := range vs {
		if v["id"] == id {
			return v
		}
	}
	return nil
}

// ---------------------------------------------------------------- MUST KEEP

// Depended on by: voice-studio (a node counts as usable only if status == ok
// and model_ready == true). No token needed even when one is configured.
func TestContract_MustKeep_HealthIsOpenWithStatusAndModelReady(t *testing.T) {
	st := newContractStack(t, contractToken, contractOpts{})
	r := contractGet(t, st.url+"/health", "")
	if r.code != http.StatusOK {
		t.Fatalf("health status = %d, want 200: %s", r.code, r.text())
	}
	var h map[string]any
	if err := json.Unmarshal(r.body, &h); err != nil {
		t.Fatalf("health is not JSON: %v: %s", err, r.text())
	}
	if h["status"] != "ok" {
		t.Fatalf("health status field = %v, want \"ok\"", h["status"])
	}
	if h["model_ready"] != true {
		t.Fatalf("health model_ready = %v, want true", h["model_ready"])
	}
}

// Depended on by: voice-studio (sends only input, voice, language,
// response_format; no model/speed/tier/token) and a0 (passthrough).
func TestContract_MustKeep_SpeechRegistryVoiceNoTokenReturnsWav(t *testing.T) {
	st := newContractStack(t, contractToken, contractOpts{}) // token configured, request sends none
	st.putDesign(t, "billie", "warm")
	r := contractSpeech(t, st.url, "billie")
	if r.code != http.StatusOK {
		t.Fatalf("speech status = %d, want 200: %s", r.code, r.text())
	}
	if ct := r.header.Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("Content-Type = %q, want audio/wav", ct)
	}
	if r.text() != "QWENWAV" {
		t.Fatalf("body = %q, want the engine's bytes %q", r.text(), "QWENWAV")
	}
	if st.fb.lastText != "hello there" || st.fb.lastSpec.Language != "English" || st.fb.lastSpec.Format != "wav" {
		t.Fatalf("engine got text=%q language=%q format=%q", st.fb.lastText, st.fb.lastSpec.Language, st.fb.lastSpec.Format)
	}
}

// Depended on by: voice-studio (lists nodes' voices; keeps tier == "fast"
// entries as builtins; reads models_warm) and a0 (/v1/voices passthrough).
func TestContract_MustKeep_VoicesListShape(t *testing.T) {
	st := newContractStack(t, contractToken, contractOpts{})
	st.putDesign(t, "dsn", "SECRET-INSTRUCT")
	st.putClone(t, "cln")

	t.Run("open_without_token_with_required_keys", func(t *testing.T) {
		r := contractGet(t, st.url+"/v1/voices", "")
		if r.code != http.StatusOK {
			t.Fatalf("status = %d, want 200 without token", r.code)
		}
		vs, _ := decodeVoices(t, r)
		for _, id := range []string{"dsn", "cln", "af_heart"} {
			v := findVoice(vs, id)
			if v == nil {
				t.Fatalf("voice %q missing from list: %s", id, r.text())
			}
			for _, k := range []string{"id", "name", "type", "language", "tier", "builtin", "has_sample"} {
				if _, ok := v[k]; !ok {
					t.Fatalf("voice %q lacks key %q: %v", id, k, v)
				}
			}
		}
	})

	t.Run("sensitive_fields_only_with_matching_token", func(t *testing.T) {
		sensitive := []string{"instruct", "ref_text", "x_vector_only"}
		for _, tok := range []string{"", "wrong-token"} {
			vs, _ := decodeVoices(t, contractGet(t, st.url+"/v1/voices", tok))
			for _, id := range []string{"dsn", "cln"} {
				v := findVoice(vs, id)
				if v == nil {
					t.Fatalf("token %q: voice %q missing", tok, id)
				}
				for _, k := range sensitive {
					if _, ok := v[k]; ok {
						t.Fatalf("token %q: voice %q leaks %q", tok, id, k)
					}
				}
			}
		}
		vs, _ := decodeVoices(t, contractGet(t, st.url+"/v1/voices", contractToken))
		if v := findVoice(vs, "dsn"); v == nil || v["instruct"] != "SECRET-INSTRUCT" {
			t.Fatalf("design voice with token lacks instruct: %v", v)
		}
		v := findVoice(vs, "cln")
		if v == nil || v["ref_text"] != "the transcript" || v["x_vector_only"] != true {
			t.Fatalf("clone voice with token lacks ref_text/x_vector_only: %v", v)
		}
	})

	t.Run("models_warm_has_fast_and_premium", func(t *testing.T) {
		_, warm := decodeVoices(t, contractGet(t, st.url+"/v1/voices", ""))
		for _, k := range []string{"fast", "premium"} {
			if _, ok := warm[k]; !ok {
				t.Fatalf("models_warm lacks %q: %v", k, warm)
			}
		}
	})

	t.Run("kokoro_builtins_are_fast_and_builtin", func(t *testing.T) {
		vs, _ := decodeVoices(t, contractGet(t, st.url+"/v1/voices", ""))
		v := findVoice(vs, "af_heart")
		if v == nil || v["tier"] != "fast" || v["builtin"] != true {
			t.Fatalf("af_heart = %v, want tier fast and builtin true", v)
		}
		for _, id := range []string{"dsn", "cln"} {
			if v := findVoice(vs, id); v["tier"] == "fast" || v["builtin"] == true {
				t.Fatalf("registry voice %q must not look like a builtin: %v", id, v)
			}
		}
	})
}

// Depended on by: voice-studio (deploys a voice to a node; rename = delete +
// re-import). Today a successful import answers 201 Created.
func TestContract_MustKeep_ImportMultipart(t *testing.T) {
	t.Run("design_and_clone_succeed_and_land_in_registry", func(t *testing.T) {
		st := newContractStack(t, contractToken, contractOpts{})
		r := contractImport(t, st.url, contractToken, designFields("dsn1"), nil)
		if r.code != http.StatusCreated {
			t.Fatalf("design import = %d, want 201: %s", r.code, r.text())
		}
		r = contractImport(t, st.url, contractToken, cloneFields("cln1"),
			map[string][]byte{"ref_audio": []byte("refwav"), "sample": []byte("samplewav")})
		if r.code != http.StatusCreated {
			t.Fatalf("clone import = %d, want 201: %s", r.code, r.text())
		}
		d, ok, _ := st.reg.Get("dsn1")
		if !ok || d.Design == nil || d.Design.Instruct != "a warm voice" || d.Language != "English" {
			t.Fatalf("design voice not stored as sent: %+v", d)
		}
		c, ok, _ := st.reg.Get("cln1")
		if !ok || c.Clone == nil || c.Clone.RefText != "the transcript" || !c.Clone.XVectorOnly {
			t.Fatalf("clone voice not stored as sent: %+v", c)
		}
		vs, _ := decodeVoices(t, contractGet(t, st.url+"/v1/voices", ""))
		if v := findVoice(vs, "cln1"); v == nil || v["has_sample"] != true {
			t.Fatalf("clone imported with a sample should list has_sample=true: %v", v)
		}
	})

	t.Run("wrong_or_missing_token_is_404_not_401", func(t *testing.T) {
		st := newContractStack(t, contractToken, contractOpts{})
		for _, tok := range []string{"", "wrong-token"} {
			r := contractImport(t, st.url, tok, designFields("nope"), nil)
			if r.code != http.StatusNotFound {
				t.Fatalf("token %q: import = %d, want 404", tok, r.code)
			}
		}
		if _, ok, _ := st.reg.Get("nope"); ok {
			t.Fatal("unauthorized import must not store anything")
		}
	})

	t.Run("unknown_type_is_400", func(t *testing.T) {
		st := newContractStack(t, contractToken, contractOpts{})
		f := designFields("odd")
		f[2] = [2]string{"type", "hologram"}
		if r := contractImport(t, st.url, contractToken, f, nil); r.code != http.StatusBadRequest {
			t.Fatalf("unknown type = %d, want 400: %s", r.code, r.text())
		}
	})

	t.Run("unknown_extra_field_is_ignored", func(t *testing.T) {
		// voice-studio will start sending `engine` once forge-tts is updated;
		// older and newer sides must interoperate in both directions.
		st := newContractStack(t, contractToken, contractOpts{})
		f := append(designFields("extra1"), [2]string{"engine", "voicedesign-1.7b"}, [2]string{"future_field", "x"})
		if r := contractImport(t, st.url, contractToken, f, nil); r.code != http.StatusCreated {
			t.Fatalf("design import with unknown fields = %d, want 201: %s", r.code, r.text())
		}
		f = append(cloneFields("extra2"), [2]string{"engine", "base-0.6b"})
		if r := contractImport(t, st.url, contractToken, f, map[string][]byte{"ref_audio": []byte("refwav")}); r.code != http.StatusCreated {
			t.Fatalf("clone import with unknown field = %d, want 201: %s", r.code, r.text())
		}
		for _, id := range []string{"extra1", "extra2"} {
			if _, ok, _ := st.reg.Get(id); !ok {
				t.Fatalf("voice %q not stored", id)
			}
		}
	})

	t.Run("same_id_twice_overwrites_and_succeeds", func(t *testing.T) {
		st := newContractStack(t, contractToken, contractOpts{})
		for i, instruct := range []string{"first", "second"} {
			f := designFields("again")
			f[4] = [2]string{"instruct", instruct}
			if r := contractImport(t, st.url, contractToken, f, nil); r.code != http.StatusCreated {
				t.Fatalf("import #%d = %d, want 201: %s", i+1, r.code, r.text())
			}
		}
		v, ok, _ := st.reg.Get("again")
		if !ok || v.Design == nil || v.Design.Instruct != "second" {
			t.Fatalf("second import should have overwritten the first: %+v", v)
		}
	})
}

// Depended on by: voice-studio (rename deletes the voice on every node then
// redeploys; 404 is tolerated).
func TestContract_MustKeep_DeleteVoice(t *testing.T) {
	t.Run("with_token_succeeds", func(t *testing.T) {
		st := newContractStack(t, contractToken, contractOpts{})
		st.putDesign(t, "gone", "x")
		r := contractDelete(t, st.url+"/v1/voices/gone", contractToken)
		if r.code != http.StatusOK {
			t.Fatalf("delete = %d, want 200: %s", r.code, r.text())
		}
		if _, ok, _ := st.reg.Get("gone"); ok {
			t.Fatal("voice still in registry after delete")
		}
	})
	t.Run("without_or_wrong_token_is_404_and_keeps_voice", func(t *testing.T) {
		st := newContractStack(t, contractToken, contractOpts{})
		st.putDesign(t, "stay", "x")
		for _, tok := range []string{"", "wrong-token"} {
			if r := contractDelete(t, st.url+"/v1/voices/stay", tok); r.code != http.StatusNotFound {
				t.Fatalf("token %q: delete = %d, want 404", tok, r.code)
			}
		}
		if _, ok, _ := st.reg.Get("stay"); !ok {
			t.Fatal("unauthorized delete removed the voice")
		}
	})
}

// Mechanism of the 2026-09 incident: the internal-token env var was silently
// unset, which turned every management endpoint open. Depended on by:
// nobody (this is a hazard, documented so a change to it is deliberate).
func TestContract_MustKeep_EmptyServerTokenLeavesManagementEndpointsOpen(t *testing.T) {
	st := newContractStack(t, "", contractOpts{})
	if r := contractImport(t, st.url, "", designFields("open1"), nil); r.code != http.StatusCreated {
		t.Fatalf("tokenless import with empty server token = %d, want 201: %s", r.code, r.text())
	}
	vs, _ := decodeVoices(t, contractGet(t, st.url+"/v1/voices", ""))
	if v := findVoice(vs, "open1"); v == nil || v["instruct"] != "a warm voice" {
		t.Fatalf("with an empty server token, tokenless list exposes instruct: %v", v)
	}
	if r := contractDelete(t, st.url+"/v1/voices/open1", ""); r.code != http.StatusOK {
		t.Fatalf("tokenless delete with empty server token = %d, want 200", r.code)
	}
}

// ------------------------------------------------------------------ CURRENT

// Depended on by: voice-studio and a0 see only "HTTP 500 + text".
// CHANGES in P2: errors get a stable status and a JSON body with a code
// (unknown voice -> 4xx, not-ready -> 503 model_not_ready) instead of 500 text.
func TestContract_Current_EngineErrorsOnSpeechAreHTTP500PlainText(t *testing.T) {
	cases := []struct {
		name string
		opts contractOpts
		voic string
		body string
	}{
		{"unknown_voice", contractOpts{}, "ghost", "unknown voice \"ghost\"\n"},
		{"backend_failure", contractOpts{backend: &contractErrBackend{err: fmt.Errorf("backend exploded")}}, "dsn", "backend exploded\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newContractStack(t, contractToken, c.opts)
			st.putDesign(t, "dsn", "warm")
			r := contractSpeech(t, st.url, c.voic)
			if r.code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", r.code)
			}
			if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
				t.Fatalf("Content-Type = %q, want text/plain", ct)
			}
			if r.text() != c.body {
				t.Fatalf("body = %q, want %q", r.text(), c.body)
			}
		})
	}
}

// voice-studio retries (5-15 s backoff) only on a 503 whose body contains
// model_not_ready. Today forge-tts never produces one; pin that fact.
// CHANGES in P2: a not-ready engine will answer 503 with model_not_ready.
func TestContract_Current_SpeechNeverProducesA503OrModelNotReady(t *testing.T) {
	scenarios := map[string]struct {
		opts contractOpts
		voic string
	}{
		"ok":              {contractOpts{}, "dsn"},
		"unknown_voice":   {contractOpts{}, "ghost"},
		"backend_failure": {contractOpts{backend: &contractErrBackend{err: fmt.Errorf("model not loaded")}}, "dsn"},
	}
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			st := newContractStack(t, contractToken, sc.opts)
			st.putDesign(t, "dsn", "warm")
			r := contractSpeech(t, st.url, sc.voic)
			if r.code == http.StatusServiceUnavailable {
				t.Fatalf("speech returned 503: %s", r.text())
			}
			if strings.Contains(r.text(), "model_not_ready") {
				t.Fatalf("speech body contains model_not_ready: %s", r.text())
			}
		})
	}
}

// A mode with no engine, or an engine that is turned off, used to be served silently by the reload-every-call command-line backend (slow, and nothing
// said so). Since P5 there is no such path: the daemon's answer is shown. Hard refusals are a 503 with their own code and WITHOUT the text voice-studio
// retries on; "loading" is a 503 WITH it. A caller never gets audio from some other engine.
func TestContract_MustKeep_NoEngineOrTurnedOffEngineIsAClearErrorNeverAFallback(t *testing.T) {
	cases := []struct {
		name      string
		err       *EngineError
		wantCode  string
		retryable bool
	}{
		{"no engine for the kind", &EngineError{Code: "speech_no_engine", Message: "no engine can play voicedesign voices"}, "speech_no_engine", false},
		{"engine turned off", &EngineError{Code: "speech_engine_disabled", Message: "engine e1 is turned off", Engine: "e1"}, "speech_engine_disabled", false},
		{"engine loading", &EngineError{Code: "specialist_unavailable", Message: "engine e1 is starting", Transient: true, RetryAfter: 3 * time.Second}, "model_not_ready", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newContractStack(t, contractToken, contractOpts{backend: &contractErrBackend{err: c.err}})
			st.putDesign(t, "dsn", "warm")
			r := contractSpeech(t, st.url, "dsn")
			if r.code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", r.code, r.text())
			}
			if !strings.Contains(r.text(), c.wantCode) {
				t.Fatalf("body %q does not carry %q", r.text(), c.wantCode)
			}
			if got := strings.Contains(r.text(), "model_not_ready"); got != c.retryable {
				t.Fatalf("model_not_ready in body = %v, want %v (voice-studio retries on it): %s", got, c.retryable, r.text())
			}
		})
	}
}

// voice-studio's delete client tolerates exactly one failure: 404 (it deletes on every node before a rename or redeploy, including nodes the voice was
// never on). P0 found that forge-tts answered 500 for a voice that is not there, so that tolerance never applied and a rename of a voice that was not
// deployed everywhere failed. Fixed in P2: deleting what is not there is a 404.
func TestContract_MustKeep_DeleteMissingIdWithTokenIs404(t *testing.T) {
	st := newContractStack(t, contractToken, contractOpts{})
	r := contractDelete(t, st.url+"/v1/voices/never-existed", contractToken)
	if r.code != http.StatusNotFound {
		t.Fatalf("delete missing id = %d, want 404 (voice-studio tolerates exactly that): %s", r.code, r.text())
	}
}
