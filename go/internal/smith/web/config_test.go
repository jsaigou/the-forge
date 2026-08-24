// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeSettings is an in-memory store.Settings (mirrors router/fakes_test.go's).
type fakeSettings struct {
	mu sync.Mutex
	kv map[string][]byte
}

func newFakeSettings() *fakeSettings { return &fakeSettings{kv: map[string][]byte{}} }

func (f *fakeSettings) set(key string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(v)
	f.kv[key] = raw
}

func (f *fakeSettings) setRaw(key, raw string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = []byte(raw)
}

func (f *fakeSettings) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.kv[key]; ok {
		return v, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeSettings) Set(_ context.Context, key string, v []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = v
	return nil
}

func TestLoadConfig_NilSettings(t *testing.T) {
	cfg := LoadConfig(context.Background(), nil)
	if !cfg.Enabled {
		t.Fatal("nil Settings should default to enabled")
	}
	if len(cfg.fetchOrder()) != 1 || cfg.fetchOrder()[0] != "direct" {
		t.Fatalf("fetchOrder = %v, want [direct] (only direct enabled by default)", cfg.fetchOrder())
	}
	if len(cfg.searchOrder()) != 0 {
		t.Fatalf("searchOrder = %v, want empty (searxng not configured)", cfg.searchOrder())
	}
}

func TestLoadConfig_Unset(t *testing.T) {
	// A real settings store with zero smith.web.* keys set — same as
	// DefaultConfig, never panics.
	s := newFakeSettings()
	cfg := LoadConfig(context.Background(), s)
	if !cfg.Enabled {
		t.Fatal("unset settings should default to enabled")
	}
	if cfg.CacheTTL != 6*time.Hour {
		t.Fatalf("CacheTTL = %v, want 6h default", cfg.CacheTTL)
	}
}

func TestLoadConfig_GarbageJSON(t *testing.T) {
	s := newFakeSettings()
	s.setRaw(SettingEnabled, `not json`)
	s.setRaw(SettingSearxng, `{"base_url": 123}`) // wrong type
	cfg := LoadConfig(context.Background(), s)
	if !cfg.Enabled {
		t.Fatal("garbage smith.web.enabled should fall back to default (true)")
	}
	if cfg.Searxng.BaseURL != "" {
		t.Fatalf("garbage smith.web.searxng should fall back to default, got %+v", cfg.Searxng)
	}
}

func TestLoadProviderConfig_AbsentFieldFallsBackToDefault(t *testing.T) {
	// A blob missing "enabled"/"api_key" must fall back to def's values for
	// those fields, not zero them — the pointer-shadow-struct convention
	// (loadProviderConfig mirrors smith.Schedule's "absent ≠ zero" rule).
	s := newFakeSettings()
	s.setRaw(SettingSearxng, `{"base_url":"https://searxng.example"}`)
	def := ProviderConfig{Enabled: true, APIKey: "carried-over"}
	got := loadProviderConfig(context.Background(), s, SettingSearxng, def)
	if got.BaseURL != "https://searxng.example" {
		t.Fatalf("base_url not applied: %+v", got)
	}
	if !got.Enabled || got.APIKey != "carried-over" {
		t.Fatalf("absent fields should carry over def's values, got %+v", got)
	}
}

func TestConfig_FetchOrder_DirectAlwaysLast(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProviderOrder = []string{"direct", "firecrawl", "searxng"} // direct listed FIRST
	cfg.Firecrawl = ProviderConfig{Enabled: true, BaseURL: "https://firecrawl.example"}
	cfg.Direct = ProviderConfig{Enabled: true}

	order := cfg.fetchOrder()
	if len(order) != 2 || order[0] != "firecrawl" || order[1] != "direct" {
		t.Fatalf("fetchOrder = %v, want [firecrawl direct] (direct forced last regardless of provider_order)", order)
	}
}

func TestConfig_FetchOrder_FirecrawlUnconfiguredSkipped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProviderOrder = []string{"firecrawl", "direct"}
	cfg.Firecrawl = ProviderConfig{Enabled: true, BaseURL: ""} // enabled but no base_url
	cfg.Direct = ProviderConfig{Enabled: true}

	order := cfg.fetchOrder()
	if len(order) != 1 || order[0] != "direct" {
		t.Fatalf("fetchOrder = %v, want [direct] (firecrawl unconfigured despite enabled=true)", order)
	}
}

func TestConfig_FetchOrder_DirectDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Direct = ProviderConfig{Enabled: false}
	if order := cfg.fetchOrder(); len(order) != 0 {
		t.Fatalf("fetchOrder = %v, want empty when direct explicitly disabled and firecrawl unconfigured", order)
	}
}

func TestConfig_SearchOrder(t *testing.T) {
	cfg := DefaultConfig()
	if order := cfg.searchOrder(); len(order) != 0 {
		t.Fatalf("searchOrder = %v, want empty (searxng unconfigured)", order)
	}
	cfg.Searxng = ProviderConfig{Enabled: true, BaseURL: "https://searxng.example"}
	if order := cfg.searchOrder(); len(order) != 1 || order[0] != "searxng" {
		t.Fatalf("searchOrder = %v, want [searxng]", order)
	}
}
