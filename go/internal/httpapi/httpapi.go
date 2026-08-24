// SPDX-License-Identifier: Apache-2.0

// Package httpapi implements Contract 1 (docs/v5-api-contract.md): the
// dashboard JSON API, the SSE bus endpoint, and embed.FS serving of the
// React PWA. Owned by track C (Phase 4).
//
// Handlers read collector snapshots and call engine/sched/store interfaces
// only — no probing, no direct systemd/sysfs access (design decision 2,
// docs/v5-plan.md). Request bodies are validated to Contract 1 shapes with
// 422 field-level errors ({"error": "validation_failed", "fields": {...}} —
// Pydantic parity with forge/validators.py at the freeze commit).
//
// # Contract-amendment status (formerly the Phase 4 open questions)
//
// Resolved by the integrator (2026-07-22, see progress.md):
//
//   - C1-Q2 (RESOLVED for service-mode + TTS): the engine.Engine interface
//     gained StartUnit/StopUnit (aux-unit control over the D-Bus adapter,
//     no handler shell-out — design decision 2). POST /api/v1/tts/{start,
//     stop} and POST /api/v1/service-mode/<name>/{start,stop} are wired.
//     The Compressor half — POST /api/v1/compressor/{restart,proxy/teardown} —
//     is now wired (Phase 2, docs/v5-headroom-topology.md §5 option (a)):
//     Deps.CompressorProvisioner writes per-instance env files and
//     starts/stops/restarts the `headroom@<service>` template unit instance
//     (no unit-file authoring at runtime — that template + its polkit grant
//     is a one-time manual root install, see systemd/headroom@.service +
//     polkit/51-headroom.rules). nil
//     Deps.CompressorProvisioner (Phase 4 stub environment, most tests) keeps
//     handleCompressorLifecycle at 501.
//
//   - C1-Q3 (RESOLVED as WON'T-DO): GET /api/v1/modes is read-only; POST/
//     PUT/DELETE stay 501 by design. Modes live in the human-owned,
//     read-only config file (design decision 1) and the PWA never calls the
//     mutation routes.
//
//   - C1-Q5 (RESOLVED): PUT /api/v1/router/settings persists
//     `router.busy_mode` to store.Settings (V5 config is read-only; the a0
//     router reads the value from the settings KV). GET unchanged.
//
//   - C1-Q4 (RESOLVED): GET /api/v1/models/cards is backed by
//     internal/registry (catalog DB + GGUF metadata + on-disk weight size
//
//   - per-config history/reliability, all derived live). nil
//     Deps.Registry (Phase 4 stub environment) still returns an empty list.
//     Phase B also adds GET /api/v1/configs/cards (config-scoped).
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/fx"
	"github.com/jsaigou/the-forge/internal/compressorctl"
	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/profile"
	"github.com/jsaigou/the-forge/internal/providers"
	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/store"
)

// Deps are the frozen-interface dependencies (Contract 2). Phase 9 swaps
// stubs for real implementations here and nowhere else.
//
// The store.* sub-interfaces are optional (nil-able) because Phase 3 has
// not shipped at Phase 4 time. Handlers that need them return empty or
// default responses when nil; Phase 9 wires the real store.Open() result
// into these fields and the same handlers start returning live data.
type Deps struct {
	Snapshots collector.Source
	Engine    engine.Engine
	Sched     sched.Scheduler
	Auth      authz.Authenticator
	Events    bus.Subscriber
	Publish   bus.Publisher
	Config    func() *config.Config
	Hostname  string
	Version   string

	// AuthSetup is the account-creation/session surface for the
	// server-rendered POST /login and POST /setup handlers. nil (Phase 4
	// stub environment, and any test not exercising real auth) makes both
	// routes answer 503 rather than panic.
	AuthSetup authz.LoginService

	// Sprint 0-AUTH (§3.2): NetworkIdentityProvider resolves trusted network
	// principals (e.g. tailnet WhoIs). nil = network bootstrap disabled
	// (all requests must authenticate via session/bearer).
	NetworkIdentity authz.NetworkIdentityProvider

	// Sprint 0-AUTH (§3.3): identity-link store for network↔account linking.
	// nil = identity-link routes return 503.
	IdentityLinks store.IdentityLinks

	// Sprint 0-AUTH (§6): TOTP secret store. nil = TOTP routes return 503.
	TOTPStore store.TOTP

	// Sprint 0-AUTH (§3.4): policy matrix store. nil = policy routes return
	// 503, and requireAssurance is a no-op (passes through).
	PolicyStore *authz.PolicyStore

	// Sprint 0-AUTH (§3.5): step-up TTL. Zero = default 15 minutes.
	StepUpTTL time.Duration

	// Sprint 0-AUTH (§3.3): role for anonymous network sessions (unlinked
	// tailnet principals). Zero = RoleViewer.
	NetworkDefaultRole authz.Role

	// StepUpVerifier verifies password factors for step-up. nil = step-up
	// route returns 503. This is *authz.Authorizer in production.
	StepUpVerifier authz.StepUpVerifier

	// KeyManager mints + revokes bearer keys (§6 API-key management).
	// nil = key routes return 503. This is *authz.Authorizer in production.
	KeyManager authz.KeyManager

	// Keys is the bearer-key store (for listing masked keys). nil = key
	// list route returns 503.
	Keys store.Keys

	// Sprint 0-AUTH Phase B (§7): WebAuthn/passkey service. nil = WebAuthn
	// routes return 503.
	WebAuthnService *authz.WebAuthnService

	// Sprint 0-AUTH Phase C (§8): recovery code service. nil = recovery
	// code routes return 503.
	RecoveryService *authz.RecoveryService

	// Optional store sub-interfaces. nil = not yet wired (Phase 4 with
	// stubs). Handlers must nil-check before use.
	Usage    store.Usage
	Routing  store.Routing
	Settings store.Settings
	Audit    store.Audit
	Sessions store.Sessions

	// CompressorProvisioner drives the compressor proxy lifecycle (Phase 2,
	// docs/v5-headroom-topology.md §5; generalized Sprint 3,
	// docs/v5-headroom-replacement.md): env-file write + systemd
	// start/stop/restart of a `forge-compress@<service>` template-unit
	// instance. nil = restart/teardown/provider-link-triggered provisioning
	// are no-ops (handleCompressorLifecycle stays 501; SaveProvider's
	// link/unlink hook skips provisioning entirely) — the Phase 4 stub
	// environment and most tests.
	//
	// Sprint 7 (docs/v5-headroom-replacement.md) dropped the second
	// Provisioner value this field used to coexist with (a
	// headroom-ai-shaped "headroom@" template, from before the Sprint 3
	// cutover) once Sprint 6 confirmed zero live compressor_proxies rows
	// still pointed at it — provisionerFor's dual-template dispatch is gone
	// along with it.
	CompressorProvisioner *compressorctl.Provisioner

	// Providers is the per-provider clients + read-side assembly surface
	// (Sprint 0 §0.3, BE-3). nil = not yet wired; handleProvidersList
	// returns an empty providers list (the frozen shape requires an
	// array — the FE renders an empty state cleanly).
	Providers providers.Service

	// Metrics is the metric_samples time-series surface (Sprint 0 §0.4,
	// BE-1). nil = not yet wired (Phase 4 stub environment, and most unit
	// tests): handleMetricsHistory/handleMetricsExport answer empty results
	// instead of panicking, and startMetricsSampling no-ops.
	Metrics store.Metrics

	// Compressors is the compressor_samples surface (Sprint 4, resource
	// bounding + monitoring, docs/v5-headroom-replacement.md). nil = not yet
	// wired: startCompressorSampling no-ops, and compressorServiceRows/
	// runCompressorHealth read no health signal (compressor_resource_health
	// stays "unknown").
	Compressors store.Compressors

	// SlotStateStore is the slot_state crash-recovery table (store.Sched's
	// SaveSlot/Slots — restart-recovery only, not the live status source;
	// see docs/modes.md's killLingering writeup, 2026-07-29). nil = not yet
	// wired (Phase 4 stub environment, and most unit tests):
	// startSlotStateSync no-ops.
	SlotStateStore store.Sched

	// Registry is the model-registry surface (C1-Q4). nil = not yet wired;
	// handleModelCards returns an empty list and handleUsage skips local
	// cost computation.
	Registry registry.Registry

	// FX is the daemon-side billing-currency surface (Sprint 0 §0.2). nil =
	// not yet wired (Phase 4 stub or tests); handleUsage treats every
	// currency as USD 1:1 and reports fx_as_of=null / fx_stale=false.
	FX fx.Source

	// Profiles is the model-profiling + benchmark surface (PROFILE track,
	// docs/v5-profiling-benchmarks.md). nil = not yet wired; profile routes
	// return 503 and the fit check falls back to the registry/weight estimate.
	Profiles *profile.Runner

	// Prober optionally triggers an immediate collector poll. When non-nil,
	// lifecycle-heavy operations (profile runs) call it to force a fresh
	// snapshot + status_update push so the Dashboard/Console reflect slot
	// state changes immediately rather than waiting for the next collector poll
	// cadence. This is *collector.Collector in production; nil in tests.
	Prober func(ctx context.Context)

	// Catalog is the model-catalog store surface (MODEL CATALOG sprint Phase 3).
	// nil = not yet wired; catalog CRUD handlers return 503, read handlers
	// return empty lists.
	Catalog store.Catalog

	// Llama is a stateless scraper client used for the Compressor time-saved
	// estimate's last-resort live-TPS fallback (cost/savings sprint Phase 3,
	// 2026-07-30) — a fresh 3s-timeout HTTP call per request, not cached;
	// cheap enough to construct with collector.NewLlamaClient(nil) even when
	// unused. nil = that fallback step is skipped (falls through to
	// "unavailable" rather than panicking).
	Llama *collector.LlamaClient

	// PrefillStats is the durable, passively-observed per-mode real prefill
	// throughput surface (Compressor local-savings prefill sprint, 2026-08-06
	// — see migrations/0031_model_prefill_stats.sql). Primary source in
	// lookupPrefillTPS's fallback chain: real measured data from ordinary
	// traffic, not a destructive profiling run. nil = that step is skipped.
	PrefillStats store.PrefillStats

	// InvalidateConfig clears the merged-config cache so the engine/
	// collector/registry pick up store-backed catalog changes immediately
	// (the 5s TTL otherwise delays visibility). Called after any catalog
	// mutation. nil = no cache (stub environment or tests).
	InvalidateConfig func()

	// Notifications is the persisted, deduplicated alert surface (product/
	// QA sprint, 2026-07-29 — Dashboard notifications panel). nil = not
	// yet wired (Phase 4 stub environment, and most unit tests):
	// startNotificationSync no-ops and the list route returns an empty list.
	Notifications store.Notifications

	// Favorites is the per-operator starred-config-card surface (product/QA
	// sprint, 2026-07-29 — Console "Choose a config" starring). nil = not
	// yet wired; favorites routes return an empty list / no-op.
	Favorites store.Favorites

	// SchedulerJobs is the cron-style forced-load job surface (P3 track,
	// migration 0066). nil = not yet wired; scheduler-jobs routes return
	// empty lists / 503 on mutations.
	SchedulerJobs store.SchedulerJobs

	// Smith is the self-diagnosis agent (docs/v5-smith.md). P1 is the
	// deterministic tier: GET /api/v1/smith/status, POST .../checks/run,
	// GET .../findings. nil = smith routes return 503 (Phase 4 stub
	// environment, and most unit tests).
	Smith *smith.Smith

	// Maintenance is the system-wide quiet-host gate (go/internal/
	// maintenance) — the autonomous-remediation plan's Sprint 1. The real
	// enforcement is the wrapped Engine value main.go hands to every
	// subsystem; this field is only what the maintenance API handlers need
	// to read/enter/exit a window. nil = maintenance routes return 503
	// (Phase 4 stub environment, and most unit tests) — every load/unload
	// path behaves exactly as before an unwired daemon.
	Maintenance *maintenance.Gate

	// ReloadConfig re-reads the store-backed infra config (mirrors the
	// SIGHUP path, cmd/forge/main.go's reloadConfigFromStore +
	// mergedProvider.Invalidate) so a settings write takes effect
	// immediately rather than needing a real signal. nil = a cost-settings
	// write persists but the running daemon only picks it up on its next
	// SIGHUP or restart (Phase 4 stub environment, and most unit tests).
	ReloadConfig func()

	// SystemRestart issues a real systemd restart of forge-daemon.service
	// itself via the shared D-Bus connection (Sprint 12, was H, Phase 3 —
	// engine.DBus.Restart, wired as a closure so httpapi doesn't import
	// internal/engine's DBus type directly). nil = POST /api/v1/system/restart
	// returns 503 (Phase 4 stub environment, and every unit test — none of
	// them should ever actually restart a process).
	SystemRestart func(ctx context.Context) error
}

// Server is the dashboard HTTP server. The zero value is not usable —
// callers must construct via New.
type Server struct {
	deps Deps

	// In-process transition state, mirroring V4's _switch_state /
	// _slot_loading / _slot_unloading. The collector publishes what it
	// observes on its cadence; this state overlays the snapshot so a
	// status_update immediately after POST /api/v1/load reflects the
	// load-in-progress rather than waiting up to PollIntervalS for the
	// collector to notice. See _build_status() in forge/app.py.
	mu            sync.Mutex
	switchState   switchState
	slotLoading   map[string]slotTransition
	slotUnloading map[string]slotTransition
	heartbeatStop chan struct{}
	heartbeatOnce sync.Once

	// metricsSamplerOnce guards startMetricsSampling (metrics_handlers.go,
	// Sprint 0 §0.4) against double-start — Handler() may be called more
	// than once per Server in tests, same pattern as heartbeatOnce.
	metricsSamplerOnce sync.Once

	// slotStateSyncOnce guards startSlotStateSync against double-start,
	// same pattern as metricsSamplerOnce.
	slotStateSyncOnce sync.Once

	// compressorSamplerOnce guards startCompressorSampling (Sprint 4)
	// against double-start, same pattern as metricsSamplerOnce.
	compressorSamplerOnce sync.Once

	// notifySyncState guards startNotificationSync (notifications_handlers.go)
	// and tracks which alert dedupe keys were active as of the previous
	// sync tick (process-lifetime only — see syncNotificationsOnce).
	notifySyncState

	// bgCtx is the parent context for switch/load/unload background
	// goroutines. It must NOT be r.Context(): net/http cancels a request's
	// context as soon as its handler returns (see the Handler doc for
	// http.Request.Context()), which for these handlers is immediately
	// after spawning the goroutine that is supposed to keep running for
	// the minutes a real model load/switch takes. Using r.Context() here
	// made every load/switch/unload fail near-instantly with
	// context.Canceled once the triggering request completed — caught by
	// live-verifying a real load against ForgeHost's D-Bus, not by the
	// httptest-based unit tests (whose synthetic requests are never
	// canceled). bgCtx lives for the Server's lifetime and is canceled
	// only by Close(), so an in-flight operation is aborted on shutdown,
	// not on request completion.
	bgCtx    context.Context
	bgCancel context.CancelFunc

	// systemPreflightFn backs the Danger Zone's real validate-before-save
	// check for PUT/POST /api/v1/system/settings+/preflight (Sprint 12
	// Phase 3, preflight.go's preflightSystem). Wired to s.preflightSystem
	// in New() below — a plain method value, not a Deps field, since it
	// closes over *Server itself (comparing a candidate address against
	// what's already live needs s.resolvedSystemSettings).
	systemPreflightFn func(ctx context.Context, cand systemSettingsResponse) (checks []preflightCheck, failFields map[string]string)
}

// switchState mirrors V4's _switch_state dict.
type switchState struct {
	inProgress bool
	target     string
	startedAt  time.Time // zero = not started
	lastResult *lifecycleResult
}

// slotTransition mirrors V4's _slot_loading / _slot_unloading entries.
type slotTransition struct {
	inProgress bool
	mode       string
	startedAt  time.Time
}

// lifecycleResult mirrors V4's {"success": bool, "message": str} dicts
// (engine.Result has the same shape but is a different type).
type lifecycleResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	NCtx    int    `json:"n_ctx,omitempty"`
}

// New builds a Server on its dependencies.
func New(deps Deps) *Server {
	bgCtx, bgCancel := context.WithCancel(context.Background())
	s := &Server{
		deps:          deps,
		slotLoading:   map[string]slotTransition{},
		slotUnloading: map[string]slotTransition{},
		heartbeatStop: make(chan struct{}),
		bgCtx:         bgCtx,
		bgCancel:      bgCancel,
	}
	if deps.Engine != nil {
		for _, slot := range deps.Engine.Slots() {
			s.slotLoading[slot] = slotTransition{}
			s.slotUnloading[slot] = slotTransition{}
		}
	}
	s.systemPreflightFn = s.preflightSystem
	return s
}

// Close stops background goroutines (heartbeat). Idempotent.
func (s *Server) Close() error {
	s.heartbeatOnce.Do(func() { close(s.heartbeatStop) })
	s.bgCancel()
	return nil
}

// Handler returns the routing table for the dashboard listener. It mounts
// the Contract 1 surface under /api/v1/, the SSE endpoint at
// /api/v1/events, and the PWA shell at the Vite-published paths.
//
// Auth is enforced by middleware wrapping the v1 mux: every /api/v1/*
// route except /api/v1/health requires an authenticated Identity (session
// cookie or sk-forge-* bearer). RBAC role requirements are enforced
// per-route via the requireRole wrapper.
func (s *Server) Handler() http.Handler {
	s.startHeartbeat()
	s.startMetricsSampling()
	s.startSlotStateSync()
	s.startCompressorSampling()
	s.startNotificationSync()

	mux := http.NewServeMux()

	// Liveness — no auth, no logging (gunicorn-style readiness probe).
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)

	// Auth + CSRF gate the rest of the v1 surface.
	v1 := http.NewServeMux()
	s.registerV1Routes(v1)
	mux.Handle("/api/v1/", s.withAuth(v1))

	// Login / logout / setup are server-rendered (not part of the PWA
	// bundle — Contract 1 §1). PWA shell routes are served from embed.FS.
	s.registerPWARoutes(mux)

	return withGzip(s.withSecurityHeaders(mux))
}

// registerV1Routes wires every Contract 1 §2 endpoint behind auth.
func (s *Server) registerV1Routes(mux *http.ServeMux) {
	// Bootstrap / identity.
	mux.HandleFunc("GET /api/v1/session", s.handleSession)

	// Snapshot readers (Contract 1 §2 #3–9, 16, 22).
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/v1/scheduler/status", s.handleSchedulerStatus)
	mux.HandleFunc("GET /api/v1/infra-services", s.handleInfraServices)
	mux.HandleFunc("GET /api/v1/usage", s.handleUsage)
	mux.HandleFunc("GET /api/v1/usage/events", s.handleUsageEvents)
	mux.HandleFunc("GET /api/v1/usage/heatmap", s.handleUsageHeatmap)
	mux.HandleFunc("GET /api/v1/models/cards", s.handleModelCards)
	mux.HandleFunc("GET /api/v1/configs/cards", s.handleConfigCards)
	// Sprint 12 (was H) Phase 1: these two were bare mux.HandleFunc — any
	// authenticated identity, including an anonymous L0 network viewer,
	// could read scheduler + router config with no role gate at all. Every
	// other Settings-adjacent GET (cost/settings, billing/settings,
	// providers, compressor/config) requires at least operator; these two had
	// no reason to be the exception. Gated to match, with page.settings
	// assurance to match cost/billing's pattern.
	mux.Handle("GET /api/v1/scheduler/config", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleSchedulerConfigGet))))
	mux.Handle("GET /api/v1/router/settings", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleRouterSettingsGet))))
	// Routing preview (Phase 7): read-only, simulates resolution — same
	// operator+page.settings gate as the settings it previews.
	mux.Handle("GET /api/v1/routing/preview", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleRoutingPreview))))

	// Notifications (product/QA sprint, 2026-07-29). List is readable by any
	// authenticated role (same tier as status/usage); ack/dismiss mutate, so
	// operator+ like the other lifecycle-adjacent mutations.
	mux.HandleFunc("GET /api/v1/notifications", s.handleNotificationsList)
	mux.Handle("POST /api/v1/notifications/{id}/ack", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleNotificationAck)))
	mux.Handle("POST /api/v1/notifications/{id}/dismiss", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleNotificationDismiss)))
	mux.Handle("POST /api/v1/notifications/ack-all", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleNotificationAckAll)))

	// Favorites (product/QA sprint, 2026-07-29) — personal preference, not
	// shared system state, so gated on authentication alone (see
	// favorites_handlers.go's file doc comment).
	mux.HandleFunc("GET /api/v1/favorites", s.handleFavoritesList)
	mux.HandleFunc("PUT /api/v1/favorites/{subject_type}/{id}", s.handleFavoriteAdd)
	mux.HandleFunc("DELETE /api/v1/favorites/{subject_type}/{id}", s.handleFavoriteRemove)

	// Smith — self-diagnosis (docs/v5-smith.md §5). P1 deterministic tier:
	// reads + check runs, all operator-gated. The reasoning tier (chat) and
	// the action/approval mutations land in later phases.
	mux.Handle("GET /api/v1/smith/status", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithStatus)))
	mux.Handle("POST /api/v1/smith/checks/run", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithChecksRun)))
	mux.Handle("GET /api/v1/smith/findings", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithFindings)))
	mux.Handle("POST /api/v1/smith/findings/purge", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithFindingsPurge)))
	// Smith investigations (Wave 2 — docs/v5-smith-wave2.md §3): list,
	// manual open, detail (with findings), run-more-checks, resolve/dismiss,
	// and the check catalog for the FE picker.
	mux.Handle("GET /api/v1/smith/investigations", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithInvestigationsList)))
	mux.Handle("POST /api/v1/smith/investigations", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithInvestigationCreate)))
	mux.Handle("GET /api/v1/smith/investigations/{id}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithInvestigationDetail)))
	mux.Handle("POST /api/v1/smith/investigations/{id}/checks", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithInvestigationChecks)))
	mux.Handle("PATCH /api/v1/smith/investigations/{id}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithInvestigationResolve)))
	mux.Handle("GET /api/v1/smith/checks", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithChecksList)))
	// Smith actions (P2 — docs/v5-smith.md §4.6): the propose/approve/
	// execute/verify action model + the self-eviction handoff flow. create
	// and approve carry the step-up gate (action.smith.execute) — create
	// because the payload it plants is what a subsequent approve executes
	// (a confused-deputy path at low assurance), approve because it's what
	// actually triggers execution. reject and handoff are ungated: reject is
	// always the safe direction, and handoff resolution short of approval
	// carries no privilege beyond what create already required.
	mux.Handle("GET /api/v1/smith/actions", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithActionsList)))
	mux.Handle("POST /api/v1/smith/actions",
		s.requireRole(authz.RoleOperator)(
			s.requireAssurance(authz.ResourceActionSmithExecute)(
				http.HandlerFunc(s.handleSmithActionCreate))))
	mux.Handle("GET /api/v1/smith/actions/{id}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithActionDetail)))
	mux.Handle("POST /api/v1/smith/actions/{id}/approve",
		s.requireRole(authz.RoleOperator)(
			s.requireAssurance(authz.ResourceActionSmithExecute)(
				http.HandlerFunc(s.handleSmithActionApprove))))
	mux.Handle("POST /api/v1/smith/actions/{id}/reject", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithActionReject)))
	mux.Handle("POST /api/v1/smith/actions/{id}/handoff", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithActionHandoff)))
	mux.Handle("POST /api/v1/smith/actions/{id}/recheck", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithActionRecheck)))
	// Procedure runs (autonomous-remediation Sprint 2 — go/internal/smith's
	// procedure.go): read-side is role-only like the action detail route it
	// mirrors; checkpoint-approve carries the same step-up as approve (it's
	// what lets execution continue), checkpoint-abort is role-only like
	// reject (always the safe direction).
	mux.Handle("GET /api/v1/smith/actions/{id}/procedure", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithProcedureRun)))
	mux.Handle("POST /api/v1/smith/actions/{id}/procedure/checkpoint/approve",
		s.requireRole(authz.RoleOperator)(
			s.requireAssurance(authz.ResourceActionSmithExecute)(
				http.HandlerFunc(s.handleSmithProcedureCheckpointApprove))))
	mux.Handle("POST /api/v1/smith/actions/{id}/procedure/checkpoint/abort",
		s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithProcedureCheckpointAbort)))
	// "Let smith fix it" (autonomous-remediation Sprint 3, docs/v5-smith.md
	// §13): converts a pending atomic action into its equivalent procedure.
	// procedure_preview is a read (no step-up, same posture as the action
	// detail route it feeds — the downtime-disclosure modal); procedurize
	// carries the same step-up gate as approve, since it IS an approve
	// (of the newly-created procedure action) under the hood.
	mux.Handle("GET /api/v1/smith/actions/{id}/procedure_preview", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithActionProcedurePreview)))
	mux.Handle("POST /api/v1/smith/actions/{id}/procedurize",
		s.requireRole(authz.RoleOperator)(
			s.requireAssurance(authz.ResourceActionSmithExecute)(
				http.HandlerFunc(s.handleSmithActionProcedurize))))
	// Supervision & evaluation harness (autonomous-remediation Sprint 4,
	// docs/v5-smith.md §13): a browsable run history plus a per-run
	// scorecard (unattended completion, checkpoints reached, post-verify,
	// downtime estimate vs. actual) — both reads, role-only like every
	// other procedure-run route above.
	mux.Handle("GET /api/v1/smith/procedures/runs", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithProcedureRunsList)))
	mux.Handle("GET /api/v1/smith/actions/{id}/procedure/scorecard", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithProcedureScorecard)))
	// Standing autonomy policy (autonomous-remediation Sprint 5,
	// docs/v5-smith.md §13.3): opt-in per-procedure, default off, gated by a
	// global kill switch. PUT evaluates action.smith.autonomy step-up itself
	// (role-only here) because the gate is conditional on whether the
	// request body escalates trust — see smith_autonomy_handlers.go's file
	// doc comment for why this can't be a route-level requireAssurance wrap.
	mux.Handle("GET /api/v1/smith/autonomy", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithAutonomyGet)))
	mux.Handle("PUT /api/v1/smith/autonomy", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithAutonomyPut)))
	// Maintenance (autonomous-remediation plan, Sprint 1 — go/internal/
	// maintenance): the system-wide quiet-host gate. Not under /smith/
	// because it's a general operational gate the operator can also drive
	// by hand, independent of any smith-proposed repair. POST/DELETE carry
	// the same step-up gate as the smith action model — entering/exiting a
	// window is exactly as consequential as approving a state-changing
	// action. GET is an unauthenticated-beyond-role status read.
	mux.Handle("GET /api/v1/maintenance", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleMaintenanceGet)))
	mux.Handle("POST /api/v1/maintenance",
		s.requireRole(authz.RoleOperator)(
			s.requireAssurance(authz.ResourceActionSmithExecute)(
				http.HandlerFunc(s.handleMaintenanceEnter))))
	mux.Handle("DELETE /api/v1/maintenance",
		s.requireRole(authz.RoleOperator)(
			s.requireAssurance(authz.ResourceActionSmithExecute)(
				http.HandlerFunc(s.handleMaintenanceExit))))
	// Smith P3 — reasoning tier (docs/v5-smith.md §5): conversations, the
	// chat turn endpoint, settings, and investigation Tier 2 commentary.
	// No requireAssurance beyond operator role — smith.model is explicitly
	// "freely changeable" (§4.3 decision-log item 2); the mutations chat can
	// still only *propose* go through the existing ResourceActionSmithExecute
	// gate on POST/approve /actions above.
	mux.Handle("GET /api/v1/smith/conversations", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithConversationsList)))
	mux.Handle("POST /api/v1/smith/conversations", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithConversationCreate)))
	mux.Handle("GET /api/v1/smith/conversations/{id}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithConversationDetail)))
	mux.Handle("DELETE /api/v1/smith/conversations/{id}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithConversationDelete)))
	mux.Handle("POST /api/v1/smith/chat", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithChat)))
	mux.Handle("GET /api/v1/smith/settings", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithSettingsGet)))
	mux.Handle("PUT /api/v1/smith/settings", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithSettingsPut)))
	mux.Handle("POST /api/v1/smith/investigations/{id}/analyze", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithInvestigationAnalyze)))
	// Smith P4 — knowledge base (docs/v5-smith.md §4.7/§5): the embedded doc
	// corpus + live-DB evidence search, one KBRef's chunk, and the parsed
	// docs/investigations.md "externally blocked work" list. All read-only,
	// no requireAssurance beyond operator role (same posture as the P3
	// routes above — nothing here mutates state). /kb/search and /kb/blocked
	// are registered alongside /kb/{ref}; net/http's ServeMux gives literal
	// segments precedence over a wildcard, so "search"/"blocked" never fall
	// through to the {ref} handler regardless of registration order.
	mux.Handle("GET /api/v1/smith/kb/search", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithKBSearch)))
	mux.Handle("GET /api/v1/smith/kb/blocked", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithKBBlocked)))
	mux.Handle("GET /api/v1/smith/kb/{ref}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithKBRef)))
	// Smith P5 — web research (docs/v5-smith.md §4.8): an explicit re-probe
	// of provider reachability. No requireAssurance: this is a read-only
	// external GET/POST against searxng/firecrawl plus updating an
	// in-memory status map — no state change beyond that, same posture as
	// the reads above it.
	mux.Handle("POST /api/v1/smith/web/probe", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithWebProbe)))
	// Smith P6 — domain modules (docs/v5-smith.md §4.9). FR6/FR7 (binary
	// tracking, ComfyUI pruning) are ordinary checks + a delete_files
	// action, no dedicated routes; FR4 (model sourcing) is a standalone
	// read-only research call, same no-requireAssurance posture as
	// /web/probe above (real external fetches, no state mutation).
	mux.Handle("POST /api/v1/smith/sourcing/evaluate", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSmithSourcingEvaluate)))

	// Sprint 0 frozen stubs — registered so the shapes are stable and the
	// parallel matrix builds against them; every handler is a 501 against a
	// frozen shape until its owning track (BE-1/2/3/5) lands.
	// §0.4 metrics history/export (BE-1).
	mux.HandleFunc("GET /api/v1/metrics/history", s.handleMetricsHistory)
	mux.HandleFunc("GET /api/v1/metrics/export", s.handleMetricsExport)
	// Cost/savings sprint (2026-07-30): measured-electricity endpoints.
	// summary/energy-history are read-only informational (operator);
	// settings PUT is the same admin+ResourcePageSettings gate as billing.
	mux.Handle("GET /api/v1/cost/summary", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleCostSummary)))
	mux.Handle("GET /api/v1/cost/energy-history", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleCostEnergyHistory)))
	mux.Handle("GET /api/v1/cost/settings", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCostSettingsGet))))
	mux.Handle("PUT /api/v1/cost/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCostSettingsPut))))
	mux.Handle("GET /api/v1/compressor/summary", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleCompressorSummary)))

	// Sprint 12 (was H) Phase 2: typed GET/PUT for infra.*/metrics.*/ui.*
	// groups that previously had no HTTP surface at all — see
	// infra_handlers.go's doc comment. Same page.settings gate every other
	// Settings mutation uses, except system/settings (the Danger Zone
	// group), which is admin + area.settings.system.
	mux.Handle("GET /api/v1/router/config", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleRouterConfigGet))))
	mux.Handle("PUT /api/v1/router/config", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleRouterConfigPut))))
	mux.Handle("GET /api/v1/monitor/settings", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleMonitorSettingsGet))))
	mux.Handle("PUT /api/v1/monitor/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleMonitorSettingsPut))))
	mux.Handle("GET /api/v1/metrics/settings", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleMetricsSettingsGet))))
	mux.Handle("PUT /api/v1/metrics/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleMetricsSettingsPut))))
	mux.Handle("GET /api/v1/ui/settings", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleUISettingsGet))))
	mux.Handle("PUT /api/v1/ui/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleUISettingsPut))))
	// ADR-0011: custom dashboard pages — system-wide settings key
	// "dashboard.pages", full-replace PUT (frontend sends complete layout).
	mux.Handle("GET /api/v1/dashboard/layout", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleDashboardLayoutGet))))
	mux.Handle("PUT /api/v1/dashboard/layout", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleDashboardLayoutPut))))
	// Sprint 12 Phase 6: infra.scheduler (the boot seed sched.Config falls
	// back to when scheduler.config is unset) — same page.settings gate as
	// its peers above; apply="seed" so no ReloadConfig/restart marker.
	mux.Handle("GET /api/v1/scheduler/seed", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleSchedulerSeedGet))))
	mux.Handle("PUT /api/v1/scheduler/seed", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleSchedulerSeedPut))))
	mux.Handle("GET /api/v1/system/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSystem)(http.HandlerFunc(s.handleSystemSettingsGet))))
	mux.Handle("PUT /api/v1/system/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSystem)(http.HandlerFunc(s.handleSystemSettingsPut))))
	// Sprint 12 Phase 3: the Danger Zone's dry-run checklist + the daemon
	// restart action. Both admin; restart carries its own resource
	// (action.system.restart) distinct from area.settings.system, since
	// "can edit boot-critical config" and "can restart the process" are
	// separable trust decisions even though the UI puts them side by side.
	mux.Handle("POST /api/v1/system/preflight", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSystem)(http.HandlerFunc(s.handleSystemPreflightPost))))
	mux.Handle("POST /api/v1/system/restart", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceActionSystemRestart)(http.HandlerFunc(s.handleSystemRestart))))
	// §0.3 providers read (BE-3, operator).
	mux.Handle("GET /api/v1/providers", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleProvidersList)))
	// §0.9 provider CRUD (BE-3, admin; mutating → CSRF-checked by withAuth).
	// Sprint 0-AUTH: gated by area.settings.provider_keys assurance.
	mux.Handle("POST /api/v1/providers", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsProviderK)(http.HandlerFunc(s.handleProviderCreate))))
	mux.Handle("PUT /api/v1/providers/{ref}", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsProviderK)(http.HandlerFunc(s.handleProviderUpdate))))
	mux.Handle("PUT /api/v1/providers/{ref}/key", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsProviderK)(http.HandlerFunc(s.handleProviderKey))))
	mux.Handle("DELETE /api/v1/providers/{ref}", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsProviderK)(http.HandlerFunc(s.handleProviderDelete))))
	// Billing-API auto-discovery (product/QA sprint, 2026-07-29).
	mux.Handle("POST /api/v1/providers/{ref}/discover-billing", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsProviderK)(http.HandlerFunc(s.handleProviderDiscoverBilling))))
	// §0.9 billing settings (BE-2/BE-5).
	mux.Handle("GET /api/v1/billing/settings", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleBillingSettingsGet))))
	mux.Handle("PUT /api/v1/billing/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleBillingSettingsPut))))

	// Sprint 0-AUTH frozen stubs (docs/v5-sprint0-auth-design.md §6). Every
	// route is a 501 against the frozen shape in shapes.go; BE-AUTH Phase
	// A/B/C fills them. Session/step-up + TOTP + WebAuthn self-service are
	// role-agnostic (any authenticated identity); policy/config + key mgmt +
	// identity-link writes are admin.
	mux.HandleFunc("POST /api/v1/auth/step-up", s.handleAuthStepUp)

	mux.HandleFunc("POST /api/v1/auth/webauthn/register/begin", s.handleWebAuthnRegisterBegin)
	mux.HandleFunc("POST /api/v1/auth/webauthn/register/finish", s.handleWebAuthnRegisterFinish)
	mux.HandleFunc("POST /api/v1/auth/webauthn/assert/begin", s.handleWebAuthnAssertBegin)
	mux.HandleFunc("POST /api/v1/auth/webauthn/assert/finish", s.handleWebAuthnAssertFinish)
	mux.HandleFunc("GET /api/v1/auth/webauthn/credentials", s.handleWebAuthnCredentialsList)
	mux.HandleFunc("DELETE /api/v1/auth/webauthn/credentials/{id}", s.handleWebAuthnCredentialDelete)

	mux.HandleFunc("POST /api/v1/auth/totp/enroll", s.handleTOTPEnroll)
	mux.HandleFunc("POST /api/v1/auth/totp/confirm", s.handleTOTPConfirm)
	mux.HandleFunc("DELETE /api/v1/auth/totp", s.handleTOTPDelete)

	// Recovery codes (Phase C, §8): self-service — any authenticated session
	// can manage their own recovery codes. Generation requires password
	// assurance (area.settings.security) to prevent drive-by code generation.
	mux.HandleFunc("GET /api/v1/auth/recovery-codes", s.handleRecoveryCodesStatus)
	mux.Handle("POST /api/v1/auth/recovery-codes/generate", s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleRecoveryCodesGenerate)))

	mux.HandleFunc("GET /api/v1/auth/identity-links", s.handleIdentityLinksList)
	mux.Handle("POST /api/v1/auth/identity-links", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleIdentityLinkCreate))))
	mux.Handle("DELETE /api/v1/auth/identity-links/{provider}/{principal}", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleIdentityLinkDelete))))

	mux.Handle("GET /api/v1/keys", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleKeysList))))
	mux.Handle("POST /api/v1/keys", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleKeyCreate))))
	mux.Handle("DELETE /api/v1/keys/{keyid}", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleKeyRevoke))))

	// Sprint C — audit_log's first read surface (see audit_handlers.go).
	// Gated the same as the catalog mutation routes it's mostly read
	// alongside (ResourcePageSettings), not ResourceAreaSettingsSecurity —
	// this is catalog change history, not a security-relevant credential.
	mux.Handle("GET /api/v1/audit", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleAuditList))))

	mux.Handle("GET /api/v1/auth/policy", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleAuthPolicyGet))))
	mux.Handle("PUT /api/v1/auth/policy", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleAuthPolicyPut))))
	mux.Handle("GET /api/v1/auth/config", s.requireRole(authz.RoleOperator)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleAuthConfigGet))))
	mux.Handle("PUT /api/v1/auth/config", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourceAreaSettingsSecurity)(http.HandlerFunc(s.handleAuthConfigPut))))

	// Reservations (Contract 1 §2 #13–15 + PUT amendment).
	mux.HandleFunc("GET /api/v1/reservations", s.handleReservationsList)
	mux.Handle("POST /api/v1/reservations", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleReservationCreate)))
	mux.Handle("PUT /api/v1/reservations/{label}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleReservationUpdate)))
	mux.Handle("DELETE /api/v1/reservations/{label}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleReservationCancel)))

	// Scheduler jobs (P3 track — cron-style forced loads). List + run-now
	// are operator; create/update/delete are admin, matching the
	// reservation routes' role posture.
	mux.Handle("GET /api/v1/scheduler/jobs", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSchedulerJobsList)))
	mux.Handle("POST /api/v1/scheduler/jobs", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleSchedulerJobCreate)))
	mux.Handle("PUT /api/v1/scheduler/jobs/{id}", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleSchedulerJobUpdate)))
	mux.Handle("DELETE /api/v1/scheduler/jobs/{id}", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleSchedulerJobDelete)))
	mux.Handle("POST /api/v1/scheduler/jobs/{id}/run-now", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSchedulerJobRunNow)))

	// Scheduler config (Contract 1 §2 #17). Sprint 12 Phase 1: this was the
	// only Settings-page mutation with no requireAssurance gate at all —
	// every peer config PUT (cost, billing, providers) requires at least
	// page.settings. Gated to match. (web/src/components/SchedulerTunables.tsx
	// already carries useStepUpGate + StepUpModal as of Phase 0, specifically
	// so this change doesn't silently 403 the Scheduling page's save.)
	mux.Handle("PUT /api/v1/scheduler/config", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleSchedulerConfigPut))))

	// Lifecycle (Contract 1 §2 #10–12). Engine calls happen in goroutines;
	// events are published on the bus.
	mux.Handle("POST /api/v1/switch/{mode}", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleSwitch)))
	mux.Handle("POST /api/v1/load", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleLoad)))
	mux.Handle("POST /api/v1/unload", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleUnload)))

	// Compressor (Contract 1 §2 #18–21). Restart + teardown drive the
	// Compressor proxy *lifecycle* (systemd unit-file deploy/remove), a
	// subsystem not yet ported to Go — they stay 501 (see handleCompressorLifecycle
	// and the package doc); passthrough + config are wired.
	mux.Handle("GET /api/v1/compressor/config", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleCompressorConfig)))
	mux.Handle("POST /api/v1/compressor/restart", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleCompressorLifecycle)))
	mux.Handle("POST /api/v1/compressor/proxy/teardown", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleCompressorLifecycle)))
	mux.Handle("POST /api/v1/compressor/proxy/create", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleCompressorProxyCreate)))
	mux.Handle("POST /api/v1/compressor/proxy/migrate", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleCompressorMigrate)))
	mux.Handle("PUT /api/v1/compressor/passthrough", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleCompressorPassthrough)))

	// Service-mode + TTS (Contract 1 §2 #23–26). Wired via the C1-Q2
	// engine.StartUnit/StopUnit amendment (no handler shell-out).
	mux.Handle("POST /api/v1/service-mode/{name}/start", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleServiceMode)))
	mux.Handle("POST /api/v1/service-mode/{name}/stop", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleServiceMode)))
	mux.Handle("POST /api/v1/tts/start", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleTTS)))
	mux.Handle("POST /api/v1/tts/stop", s.requireRole(authz.RoleOperator)(http.HandlerFunc(s.handleTTS)))

	// Modes CRUD (Contract 1 §5). C1-Q3 resolved as WON'T-DO: modes live in
	// the read-only, human-owned config file (design decision 1) and the PWA
	// never calls these routes — mutations stay 501 by design.
	mux.HandleFunc("GET /api/v1/modes", s.handleModesList)
	mux.Handle("POST /api/v1/modes", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleNotImplemented)))
	mux.Handle("PUT /api/v1/modes/{name}", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleNotImplemented)))
	mux.Handle("DELETE /api/v1/modes/{name}", s.requireRole(authz.RoleAdmin)(http.HandlerFunc(s.handleNotImplemented)))

	// Router settings PUT (C1-Q5) — persists router.busy_mode to store.Settings.
	// Sprint 12 Phase 1: was requireRole(operator) while every peer config PUT
	// (scheduler/config, cost/settings, billing/settings) is admin — an
	// inconsistency, not a deliberate looser gate. Raised to admin + the same
	// page.settings assurance the peers use.
	mux.Handle("PUT /api/v1/router/settings", s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleRouterSettingsPut))))

	// Model catalog (MODEL CATALOG Phase 3). All mutations are admin + step-up
	// (page.settings). Read endpoints are role-agnostic (any authed viewer
	// may browse the catalog).
	mux.HandleFunc("GET /api/v1/models/files", s.handleModelFiles)

	// Genealogy + Family CRUD (product/QA sprint, 2026-07-29 — families
	// were list-only before this; genealogy is new, the level above family).
	mux.HandleFunc("GET /api/v1/catalog/genealogies", s.handleCatalogGenealogiesList)
	mux.HandleFunc("GET /api/v1/catalog/genealogies/{id}", s.handleCatalogGenealogyGet)
	mux.Handle("POST /api/v1/catalog/genealogies",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogGenealogyCreate))))
	mux.Handle("PUT /api/v1/catalog/genealogies/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogGenealogyUpdate))))
	mux.Handle("DELETE /api/v1/catalog/genealogies/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogGenealogyDelete))))
	mux.Handle("PUT /api/v1/catalog/genealogies/{id}/icon",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogGenealogyIcon))))

	mux.HandleFunc("GET /api/v1/catalog/families", s.handleCatalogFamiliesList)
	mux.HandleFunc("GET /api/v1/catalog/families/{id}", s.handleCatalogFamilyGet)
	mux.Handle("POST /api/v1/catalog/families",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogFamilyCreate))))
	mux.Handle("PUT /api/v1/catalog/families/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogFamilyUpdate))))
	mux.Handle("DELETE /api/v1/catalog/families/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogFamilyDelete))))
	mux.Handle("PUT /api/v1/catalog/families/{id}/icon",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogFamilyIcon))))
	mux.HandleFunc("GET /api/v1/catalog/quantizations", s.handleCatalogQuantizationsList)
	mux.HandleFunc("GET /api/v1/catalog/formats", s.handleCatalogFormatsList)
	mux.HandleFunc("GET /api/v1/catalog/engines", s.handleCatalogEnginesList)
	mux.HandleFunc("GET /api/v1/catalog/builds", s.handleCatalogBuildsList)
	mux.HandleFunc("GET /api/v1/catalog/artifacts", s.handleCatalogArtifactsList)

	mux.HandleFunc("GET /api/v1/catalog/models", s.handleCatalogModelsList)
	mux.HandleFunc("GET /api/v1/catalog/models/{id}", s.handleCatalogModelGet)
	mux.Handle("POST /api/v1/catalog/models",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogModelCreate))))
	mux.Handle("PUT /api/v1/catalog/models/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogModelUpdate))))
	mux.Handle("DELETE /api/v1/catalog/models/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogModelDelete))))
	mux.Handle("POST /api/v1/catalog/models/validate",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogModelValidate))))
	mux.Handle("PUT /api/v1/catalog/models/{id}/icon",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogModelIcon))))

	mux.HandleFunc("GET /api/v1/catalog/variants", s.handleCatalogVariantsList)
	mux.HandleFunc("GET /api/v1/catalog/variants/{id}", s.handleCatalogVariantGet)
	mux.Handle("POST /api/v1/catalog/variants",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogVariantCreate))))
	mux.Handle("PUT /api/v1/catalog/variants/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogVariantUpdate))))
	mux.Handle("DELETE /api/v1/catalog/variants/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogVariantDelete))))
	mux.Handle("POST /api/v1/catalog/variants/validate",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogVariantValidate))))

	mux.HandleFunc("GET /api/v1/catalog/configs", s.handleCatalogConfigsList)
	mux.HandleFunc("GET /api/v1/catalog/configs/{id}", s.handleCatalogConfigGet)
	mux.Handle("POST /api/v1/catalog/configs",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogConfigCreate))))
	mux.Handle("PUT /api/v1/catalog/configs/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogConfigUpdate))))
	mux.Handle("DELETE /api/v1/catalog/configs/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogConfigDelete))))
	mux.Handle("POST /api/v1/catalog/configs/validate",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogConfigValidate))))
	mux.Handle("PUT /api/v1/catalog/configs/{id}/icon",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogConfigIcon))))

	mux.HandleFunc("GET /api/v1/catalog/offerings", s.handleCatalogOfferingsList)
	mux.HandleFunc("GET /api/v1/catalog/offerings/{id}", s.handleCatalogOfferingGet)
	mux.Handle("POST /api/v1/catalog/offerings",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogOfferingCreate))))
	mux.Handle("PUT /api/v1/catalog/offerings/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogOfferingUpdate))))
	mux.Handle("DELETE /api/v1/catalog/offerings/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogOfferingDelete))))
	mux.Handle("POST /api/v1/catalog/offerings/validate",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogOfferingValidate))))

	mux.HandleFunc("GET /api/v1/catalog/benchmarks", s.handleCatalogBenchmarksList)
	mux.HandleFunc("GET /api/v1/catalog/benchmarks/{id}", s.handleCatalogBenchmarkGet)
	mux.Handle("POST /api/v1/catalog/benchmarks",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogBenchmarkCreate))))
	mux.Handle("PUT /api/v1/catalog/benchmarks/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogBenchmarkUpdate))))
	mux.Handle("DELETE /api/v1/catalog/benchmarks/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogBenchmarkDelete))))
	mux.Handle("POST /api/v1/catalog/benchmarks/validate",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogBenchmarkValidate))))

	mux.HandleFunc("GET /api/v1/catalog/notes", s.handleCatalogNotesList)
	mux.HandleFunc("GET /api/v1/catalog/notes/{id}", s.handleCatalogNoteGet)
	mux.Handle("POST /api/v1/catalog/notes",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogNoteCreate))))
	mux.Handle("PUT /api/v1/catalog/notes/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogNoteUpdate))))
	mux.Handle("DELETE /api/v1/catalog/notes/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogNoteDelete))))

	mux.HandleFunc("GET /api/v1/catalog/services", s.handleCatalogServicesList)
	mux.HandleFunc("GET /api/v1/catalog/services/{id}", s.handleCatalogServiceGet)
	mux.Handle("POST /api/v1/catalog/services",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogServiceCreate))))
	mux.Handle("PUT /api/v1/catalog/services/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogServiceUpdate))))
	mux.Handle("DELETE /api/v1/catalog/services/{id}",
		s.requireRole(authz.RoleAdmin)(s.requireAssurance(authz.ResourcePageSettings)(http.HandlerFunc(s.handleCatalogServiceDelete))))

	// SSE (Contract 1 §4) — authed, but role-agnostic (any authed viewer
	// may subscribe; payloads are no more privileged than GET /api/v1/status).
	mux.HandleFunc("GET /api/v1/events", s.handleSSE)

	// Profiling (PROFILE track — docs/v5-profiling-benchmarks.md §7).
	// POST /run is admin + step-up (destructive: evicts all A1–A4).
	mux.Handle("POST /api/v1/profile/run",
		s.requireRole(authz.RoleAdmin)(
			s.requireAssurance(authz.ResourceActionModelProfile)(
				http.HandlerFunc(s.handleProfileRun))))
	mux.HandleFunc("GET /api/v1/profile", s.handleProfilesList)
	mux.HandleFunc("GET /api/v1/profile/{mode}", s.handleProfileGet)
	mux.Handle("DELETE /api/v1/profile/{mode}",
		s.requireRole(authz.RoleAdmin)(
			s.requireAssurance(authz.ResourceActionModelProfile)(
				http.HandlerFunc(s.handleProfileDelete))))
}

// ── Middleware ───────────────────────────────────────────────────────────────

type contextKey string

const (
	identityKey contextKey = "identity"
	csrfKey     contextKey = "csrf"
	sessionKey  contextKey = "session" // store.Session (for step-up)
)

// withAuth resolves the caller Identity via session cookie or sk-forge-*
// bearer token, then enforces CSRF on mutations. Unauthenticated /api/v1/*
// requests get 401 (which makes the PWA redirect to /login?next=<path>).
//
// Sprint 0-AUTH (§3.2/§5): if no session/bearer is present, the middleware
// attempts a network-identity bootstrap. If the NetworkIdentityProvider
// resolves a trusted principal that is linked to a local account, a session
// is created at L0 (network). Unlinked principals get an anonymous L0
// identity with role NetworkDefaultRole (default viewer). Bearer-key paths
// skip the policy matrix entirely.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			ident authz.Identity
			sess  store.Session
			err   error
		)
		switch {
		case s.deps.Auth == nil:
			// No authenticator wired — fail closed. The stub in cmd/forge
			// always wires authz.StubAuthenticator, so this branch means
			// misconfiguration, not a normal request path.
			writeError(w, http.StatusUnauthorized, "Authentication required")
			return
		case bearerToken(r) != "":
			ident, err = s.deps.Auth.VerifyBearer(bearerToken(r), authz.KindForge)
		case sessionCookie(r) != "":
			ident, sess, err = s.resolveSession(r)
		default:
			// No session or bearer — try network bootstrap (§3.2/§5).
			ident, sess, err = s.bootstrapNetworkIdentity(w, r)
			if err != nil {
				// bootstrapNetworkIdentity already wrote the 401 or set
				// a cookie + identity. If err != nil here, it couldn't
				// resolve anything.
				return
			}
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "Authentication required")
			return
		}

		// CSRF: bearer-authed requests are immune. Session-authed
		// mutations must carry X-CSRF-Token matching the session's token.
		// When the store.Sessions dependency is nil (Phase 4 stub), the
		// CSRF check is skipped — the authz.StubAuthenticator is not
		// enforcing real sessions anyway, and Phase 9 wires the real
		// store before any cutover.
		if r.Method != "GET" && r.Method != "HEAD" && bearerToken(r) == "" {
			if !s.csrfOKForSession(r, sess) {
				writeError(w, http.StatusForbidden, "CSRF token missing or invalid")
				return
			}
		}

		ctx := context.WithValue(r.Context(), identityKey, ident)
		if sess.ID != "" {
			ctx = context.WithValue(ctx, sessionKey, sess)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// resolveSession verifies the session cookie and returns the Identity +
// Session (the Session carries the user ID + assurance for step-up).
func (s *Server) resolveSession(r *http.Request) (authz.Identity, store.Session, error) {
	sid := sessionCookie(r)
	if sid == "" {
		return authz.Identity{}, store.Session{}, authz.ErrUnauthenticated
	}
	// VerifySession returns the Identity (with assurance from the session row).
	ident, err := s.deps.Auth.VerifySession(sid)
	if err != nil {
		return authz.Identity{}, store.Session{}, err
	}
	// Fetch the session row for the user ID (needed by step-up + TOTP).
	// When Sessions is nil (stub environment), we proceed with an empty
	// session — the step-up/TOTP handlers will reject requests that need
	// a user ID.
	var sess store.Session
	if s.deps.Sessions != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		sess, err = s.deps.Sessions.Get(ctx, sid)
		if err != nil {
			return authz.Identity{}, store.Session{}, err
		}
	}
	return ident, sess, nil
}

// bootstrapNetworkIdentity resolves a trusted network principal and creates
// an L0 session for linked accounts, or an anonymous L0 identity for
// unlinked principals. Returns ErrUnauthenticated when no network identity
// can be resolved (the caller sends 401).
func (s *Server) bootstrapNetworkIdentity(w http.ResponseWriter, r *http.Request) (authz.Identity, store.Session, error) {
	if s.deps.NetworkIdentity == nil {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return authz.Identity{}, store.Session{}, authz.ErrUnauthenticated
	}
	principal, ok := s.deps.NetworkIdentity.Identify(r)
	if !ok || principal == "" {
		writeError(w, http.StatusUnauthorized, "Authentication required")
		return authz.Identity{}, store.Session{}, authz.ErrUnauthenticated
	}

	// Try to link the principal to a local account (§3.3).
	if s.deps.IdentityLinks != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		userID, err := s.deps.IdentityLinks.Lookup(ctx, s.deps.NetworkIdentity.Name(), principal)
		if err == nil && userID > 0 {
			return s.createNetworkSession(w, r, userID, principal)
		}
		// ErrNotFound → anonymous; other errors → fail closed.
		if !isNotFound(err) && err != nil {
			writeError(w, http.StatusUnauthorized, "Authentication required")
			return authz.Identity{}, store.Session{}, authz.ErrUnauthenticated
		}
	}

	// Anonymous L0: no session, no CSRF. Read-only (mutations need CSRF).
	role := s.deps.NetworkDefaultRole
	if role == "" {
		role = authz.RoleViewer
	}
	ident := authz.Identity{
		Name:             principal,
		Role:             role,
		Assurance:        authz.AssuranceNetwork,
		NetworkPrincipal: principal,
	}
	return ident, store.Session{}, nil
}

// createNetworkSession creates an L0 (network) session for the linked user
// and sets the session cookie. This is the linked-principal bootstrap path
// (§3.3): a network-identified request bootstraps the account's session at
// L0 with no password prompt.
func (s *Server) createNetworkSession(w http.ResponseWriter, r *http.Request, userID int64, principal string) (authz.Identity, store.Session, error) {
	if s.deps.Sessions == nil {
		// No session store — fall back to anonymous identity.
		ident := authz.Identity{
			Role:             s.deps.NetworkDefaultRole,
			Assurance:        authz.AssuranceNetwork,
			NetworkPrincipal: principal,
		}
		if ident.Role == "" {
			ident.Role = authz.RoleViewer
		}
		return ident, store.Session{}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	// Resolve the user's username + role for the Identity.
	var ident authz.Identity
	if auth, ok := s.deps.Auth.(*authz.Authorizer); ok {
		var err error
		ident, err = auth.IdentityByID(ctx, userID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "Authentication required")
			return authz.Identity{}, store.Session{}, err
		}
	} else {
		ident = authz.Identity{Role: authz.RoleViewer}
	}

	// Create the L0 session.
	sid, err := authz.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session creation failed")
		return authz.Identity{}, store.Session{}, err
	}
	csrf, err := authz.RandomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session creation failed")
		return authz.Identity{}, store.Session{}, err
	}
	now := time.Now()
	sess := store.Session{
		ID:               sid,
		UserID:           userID,
		CSRFToken:        csrf,
		CreatedAt:        now,
		ExpiresAt:        now.Add(7 * 24 * time.Hour),
		LastSeenAt:       now,
		RemoteAddr:       clientIP(r),
		UserAgent:        r.UserAgent(),
		Assurance:        string(authz.AssuranceNetwork),
		AssuranceAt:      now,
		NetworkPrincipal: principal,
	}
	if err := s.deps.Sessions.Create(ctx, sess); err != nil {
		writeError(w, http.StatusInternalServerError, "session creation failed")
		return authz.Identity{}, store.Session{}, err
	}
	s.setSessionCookie(w, sess)
	ident.Assurance = authz.AssuranceNetwork
	ident.AssuranceAt = now
	ident.NetworkPrincipal = principal
	return ident, sess, nil
}

// requireRole wraps a handler with an RBAC check. Must run after withAuth
// (the Identity must already be in the request context).
func (s *Server) requireRole(need authz.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ident, ok := r.Context().Value(identityKey).(authz.Identity)
			if !ok {
				writeError(w, http.StatusUnauthorized, "Authentication required")
				return
			}
			if !ident.Role.Allows(need) {
				writeError(w, http.StatusForbidden, string(need)+" role required")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrfOKForSession reports whether the request carries a valid CSRF token
// matching the session. When the session has no ID (anonymous network
// identity), CSRF always fails — anonymous users can't make mutations.
// When Sessions is nil (Phase 4 stub), CSRF is skipped.
func (s *Server) csrfOKForSession(r *http.Request, sess store.Session) bool {
	if s.deps.Sessions == nil {
		return true
	}
	if sess.ID == "" {
		// No session (anonymous network identity) — CSRF fails. But
		// check the cookie path as fallback (the session may have been
		// set by the bootstrap but not passed in sess).
		return s.csrfOK(r)
	}
	if sess.CSRFToken == "" {
		return false
	}
	return sess.CSRFToken == r.Header.Get("X-CSRF-Token")
}

// csrfOK reports whether the request carries a valid CSRF token matching
// the session. Lax when Sessions is nil (Phase 4 with stubs).
func (s *Server) csrfOK(r *http.Request) bool {
	if s.deps.Sessions == nil {
		return true
	}
	sid := sessionCookie(r)
	if sid == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	sess, err := s.deps.Sessions.Get(ctx, sid)
	if err != nil {
		return false
	}
	return sess.CSRFToken != "" && sess.CSRFToken == r.Header.Get("X-CSRF-Token")
}

// requireAssurance wraps a handler with an assurance-level check (§3.5).
// Must run after withAuth (the Identity must already be in the request
// context). Bearer-key identities (KeyID != "") skip the policy matrix
// (§5: "Bearer-key API paths are unchanged and skip this"). Session
// identities are checked against the policy for the given resource key.
// On shortfall, returns 403 { "error": "step_up_required", "required":
// "<factor>", "resource": "<key>" }.
func (s *Server) requireAssurance(resourceKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ident, ok := r.Context().Value(identityKey).(authz.Identity)
			if !ok {
				writeError(w, http.StatusUnauthorized, "Authentication required")
				return
			}
			// Bearer-key paths skip the policy matrix (§5).
			if ident.KeyID != "" {
				next.ServeHTTP(w, r)
				return
			}
			// No policy store wired — pass through (Phase 4 stub).
			if s.deps.PolicyStore == nil {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			policy, err := s.deps.PolicyStore.Load(ctx)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "policy load failed")
				return
			}
			ttl := s.deps.StepUpTTL
			if ttl == 0 {
				ttl = authz.DefaultStepUpTTL
			}
			eval := authz.NewPolicyEvaluator(policy, ttl, time.Now)
			decision := eval.Evaluate(resourceKey, ident.Assurance, ident.AssuranceAt)
			if decision.Allowed {
				next.ServeHTTP(w, r)
				return
			}
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error":    "step_up_required",
				"required": string(decision.Required),
				"resource": decision.Resource,
			})
		})
	}
}

// withSecurityHeaders sets the V4 response headers (forge/app.py
// _security_headers). The PWA's CSP allows same-origin + the tailnet
// iframe hosts used by the embedded ops views.
func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		// Only set CSP on HTML responses; assets are served with their
		// own Content-Type and don't need the policy.
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			h.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
					"style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; "+
					"media-src 'self' data: blob:; "+
					"frame-src 'self' https://*.example-tailnet.ts.net")
		}
		next.ServeHTTP(w, r)
	})
}

// ── Helpers ─────────────────────────────────────────────────────────────────

// bearerToken extracts a Bearer token from the Authorization header.
func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(v, "Bearer "))
}

// sessionCookie returns the session ID from the forge_session cookie.
// Empty string when absent or empty.
func sessionCookie(r *http.Request) string {
	c, err := r.Cookie("forge_session")
	if err != nil {
		return ""
	}
	return c.Value
}

// writeJSON marshals v as JSON and writes it with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Connection is broken; nothing useful to do but log via header.
		_, _ = w.Write([]byte(`{"error":"encode_failed"}`))
	}
}

// writeError emits the V4 generic error shape {"error": "<message>"}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// writeValidationError emits the 422 field-level shape
// {"error": "validation_failed", "fields": {...}} (Pydantic parity).
func writeValidationError(w http.ResponseWriter, fields map[string]string) {
	if fields == nil {
		fields = map[string]string{}
	}
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error":  "validation_failed",
		"fields": fields,
	})
}

// identity returns the Identity from the request context, or the zero
// Identity if somehow missing (should not happen — withAuth runs first).
func identity(r *http.Request) authz.Identity {
	if i, ok := r.Context().Value(identityKey).(authz.Identity); ok {
		return i
	}
	return authz.Identity{}
}

// audit writes an audit entry if the Audit dependency is wired. Best-effort:
// failures are silently dropped (audit logging must never block a request).
func (s *Server) audit(r *http.Request, actor, action, target, detail string) {
	if s.deps.Audit == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	_ = s.deps.Audit.Write(ctx, store.AuditEntry{
		TS:         time.Now(),
		Actor:      actor,
		Action:     action,
		Target:     target,
		Detail:     detail,
		RemoteAddr: r.RemoteAddr,
	})
}
