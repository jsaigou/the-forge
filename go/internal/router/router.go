// SPDX-License-Identifier: Apache-2.0

// Package router ports the a0 OpenAI-compatible routing/failover proxy
// (forge/router_app.py, router.py, router_catalog.py — see
// docs/llm-router.md). Owned by track D (Phase 6).
//
// Frozen external surface (Contract 1 §a0): GET /healthz, GET /v1/models,
// POST /v1/chat/completions (streaming passthrough). Auth is
// tailnet-conditional via authz.EffectiveRemoteAddr + authz.IsTailnetAddr —
// the CGNAT/XFF semantics live in internal/authz and MUST be used, not
// reimplemented. Read docs/llm-router.md "Compressor Passthrough" (the
// get_provider_credential base_url trap) before porting credentials.
//
// Until Phase 9 integration the router fronts upstreams directly (scheduler
// hookup is one interface call, wired at integration). New() returns a
// skeleton (healthz-only) server so cmd/forge keeps compiling; tests and
// Phase 9 wiring use NewWithDeps.
package router

import (
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// Deps are the frozen-interface dependencies (Contract 2). All fields
// optional (nil → stub/default); Phase 9 swaps real implementations in
// cmd/forge and nowhere else.
type Deps struct {
	// Cfg is the read-only router config (backends, routes, timeouts). Nil
	// → skeleton mode: /v1/* returns 503, only /healthz is served.
	Cfg *RouterConfig
	// Auth verifies inbound sk-router-* bearer tokens for non-tailnet
	// requests. Nil → non-tailnet requests are 401.
	Auth authz.Authenticator
	// Sched is the on-demand load hook for foundry_slot backends gated
	// "unhealthy". Nil → on-demand load fails with "scheduler not wired"
	// (the route then fails over or 502s). Use sched.Stub for tests.
	Sched sched.Scheduler
	// Compressor resolves remote-backend credentials + compressor proxy targets
	// (V5 home for V4's auth.get_provider_credential /
	// get_headroom_proxy_targets / get_router_providers). Nil → remote
	// backends fail resolve_error.
	Routing store.Routing
	// Settings is the JSON KV surface for app-mutated values
	// (router.busy_mode, compressor.passthrough_all). Nil → config-file
	// defaults are used.
	Settings store.Settings
	// Audit records routing decisions (backend names + coarse status labels
	// only — never bodies, prompts, or credentials). Nil → no audit.
	Audit store.Audit
	// Catalog answers slot health/busy/wire-model questions. Nil → a
	// default ttlCatalog (V4-style HTTP probing with TTL cache) is lazily
	// created. Phase 9 swaps in a collector-snapshot-backed implementation.
	Catalog SlotCatalog
	// StoreCatalog reads remote Offerings from the model catalog store
	// (MODEL CATALOG Phase 2). Nil → /v1/models lists only file-based
	// router routes (the V4 behavior). When populated, Offerings appear
	// alongside file-based routes.
	StoreCatalog store.Catalog
	// HTTPClient is used for upstream chat-completion requests. Nil →
	// http.DefaultClient. The catalog's /health+/props+/metrics probes use
	// a separate client (see ttlCatalog).
	HTTPClient *http.Client
	// Slots maps slot name → raw llama-server port, from config.Config.Slots
	// (the V5 read-only config file). Used to pin on-demand loads to the
	// slot a foundry_slot backend points at. Nil → on-demand load fails
	// with "no slot configured for port N".
	Slots map[string]int
	// Usage records real remote-provider spend as kind="external_request"
	// rows (cost/savings sprint Phase 4, 2026-07-30). Nil → usage tracking
	// is skipped entirely (streaming/non-streaming responses still proxy
	// normally; only the cost-recording side effect is absent).
	Usage store.Usage
	// Activity is the per-slot consumer attribution registry (shared with
	// smith + httpapi — one instance wired in cmd/forge). Foundry_slot
	// attempts Mark the slot with the caller's derived consumer label on
	// request start and again when the upstream body is fully streamed.
	// Nil → no attribution.
	Activity *activity.Registry
}

// Server is the a0 router.
type Server struct {
	deps Deps

	// catalogOnce lazily creates a ttlCatalog when deps.Catalog is nil.
	catalogOnce sync.Once
	lazyCatalog *ttlCatalog

	// slotErrors tracks consecutive/short-window 5xx+transport failures per
	// foundry_slot backend, keyed by port. This is the device-lost
	// early-warning signal: a wedged llama-server returns 5xx on every
	// request while /health stays green (the 2026-08-16 qwen38-27b
	// incident), so the collector's stall detector never fires. The
	// collector reads SlotErrorCount each cycle and raises
	// SLOT_ERROR_STORM when a threshold is crossed.
	slotErrors   map[int]*slotErrorWindow
	slotErrorsMu sync.Mutex
}

// slotErrorWindow is a sliding window of recent failure timestamps for one
// slot port (maxSlotsErrorWindow entries; entries older than the window are
// dropped on read). A windowed count, not an unbounded counter, so a slot
// that recovered doesn't keep a stale alarm forever.
type slotErrorWindow struct {
	at    []int64 // unix seconds of 5xx/transport failures
	total int64   // lifetime failures (for the /props exposure)
}

const maxSlotsErrorWindow = 12

// New returns a skeleton router server (healthz-only) — kept zero-arg so
// cmd/forge's `router.New().Handler()` call site stays unchanged. Tests
// and Phase 9 wiring use NewWithDeps for the full surface.
func New() *Server { return &Server{} }

// NewWithDeps returns a router server wired with real dependencies. This is
// the constructor tests and Phase 9 use; New() is the skeleton entry point.
func NewWithDeps(deps Deps) *Server {
	return &Server{deps: deps, slotErrors: map[int]*slotErrorWindow{}}
}

// recordSlotError records one 5xx/transport failure for a foundry_slot
// backend port. Called from tryBackends' ErrorHandler / ModifyResponse 5xx
// path. Never blocks the hot path on lock contention beyond the mutex.
func (s *Server) recordSlotError(port int) {
	if port == 0 {
		return
	}
	s.slotErrorsMu.Lock()
	defer s.slotErrorsMu.Unlock()
	w := s.slotErrors[port]
	if w == nil {
		w = &slotErrorWindow{}
		s.slotErrors[port] = w
	}
	now := time.Now().Unix()
	w.at = append(w.at, now)
	w.total++
	if len(w.at) > maxSlotsErrorWindow {
		w.at = w.at[len(w.at)-maxSlotsErrorWindow:]
	}
}

// SlotErrorCount returns the number of 5xx/transport failures recorded for
// port within windowSeconds (0 = lifetime). Used by the collector to raise
// SLOT_ERROR_STORM. Reads are lock-brief and window pruning happens here, so
// a stale entry self-expires on the next read.
func (s *Server) SlotErrorCount(port int, windowSeconds int64) (int, int64) {
	if s == nil {
		return 0, 0
	}
	s.slotErrorsMu.Lock()
	defer s.slotErrorsMu.Unlock()
	w := s.slotErrors[port]
	if w == nil {
		return 0, 0
	}
	if windowSeconds <= 0 {
		return len(w.at), w.total
	}
	cutoff := time.Now().Unix() - windowSeconds
	keep := w.at[:0]
	for _, ts := range w.at {
		if ts >= cutoff {
			keep = append(keep, ts)
		}
	}
	w.at = keep
	return len(w.at), w.total
}

// Handler serves the frozen a0 surface (Contract 1 §7):
//
//   - GET /healthz              — unauthenticated liveness (systemd ExecStartPost)
//   - GET /v1/models            — OpenAI-shaped model list (tailnet-conditional auth)
//   - POST /v1/chat/completions — OpenAI-compatible passthrough (tailnet-conditional auth)
//   - POST /v1/embeddings       — static passthrough to the embedding service
//     (tailnet-conditional auth; no routing/failover — see embeddings.go)
//   - GET /v1/load-status       — model-keyed load-progress poll (Sprint 1, a0 load
//     visibility, 2026-08-27; tailnet-conditional auth — see load_status.go)
//   - POST /v1/audio/speech     — static passthrough to forge-tts (Sprint 3, a0 TTS
//     passthrough, 2026-08-27; tailnet-conditional auth — see speech.go)
//   - GET /v1/voices            — static passthrough to forge-tts's voice list
//     (same auth, same file; GET only, matching forge-tts's own auth scope)
//
// In skeleton mode (New() with no deps), only /healthz answers; /v1/* paths
// return 503 "router not configured".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","service":"a0"}`))
	})

	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(r).ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
			return
		}
		if s.deps.Cfg == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "router not configured"})
			return
		}
		resp := BuildModelsResponse(r.Context(), s.deps.StoreCatalog, s.providerRows(r.Context()))
		writeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	mux.HandleFunc("POST /v1/embeddings", s.embeddings)
	mux.HandleFunc("GET /v1/load-status", s.loadStatus)
	mux.HandleFunc("POST /v1/audio/speech", s.speechProxy("/audio/speech"))
	mux.HandleFunc("GET /v1/voices", s.speechProxy("/voices"))

	return mux
}

// cfg returns the router config (nil-safe — returns an empty default when
// Cfg is nil, so callers can use s.cfg().healthTTL() etc. without nil checks
// on the hot path; the chatCompletions handler guards deps.Cfg == nil
// explicitly before routing).
func (s *Server) cfg() *RouterConfig {
	if s.deps.Cfg != nil {
		return s.deps.Cfg
	}
	return &emptyRouterConfig
}

// catalog returns the SlotCatalog, lazily creating a ttlCatalog if none was
// injected via Deps. The lazy catalog uses http.DefaultClient for /health,
// /props, /metrics probes against 127.0.0.1:<slot port>.
func (s *Server) catalog() SlotCatalog {
	if s.deps.Catalog != nil {
		return s.deps.Catalog
	}
	s.catalogOnce.Do(func() {
		s.lazyCatalog = newTTLCatalog(s.deps.HTTPClient)
	})
	return s.lazyCatalog
}

// httpClient returns the upstream HTTP client, defaulting to one with a dial
// timeout from the config (connect_timeout_s). The catalog prober uses its
// own client (see ttlCatalog).
func (s *Server) httpClient() *http.Client {
	if s.deps.HTTPClient != nil {
		return s.deps.HTTPClient
	}
	cfg := s.cfg()
	dialer := &net.Dialer{Timeout: cfg.connectTimeout()}
	return &http.Client{
		Transport: &http.Transport{
			DialContext:       dialer.DialContext,
			ForceAttemptHTTP2: true,
		},
	}
}

// emptyRouterConfig is the zero-value RouterConfig with defaults applied —
// used by cfg() when no config is wired. Callers that route /v1/* traffic
// check deps.Cfg == nil first and return 503.
var emptyRouterConfig = func() RouterConfig {
	c := RouterConfig{}
	c.applyDefaults()
	return c
}()
