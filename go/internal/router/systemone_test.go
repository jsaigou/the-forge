// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// d1Env is a router with one fake slot behind a fake scheduler and a small catalog:
//
//	d1-test     visible, produces decisions            (the decision model)
//	d1-hidden   hidden,  produces decisions
//	chat-test   visible, produces text                 (a chat model)
//	d1-vision   visible, decisions, vision input via an mmproj
type d1Env struct {
	srv       *Server
	db        *store.DB
	audit     *fakeAudit
	loads     *int32
	upstream  *httptest.Server
	mu        sync.Mutex
	gotPath   string
	gotBody   []byte
	gotCalls  int
	status    int    // upstream status (default 200)
	respBody  string // upstream body (default a decision answer)
	targetCfg map[string]int64
}

const d1Answer = `{"model":"d1-test","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":43,"output_tokens":0}}`

func newD1Env(t *testing.T) *d1Env {
	t.Helper()
	e := &d1Env{status: http.StatusOK, respBody: d1Answer, targetCfg: map[string]int64{}}
	e.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.gotPath, e.gotBody = r.URL.Path, b
		e.gotCalls++
		status, body := e.status, e.respBody
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(e.upstream.Close)
	port := portFromURL(t, e.upstream.URL)

	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.db = db
	cat := db.Catalog()
	dec := []string{store.OutputDecisions}
	e.targetCfg["d1-test"] = seedCatalogConfig(t, cat, "d1-test", 4096, "visible", seedConfigOpts{modelOutputs: dec})
	seedCatalogConfig(t, cat, "d1-hidden", 4096, "hidden", seedConfigOpts{modelOutputs: dec})
	seedCatalogConfig(t, cat, "chat-test", 4096, "visible")
	seedCatalogConfig(t, cat, "d1-vision", 4096, "visible", seedConfigOpts{
		modelOutputs: dec, modelModalities: []string{"text", "vision"}, withMMProj: true,
	})

	fc := newFakeCatalog()
	fc.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
	e.audit = &fakeAudit{}
	e.loads = new(int32)
	e.srv = NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		Catalog:      fc,
		StoreCatalog: cat,
		Sched:        &fixedSlotSched{slot: "a3", onLoad: func() { atomic.AddInt32(e.loads, 1) }},
		Slots:        map[string]int{"a3": port},
		Auth:         &stubAuth{validToken: "x"},
		Audit:        e.audit,
	})
	return e
}

func (e *d1Env) post(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *d1Env) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gotCalls
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	return m
}

const oneQuestion = `"questions":{"q":{"type":"noul","instructions":"Is it urgent?"}}`

// The request reaches the slot's /v1/systemone (not /v1/chat/completions), byte-for-byte, even though its `model` is
// only a name a0 resolved, loads the model on demand, and is audited as a systemone request with the usage figures.
func TestSystemOne_RoutesByNameAndForwardsBodyUnchanged(t *testing.T) {
	e := newD1Env(t)
	// Odd spacing and key order on purpose: nothing may be re-serialized.
	body := "{ " + oneQuestion + ",  \"state\" : \"I was charged twice\",\n \"model\":\"d1-test\" }"
	rec := e.post("/v1/systemone", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != d1Answer {
		t.Errorf("response was altered: %s", rec.Body.String())
	}
	e.mu.Lock()
	gotPath, gotBody := e.gotPath, string(e.gotBody)
	e.mu.Unlock()
	if gotPath != "/v1/systemone" {
		t.Errorf("upstream path = %q, want /v1/systemone", gotPath)
	}
	if gotBody != body {
		t.Errorf("upstream body was modified:\n got  %q\n want %q", gotBody, body)
	}
	if n := atomic.LoadInt32(e.loads); n != 1 {
		t.Errorf("on-demand load calls = %d, want 1", n)
	}
	ent, ok := e.audit.last()
	if !ok || ent.Action != "systemone" || ent.Target != "d1-test" {
		t.Fatalf("audit = %+v (ok=%v), want an action=systemone row for d1-test", ent, ok)
	}
	if !strings.Contains(ent.Detail, "input_tokens=43") || !strings.Contains(ent.Detail, `\"result\":\"ok\"`) && !strings.Contains(ent.Detail, `"result":"ok"`) {
		t.Errorf("audit detail lacks result/usage: %s", ent.Detail)
	}
}

// llama-server ignores `model`, so a0 is the only thing standing between a typo and a wrong model answering.
func TestSystemOne_UnknownAndHiddenModels(t *testing.T) {
	e := newD1Env(t)
	rec := e.post("/v1/systemone", `{"model":"no-such-model",`+oneQuestion+`,"state":"x"}`)
	if rec.Code != http.StatusNotFound || errCode(t, rec)["error"] != "model_not_found" {
		t.Errorf("unknown: %d %s, want 404 model_not_found", rec.Code, rec.Body.String())
	}
	rec = e.post("/v1/systemone", `{"model":"d1-hidden",`+oneQuestion+`,"state":"x"}`)
	if rec.Code != http.StatusNotFound || errCode(t, rec)["error"] != "model_not_routable" {
		t.Errorf("hidden: %d %s, want 404 model_not_routable", rec.Code, rec.Body.String())
	}
	if e.calls() != 0 || atomic.LoadInt32(e.loads) != 0 {
		t.Errorf("a refused request must not load a model or reach a slot (calls=%d loads=%d)", e.calls(), atomic.LoadInt32(e.loads))
	}
}

// Gate (a): a model that does not produce decisions cannot answer, and nothing is loaded for it.
func TestSystemOne_ChatModelIsRefused(t *testing.T) {
	e := newD1Env(t)
	rec := e.post("/v1/systemone", `{"model":"chat-test",`+oneQuestion+`,"state":"x"}`)
	got := errCode(t, rec)
	if rec.Code != http.StatusBadRequest || got["error"] != "unsupported_capability" || got["capability"] != "decisions" {
		t.Errorf("%d %s, want 400 unsupported_capability (decisions)", rec.Code, rec.Body.String())
	}
	if e.calls() != 0 || atomic.LoadInt32(e.loads) != 0 {
		t.Errorf("no load and no upstream call expected (calls=%d loads=%d)", e.calls(), atomic.LoadInt32(e.loads))
	}
}

// Gate (c): media in `files`/`images` needs the matching input modality on the config.
func TestSystemOne_MediaNeedsInputModality(t *testing.T) {
	e := newD1Env(t)
	img := `"data:image/png;base64,AAAA"`
	for _, key := range []string{"files", "images"} {
		rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`,"state":null,"`+key+`":[`+img+`]}`)
		got := errCode(t, rec)
		if rec.Code != http.StatusBadRequest || got["error"] != "unsupported_modality" || got["modality"] != "vision" {
			t.Errorf("%s image to a text-input decision model: %d %s, want 400 unsupported_modality vision", key, rec.Code, rec.Body.String())
		}
	}
	rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`,"state":null,"files":["data:audio/wav;base64,AAAA"]}`)
	if got := errCode(t, rec); rec.Code != http.StatusBadRequest || got["modality"] != "audio" {
		t.Errorf("audio: %d %s, want 400 unsupported_modality audio", rec.Code, rec.Body.String())
	}
	if e.calls() != 0 {
		t.Errorf("refused media must not reach the slot (calls=%d)", e.calls())
	}
	// A vision-capable decision model (mmproj linked) takes the same request.
	rec = e.post("/v1/systemone", `{"model":"d1-vision",`+oneQuestion+`,"state":null,"files":[`+img+`]}`)
	if rec.Code != http.StatusOK {
		t.Errorf("vision decision model: %d %s, want 200", rec.Code, rec.Body.String())
	}
	// A remote URL is not provable from the string, so a0 leaves it to the upstream.
	rec = e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`,"state":"x","files":["https://example.com/a.png"]}`)
	if rec.Code != http.StatusOK {
		t.Errorf("remote url: %d, want 200 (left to the upstream)", rec.Code)
	}
}

func TestSystemOne_Validation(t *testing.T) {
	e := newD1Env(t)
	for name, body := range map[string]string{
		"not json":         `nope`,
		"no model":         `{` + oneQuestion + `,"state":"x"}`,
		"model too long":   `{"model":"` + strings.Repeat("m", 129) + `",` + oneQuestion + `}`,
		"no questions":     `{"model":"d1-test","state":"x"}`,
		"empty questions":  `{"model":"d1-test","state":"x","questions":{}}`,
		"questions a list": `{"model":"d1-test","state":"x","questions":[1]}`,
	} {
		rec := e.post("/v1/systemone", body)
		if rec.Code != http.StatusUnprocessableEntity || errCode(t, rec)["error"] != "validation_failed" {
			t.Errorf("%s: %d %s, want 422 validation_failed", name, rec.Code, rec.Body.String())
		}
	}
	if e.calls() != 0 {
		t.Errorf("validation failures must not reach a slot")
	}
}

func TestSystemOne_RequiresAuth(t *testing.T) {
	e := newD1Env(t)
	req := httptest.NewRequest("POST", "/v1/systemone", strings.NewReader(`{"model":"d1-test",`+oneQuestion+`}`))
	req.RemoteAddr = "8.8.8.8:54321" // off the tailnet, no bearer
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if e.calls() != 0 || atomic.LoadInt32(e.loads) != 0 {
		t.Errorf("unauthenticated request must not load or call anything")
	}
}

// An alias resolves to its target config for routing and gating; its chat request_defaults are NOT applied (the body is
// forwarded as sent). An alias of a chat model is refused like the chat model itself.
func TestSystemOne_Alias(t *testing.T) {
	e := newD1Env(t)
	ctx := context.Background()
	cat := e.db.Catalog()
	chatCfg, err := cat.ConfigByName(ctx, "chat-test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateModelAlias(ctx, store.ModelAlias{Name: "decider", ConfigID: e.targetCfg["d1-test"],
		RequestDefaults: map[string]any{"reasoning_effort": "none"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.CreateModelAlias(ctx, store.ModelAlias{Name: "chatty", ConfigID: chatCfg.ID}); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"decider",` + oneQuestion + `,"state":"x"}`
	rec := e.post("/v1/systemone", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("alias of a decision model: %d %s, want 200", rec.Code, rec.Body.String())
	}
	e.mu.Lock()
	got := string(e.gotBody)
	e.mu.Unlock()
	if got != body {
		t.Errorf("alias request_defaults leaked into a systemone body: %s", got)
	}
	rec = e.post("/v1/systemone", `{"model":"chatty",`+oneQuestion+`,"state":"x"}`)
	if rec.Code != http.StatusBadRequest || errCode(t, rec)["error"] != "unsupported_capability" {
		t.Errorf("alias of a chat model: %d %s, want 400 unsupported_capability", rec.Code, rec.Body.String())
	}
}

func TestSystemOne_VirtualModelIsRefused(t *testing.T) {
	e := newD1Env(t)
	if _, err := e.db.Catalog().CreateVirtualModel(context.Background(), store.VirtualModel{Name: "fastest", Kind: "throughput"}); err != nil {
		t.Fatal(err)
	}
	rec := e.post("/v1/systemone", `{"model":"fastest",`+oneQuestion+`,"state":"x"}`)
	got := errCode(t, rec)
	if rec.Code != http.StatusBadRequest || got["error"] != "unsupported_capability" || !strings.Contains(got["message"].(string), "virtual") {
		t.Errorf("%d %s, want 400 unsupported_capability naming the virtual model", rec.Code, rec.Body.String())
	}
	if e.calls() != 0 || atomic.LoadInt32(e.loads) != 0 {
		t.Errorf("a virtual model must not load anything")
	}
}

// A request-shaped upstream failure keeps its status and body: it is not a dead backend, so there is no failover and no
// "all_backends_unavailable" 502 hiding the reason. Same for the upstream's 4xx.
func TestSystemOne_UpstreamErrorsPassThrough(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusNotImplemented} {
		e := newD1Env(t)
		msg := `{"error":{"message":"input (2398 tokens) is too large to process. increase the physical batch size"}}`
		e.mu.Lock()
		e.status, e.respBody = status, msg
		e.mu.Unlock()
		rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`,"state":"x"}`)
		if rec.Code != status || rec.Body.String() != msg {
			t.Errorf("upstream %d: got %d %s, want the upstream's own answer", status, rec.Code, rec.Body.String())
		}
		if e.calls() != 1 {
			t.Errorf("upstream %d: %d upstream calls, want exactly 1 (no failover)", status, e.calls())
		}
	}
}

// Capability-tier substitution is switched off for systemone (and only for it): a decision model is not interchangeable.
func TestSystemOne_NoCapabilitySubstitution(t *testing.T) {
	e := newD1Env(t)
	cfg, err := e.db.Catalog().ConfigByName(context.Background(), "d1-test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := withRouteOpts(context.Background(), routeOpts{noSubstitute: true})
	if b := e.srv.trySubstitute(ctx, cfg, map[string]any{}, ""); b != nil {
		t.Errorf("trySubstitute with noSubstitute returned %v, want nil", b)
	}
	if b := e.srv.trySubstituteAfterFailure(ctx, cfg, map[string]any{}); b != nil {
		t.Errorf("trySubstituteAfterFailure with noSubstitute returned %v, want nil", b)
	}
}

// Gate (b): chat to a decision-only model is refused up front; a text model is unaffected.
func TestChat_DecisionOnlyModelIsRefused_TextModelUnchanged(t *testing.T) {
	e := newD1Env(t)
	rec := e.post("/v1/chat/completions", `{"model":"d1-test","messages":[{"role":"user","content":"hi"}]}`)
	got := errCode(t, rec)
	if rec.Code != http.StatusBadRequest || got["error"] != "unsupported_capability" || got["capability"] != "chat" ||
		!strings.Contains(got["message"].(string), "/v1/systemone") {
		t.Errorf("%d %s, want 400 unsupported_capability pointing at /v1/systemone", rec.Code, rec.Body.String())
	}
	if e.calls() != 0 || atomic.LoadInt32(e.loads) != 0 {
		t.Errorf("a refused chat request must not load or call anything (calls=%d loads=%d)", e.calls(), atomic.LoadInt32(e.loads))
	}
	rec = e.post("/v1/chat/completions", `{"model":"chat-test","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Errorf("chat to a text model: %d %s, want 200", rec.Code, rec.Body.String())
	}
	e.mu.Lock()
	path := e.gotPath
	e.mu.Unlock()
	if path != "/v1/chat/completions" {
		t.Errorf("chat upstream path = %q, want /v1/chat/completions (the path parameter must default to chat)", path)
	}
}

func listIDs(t *testing.T, e *d1Env, query string) (ids []string, byID map[string]ModelEntry, code int) {
	t.Helper()
	req := httptest.NewRequest("GET", "/v1/models"+query, nil)
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return nil, nil, rec.Code
	}
	var resp ModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	byID = map[string]ModelEntry{}
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
		byID[m.ID] = m
	}
	return ids, byID, rec.Code
}

func has(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

// D17: the default listing is chat-only (LibreChat/OpenCode pickers), the decisions filter shows only decision models, and
// every returned entry carries its real output_modalities.
func TestModels_OutputFilter(t *testing.T) {
	e := newD1Env(t)
	ctx := context.Background()
	cat := e.db.Catalog()
	if _, err := cat.CreateModelAlias(ctx, store.ModelAlias{Name: "decider", ConfigID: e.targetCfg["d1-test"]}); err != nil {
		t.Fatal(err)
	}
	// A speech model: text + audio outputs, with an mmproj (the codec lives there).
	seedCatalogConfig(t, cat, "voice-test", 4096, "visible", seedConfigOpts{
		modelOutputs: []string{"text", "audio"}, modelModalities: []string{"text", "audio"}, withMMProj: true,
	})
	// The same speech model with no mmproj cannot speak: audio is dropped from what it advertises.
	seedCatalogConfig(t, cat, "voice-nomm", 4096, "visible", seedConfigOpts{
		modelOutputs: []string{"text", "audio"}, modelModalities: []string{"text", "audio"},
	})

	ids, byID, code := listIDs(t, e, "")
	if code != http.StatusOK {
		t.Fatalf("default listing status %d", code)
	}
	for _, hidden := range []string{"d1-test", "d1-vision", "decider", "d1-hidden"} {
		if has(ids, hidden) {
			t.Errorf("default listing must not offer %q (decision-only or hidden): %v", hidden, ids)
		}
	}
	for _, shown := range []string{"chat-test", "voice-test", "voice-nomm"} {
		if !has(ids, shown) {
			t.Errorf("default listing lost %q: %v", shown, ids)
		}
	}
	if a := byID["chat-test"].Architecture; a == nil || strings.Join(a.OutputModalities, ",") != "text" {
		t.Errorf("a text model's output_modalities must stay [text], got %+v", a)
	}
	if a := byID["voice-test"].Architecture; a == nil || strings.Join(a.OutputModalities, ",") != "text,audio" {
		t.Errorf("voice-test output_modalities = %+v, want [text audio]", a)
	}
	if a := byID["voice-nomm"].Architecture; a == nil || strings.Join(a.OutputModalities, ",") != "text" {
		t.Errorf("voice-nomm (no mmproj) output_modalities = %+v, want [text]", a)
	}

	for _, q := range []string{"?output=text"} {
		ids2, _, _ := listIDs(t, e, q)
		if strings.Join(ids2, ",") != strings.Join(ids, ",") {
			t.Errorf("%s must equal the default listing:\n %v\n %v", q, ids2, ids)
		}
	}

	ids, byID, code = listIDs(t, e, "?output=decisions")
	if code != http.StatusOK {
		t.Fatalf("decisions listing status %d", code)
	}
	if got := strings.Join(sorted(ids), ","); got != "d1-test,d1-vision,decider" {
		t.Errorf("?output=decisions = %s, want d1-test,d1-vision,decider (hidden and chat models excluded)", got)
	}
	for _, id := range ids {
		if a := byID[id].Architecture; a == nil || strings.Join(a.OutputModalities, ",") != "decisions" {
			t.Errorf("%s output_modalities = %+v, want [decisions]", id, a)
		}
	}
	if a := byID["d1-vision"].Architecture; a == nil || strings.Join(a.InputModalities, ",") != "text,image" {
		t.Errorf("d1-vision input_modalities = %+v, want [text image]", a)
	}

	ids, _, _ = listIDs(t, e, "?output=audio")
	if got := strings.Join(sorted(ids), ","); got != "voice-test" {
		t.Errorf("?output=audio = %s, want voice-test only", got)
	}

	if _, _, code := listIDs(t, e, "?output=bogus"); code != http.StatusUnprocessableEntity {
		t.Errorf("?output=bogus status = %d, want 422", code)
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
