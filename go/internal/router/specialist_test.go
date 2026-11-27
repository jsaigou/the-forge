// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/specialist"
)

// fakePool answers Resolve from a table: the member's base URL, ErrNoMatch, or an error.
type fakePool struct {
	mu      sync.Mutex
	target  string
	err     error
	asked   []string
	touched int
	// configs: catalog configs the pool hosts, by name, to the port of the member serving each. A config not listed is ErrNoMatch.
	configs   map[string]int
	configErr error
	// leases: when set, targets are handed out with a lease like the real pool does, and these count what happened to it.
	leases                            bool
	held, acquired, released, renewed int
}

// leased adds a lease to tg when the fake is set up for it.
func (f *fakePool) leased(tg specialist.Target) specialist.Target {
	if !f.leases {
		return tg
	}
	f.held++
	f.acquired++
	var once sync.Once
	tg.Release = func() { once.Do(func() { f.mu.Lock(); f.held--; f.released++; f.mu.Unlock() }) }
	tg.Renew = func() bool { f.mu.Lock(); defer f.mu.Unlock(); f.renewed++; return f.held > 0 }
	return tg
}

func (f *fakePool) snapshot() (held, acquired, released, renewed, touched int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held, f.acquired, f.released, f.renewed, f.touched
}

func (f *fakePool) ResolveConfig(_ context.Context, config string) (specialist.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, "config/"+config)
	if f.configErr != nil {
		return specialist.Target{}, f.configErr
	}
	port, ok := f.configs[config]
	if !ok {
		return specialist.Target{}, specialist.ErrNoMatch
	}
	return f.leased(specialist.Target{Occupant: "member-" + config, Slot: "s7", Port: port, Touch: func() { f.mu.Lock(); f.touched++; f.mu.Unlock() }}), nil
}

func (f *fakePool) Resolve(_ context.Context, function, model string) (specialist.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, function+"/"+model)
	if f.err != nil {
		return specialist.Target{}, f.err
	}
	return f.leased(specialist.Target{Occupant: "member", Slot: "s2", BaseURL: f.target, Touch: func() { f.mu.Lock(); f.touched++; f.mu.Unlock() }}), nil
}

// poolMember is a fake specialist process recording what reaches it.
func poolMember(t *testing.T) (*httptest.Server, *struct {
	sync.Mutex
	path, ctype, auth, body string
}) {
	t.Helper()
	rec := &struct {
		sync.Mutex
		path, ctype, auth, body string
	}{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.Lock()
		rec.path, rec.ctype, rec.auth, rec.body = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), string(b)
		rec.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"from":"pool-member"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func TestPool_EmbeddingsForwardedIntactAndAuthStripped(t *testing.T) {
	e, _ := newAudioEnv(t)
	member, rec := poolMember(t)
	pool := &fakePool{target: member.URL}
	e.srv.deps.Specialists = pool

	body := `{ "model":"text-embedding-3-small",  "input":["a","b"] }` // odd spacing: must arrive byte for byte
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer sk-secret-a0-token")
	req.RemoteAddr = "100.64.0.1:1234"
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, req)

	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "pool-member") {
		t.Fatalf("got %d %s", rr.Code, rr.Body)
	}
	rec.Lock()
	defer rec.Unlock()
	if rec.path != "/v1/embeddings" || rec.body != body || rec.ctype != "application/json" {
		t.Errorf("member got path=%q ctype=%q body=%q", rec.path, rec.ctype, rec.body)
	}
	if rec.auth != "" {
		t.Error("the caller's a0 credential was forwarded to the pool member")
	}
	if pool.asked[0] != "embeddings/text-embedding-3-small" || pool.touched != 1 {
		t.Errorf("asked=%v touched=%d", pool.asked, pool.touched)
	}
}

// With no member for the request the pool says ErrNoMatch and the static passthrough answers exactly as before.
func TestPool_NoMatchFallsBackToTheStaticEmbeddingsPassthrough(t *testing.T) {
	e, _ := newAudioEnv(t)
	static, rec := poolMember(t)
	e.srv.cfg().EmbeddingURL = static.URL + "/v1"
	e.srv.deps.Specialists = &fakePool{err: specialist.ErrNoMatch}

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"x","input":"hi"}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rr := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rr, req)
	rec.Lock()
	defer rec.Unlock()
	if rr.Code != 200 || rec.path != "/v1/embeddings" || rec.body != `{"model":"x","input":"hi"}` {
		t.Fatalf("static path: %d path=%q body=%q", rr.Code, rec.path, rec.body)
	}
}

func TestPool_FailsFastWithStableCodes(t *testing.T) {
	e, _ := newAudioEnv(t)
	for name, tc := range map[string]struct {
		err        error
		code       string
		retryAfter string
	}{
		"pool full":   {&specialist.PoolFullError{Occupant: "asr", Holders: []string{"s1 a (always_on, running)"}, RetryAfter: 29 * time.Second}, "specialist_pool_full", "29"},
		"unavailable": {&specialist.UnavailableError{Occupant: "asr", Reason: "failed: exited 1"}, "specialist_unavailable", ""},
	} {
		e.srv.deps.Specialists = &fakePool{err: tc.err}
		req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"input":"hi"}`))
		req.RemoteAddr = "100.64.0.1:1234"
		rr := httptest.NewRecorder()
		e.srv.Handler().ServeHTTP(rr, req)
		if rr.Code != 503 || errCode(t, rr)["error"] != tc.code || rr.Header().Get("Retry-After") != tc.retryAfter {
			t.Errorf("%s: %d %s Retry-After=%q", name, rr.Code, rr.Body, rr.Header().Get("Retry-After"))
		}
	}
}

func TestPool_TranscriptionForwardedWhenNotACatalogModel(t *testing.T) {
	e, _ := newAudioEnv(t)
	member, rec := poolMember(t)
	e.srv.deps.Specialists = &fakePool{target: member.URL}

	// A catalog model still takes the catalog path, whether or not a pool exists.
	if rr := e.transcribe(t, map[string]string{"model": "omni-test"}, "a.wav", []byte("x")); rr.Code != 200 || strings.Contains(rr.Body.String(), "pool-member") {
		t.Fatalf("catalog model went to the pool: %d %s", rr.Code, rr.Body)
	}
	// Anything else goes to the pool, whole: the pool member judges format and language, not the catalog adapter's rules.
	rr := e.transcribe(t, map[string]string{"model": "whisper-1", "language": "ja", "response_format": "verbose_json"}, "clip.flac", []byte("FLACDATA"))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "pool-member") {
		t.Fatalf("got %d %s", rr.Code, rr.Body)
	}
	rec.Lock()
	defer rec.Unlock()
	if rec.path != "/v1/audio/transcriptions" || !strings.HasPrefix(rec.ctype, "multipart/form-data; boundary=") ||
		!strings.Contains(rec.body, "FLACDATA") || !strings.Contains(rec.body, "verbose_json") {
		t.Errorf("member got path=%q ctype=%q body=%q", rec.path, rec.ctype, rec.body)
	}

	// No pool member either: still the 404 it always was.
	e.srv.deps.Specialists = &fakePool{err: specialist.ErrNoMatch}
	if rr := e.transcribe(t, map[string]string{"model": "whisper-1"}, "a.wav", []byte("x")); rr.Code != 404 || errCode(t, rr)["error"] != "model_not_found" {
		t.Errorf("no pool match: %d %s", rr.Code, rr.Body)
	}
}

func TestPool_AlignForwardedAndUnavailableWithoutAMember(t *testing.T) {
	e, _ := newAudioEnv(t)
	member, rec := poolMember(t)
	e.srv.deps.Specialists = &fakePool{target: member.URL}
	body := `{"audio":"data:audio/wav;base64,AAAA","text":"hi","language":"English"}`
	rr := e.post("/v1/align", body)
	rec.Lock()
	if rr.Code != 200 || rec.path != "/v1/align" || rec.body != body {
		t.Errorf("align: %d path=%q body=%q", rr.Code, rec.path, rec.body)
	}
	rec.Unlock()
	e.srv.deps.Specialists = &fakePool{err: specialist.ErrNoMatch}
	if rr := e.post("/v1/align", body); rr.Code != 503 || errCode(t, rr)["error"] != "specialist_unavailable" {
		t.Errorf("no member: %d %s", rr.Code, rr.Body)
	}
	e.srv.deps.Specialists = nil
	if rr := e.post("/v1/align", body); rr.Code != 503 {
		t.Errorf("no pool at all: %d", rr.Code)
	}
}

// One owner (ADR-0022): a catalog config the specialist pool hosts is answered by the pool member, not by an LLM slot, and the scheduler is never
// asked to load it. Everything else about the request (gating, audit) is the ordinary path.
func TestPool_PooledCatalogConfigIsServedByTheMemberNotAnLLMSlot(t *testing.T) {
	e := newD1Env(t)
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/systemone" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"d1-test","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":7,"output_tokens":0},"from":"pool-member"}`))
			return
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(member.Close)
	port := portFromURL(t, member.URL)
	e.srv.deps.Catalog.(*fakeCatalog).setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
	pool := &fakePool{configs: map[string]int{"d1-test": port}}
	e.srv.deps.Specialists = pool

	rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pool-member") {
		t.Fatalf("pooled config: %d %s", rec.Code, rec.Body)
	}
	if e.calls() != 0 {
		t.Errorf("the LLM slot upstream was called %d times for a pooled config", e.calls())
	}
	if n := atomic.LoadInt32(e.loads); n != 0 {
		t.Errorf("the scheduler was asked to load a pooled config %d times", n)
	}
	// Once when the request is routed and once when it finishes (the idle clock restarts at the END of a request).
	if pool.touched != 2 {
		t.Errorf("the member's idle clock was touched %d times, want 2 (start and finish)", pool.touched)
	}

	// A config the pool does not host takes the scheduler path, as ever.
	pool.configs = map[string]int{}
	if rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`}`); rec.Code != 200 || strings.Contains(rec.Body.String(), "pool-member") || e.calls() != 1 {
		t.Errorf("unpooled config: %d %s calls=%d", rec.Code, rec.Body, e.calls())
	}
}

// Regression (idle clock): a pooled config's idle clock must be refreshed when the request FINISHES, not only when it starts, or a request
// longer than idle_unload_s leaves the member instantly evictable the moment it completes. While the member is still answering, only the
// start touch has happened; after the response the finish touch has too.
func TestPool_PooledCatalogConfigTouchesAgainWhenTheRequestFinishes(t *testing.T) {
	e := newD1Env(t)
	var pool *fakePool
	touchedDuring := -1
	member := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pool.mu.Lock()
		touchedDuring = pool.touched
		pool.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"d1-test","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":7,"output_tokens":0},"from":"pool-member"}`))
	}))
	t.Cleanup(member.Close)
	port := portFromURL(t, member.URL)
	e.srv.deps.Catalog.(*fakeCatalog).setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})
	pool = &fakePool{configs: map[string]int{"d1-test": port}}
	e.srv.deps.Specialists = pool

	if rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`}`); rec.Code != 200 {
		t.Fatalf("pooled config: %d %s", rec.Code, rec.Body)
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if touchedDuring != 1 {
		t.Errorf("touched %d times while the member was answering, want 1 (the start touch only)", touchedDuring)
	}
	if pool.touched != 2 {
		t.Errorf("touched %d times after the request finished, want 2", pool.touched)
	}
}

// A pool that cannot place the member answers with its own plain reason, not a scheduler error.
func TestPool_PooledCatalogConfigReportsThePoolsReason(t *testing.T) {
	e := newD1Env(t)
	e.srv.deps.Specialists = &fakePool{configErr: &specialist.PoolFullError{Occupant: "d1-member", Holders: []string{"s1 a (always_on, running)"}, RetryAfter: 20 * time.Second}}
	rec := e.post("/v1/systemone", `{"model":"d1-test",`+oneQuestion+`}`)
	if rec.Code == 200 || !strings.Contains(rec.Body.String(), "specialist pool is full") {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if e.calls() != 0 || atomic.LoadInt32(e.loads) != 0 {
		t.Error("a pool refusal fell through to an LLM slot")
	}
}
