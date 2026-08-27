// SPDX-License-Identifier: Apache-2.0

package httpapi

// infra_handlers.go — Sprint 12 (was H) Phase 2: typed GET/PUT endpoints for
// every infra.*/metrics.*/ui.* settings group that had no HTTP surface at
// all before this sprint (previously reachable only via `forge config
// set`). Follows the exact shape cost_handlers.go established for
// infra.cost: GET returns the live resolved value, PUT merges a partial
// body onto it, validates, persists to the store, and either reloads the
// running config live (ReloadConfig — infra.monitor picks this up on its
// next collector cycle) or marks a restart pending (markRestartRequired —
// infra.router/infra.server/infra.paths/infra.tailscale don't take effect
// until the daemon restarts; see cmd/forge/main.go's cfgHolder/routerCfg
// construction for why).
//
// Every PUT here merges at the raw-JSON-object level (mergeSettingJSON),
// never by decoding into a Go struct that only knows a subset of fields and
// marshaling it back out — that would silently drop any field the struct
// doesn't declare. This is not a hypothetical: see memory "Full-replace curl
// verification hazard" (2026-08-05) — a hand-crafted partial PUT against a
// similarly-shaped full-replace catalog endpoint wiped real model data.
//
// Preflight validation for the boot-critical system group (server/paths/
// ports/tailscale) lands in Phase 3 (preflight.go) — this file wires the
// group's plain GET/PUT + light structural validation first, matching the
// phase-by-phase plan. handleSystemSettingsPut calls s.systemPreflight via
// a func field the Server sets once Phase 3 lands (nil here = skipped).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// preflightCheck is one row of a Danger Zone check-before-save result.
// Declared here (Phase 2) rather than in preflight.go (Phase 3) because
// handleSystemSettingsPut's signature needs it before the real checks
// exist — Phase 3 fills in the actual check functions, this type doesn't
// change shape when it does. Wire shape matches the plan doc verbatim:
// POST /api/v1/system/preflight returns {ok, checks:[...]}; a failed PUT
// 422s with {error:"preflight_failed", fields:{...}, checks:[...]}.
type preflightCheck struct {
	Field   string `json:"field"`
	Level   string `json:"level"` // "ok" | "warn" | "error"
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
}

// ── shared merge/apply plumbing ─────────────────────────────────────────────
//
// No generic putInfraGroup dispatcher: the original design (see the sprint
// plan) sketched one keyed by a single storeKey, but system/settings alone
// spans 4 independent keys (infra.server/paths/ports/tailscale) and every
// group's merge must preserve unknown JSON fields (mergeSettingJSON, not a
// narrow Go struct round-trip — see this file's doc comment), which needs
// per-group field lists anyway. Each handler below is explicit instead;
// putIntField/putFloatField/putStringField carry the actually-shared part
// (bounds-checked "if the caller sent this field, patch it in").

// mergeSettingJSON reads storeKey's current raw JSON as a generic object and
// overlays patch on top, key by key, returning the merged raw bytes. See
// this file's doc comment for why every group merges at this level instead
// of round-tripping through a narrower Go struct.
func mergeSettingJSON(current []byte, patch map[string]json.RawMessage) ([]byte, error) {
	merged := map[string]json.RawMessage{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &merged); err != nil {
			return nil, fmt.Errorf("parse current value: %w", err)
		}
	}
	for k, v := range patch {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// putJSON marshals v (a single value, not a patch map) and writes it to
// key, used by groups that are one independent scalar setting rather than a
// merged struct (e.g. metrics.retention_days) — nothing to preserve.
func putJSON(v any) (json.RawMessage, error) {
	return json.Marshal(v)
}

// settingRestartRequired is the settings-KV key backing the "a restart-mode
// field was changed but the daemon hasn't picked it up yet" signal exposed
// on GET /api/v1/status (see status_handlers.go). Cleared at boot in
// cmd/forge/main.go, right after config.LoadFromStore — a fresh process
// start naturally clears whatever it left pending on its last life.
const settingRestartRequired = "system.restart_required"

type restartRequiredInfo struct {
	Keys  []string  `json:"keys"`
	Since time.Time `json:"since"`
	By    string    `json:"by"`
}

// markRestartRequired records that storeKey was changed and needs a daemon
// restart to take effect, merging into any already-pending set of keys so
// two different restart-mode saves in a row don't clobber each other's
// record. Best-effort: a failure here must never fail the settings write
// that triggered it (same "must never block" contract as s.audit).
func (s *Server) markRestartRequired(r *http.Request, storeKey string) {
	if s.deps.Settings == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	info := restartRequiredInfo{Since: time.Now(), By: identity(r).Name}
	if raw, err := s.deps.Settings.Get(ctx, settingRestartRequired); err == nil {
		var existing restartRequiredInfo
		if json.Unmarshal(raw, &existing) == nil {
			info.Keys = existing.Keys
		}
	}
	if !slices.Contains(info.Keys, storeKey) {
		info.Keys = append(info.Keys, storeKey)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return
	}
	_ = s.deps.Settings.Set(ctx, settingRestartRequired, raw)
}

// restartRequired reads the pending-restart signal for GET /api/v1/status.
// nil = nothing pending (unset key, or the Settings dependency isn't wired).
func (s *Server) restartRequired(ctx context.Context) *restartRequiredInfo {
	if s.deps.Settings == nil {
		return nil
	}
	raw, err := s.deps.Settings.Get(ctx, settingRestartRequired)
	if err != nil {
		// Collapses ErrNotFound (the common case: nothing pending) and any
		// other read failure into the same "nothing pending" answer — this
		// is an advisory banner, not a source of truth worth failing loudly
		// over.
		return nil
	}
	var info restartRequiredInfo
	if json.Unmarshal(raw, &info) != nil || len(info.Keys) == 0 {
		return nil
	}
	return &info
}

// ClearRestartRequired deletes the pending-restart signal. Exported for
// cmd/forge/main.go to call once at boot, right after config.LoadFromStore
// succeeds — a fresh process start has, by definition, already picked up
// every stored infra.* value, so anything recorded from a previous life is
// stale. Best-effort, same contract as markRestartRequired.
func (s *Server) ClearRestartRequired(ctx context.Context) {
	if s.deps.Settings == nil {
		return
	}
	_ = s.deps.Settings.Set(ctx, settingRestartRequired, []byte("null"))
}

// getRawSetting fetches a key's current raw JSON, treating "not set" as
// empty rather than an error (mergeSettingJSON already handles nil/empty
// input as "no prior fields").
func (s *Server) getRawSetting(ctx context.Context, key string) []byte {
	if s.deps.Settings == nil {
		return nil
	}
	raw, err := s.deps.Settings.Get(ctx, key)
	if err != nil {
		// Same collapse as restartRequired just above: this func's own doc
		// comment already treats "not set" as the expected empty case, so a
		// real read error degrades to the same thing rather than a distinct
		// error path callers would have to handle.
		return nil
	}
	return raw
}

// portsKeyRE matches the infra.ports map's key shape — each key becomes a
// `forge-<key>` systemd unit name (cmd/forge/main.go's extraUnits),
// same pattern services_handlers.go's serviceRE already enforces for the
// catalog's own service names.
var portsKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// validateListenAddr applies the same structural check main.go's own
// net.Listen call would ultimately need to succeed: parses as host:port
// with a well-formed numeric port. Does not attempt to bind — that's
// preflightSystem's job (Phase 3), which also has to skip already-bound
// addresses to avoid failing every save against the daemon's own listener.
func validateListenAddr(addr string) error {
	if addr == "" {
		return fmt.Errorf("must not be empty")
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port (e.g. \":5000\" or \"0.0.0.0:5000\"): %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port must be 1-65535")
	}
	return nil
}

// ── GET/PUT /api/v1/monitor/settings — infra.monitor (live) ────────────────

type monitorSettingsBody struct {
	PollIntervalS   *int `json:"poll_interval_s"`
	HangTPSThousand *int `json:"hang_tps_thousandth"`
	HangSustainS    *int `json:"hang_sustain_s"`
	SwitchCooldownS *int `json:"switch_cooldown_s"`
	GTTWarnPct      *int `json:"gtt_warn_pct"`
}

type monitorSettingsResponse struct {
	PollIntervalS   int `json:"poll_interval_s"`
	HangTPSThousand int `json:"hang_tps_thousandth"`
	HangSustainS    int `json:"hang_sustain_s"`
	SwitchCooldownS int `json:"switch_cooldown_s"`
	GTTWarnPct      int `json:"gtt_warn_pct"`
}

// Defaults mirror config.Config.applyDefaults() exactly (config.go:398-412).
// Duplicated here rather than read from s.deps.Config() for the same reason
// as the router group: reading straight off the raw settings key (like
// PUT's own merge base already does) keeps GET self-consistent immediately
// after a PUT even in an environment where ReloadConfig isn't wired
// (Phase 4 stub, most tests) — s.deps.Config() only reflects a write once
// something has called ReloadConfig(), which is a separate, best-effort
// notification to the engine/collector, not this handler's own source of
// truth. Found by this file's own TestSystemSettingsPartialPutPreservesOtherFields
// catching the equivalent bug in the system group before either shipped.
const (
	monitorDefaultPollIntervalS   = 2
	monitorDefaultHangTPSThousand = 100
	monitorDefaultHangSustainS    = 90
	monitorDefaultSwitchCooldownS = 120
	monitorDefaultGTTWarnPct      = 85
)

func (r *monitorSettingsResponse) applyMonitorDefaults() {
	if r.PollIntervalS == 0 {
		r.PollIntervalS = monitorDefaultPollIntervalS
	}
	if r.HangTPSThousand == 0 {
		r.HangTPSThousand = monitorDefaultHangTPSThousand
	}
	if r.HangSustainS == 0 {
		r.HangSustainS = monitorDefaultHangSustainS
	}
	if r.SwitchCooldownS == 0 {
		r.SwitchCooldownS = monitorDefaultSwitchCooldownS
	}
	if r.GTTWarnPct == 0 {
		r.GTTWarnPct = monitorDefaultGTTWarnPct
	}
}

func (s *Server) resolvedMonitorSettings(ctx context.Context) monitorSettingsResponse {
	var resp monitorSettingsResponse
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.monitor"), &resp); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applyMonitorDefaults()
	return resp
}

// handleMonitorSettingsGet — GET /api/v1/monitor/settings (operator).
func (s *Server) handleMonitorSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.resolvedMonitorSettings(r.Context()))
}

// handleMonitorSettingsPut — PUT /api/v1/monitor/settings (admin,
// page.settings). Applies live via ReloadConfig — the collector re-reads
// *config.Config every cycle, so this typically lands within one
// poll_interval_s of the save.
func (s *Server) handleMonitorSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body monitorSettingsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	patch := map[string]json.RawMessage{}
	fields := map[string]string{}
	putIntField(patch, fields, "poll_interval_s", body.PollIntervalS, 1, 3600)
	putIntField(patch, fields, "hang_tps_thousandth", body.HangTPSThousand, 0, 100000)
	putIntField(patch, fields, "hang_sustain_s", body.HangSustainS, 1, 3600)
	putIntField(patch, fields, "switch_cooldown_s", body.SwitchCooldownS, 0, 3600)
	putIntField(patch, fields, "gtt_warn_pct", body.GTTWarnPct, 1, 100)
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	merged, err := mergeSettingJSON(s.getRawSetting(ctx, "infra.monitor"), patch)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, "infra.monitor", merged); err != nil {
		writeInternalError(w, err)
		return
	}
	if s.deps.ReloadConfig != nil {
		s.deps.ReloadConfig()
	}
	s.audit(r, identity(r).Name, "monitor_settings", "infra.monitor", string(merged))

	var resp monitorSettingsResponse
	if err := json.Unmarshal(merged, &resp); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applyMonitorDefaults()
	writeJSON(w, http.StatusOK, resp)
}

// putIntField is the small manual "if the caller sent this field, marshal
// it into the patch map under its JSON key, after checking bounds" helper
// every group below repeats — deliberately not reflection-based (see this
// file's doc comment on why merges stay explicit).
func putIntField(patch map[string]json.RawMessage, fieldErrs map[string]string, key string, v *int, min, max int) {
	if v == nil {
		return
	}
	if *v < min || *v > max {
		fieldErrs[key] = fmt.Sprintf("must be %d-%d", min, max)
		return
	}
	b, _ := json.Marshal(*v)
	patch[key] = b
}

func putFloatField(patch map[string]json.RawMessage, fieldErrs map[string]string, key string, v *float64, min, max float64, allowZero bool) {
	if v == nil {
		return
	}
	if allowZero && *v == 0 {
		b, _ := json.Marshal(*v)
		patch[key] = b
		return
	}
	if *v < min || *v > max {
		fieldErrs[key] = fmt.Sprintf("must be %g-%g", min, max)
		return
	}
	b, _ := json.Marshal(*v)
	patch[key] = b
}

func putStringField(patch map[string]json.RawMessage, key string, v *string) {
	if v == nil {
		return
	}
	b, _ := json.Marshal(*v)
	patch[key] = b
}

func putBoolField(patch map[string]json.RawMessage, key string, v *bool) {
	if v == nil {
		return
	}
	b, _ := json.Marshal(*v)
	patch[key] = b
}

// ── GET/PUT /api/v1/router/config — infra.router (restart) ─────────────────
//
// Deliberately excludes listen_port: it is a genuine dead field on
// router.RouterConfig — a[[]] confirmed live in this sprint's investigation
// that main.go only ever logs it (`listen_port=%d`), never binds to it (the
// real a0 listen address is infra.server.router_listen, exposed on the
// system/settings group below). Building a control for it would present a
// setting that does nothing; mergeSettingJSON leaves whatever raw value is
// already stored under "listen_port" untouched either way, so this endpoint
// doesn't destroy it, just never offers to change it.

// Defaults mirror router/config.go's applyDefaults() exactly — duplicated
// here (httpapi has no live *router.RouterConfig to read, since the router
// is a separate process-internal component constructed once in main.go and
// never re-read) rather than exported, to avoid a cross-package dependency
// for five constants. TestRouterConfigDefaultsMatchRouterPackage pins them
// against the real router package so a future change there can't drift
// silently.
const (
	routerDefaultConnectTimeoutS      = 5.0
	routerDefaultHealthTTLS           = 4.0
	routerDefaultMaxRetriesPerBackend = 1
	routerDefaultEnsureLoadedTimeoutS = 320.0
	// RequestTimeoutS deliberately has NO default here either — 0 means
	// unbounded and is the intended value, not an unset placeholder (see
	// router/config.go's requestTimeout doc comment; this was the
	// laguna-s-21 fix, 2026-07-30). Never default this to a non-zero value.
)

type routerConfigResponse struct {
	ConnectTimeoutS      float64 `json:"connect_timeout_s"`
	RequestTimeoutS      float64 `json:"request_timeout_s"`
	HealthTTLS           float64 `json:"health_ttl_s"`
	MaxRetriesPerBackend int     `json:"max_retries_per_backend"`
	EnsureLoadedTimeoutS float64 `json:"ensure_loaded_timeout_s"`
	EmbeddingURL         string  `json:"embedding_url"`
	TTSURL               string  `json:"tts_url"`
}

func (r *routerConfigResponse) applyRouterDefaults() {
	if r.ConnectTimeoutS == 0 {
		r.ConnectTimeoutS = routerDefaultConnectTimeoutS
	}
	if r.HealthTTLS == 0 {
		r.HealthTTLS = routerDefaultHealthTTLS
	}
	if r.MaxRetriesPerBackend == 0 {
		r.MaxRetriesPerBackend = routerDefaultMaxRetriesPerBackend
	}
	if r.EnsureLoadedTimeoutS == 0 {
		r.EnsureLoadedTimeoutS = routerDefaultEnsureLoadedTimeoutS
	}
}

func (s *Server) resolvedRouterConfig(ctx context.Context) routerConfigResponse {
	var resp routerConfigResponse
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.router"), &resp); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applyRouterDefaults()
	return resp
}

func (s *Server) handleRouterConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.resolvedRouterConfig(r.Context()))
}

type routerConfigBody struct {
	ConnectTimeoutS      *float64 `json:"connect_timeout_s"`
	RequestTimeoutS      *float64 `json:"request_timeout_s"`
	HealthTTLS           *float64 `json:"health_ttl_s"`
	MaxRetriesPerBackend *int     `json:"max_retries_per_backend"`
	EnsureLoadedTimeoutS *float64 `json:"ensure_loaded_timeout_s"`
	EmbeddingURL         *string  `json:"embedding_url"`
	TTSURL               *string  `json:"tts_url"`
}

// handleRouterConfigPut — PUT /api/v1/router/config (admin, page.settings).
// Restart-mode: internal/router.Deps.Cfg is constructed once in main.go and
// never re-read from the store, so this takes effect on the next daemon
// restart, not immediately (see the "restart-required" banner this sets).
func (s *Server) handleRouterConfigPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body routerConfigBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	patch := map[string]json.RawMessage{}
	fields := map[string]string{}
	putFloatField(patch, fields, "connect_timeout_s", body.ConnectTimeoutS, 0.1, 300, false)
	// request_timeout_s: 0 is the deliberate "unbounded" value (see the
	// const block above) — allowZero=true so a save that explicitly sets it
	// back to 0 is accepted, not rejected as "below minimum".
	putFloatField(patch, fields, "request_timeout_s", body.RequestTimeoutS, 1, 3600, true)
	putFloatField(patch, fields, "health_ttl_s", body.HealthTTLS, 0.5, 300, false)
	putIntField(patch, fields, "max_retries_per_backend", body.MaxRetriesPerBackend, 0, 10)
	putFloatField(patch, fields, "ensure_loaded_timeout_s", body.EnsureLoadedTimeoutS, 5, 3600, false)
	if body.EmbeddingURL != nil {
		// Same rule router/config.go's own validate() applies — duplicated
		// rather than imported to avoid depending on router package
		// internals; TestEmbeddingURLValidationMatchesRouterPackage pins
		// the two against each other.
		if *body.EmbeddingURL != "" {
			u, err := url.Parse(*body.EmbeddingURL)
			if err != nil || u.Scheme == "" || u.Host == "" {
				fields["embedding_url"] = "must be an absolute http(s) URL"
			}
		}
		if fields["embedding_url"] == "" {
			putStringField(patch, "embedding_url", body.EmbeddingURL)
		}
	}
	if body.TTSURL != nil {
		// Same rule as embedding_url just above (and the same rule
		// router/config.go's own validate() applies to TTSURL).
		if *body.TTSURL != "" {
			u, err := url.Parse(*body.TTSURL)
			if err != nil || u.Scheme == "" || u.Host == "" {
				fields["tts_url"] = "must be an absolute http(s) URL"
			}
		}
		if fields["tts_url"] == "" {
			putStringField(patch, "tts_url", body.TTSURL)
		}
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	merged, err := mergeSettingJSON(s.getRawSetting(ctx, "infra.router"), patch)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, "infra.router", merged); err != nil {
		writeInternalError(w, err)
		return
	}
	s.markRestartRequired(r, "infra.router")
	s.audit(r, identity(r).Name, "router_config", "infra.router", string(merged))

	var resp routerConfigResponse
	if err := json.Unmarshal(merged, &resp); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applyRouterDefaults()
	writeJSON(w, http.StatusOK, resp)
}

// ── GET/PUT /api/v1/metrics/settings — two independent scalar keys ─────────
//
// Different apply modes on adjacent fields, found live this sprint:
// metrics.retention_days is re-read as a func value by store.RunRetention
// every tick (live); metrics.sample_interval_s is read once inside
// metricsSamplerOnce.Do and baked into a time.NewTicker (restart) — see
// metrics_handlers.go's startMetricsSampling. Do not badge these the same.

type metricsSettingsResponse struct {
	RetentionDays  int `json:"retention_days"`
	SampleInterval int `json:"sample_interval_s"`
}

func (s *Server) handleMetricsSettingsGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, metricsSettingsResponse{
		RetentionDays:  s.metricsRetentionDaysSetting(),
		SampleInterval: s.metricsSampleIntervalS(),
	})
}

type metricsSettingsBody struct {
	RetentionDays  *int `json:"retention_days"`
	SampleInterval *int `json:"sample_interval_s"`
}

func (s *Server) handleMetricsSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body metricsSettingsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}
	fields := map[string]string{}
	if body.RetentionDays != nil && (*body.RetentionDays < 1 || *body.RetentionDays > 3650) {
		fields["retention_days"] = "must be 1-3650"
	}
	if body.SampleInterval != nil && (*body.SampleInterval < 5 || *body.SampleInterval > 3600) {
		fields["sample_interval_s"] = "must be 5-3600"
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	resp := metricsSettingsResponse{RetentionDays: s.metricsRetentionDaysSetting(), SampleInterval: s.metricsSampleIntervalS()}
	if body.RetentionDays != nil && *body.RetentionDays != resp.RetentionDays {
		raw, _ := putJSON(*body.RetentionDays)
		if err := s.deps.Settings.Set(ctx, SettingMetricsRetentionDays, raw); err != nil {
			writeInternalError(w, err)
			return
		}
		resp.RetentionDays = *body.RetentionDays
		// live: store.RunRetention re-reads this via a func value every
		// prune tick — no ReloadConfig/restart marker needed.
	}
	if body.SampleInterval != nil && *body.SampleInterval != resp.SampleInterval {
		raw, _ := putJSON(*body.SampleInterval)
		if err := s.deps.Settings.Set(ctx, SettingMetricsSampleInterval, raw); err != nil {
			writeInternalError(w, err)
			return
		}
		resp.SampleInterval = *body.SampleInterval
		s.markRestartRequired(r, SettingMetricsSampleInterval)
	}
	s.audit(r, identity(r).Name, "metrics_settings", "metrics.*", fmt.Sprintf("retention_days=%d sample_interval_s=%d", resp.RetentionDays, resp.SampleInterval))
	writeJSON(w, http.StatusOK, resp)
}

// ── GET/PUT /api/v1/system/settings — infra.server + infra.paths +
//    infra.ports + infra.tailscale (restart, boot-critical) ────────────────
//
// This is the group the Danger Zone (Phase 7) gates in the frontend. Phase
// 2 wires plain GET/PUT with structural validation only; Phase 3 adds the
// real preflight (paths exist/writable, addresses bind, ports don't
// collide) via systemPreflight, called here through a func field so this
// file doesn't need to know preflight.go exists yet.

type systemSettingsResponse struct {
	Listen       string `json:"listen"`
	RouterListen string `json:"router_listen"`
	MCPListen    string `json:"mcp_listen"`
	DBPath       string `json:"db_path"`
	TTSUnit      string `json:"tts_unit"`

	ModelsDir    string `json:"models_dir"`
	SysconfigDir string `json:"sysconfig_dir"`
	StateDir     string `json:"state_dir"`
	IconsDir     string `json:"icons_dir"`
	VulkanBin    string `json:"vulkan_bin"`
	RocmBin      string `json:"rocm_bin"`

	Ports map[string]int `json:"ports"`

	Hostname string `json:"hostname"`
	// RPID is documented reserved/unused (config.Tailscale.RPID: "reserved,
	// WebAuthn dropped in V5.0") — shown for transparency ("expose every
	// setting"), deliberately absent from systemSettingsBody below so this
	// endpoint never offers to write a field that has no effect.
	RPID string `json:"rp_id"`

	// CookieSecure mirrors config.Server.CookieSecure (issue #27, sprint 4)
	// — plain bool here rather than *bool: resolvedSystemSettings below
	// seeds the true default before unmarshaling the stored `infra.server`
	// JSON onto it, same literal-default-then-overlay idiom
	// resolvedRouterSettings uses for InjectStreamUsage, so an absent field
	// in storage never gets misread as an explicit false.
	CookieSecure bool `json:"cookie_secure"`
}

// Defaults mirror config.Config.applyDefaults() exactly (config.go:380-397)
// — Paths and Tailscale have none at all (empty string = genuinely unset,
// a deployment-time provisioning fact, not something to paper over with a
// fake default). Duplicated here rather than read via s.deps.Config() for
// the same self-consistency reason as the monitor/router groups above.
const (
	systemDefaultListen       = "127.0.0.1:5000"
	systemDefaultRouterListen = ":8085"
	systemDefaultMCPListen    = ":8095"
	systemDefaultDBPath       = "/var/lib/forge/forge.db"
	systemDefaultTTSUnit      = "forge-tts"
)

func (r *systemSettingsResponse) applySystemDefaults() {
	if r.Listen == "" {
		r.Listen = systemDefaultListen
	}
	if r.RouterListen == "" {
		r.RouterListen = systemDefaultRouterListen
	}
	if r.MCPListen == "" {
		r.MCPListen = systemDefaultMCPListen
	}
	if r.DBPath == "" {
		r.DBPath = systemDefaultDBPath
	}
	if r.TTSUnit == "" {
		r.TTSUnit = systemDefaultTTSUnit
	}
}

func (s *Server) resolvedSystemSettings(ctx context.Context) systemSettingsResponse {
	resp := systemSettingsResponse{Ports: map[string]int{}, CookieSecure: true}
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.server"), &resp); err != nil { // Listen/RouterListen/MCPListen/DBPath/TTSUnit/CookieSecure
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.paths"), &resp); err != nil { // ModelsDir/SysconfigDir/StateDir/IconsDir/VulkanBin/RocmBin
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.ports"), &resp.Ports); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.tailscale"), &resp); err != nil { // Hostname/RPID
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applySystemDefaults()
	return resp
}

func (s *Server) handleSystemSettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.resolvedSystemSettings(r.Context()))
}

type systemSettingsBody struct {
	Listen       *string `json:"listen"`
	RouterListen *string `json:"router_listen"`
	MCPListen    *string `json:"mcp_listen"`
	DBPath       *string `json:"db_path"`
	TTSUnit      *string `json:"tts_unit"`

	ModelsDir    *string `json:"models_dir"`
	SysconfigDir *string `json:"sysconfig_dir"`
	StateDir     *string `json:"state_dir"`
	IconsDir     *string `json:"icons_dir"`
	VulkanBin    *string `json:"vulkan_bin"`
	RocmBin      *string `json:"rocm_bin"`

	// Ports is a whole-map replace when present (nil = untouched) — a
	// per-key patch would need tri-state "unset vs delete this key"
	// semantics the Danger Zone's UI doesn't need yet (it always submits
	// the full map it displayed).
	Ports map[string]int `json:"ports"`

	Hostname *string `json:"hostname"`

	CookieSecure *bool `json:"cookie_secure"`
}

// buildSystemCandidate applies structural validation (address shape, ports
// key/range) and merges body onto the current resolved system settings —
// shared by handleSystemSettingsPut and handleSystemPreflightPost (Phase 3)
// so "what would this save produce" is computed identically whether or not
// anything is actually written.
func (s *Server) buildSystemCandidate(ctx context.Context, body systemSettingsBody) (systemSettingsResponse, map[string]string) {
	merged := s.resolvedSystemSettings(ctx)
	fields := map[string]string{}

	checkAddr := func(label string, v *string, dst *string) {
		if v == nil {
			return
		}
		if err := validateListenAddr(*v); err != nil {
			fields[label] = err.Error()
			return
		}
		*dst = *v
	}
	checkAddr("listen", body.Listen, &merged.Listen)
	checkAddr("router_listen", body.RouterListen, &merged.RouterListen)
	checkAddr("mcp_listen", body.MCPListen, &merged.MCPListen)
	if body.DBPath != nil {
		merged.DBPath = *body.DBPath
	}
	if body.TTSUnit != nil {
		merged.TTSUnit = *body.TTSUnit
	}
	if body.ModelsDir != nil {
		merged.ModelsDir = *body.ModelsDir
	}
	if body.SysconfigDir != nil {
		merged.SysconfigDir = *body.SysconfigDir
	}
	if body.StateDir != nil {
		merged.StateDir = *body.StateDir
	}
	if body.IconsDir != nil {
		merged.IconsDir = *body.IconsDir
	}
	if body.VulkanBin != nil {
		merged.VulkanBin = *body.VulkanBin
	}
	if body.RocmBin != nil {
		merged.RocmBin = *body.RocmBin
	}
	if body.Ports != nil {
		for k, v := range body.Ports {
			if !portsKeyRE.MatchString(k) {
				fields["ports"] = fmt.Sprintf("key %q must match ^[a-z][a-z0-9_-]*$", k)
				break
			}
			if v < 1 || v > 65535 {
				fields["ports"] = fmt.Sprintf("port %d for %q must be 1-65535", v, k)
				break
			}
		}
		if fields["ports"] == "" {
			merged.Ports = body.Ports
		}
	}
	if body.Hostname != nil {
		merged.Hostname = *body.Hostname
	}
	if body.CookieSecure != nil {
		merged.CookieSecure = *body.CookieSecure
	}
	return merged, fields
}

// handleSystemPreflightPost — POST /api/v1/system/preflight (admin,
// area.settings.system). Dry-run: builds the same candidate PUT would, runs
// the real checks, writes nothing. Lets the Danger Zone show a checklist
// before the operator commits to Apply.
func (s *Server) handleSystemPreflightPost(w http.ResponseWriter, r *http.Request) {
	var body systemSettingsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}
	cand, fields := s.buildSystemCandidate(r.Context(), body)
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	if s.systemPreflightFn == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "checks": []preflightCheck{}})
		return
	}
	checks, failFields := s.systemPreflightFn(r.Context(), cand)
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(failFields) == 0, "checks": checks})
}

// handleSystemSettingsPut — PUT /api/v1/system/settings (admin,
// area.settings.system). Splits the merged, validated result back across
// its 4 underlying settings keys (infra.server/paths/ports/tailscale) —
// each is independently stored (see config.LoadFromStore's key map), so a
// concurrent editor of just one group's fields elsewhere can't be clobbered
// by a save that only touched another.
func (s *Server) handleSystemSettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body systemSettingsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	merged, fields := s.buildSystemCandidate(r.Context(), body)
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	// Always preflight on PUT, even if the Danger Zone UI already called
	// POST /system/preflight once — this is the non-negotiable "reject at
	// save time" guarantee, not an optional convenience the UI can skip.
	if s.systemPreflightFn != nil {
		checks, failFields := s.systemPreflightFn(r.Context(), merged)
		if len(failFields) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": "preflight_failed", "fields": failFields, "checks": checks,
			})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	serverPatch := map[string]json.RawMessage{}
	putStringField(serverPatch, "listen", &merged.Listen)
	putStringField(serverPatch, "router_listen", &merged.RouterListen)
	putStringField(serverPatch, "mcp_listen", &merged.MCPListen)
	putStringField(serverPatch, "db_path", &merged.DBPath)
	putStringField(serverPatch, "tts_unit", &merged.TTSUnit)
	putBoolField(serverPatch, "cookie_secure", &merged.CookieSecure)
	serverRaw, err := mergeSettingJSON(s.getRawSetting(ctx, "infra.server"), serverPatch)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	pathsPatch := map[string]json.RawMessage{}
	putStringField(pathsPatch, "models_dir", &merged.ModelsDir)
	putStringField(pathsPatch, "sysconfig_dir", &merged.SysconfigDir)
	putStringField(pathsPatch, "state_dir", &merged.StateDir)
	putStringField(pathsPatch, "icons_dir", &merged.IconsDir)
	putStringField(pathsPatch, "vulkan_bin", &merged.VulkanBin)
	putStringField(pathsPatch, "rocm_bin", &merged.RocmBin)
	pathsRaw, err := mergeSettingJSON(s.getRawSetting(ctx, "infra.paths"), pathsPatch)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	tailscalePatch := map[string]json.RawMessage{}
	putStringField(tailscalePatch, "hostname", &merged.Hostname)
	tailscaleRaw, err := mergeSettingJSON(s.getRawSetting(ctx, "infra.tailscale"), tailscalePatch)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// infra.ports is a bare map (config.LoadFromStore unmarshals directly
	// into &cfg.Ports) — a whole-map replace when the body included it,
	// otherwise leave the stored value untouched entirely (don't even
	// re-write it).
	portsRaw := s.getRawSetting(ctx, "infra.ports")
	if body.Ports != nil {
		b, err := json.Marshal(merged.Ports)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		portsRaw = b
	}

	for key, raw := range map[string][]byte{
		"infra.server": serverRaw, "infra.paths": pathsRaw,
		"infra.tailscale": tailscaleRaw, "infra.ports": portsRaw,
	} {
		if raw == nil {
			continue
		}
		if err := s.deps.Settings.Set(ctx, key, raw); err != nil {
			writeInternalError(w, err)
			return
		}
	}
	// ReloadConfig here is NOT the same claim as "this group is live" — it
	// isn't (see markRestartRequired below: real effect needs a process
	// restart, since e.g. dashSrv's listener was already constructed from
	// the old cfg.Server.Listen value and won't move regardless of what
	// s.deps.Config() reports afterward). This call is a best-effort nudge
	// so the engine/collector's own *config.Config picks up e.g. a changed
	// ModelsDir sooner than their next natural SIGHUP — resolvedSystemSettings
	// itself no longer depends on it for correctness (it reads straight off
	// the settings store, same fix applied to the router/monitor groups
	// above after this file's own regression test caught the equivalent bug
	// in an earlier version that read via s.deps.Config()). CookieSecure is
	// the one field in this group that genuinely is live off this call —
	// unlike Listen it's read fresh off s.deps.Config() on every cookie set
	// (setSessionCookie, the WebAuthn challenge cookie), nothing was baked
	// in at process start — but markRestartRequired below still fires for
	// the whole infra.server group rather than special-casing this one
	// field, so the Danger Zone banner errs conservative.
	if s.deps.ReloadConfig != nil {
		s.deps.ReloadConfig()
	}
	s.markRestartRequired(r, "infra.server")
	s.audit(r, identity(r).Name, "system_settings", "infra.server+paths+ports+tailscale", "")
	writeJSON(w, http.StatusOK, merged)
}

// ── GET/PUT /api/v1/ui/settings — ui (general UI settings) ─────────────────
//
// ui.help_button and nfs.shares were removed as orphaned settings (no V5
// frontend component renders them). The endpoint remains for the generic
// "ui" settings key read on /status.

type uiSettingsResponse struct{}

func (s *Server) resolvedUISettings(ctx context.Context) uiSettingsResponse {
	return uiSettingsResponse{}
}

func (s *Server) handleUISettingsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.resolvedUISettings(r.Context()))
}

type uiSettingsBody struct{}

// handleUISettingsPut — PUT /api/v1/ui/settings (admin, page.settings).
// Immediate: the "ui" settings key is read fresh from the settings store on
// every /status build — no reload needed.
func (s *Server) handleUISettingsPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body uiSettingsBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	current := s.resolvedUISettings(ctx)
	s.audit(r, identity(r).Name, "ui_settings", "ui", "")
	writeJSON(w, http.StatusOK, current)
}

// ── GET/PUT /api/v1/scheduler/seed — infra.scheduler (seed) ────────────────
//
// Sprint 12 (was H) Phase 6: the one settings group referenced by the plan's
// Sections table ("Workload | scheduling | ... Second card = infra.scheduler
// labelled as the boot seed") that Phase 2 didn't add an endpoint for —
// config.go's applyDefaults reads this key only once, at boot, into
// config.Config.Scheduler (config.go:337 "infra.scheduler": &cfg.Scheduler);
// a live sched.Config change (PUT /scheduler/config, "scheduler.config" key)
// never touches it and vice versa. Same field shape as SchedulerConfig by
// construction (config.SchedulerDefault mirrors sched.Config exactly), so
// the wire shape is reused rather than duplicated.
//
// apply="seed": writing this changes nothing on the running daemon — it
// only changes what the *next* boot falls back to if scheduler.config is
// ever unset. No ReloadConfig call and no markRestartRequired: neither
// "already live" nor "needs a restart to take effect" is true here.

const (
	schedulerSeedDefaultIdleUnloadS            = 180
	schedulerSeedDefaultSmallJobTokenThreshold = 1500
	schedulerSeedDefaultPriorityJumpCap        = 2
	schedulerSeedDefaultReservationSoonMin     = 10
)

func (r *schedulerConfigResponse) applySchedulerSeedDefaults() {
	if r.IdleUnloadS == 0 {
		r.IdleUnloadS = schedulerSeedDefaultIdleUnloadS
	}
	if r.SmallJobTokenThreshold == 0 {
		r.SmallJobTokenThreshold = schedulerSeedDefaultSmallJobTokenThreshold
	}
	if r.PriorityJumpCap == 0 {
		r.PriorityJumpCap = schedulerSeedDefaultPriorityJumpCap
	}
	if r.ReservationSoonMin == 0 {
		r.ReservationSoonMin = schedulerSeedDefaultReservationSoonMin
	}
}

func (s *Server) resolvedSchedulerSeed(ctx context.Context) schedulerConfigResponse {
	var resp schedulerConfigResponse
	if err := json.Unmarshal(s.getRawSetting(ctx, "infra.scheduler"), &resp); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applySchedulerSeedDefaults()
	return resp
}

// handleSchedulerSeedGet — GET /api/v1/scheduler/seed (operator).
func (s *Server) handleSchedulerSeedGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.resolvedSchedulerSeed(r.Context()))
}

type schedulerSeedBody struct {
	IdleUnloadS            *int `json:"idle_unload_s"`
	SmallJobTokenThreshold *int `json:"small_job_token_threshold"`
	PriorityJumpCap        *int `json:"priority_jump_cap"`
	ReservationSoonMin     *int `json:"reservation_soon_min"`
}

// handleSchedulerSeedPut — PUT /api/v1/scheduler/seed (admin, page.settings).
func (s *Server) handleSchedulerSeedPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body schedulerSeedBody
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	patch := map[string]json.RawMessage{}
	fields := map[string]string{}
	putIntField(patch, fields, "idle_unload_s", body.IdleUnloadS, 0, 86400)
	putIntField(patch, fields, "small_job_token_threshold", body.SmallJobTokenThreshold, 0, 10_000_000)
	putIntField(patch, fields, "priority_jump_cap", body.PriorityJumpCap, 0, 100)
	putIntField(patch, fields, "reservation_soon_min", body.ReservationSoonMin, 0, 1440)
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	merged, err := mergeSettingJSON(s.getRawSetting(ctx, "infra.scheduler"), patch)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, "infra.scheduler", merged); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "scheduler_seed", "infra.scheduler", string(merged))

	var resp schedulerConfigResponse
	if err := json.Unmarshal(merged, &resp); err != nil {
		log.Printf("httpapi: warning: corrupt stored setting: %v", err)
	}
	resp.applySchedulerSeedDefaults()
	writeJSON(w, http.StatusOK, resp)
}

// ── GET/PUT /api/v1/dashboard/layout — dashboard.pages (layout) ────────────
//
// ADR-0011: custom dashboard pages stored as a system-wide settings key
// "dashboard.pages". The 3 default tabs (overview/cost/resources) are bespoke
// React code, not data — the store holds only custom pages, appended after
// the defaults. apply="immediate": no reload/restart needed (the layout is
// only read by the frontend, not by any backend component).
//
// ADR-0012: the PUT is a full-replace (not merge). The frontend always sends
// the complete layout (all pages, all widgets); partial merge doesn't make
// sense for a page/widget tree. Schema-evolvable: the JSON shape is additive
// — a per-user `owner` field on each page object can be added later without
// a key-shape migration.

type dashboardWidgetEntry struct {
	Slug  string         `json:"slug"`
	Props map[string]any `json:"props"`
}

type dashboardPageEntry struct {
	ID      string                `json:"id"`
	Name    string                `json:"name"`
	Widgets []dashboardWidgetEntry `json:"widgets"`
}

type dashboardLayoutResponse struct {
	Pages []dashboardPageEntry `json:"pages"`
}

func (s *Server) resolvedDashboardLayout(ctx context.Context) dashboardLayoutResponse {
	var resp dashboardLayoutResponse
	if raw := s.getRawSetting(ctx, "dashboard.pages"); len(raw) > 0 {
		if err := json.Unmarshal(raw, &resp); err != nil {
			log.Printf("httpapi: warning: corrupt dashboard.pages setting: %v", err)
		}
	}
	normalizeDashboardLayout(&resp)
	return resp
}

func normalizeDashboardLayout(resp *dashboardLayoutResponse) {
	for i := range resp.Pages {
		if resp.Pages[i].Widgets == nil {
			resp.Pages[i].Widgets = []dashboardWidgetEntry{}
		}
		for j := range resp.Pages[i].Widgets {
			if resp.Pages[i].Widgets[j].Props == nil {
				resp.Pages[i].Widgets[j].Props = map[string]any{}
			}
		}
	}
}

// handleDashboardLayoutGet — GET /api/v1/dashboard/layout (operator, page.settings).
func (s *Server) handleDashboardLayoutGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.resolvedDashboardLayout(r.Context()))
}

// handleDashboardLayoutPut — PUT /api/v1/dashboard/layout (admin, page.settings).
// Full-replace: the frontend sends the complete layout (all pages, all widgets).
func (s *Server) handleDashboardLayoutPut(w http.ResponseWriter, r *http.Request) {
	if s.deps.Settings == nil {
		writeError(w, http.StatusServiceUnavailable, "settings store not wired")
		return
	}
	var body dashboardLayoutResponse
	if fields := decodeJSONBody(r, &body); fields != nil {
		writeValidationError(w, fields)
		return
	}

	fields := map[string]string{}
	if len(body.Pages) > 50 {
		fields["pages"] = "maximum 50 custom pages"
	}
	seenIDs := make(map[string]bool)
	for i, page := range body.Pages {
		if page.ID == "" {
			fields[fmt.Sprintf("pages[%d].id", i)] = "must not be empty"
		} else if seenIDs[page.ID] {
			fields[fmt.Sprintf("pages[%d].id", i)] = "duplicate page id"
		} else {
			seenIDs[page.ID] = true
		}
		if page.Name == "" {
			fields[fmt.Sprintf("pages[%d].name", i)] = "must not be empty"
		}
		if len(page.Widgets) > 50 {
			fields[fmt.Sprintf("pages[%d].widgets", i)] = "maximum 50 widgets per page"
		}
		for j, widget := range page.Widgets {
			if widget.Slug == "" {
				fields[fmt.Sprintf("pages[%d].widgets[%d].slug", i, j)] = "must not be empty"
			}
		}
	}
	if len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}

	normalizeDashboardLayout(&body)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if err := s.deps.Settings.Set(ctx, "dashboard.pages", raw); err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "dashboard_layout", "dashboard.pages", string(raw))
	writeJSON(w, http.StatusOK, body)
}
