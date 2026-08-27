// SPDX-License-Identifier: Apache-2.0

package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
	"github.com/jsaigou/the-forge/internal/authz"
)

// TestChatCompletions_AttributesSlotToBearerKey proves the per-slot consumer
// registry gets marked on a successful foundry_slot attempt with the
// bearer-key-derived label ("opencode-examplehost" → "ExampleHost (OpenCode)") and
// that the completion mark fires once the response body is fully consumed.
func TestChatCompletions_AttributesSlotToBearerKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "brain.gguf"})

	reg := activity.New()
	srv := NewWithDeps(Deps{
		Cfg:      testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "brain", Primary: "a1"}}),
		Catalog:  cat,
		Auth:     &stubAuth{validToken: "x", identity: authz.Identity{Name: "opencode-examplehost"}},
		Slots:    map[string]int{"a1": port},
		Activity: reg,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"brain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "8.8.8.8:1234"
	req.Header.Set("Authorization", "Bearer x")
	// DeriveLabel's app-name half comes from the User-Agent, verbatim casing
	// preserved (no hardcoded brand table since 0361ef9) — without this the
	// key name alone ("opencode-examplehost") is the whole label.
	req.Header.Set("User-Agent", "OpenCode/1.2.3")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	if got := reg.Label("a1", time.Minute); got != "ExampleHost (OpenCode)" {
		t.Errorf("Label(a1) = %q, want ExampleHost (OpenCode)", got)
	}
	if got := reg.Label("a2", time.Minute); got != "" {
		t.Errorf("Label(a2) = %q, want empty (untouched slot)", got)
	}
}

// TestChatCompletions_AttributesTailnetBypassToIP: no bearer key (tailnet
// bypass — zero identity) → the effective remote address is the label.
func TestChatCompletions_AttributesTailnetBypassToIP(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "brain.gguf"})

	reg := activity.New()
	srv := NewWithDeps(Deps{
		Cfg:      testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "brain", Primary: "a1"}}),
		Catalog:  cat,
		Slots:    map[string]int{"a1": port},
		Activity: reg,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"brain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.100.100.100:54321" // tailnet direct → bypass, no identity
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := reg.Label("a1", time.Minute); got != "100.100.100.100" {
		t.Errorf("Label(a1) = %q, want 100.100.100.100 (remote IP fallback)", got)
	}
}

// TestChatCompletions_SmithRequestsSkipRouterAttribution: smith's own
// reasoning traffic (X-Forge-Requested-By: smith, loopback) must NOT be
// router-marked — smith marks its slot directly as "SMITH", and a router
// loopback/IP mark would clobber it mid-turn.
func TestChatCompletions_SmithRequestsSkipRouterAttribution(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "brain.gguf"})

	reg := activity.New()
	reg.Mark("a1", "SMITH") // smith's own start-mark stands in for reasoning.go's Mark call
	srv := NewWithDeps(Deps{
		Cfg:      testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "brain", Primary: "a1"}}),
		Catalog:  cat,
		Slots:    map[string]int{"a1": port},
		Activity: reg,
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"brain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "127.0.0.1:9999" // loopback, no XFF — the smith path
	req.Header.Set("X-Forge-Requested-By", "smith")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if got := reg.Label("a1", time.Minute); got != "SMITH" {
		t.Errorf("Label(a1) = %q, want SMITH (router must not clobber smith's own mark)", got)
	}
}

// TestChatCompletions_NilActivityRegistry: attribution is optional — nil
// registry must not panic or change behavior.
func TestChatCompletions_NilActivityRegistry(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"1","object":"chat.completion","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}]}`))
	}))
	defer upstream.Close()
	port := portFromURL(t, upstream.URL)

	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "brain.gguf"})

	srv := NewWithDeps(Deps{
		Cfg:     testCfg([]Backend{{Name: "a1", Kind: "foundry_slot", Port: port}}, []Route{{Model: "brain", Primary: "a1"}}),
		Catalog: cat,
		Slots:   map[string]int{"a1": port},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"brain","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestConsumerLabel_Derivation(t *testing.T) {
	srv := NewWithDeps(Deps{})
	cases := []struct {
		name     string
		identity authz.Identity
		remote   string
		xff      string
		ua       string
		want     string
	}{
		{name: "bearer opencode-examplehost", identity: authz.Identity{Name: "opencode-examplehost"}, remote: "8.8.8.8:1", ua: "OpenCode/1.2.3", want: "ExampleHost (OpenCode)"},
		{name: "bearer bare librechat", identity: authz.Identity{Name: "librechat"}, remote: "8.8.8.8:1", ua: "LibreChat/2.0", want: "LibreChat"},
		// No UA → raw key name; DisplayName wins over everything.
		{name: "bearer unmatched, no UA", identity: authz.Identity{Name: "sakuga-ingest"}, remote: "8.8.8.8:1", want: "sakuga-ingest"},
		{name: "preferred display name wins", identity: authz.Identity{Name: "opencode-core", DisplayName: "ExampleHost (OpenCode)"}, remote: "8.8.8.8:1", ua: "curl/8.4", want: "ExampleHost (OpenCode)"},
		{name: "tailnet bypass → IP", remote: "100.64.0.9:1", want: "100.64.0.9"},
		{name: "tailscale serve → XFF IP", remote: "127.0.0.1:1", xff: "100.100.100.100", want: "100.100.100.100"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if c.ua != "" {
			r.Header.Set("User-Agent", c.ua)
		} else {
			r.Header.Set("User-Agent", "")
		}
		got := srv.consumerLabel(r, authResult{identity: c.identity, ok: true})
		if got != c.want {
			t.Errorf("%s: consumerLabel = %q, want %q", c.name, got, c.want)
		}
	}
}
