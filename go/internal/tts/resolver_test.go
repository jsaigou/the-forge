// SPDX-License-Identifier: Apache-2.0

package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "sk-forge-testkey"

type fakeDaemon struct {
	mu       sync.Mutex
	status   int
	body     string
	header   map[string]string
	resolves []map[string]string
	releases []string
	auth     []string
}

func (f *fakeDaemon) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/api/v1/speech/resolve":
			f.resolves = append(f.resolves, in)
			for k, v := range f.header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.body))
		case "/api/v1/speech/release":
			f.releases = append(f.releases, in["lease_id"])
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	})
}

func newDaemonResolver(t *testing.T, url string, mut func(*DaemonResolverConfig)) *DaemonResolver {
	t.Helper()
	c := DaemonResolverConfig{BaseURL: url, Key: testKey, Timeout: 2 * time.Second}
	if mut != nil {
		mut(&c)
	}
	d, err := NewDaemonResolver(c)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDaemonResolver_RefusesToBuildWithoutAUsableKey(t *testing.T) {
	for _, c := range []DaemonResolverConfig{
		{BaseURL: "http://x", Key: ""},
		{BaseURL: "http://x", Key: "not-a-forge-key"},
		{BaseURL: "", Key: testKey},
	} {
		if _, err := NewDaemonResolver(c); err == nil {
			t.Fatalf("config %+v must be refused", c)
		}
	}
}

func TestDaemonResolver_ResolvesWithKeyAndReleasesTheLease(t *testing.T) {
	f := &fakeDaemon{status: 200, body: `{"engine":"qwen3-tts-base","base_url":"http://127.0.0.1:8103/","lease_id":"L1"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	d := newDaemonResolver(t, srv.URL, nil)
	got, err := d.Resolve(context.Background(), ModeBase, "qwen3-tts-base")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "http://127.0.0.1:8103" || got.Engine != "qwen3-tts-base" {
		t.Fatalf("got %+v", got)
	}
	got.Release()
	got.Release() // second call is a no-op
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.releases)
		f.mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.releases) != 1 || f.releases[0] != "L1" {
		t.Fatalf("releases = %v, want exactly [L1]", f.releases)
	}
	if f.resolves[0]["mode"] != "base" || f.resolves[0]["engine"] != "qwen3-tts-base" {
		t.Fatalf("request body = %v", f.resolves[0])
	}
	for _, a := range f.auth {
		if a != "Bearer "+testKey {
			t.Fatalf("auth header %q", a)
		}
	}
}

func TestDaemonResolver_TransientAndHardAnswers(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		header    map[string]string
		wantCode  string
		transient bool
		retry     time.Duration
	}{
		{"loading", 503, `{"code":"specialist_unavailable","error":"engine is starting","params":{"transient":true,"engine":"e1"}}`, map[string]string{"Retry-After": "5"}, "specialist_unavailable", true, 5 * time.Second},
		{"disabled", 409, `{"code":"speech_engine_disabled","error":"engine e1 is turned off","params":{"transient":false,"engine":"e1"}}`, nil, "speech_engine_disabled", false, 0},
		{"no engine, flag missing, 422", 422, `{"code":"speech_no_engine","error":"nothing plays this"}`, nil, "speech_no_engine", false, 0},
		{"unflagged 503 is transient", 503, `{"code":"specialist_pool_full","error":"pool full"}`, nil, "specialist_pool_full", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeDaemon{status: c.status, body: c.body, header: c.header}
			srv := httptest.NewServer(f.handler())
			defer srv.Close()
			d := newDaemonResolver(t, srv.URL, nil)
			_, err := d.Resolve(context.Background(), ModeVoiceDesign, "")
			ee, ok := IsEngineError(err)
			if !ok {
				t.Fatalf("want EngineError, got %v", err)
			}
			if ee.Code != c.wantCode || ee.Transient != c.transient || ee.RetryAfter != c.retry {
				t.Fatalf("got %+v", ee)
			}
		})
	}
}

func TestDaemonResolver_RefusedKeyIsLoudOnceAndShownInStatus(t *testing.T) {
	f := &fakeDaemon{status: 401, body: `{"code":"unauthorized"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	var logs []string
	d := newDaemonResolver(t, srv.URL, func(c *DaemonResolverConfig) {
		c.Logf = func(format string, _ ...any) { logs = append(logs, format) }
	})
	for i := 0; i < 3; i++ {
		_, err := d.Resolve(context.Background(), ModeBase, "")
		ee, ok := IsEngineError(err)
		if !ok || ee.Code != "speech_resolver_unauthorized" || ee.Transient {
			t.Fatalf("got %v", err)
		}
	}
	if len(logs) != 1 {
		t.Fatalf("logged %d times, want once", len(logs))
	}
	if d.Status()["key_refused"] != true {
		t.Fatalf("status = %v", d.Status())
	}
}

func TestDaemonResolver_UnreachableUsesRecentAddressThenGivesATransientAnswer(t *testing.T) {
	f := &fakeDaemon{status: 200, body: `{"engine":"e1","base_url":"http://127.0.0.1:8103","lease_id":"L1"}`}
	srv := httptest.NewServer(f.handler())
	now := time.Now()
	d := newDaemonResolver(t, srv.URL, func(c *DaemonResolverConfig) {
		c.CacheFor = time.Minute
		c.Now = func() time.Time { return now }
	})
	if _, err := d.Resolve(context.Background(), ModeBase, ""); err != nil {
		t.Fatal(err)
	}
	srv.Close() // the daemon goes away

	got, err := d.Resolve(context.Background(), ModeBase, "")
	if err != nil || got.URL != "http://127.0.0.1:8103" {
		t.Fatalf("recent address should be used: %+v %v", got, err)
	}
	if _, err := d.Resolve(context.Background(), ModeKokoro, ""); err == nil {
		t.Fatal("an engine never seen has no address to fall back on")
	} else if ee, ok := IsEngineError(err); !ok || !ee.Transient {
		t.Fatalf("want a transient EngineError, got %v", err)
	}
	now = now.Add(2 * time.Minute) // cache is now too old
	_, err = d.Resolve(context.Background(), ModeBase, "")
	if ee, ok := IsEngineError(err); !ok || !ee.Transient || ee.Code != "model_not_ready" {
		t.Fatalf("stale cache must not be used: %v", err)
	}
	if _, has := d.Status()["last_error"]; !has {
		t.Fatal("status should show the last error")
	}
}

func TestDaemonResolver_StatusKeyExpiryWarning(t *testing.T) {
	now := time.Now()
	d := newDaemonResolver(t, "http://x", func(c *DaemonResolverConfig) {
		c.Now = func() time.Time { return now }
		c.KeyExpires = now.Add(10 * 24 * time.Hour)
	})
	if d.Status()["key_warning"] == nil {
		t.Fatalf("expiring key should warn: %v", d.Status())
	}
	d2 := newDaemonResolver(t, "http://x", func(c *DaemonResolverConfig) {
		c.Now = func() time.Time { return now }
		c.KeyExpires = now.Add(200 * 24 * time.Hour)
	})
	if d2.Status()["key_warning"] != nil {
		t.Fatal("far-off expiry should not warn")
	}
}

func TestWriteEngineError_TransientContainsModelNotReadyHardDoesNot(t *testing.T) {
	rec := httptest.NewRecorder()
	if !writeEngineError(rec, &EngineError{Code: "specialist_unavailable", Message: "starting", Transient: true, RetryAfter: 3 * time.Second}) {
		t.Fatal("not handled")
	}
	if rec.Code != 503 || rec.Header().Get("Retry-After") != "3" || !strings.Contains(rec.Body.String(), "model_not_ready") {
		t.Fatalf("transient: %d %q %s", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	rec = httptest.NewRecorder()
	writeEngineError(rec, &EngineError{Code: "speech_engine_disabled", Message: "engine e1 is turned off", Engine: "e1"})
	if rec.Code != 503 || strings.Contains(rec.Body.String(), "model_not_ready") || !strings.Contains(rec.Body.String(), "speech_engine_disabled") {
		t.Fatalf("hard: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatal("hard errors must not invite a retry")
	}
	if writeEngineError(httptest.NewRecorder(), context.Canceled) {
		t.Fatal("plain errors are not engine errors")
	}
}

func TestServerBackend_DaemonModeEngineThatStoppedIsTransientNotAFallback(t *testing.T) {
	// The daemon hands out an address nothing listens on (the engine was stopped between resolve and use).
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	f := &fakeDaemon{status: 200, body: `{"engine":"e1","base_url":"` + deadURL + `","lease_id":"L9"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	b := NewServerBackend(newDaemonResolver(t, srv.URL, nil))
	_, err := b.Speech(context.Background(), SynthSpec{Mode: ModeBase, ID: "v", RefAudio: []byte("RIFF"), RefText: "x"}, "hello")
	ee, ok := IsEngineError(err)
	if !ok || !ee.Transient {
		t.Fatalf("want transient EngineError, got %v", err)
	}
}

func TestDaemonResolver_UnreadableAnswerGivesTheLeaseBack(t *testing.T) {
	f := &fakeDaemon{status: 200, body: `{"engine":"x","lease_id":"LEAK"}`} // no base_url
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	d := newDaemonResolver(t, srv.URL, nil)
	if _, err := d.Resolve(context.Background(), ModeBase, ""); err == nil {
		t.Fatal("want an error")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := len(f.releases)
		f.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.releases) != 1 || f.releases[0] != "LEAK" {
		t.Fatalf("releases = %v", f.releases)
	}
}

func TestDaemonResolver_MissingRouteIsHardAndRefusedKeyShowsDegradedInHealth(t *testing.T) {
	f := &fakeDaemon{status: 404, body: `404 page not found`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	d := newDaemonResolver(t, srv.URL, nil)
	_, err := d.Resolve(context.Background(), ModeBase, "")
	if ee, ok := IsEngineError(err); !ok || ee.Transient {
		t.Fatalf("a daemon without the route is not 'loading': %v", err)
	}
	f.mu.Lock()
	f.status, f.body = 403, `{}`
	f.mu.Unlock()
	_, _ = d.Resolve(context.Background(), ModeBase, "")
	h, _ := NewServerBackend(d).Health(context.Background())
	if h["status"] != "degraded" {
		t.Fatalf("health with a refused key = %v", h)
	}
}

func TestServerBackend_ClonePreviewWithoutAnIDIsRegisteredUnderAStableName(t *testing.T) {
	var mu sync.Mutex
	var names []string
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/v1/audio/voices" {
			n, _ := in["name"].(string)
			if n == "" {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"'name' must be a non-empty string"}}`))
				return
			}
			names = append(names, n)
		}
		_, _ = w.Write([]byte("RIFFfake"))
	}))
	defer engine.Close()
	b := staticServerBackend("", "", engine.URL)
	spec := SynthSpec{Mode: ModeBase, RefAudio: []byte("wavbytes"), RefText: "hello"}
	for i := 0; i < 2; i++ {
		if _, err := b.Speech(context.Background(), spec, "hi"); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// The same reference is named the same and registered once: the engine keeps it, and uploading it again cost about 600 ms of every request.
	if len(names) != 1 || names[0] == "" || !strings.HasPrefix(names[0], "preview-") {
		t.Fatalf("registered names = %v", names)
	}
}

func TestServerBackend_EngineThatRefusesIsNotReportedAsNotReady(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":"bad reference"}`))
	}))
	defer engine.Close()
	f := &fakeDaemon{status: 200, body: `{"engine":"e1","base_url":"` + engine.URL + `","lease_id":"L1"}`}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()
	b := NewServerBackend(newDaemonResolver(t, srv.URL, nil))
	_, err := b.Speech(context.Background(), SynthSpec{Mode: ModeBase, ID: "v", RefAudio: []byte("x"), RefText: "y"}, "hi")
	if err == nil {
		t.Fatal("want an error")
	}
	if _, isEngine := IsEngineError(err); isEngine {
		t.Fatalf("an engine that answered 400 must not look retryable: %v", err)
	}
}

func TestEnginesEndpoint(t *testing.T) {
	calls := 0
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/speech/defaults" || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+testKey {
			http.Error(w, "no", 403)
			return
		}
		_, _ = w.Write([]byte(`{"engine_info":[{"name":"qwen3-tts-base","label":"Base","mode":"base","policy":"always_on","enabled":true,"state":"running"}]}`))
	}))
	d := newDaemonResolver(t, daemon.URL, nil)

	// Without a daemon resolver the endpoint is 404: there is nothing to choose from.
	srv, _ := newTestServer("")
	ts := httptest.NewServer(srv.Handler())
	resp, _ := http.Get(ts.URL + "/v1/engines")
	resp.Body.Close()
	ts.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("static mode: %d, want 404", resp.StatusCode)
	}

	srv.SetEngineLister(d)
	ts = httptest.NewServer(srv.Handler())
	defer ts.Close()
	get := func() (int, string) {
		resp, err := http.Get(ts.URL + "/v1/engines")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get()
	if code != 200 || !strings.Contains(body, `"qwen3-tts-base"`) || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("engines: %d %s", code, body)
	}
	get()
	if calls != 1 {
		t.Fatalf("the list should be reused for a few seconds, daemon called %d times", calls)
	}
	// The daemon goes away: the last list is still served.
	daemon.Close()
	d.mu.Lock()
	d.enginesAt = time.Time{}
	d.mu.Unlock()
	if code, body := get(); code != 200 || !strings.Contains(body, "qwen3-tts-base") {
		t.Fatalf("stale list: %d %s", code, body)
	}
}
