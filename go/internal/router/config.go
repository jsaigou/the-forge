// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// RouterConfig is the a0 router configuration, built by LoadFromStore.
// Originally the V4 [router] / [[router.backends]] / [[router.routes]]
// TOML tables, ported to V5 as a read-only file; retired as a file once
// LoadFromStore replaced ParseConfig as the sole loader (TOML decommission,
// docs/v5-toml-decommission.md, cutover live 2026-07-28, code deleted
// Phase 8 §8). busy_mode + compressor.passthrough_all are app-mutated and
// live in the store's Settings KV, not here.
type RouterConfig struct {
	ListenPort           int     `json:"listen_port"`
	ConnectTimeoutS      float64 `json:"connect_timeout_s"`
	RequestTimeoutS      float64 `json:"request_timeout_s"`
	HealthTTLS           float64 `json:"health_ttl_s"`
	MaxRetriesPerBackend int     `json:"max_retries_per_backend"`
	// BusyMode / CompressorPassthrough* are NOT part of the store-backed
	// `infra.router` shape (docs/v5-toml-decommission.md §3.1) — they
	// already live at their own `router.busy_mode` / `compressor.passthrough_*`
	// settings keys (see routing.go's busyMode/passthroughAll) and stay
	// there unchanged. LoadFromStore leaves these at applyDefaults' zero
	// value; they exist on this struct only for NewConfig()'s test callers.
	BusyMode                    BusyMode `json:"-"`
	CompressorPassthroughAll      bool     `json:"-"`
	CompressorPassthroughServices []string `json:"-"`
	EnsureLoadedTimeoutS        float64  `json:"ensure_loaded_timeout_s"`
	EmbeddingURL                string   `json:"embedding_url"`
	// Backends / Routes have no store-backed replacement (ADR-0007): local
	// routing resolves by catalog Config name (routing.go's catalogChain),
	// remote resolves via store.Offering — neither needs a declared
	// backend/route list. LoadFromStore always leaves both nil.
	Backends []Backend `json:"-"`
	Routes   []Route   `json:"-"`
}

// BusyMode controls foundry_slot busy-slot behavior (see docs/llm-router.md
// "Busy-Slot Behavior"). "wait" (default) attempts a busy-but-alive slot;
// "fail_fast" treats it as unavailable and skips/fails.
type BusyMode string

const (
	BusyWait     BusyMode = "wait"
	BusyFailFast BusyMode = "fail_fast"
)

// Backend is one [[router.backends]] entry. The Kind field selects how the
// backend is reached, gated, and resolved (see resolve_backend in routing.go).
type Backend struct {
	Name       string
	Kind       string // "foundry_slot" | "remote"
	Port       int    // foundry_slot: raw llama-server port
	BaseURL    string // remote: upstream URL (typically compressor proxy)
	WireModel  string // remote: model string the upstream expects
	Credential string // remote: provider name → store.Routing.Providers

	// Pricing fields (cost/savings sprint Phase 4, 2026-07-30) — populated
	// only by offeringChain, from the store.Offering matched for this
	// request, so usage recording has real per-1M rates with no extra DB
	// read at response time. Zero values everywhere else (foundry_slot; the
	// dead static router.toml fallback backends) — PriceCurrency == "" is
	// the "pricing unknown, don't compute a cost" signal.
	PriceInPer1M       float64
	PriceOutPer1M      float64
	PriceCachedInPer1M *float64 // nil = provider's cache-hit discount unmodelled
	PriceCurrency      string
}

// Route is one [[router.routes]] entry: a logical model name maps to an
// ordered backend chain (primary + optional fallback). The router walks the
// chain in order, skipping backends that are gated unhealthy/busy.
type Route struct {
	Model    string
	Primary  string
	Fallback []string
}

// NewConfig builds a *RouterConfig from an in-memory value, applying
// defaults and validating exactly like LoadFromStore does — for callers
// that already have the data in Go (tests, in-process construction) rather
// than the store. The struct-literal equivalent of what the retired
// ParseConfig did for a TOML document.
func NewConfig(cfg RouterConfig) (*RouterConfig, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("router: validate config: %w", err)
	}
	return &cfg, nil
}

// LoadFromStore builds a *RouterConfig from the store-backed `infra.router`
// settings key — the sole config loader main.go wires in (TOML decommission
// cutover, live 2026-07-28). A missing key is not an error — the zero-value
// config gets the same applyDefaults() treatment.
//
// Backends and Routes are always left nil here — ADR-0007 replaces declared
// backends/routes entirely: local resolves by catalog Config name
// (routing.go's catalogChain), remote resolves via store.Offering. BusyMode
// and the CompressorPassthrough* fields are also left at their zero value:
// those are already store-backed under their own settings keys
// (router.busy_mode, compressor.passthrough_*) and read directly by
// routing.go, independent of this struct.
func LoadFromStore(ctx context.Context, st store.Store) (*RouterConfig, error) {
	var cfg RouterConfig
	raw, err := st.Settings().Get(ctx, "infra.router")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("router: load infra.router: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("router: parse infra.router: %w", err)
		}
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("router: validate config: %w", err)
	}
	return &cfg, nil
}

func (c *RouterConfig) applyDefaults() {
	if c.ConnectTimeoutS == 0 {
		c.ConnectTimeoutS = 5
	}
	// RequestTimeoutS deliberately has no non-zero default (see
	// requestTimeout()'s doc comment) — 0 means unbounded, and unbounded is
	// the intended default now, not an unset-field placeholder.
	if c.HealthTTLS == 0 {
		c.HealthTTLS = 4
	}
	if c.MaxRetriesPerBackend == 0 {
		c.MaxRetriesPerBackend = 1
	}
	if c.BusyMode == "" {
		c.BusyMode = BusyWait
	}
	if c.EnsureLoadedTimeoutS == 0 {
		c.EnsureLoadedTimeoutS = 320
	}
}

func (c *RouterConfig) validate() error {
	seen := map[string]bool{}
	for i := range c.Backends {
		b := &c.Backends[i]
		if b.Name == "" {
			return fmt.Errorf("backends[%d]: name is required", i)
		}
		if seen[b.Name] {
			return fmt.Errorf("backends[%d]: duplicate name %q", i, b.Name)
		}
		seen[b.Name] = true
		switch b.Kind {
		case "foundry_slot":
			if b.Port == 0 {
				return fmt.Errorf("backend %q: foundry_slot requires port", b.Name)
			}
		case "remote":
			if b.BaseURL == "" {
				return fmt.Errorf("backend %q: remote requires base_url", b.Name)
			}
			if b.WireModel == "" {
				return fmt.Errorf("backend %q: remote requires wire_model", b.Name)
			}
		default:
			return fmt.Errorf("backend %q: unknown kind %q", b.Name, b.Kind)
		}
	}
	if c.EmbeddingURL != "" {
		u, err := url.Parse(c.EmbeddingURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("embedding_url %q: must be an absolute http(s) URL", c.EmbeddingURL)
		}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.Model == "" {
			return fmt.Errorf("routes[%d]: model is required", i)
		}
		if r.Primary == "" {
			return fmt.Errorf("routes[%d]: primary is required", i)
		}
		if !seen[r.Primary] {
			return fmt.Errorf("routes[%d]: primary %q is not a defined backend", i, r.Primary)
		}
		for j, f := range r.Fallback {
			if !seen[f] {
				return fmt.Errorf("routes[%d]: fallback[%d] %q is not a defined backend", i, j, f)
			}
		}
	}
	return nil
}

// BackendByName returns the backend with the given name, or nil.
func (c *RouterConfig) BackendByName(name string) *Backend {
	for i := range c.Backends {
		if c.Backends[i].Name == name {
			return &c.Backends[i]
		}
	}
	return nil
}

// RouteFor returns the route for the given logical model name, or nil.
func (c *RouterConfig) RouteFor(model string) *Route {
	for i := range c.Routes {
		if c.Routes[i].Model == model {
			return &c.Routes[i]
		}
	}
	return nil
}

// connectTimeout returns the connect timeout as a time.Duration.
func (c *RouterConfig) connectTimeout() time.Duration {
	return secs(c.ConnectTimeoutS)
}

// requestTimeout returns the overall request timeout as a time.Duration.
// Zero (the default) means unbounded — callers gate on `timeout > 0` before
// wrapping the request context. This used to default to 300s, a flat
// V4-era ceiling applied to every model regardless of how long it actually
// takes to generate. laguna-s-21 (~9.6 tok/s decode, agentic-coding
// responses that can legitimately run tens of thousands of output tokens)
// routinely exceeded it, and a0 would sever the connection mid-stream —
// the client saw a broken/incomplete response, not a clean error. Removed
// 2026-07-30 rather than given a per-model override: nothing else bounds
// this per-model today, a slow response isn't itself a failure mode worth
// guarding against, and a genuinely dead upstream/client is already caught
// independently (the client disconnecting cancels r.Context(), which
// ReverseProxy honors regardless of this timeout).
func (c *RouterConfig) requestTimeout() time.Duration {
	return secs(c.RequestTimeoutS)
}

// healthTTL returns the probe cache TTL as a time.Duration.
func (c *RouterConfig) healthTTL() time.Duration {
	return secs(c.HealthTTLS)
}

// ensureLoadedTimeout returns the on-demand load timeout as a time.Duration.
func (c *RouterConfig) ensureLoadedTimeout() time.Duration {
	return secs(c.EnsureLoadedTimeoutS)
}

func secs(s float64) time.Duration {
	return time.Duration(s * float64(time.Second))
}
