// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Settings keys (docs/v5-smith.md §5, §4.8). smith.go aliases these so the
// vocabulary is discoverable beside the other four smith.* keys — this
// package can't be imported by smith's settings constants block directly
// (web owns them to avoid an import cycle: smith imports web, not the
// reverse).
const (
	SettingEnabled       = "smith.web.enabled"
	SettingProviderOrder = "smith.web.provider_order"
	SettingCacheTTL      = "smith.web.cache_ttl"
	SettingSearxng       = "smith.web.searxng"
	SettingFirecrawl     = "smith.web.firecrawl"
	SettingDirect        = "smith.web.direct"
	// Operator feedback 2026-08-14: a generic custom search provider and a
	// generic custom fetch provider, so an operator can point smith at any
	// SearxNG-compatible search endpoint and any text-returning fetch proxy
	// without a per-service adapter.
	SettingCustomSearch = "smith.web.customsearch"
	SettingCustomFetch  = "smith.web.customfetch"
)

// ProviderConfig is one adapter's operator-editable configuration.
type ProviderConfig struct {
	BaseURL string `json:"base_url"`
	Enabled bool   `json:"enabled"`
	APIKey  string `json:"api_key"`
}

// Config is the fully-resolved smith.web.* settings.
type Config struct {
	Enabled       bool
	ProviderOrder []string
	CacheTTL      time.Duration
	Searxng       ProviderConfig
	Firecrawl     ProviderConfig
	Direct        ProviderConfig
	CustomSearch  ProviderConfig
	CustomFetch   ProviderConfig
}

// DefaultConfig is used when smith.web.* is entirely unset — e.g. a
// non-ForgeHost install before migration 0037's seed runs, or a settings read
// failure. Web research defaults to ON (Sprint S1, docs/v5-smith-experience.md
// §2.6 R2): always on unless specifically disabled by the operator in
// Settings. No hostname is hardcoded here (the same "no hardcoded default
// anywhere in code" rule smith.model follows).
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		ProviderOrder: []string{"searxng", "firecrawl", "direct"},
		CacheTTL:      6 * time.Hour,
		Direct:        ProviderConfig{Enabled: true},
	}
}

// LoadConfig reads smith.web.* from the settings KV, falling back
// field-by-field to DefaultConfig for anything missing or invalid. Never
// returns an error (the smith.Schedule() template — smith.go:553-577);
// nil Settings degrades to DefaultConfig entirely.
func LoadConfig(ctx context.Context, s store.Settings) Config {
	out := DefaultConfig()
	if s == nil {
		return out
	}
	if raw, ok := getSetting(ctx, s, SettingEnabled); ok {
		var v bool
		if json.Unmarshal(raw, &v) == nil {
			out.Enabled = v
		}
	}
	if raw, ok := getSetting(ctx, s, SettingProviderOrder); ok {
		var v []string
		if json.Unmarshal(raw, &v) == nil && len(v) > 0 {
			out.ProviderOrder = v
		}
	}
	if raw, ok := getSetting(ctx, s, SettingCacheTTL); ok {
		var v string
		if json.Unmarshal(raw, &v) == nil {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				out.CacheTTL = d
			}
		}
	}
	out.Searxng = loadProviderConfig(ctx, s, SettingSearxng, out.Searxng)
	out.Firecrawl = loadProviderConfig(ctx, s, SettingFirecrawl, out.Firecrawl)
	out.Direct = loadProviderConfig(ctx, s, SettingDirect, out.Direct)
	out.CustomSearch = loadProviderConfig(ctx, s, SettingCustomSearch, out.CustomSearch)
	out.CustomFetch = loadProviderConfig(ctx, s, SettingCustomFetch, out.CustomFetch)
	return out
}

func getSetting(ctx context.Context, s store.Settings, key string) ([]byte, bool) {
	raw, err := s.Get(ctx, key)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

// loadProviderConfig decodes one smith.web.<name> blob against a
// pointer-typed shadow struct so an absent field falls back to def rather
// than zeroing it — the "absent ≠ zero" convention smith.Schedule uses.
func loadProviderConfig(ctx context.Context, s store.Settings, key string, def ProviderConfig) ProviderConfig {
	raw, ok := getSetting(ctx, s, key)
	if !ok {
		return def
	}
	var v struct {
		BaseURL *string `json:"base_url"`
		Enabled *bool   `json:"enabled"`
		APIKey  *string `json:"api_key"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	out := def
	if v.BaseURL != nil {
		out.BaseURL = *v.BaseURL
	}
	if v.Enabled != nil {
		out.Enabled = *v.Enabled
	}
	if v.APIKey != nil {
		out.APIKey = *v.APIKey
	}
	return out
}

// searchOrder returns the enabled search-capable providers in preference
// order, drawn from the operator's ProviderOrder. searxng and the generic
// customsearch are both search-capable; the first enabled one in saved
// order leads. A second search adapter extends this slice, not replaces it.
func (c Config) searchOrder() []string {
	var out []string
	for _, name := range c.ProviderOrder {
		switch name {
		case "searxng":
			if c.Searxng.Enabled && c.Searxng.BaseURL != "" {
				out = append(out, "searxng")
			}
		case "customsearch":
			if c.CustomSearch.Enabled && c.CustomSearch.BaseURL != "" {
				out = append(out, "customsearch")
			}
		}
	}
	return out
}

// fetchOrder returns the enabled fetch-capable providers, with `direct`
// always forced last regardless of its position in ProviderOrder — the
// operator's saved order only ever affects preference among the *other*
// adapters (docs/v5-smith.md §4.8; D4 in the P5 plan).
func (c Config) fetchOrder() []string {
	var out []string
	for _, name := range c.ProviderOrder {
		switch name {
		case "firecrawl":
			if c.Firecrawl.Enabled && c.Firecrawl.BaseURL != "" {
				out = append(out, "firecrawl")
			}
		case "customfetch":
			if c.CustomFetch.Enabled && c.CustomFetch.BaseURL != "" {
				out = append(out, "customfetch")
			}
		}
	}
	if c.Direct.Enabled {
		out = append(out, "direct")
	}
	return out
}
