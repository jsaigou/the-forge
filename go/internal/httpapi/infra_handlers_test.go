// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/router"
)

// TestMonitorAndSystemDefaultsMatchConfigPackage mirrors
// TestRouterConfigDefaultsMatchRouterPackage for the monitor/system groups'
// own duplicated defaults.
func TestMonitorAndSystemDefaultsMatchConfigPackage(t *testing.T) {
	real, err := config.New(config.Config{})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	if real.Monitor.PollIntervalS != monitorDefaultPollIntervalS ||
		real.Monitor.HangTPSThousand != monitorDefaultHangTPSThousand ||
		real.Monitor.HangSustainS != monitorDefaultHangSustainS ||
		real.Monitor.SwitchCooldownS != monitorDefaultSwitchCooldownS ||
		real.Monitor.GTTWarnPct != monitorDefaultGTTWarnPct {
		t.Errorf("config.Monitor defaults = %+v, httpapi copies = %d/%d/%d/%d/%d",
			real.Monitor, monitorDefaultPollIntervalS, monitorDefaultHangTPSThousand,
			monitorDefaultHangSustainS, monitorDefaultSwitchCooldownS, monitorDefaultGTTWarnPct)
	}
	if real.Server.Listen != systemDefaultListen || real.Server.RouterListen != systemDefaultRouterListen ||
		real.Server.MCPListen != systemDefaultMCPListen || real.Server.DBPath != systemDefaultDBPath ||
		real.Server.TTSUnit != systemDefaultTTSUnit {
		t.Errorf("config.Server defaults = %+v, httpapi copies = %s/%s/%s/%s/%s",
			real.Server, systemDefaultListen, systemDefaultRouterListen, systemDefaultMCPListen,
			systemDefaultDBPath, systemDefaultTTSUnit)
	}
}

// TestRouterConfigDefaultsMatchRouterPackage pins infra_handlers.go's
// duplicated router-default constants against the real router package, so a
// future change to router/config.go's applyDefaults can't drift silently
// (see routerConfigResponse.applyRouterDefaults's doc comment for why the
// duplication exists at all — httpapi has no live *router.RouterConfig to
// read from).
func TestRouterConfigDefaultsMatchRouterPackage(t *testing.T) {
	real, err := router.NewConfig(router.RouterConfig{})
	if err != nil {
		t.Fatalf("router.NewConfig: %v", err)
	}
	if real.ConnectTimeoutS != routerDefaultConnectTimeoutS {
		t.Errorf("ConnectTimeoutS default = %v, httpapi copy = %v", real.ConnectTimeoutS, routerDefaultConnectTimeoutS)
	}
	if real.HealthTTLS != routerDefaultHealthTTLS {
		t.Errorf("HealthTTLS default = %v, httpapi copy = %v", real.HealthTTLS, routerDefaultHealthTTLS)
	}
	if real.MaxRetriesPerBackend != routerDefaultMaxRetriesPerBackend {
		t.Errorf("MaxRetriesPerBackend default = %v, httpapi copy = %v", real.MaxRetriesPerBackend, routerDefaultMaxRetriesPerBackend)
	}
	if real.EnsureLoadedTimeoutS != routerDefaultEnsureLoadedTimeoutS {
		t.Errorf("EnsureLoadedTimeoutS default = %v, httpapi copy = %v", real.EnsureLoadedTimeoutS, routerDefaultEnsureLoadedTimeoutS)
	}
	// RequestTimeoutS must stay 0 (unbounded) — no default either side.
	if real.RequestTimeoutS != 0 {
		t.Errorf("RequestTimeoutS default = %v, want 0 (unbounded)", real.RequestTimeoutS)
	}
}

func TestRouterConfigGetPut(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	// GET with nothing stored returns defaults.
	w := do(t, s, authedRequest("GET", "/api/v1/router/config", nil))
	if w.Code != 200 {
		t.Fatalf("GET router/config = %d, body=%s", w.Code, w.Body)
	}
	var got routerConfigResponse
	decodeJSON(t, w.Body, &got)
	if got.ConnectTimeoutS != routerDefaultConnectTimeoutS || got.RequestTimeoutS != 0 {
		t.Errorf("defaults = %+v", got)
	}

	// PUT a partial body: only ensure_loaded_timeout_s, explicitly setting
	// request_timeout_s back to 0 (the deliberate unbounded value, not "no
	// change") to prove 0 isn't rejected as "below minimum" — this was the
	// laguna-s-21 fix and the exact regression risk flagged in the plan.
	w = do(t, s, authedRequest("PUT", "/api/v1/router/config",
		strings.NewReader(`{"ensure_loaded_timeout_s":600,"request_timeout_s":0}`)))
	if w.Code != 200 {
		t.Fatalf("PUT router/config = %d, body=%s", w.Code, w.Body)
	}
	var putResp routerConfigResponse
	decodeJSON(t, w.Body, &putResp)
	if putResp.EnsureLoadedTimeoutS != 600 {
		t.Errorf("EnsureLoadedTimeoutS = %v, want 600", putResp.EnsureLoadedTimeoutS)
	}
	if putResp.RequestTimeoutS != 0 {
		t.Errorf("RequestTimeoutS = %v, want 0 (unbounded, not rejected)", putResp.RequestTimeoutS)
	}
	// connect_timeout_s wasn't touched — should still read the default,
	// not zero out because the PUT was partial.
	if putResp.ConnectTimeoutS != routerDefaultConnectTimeoutS {
		t.Errorf("ConnectTimeoutS = %v, want untouched default %v", putResp.ConnectTimeoutS, routerDefaultConnectTimeoutS)
	}

	// Reject a bad embedding_url.
	w = do(t, s, authedRequest("PUT", "/api/v1/router/config", strings.NewReader(`{"embedding_url":"not-a-url"}`)))
	if w.Code != 422 {
		t.Fatalf("bad embedding_url = %d, want 422", w.Code)
	}

	// Reject an out-of-range value.
	w = do(t, s, authedRequest("PUT", "/api/v1/router/config", strings.NewReader(`{"connect_timeout_s":9999}`)))
	if w.Code != 422 {
		t.Fatalf("out-of-range connect_timeout_s = %d, want 422", w.Code)
	}

	// A restart-mode PUT must mark the pending-restart signal.
	info := s.restartRequired(context.Background())
	if info == nil {
		t.Fatal("expected restart_required to be set after infra.router PUT")
	}
	found := false
	for _, k := range info.Keys {
		if k == "infra.router" {
			found = true
		}
	}
	if !found {
		t.Errorf("restart_required.keys = %v, want to include infra.router", info.Keys)
	}
}

func TestMonitorSettingsGetPutIsLiveNotRestart(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	w := do(t, s, authedRequest("PUT", "/api/v1/monitor/settings", strings.NewReader(`{"hang_sustain_s":45}`)))
	if w.Code != 200 {
		t.Fatalf("PUT monitor/settings = %d, body=%s", w.Code, w.Body)
	}
	var resp monitorSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.HangSustainS != 45 {
		t.Errorf("HangSustainS = %d, want 45", resp.HangSustainS)
	}
	// infra.monitor is live (ReloadConfig), never a restart marker.
	if info := s.restartRequired(context.Background()); info != nil {
		t.Errorf("monitor settings should not mark restart_required, got %+v", info)
	}

	// Out-of-range rejected.
	w = do(t, s, authedRequest("PUT", "/api/v1/monitor/settings", strings.NewReader(`{"gtt_warn_pct":150}`)))
	if w.Code != 422 {
		t.Fatalf("gtt_warn_pct=150 = %d, want 422", w.Code)
	}
}

func TestMetricsSettingsSplitApplyMode(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	// retention_days alone: live, no restart marker.
	w := do(t, s, authedRequest("PUT", "/api/v1/metrics/settings", strings.NewReader(`{"retention_days":30}`)))
	if w.Code != 200 {
		t.Fatalf("PUT metrics/settings (retention) = %d, body=%s", w.Code, w.Body)
	}
	if info := s.restartRequired(context.Background()); info != nil {
		t.Errorf("retention_days alone should not mark restart_required, got %+v", info)
	}

	// sample_interval_s: restart-mode.
	w = do(t, s, authedRequest("PUT", "/api/v1/metrics/settings", strings.NewReader(`{"sample_interval_s":120}`)))
	if w.Code != 200 {
		t.Fatalf("PUT metrics/settings (sample_interval) = %d, body=%s", w.Code, w.Body)
	}
	info := s.restartRequired(context.Background())
	if info == nil {
		t.Fatal("expected restart_required after sample_interval_s change")
	}
	found := false
	for _, k := range info.Keys {
		if k == SettingMetricsSampleInterval {
			found = true
		}
	}
	if !found {
		t.Errorf("restart_required.keys = %v, want %s", info.Keys, SettingMetricsSampleInterval)
	}

	var resp metricsSettingsResponse
	w = do(t, s, authedRequest("GET", "/api/v1/metrics/settings", nil))
	decodeJSON(t, w.Body, &resp)
	if resp.RetentionDays != 30 || resp.SampleInterval != 120 {
		t.Errorf("GET metrics/settings = %+v, want retention_days=30 sample_interval_s=120", resp)
	}

	// Out-of-range rejected.
	w = do(t, s, authedRequest("PUT", "/api/v1/metrics/settings", strings.NewReader(`{"retention_days":0}`)))
	if w.Code != 422 {
		t.Fatalf("retention_days=0 = %d, want 422", w.Code)
	}
}

// TestSystemSettingsPartialPutPreservesOtherFields is the regression test
// for the exact hazard this file's merge design exists to avoid (see
// memory "Full-replace curl verification hazard", 2026-08-05): a PUT that
// only names one field must never blank out sibling fields stored under the
// same settings key.
func TestSystemSettingsPartialPutPreservesOtherFields(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	// Real, existing paths — Phase 3's preflight now runs on every PUT, so
	// this test (whose actual point is the merge behavior, not preflight)
	// needs paths that would genuinely pass. modelsDir/iconsDir/stateDir all
	// need to exist; state_dir also needs to be writable for its probe.
	modelsDir, iconsDir, stateDir := t.TempDir(), t.TempDir(), t.TempDir()
	dbPath := stateDir + "/forge.db"
	w := do(t, s, authedRequest("PUT", "/api/v1/system/settings",
		strings.NewReader(`{"models_dir":"`+modelsDir+`","icons_dir":"`+iconsDir+`","state_dir":"`+stateDir+`","db_path":"`+dbPath+`"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT system/settings (seed) = %d, body=%s", w.Code, w.Body)
	}

	// A second PUT only touches hostname — models_dir/icons_dir must survive.
	w = do(t, s, authedRequest("PUT", "/api/v1/system/settings", strings.NewReader(`{"hostname":"forgehost.example.ts.net"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT system/settings (hostname only) = %d, body=%s", w.Code, w.Body)
	}
	var resp systemSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.ModelsDir != modelsDir {
		t.Errorf("ModelsDir = %q, want it preserved from the earlier PUT (%q)", resp.ModelsDir, modelsDir)
	}
	if resp.IconsDir != iconsDir {
		t.Errorf("IconsDir = %q, want it preserved", resp.IconsDir)
	}
	if resp.Hostname != "forgehost.example.ts.net" {
		t.Errorf("Hostname = %q, want the new value", resp.Hostname)
	}

	// Also verify the raw stored infra.paths blob itself wasn't
	// narrowed — not just the response, which is reconstructed from
	// s.deps.Config() in this test setup and wouldn't necessarily catch a
	// store-level regression on its own.
	raw, err := set.Get(context.Background(), "infra.paths")
	if err != nil {
		t.Fatalf("infra.paths not stored: %v", err)
	}
	var paths map[string]any
	if err := json.Unmarshal(raw, &paths); err != nil {
		t.Fatalf("infra.paths not valid JSON: %v", err)
	}
	if paths["models_dir"] != modelsDir {
		t.Errorf("stored infra.paths.models_dir = %v, want preserved (%q)", paths["models_dir"], modelsDir)
	}

	if info := s.restartRequired(context.Background()); info == nil {
		t.Error("expected restart_required to be set after a system/settings PUT")
	}
}

func TestSystemSettingsRejectsBadPorts(t *testing.T) {
	s := serverWithSettings(t, newFakeSettings())
	w := do(t, s, authedRequest("PUT", "/api/v1/system/settings", strings.NewReader(`{"ports":{"BadKey!":8083}}`)))
	if w.Code != 422 {
		t.Fatalf("bad ports key = %d, want 422", w.Code)
	}
	w = do(t, s, authedRequest("PUT", "/api/v1/system/settings", strings.NewReader(`{"ports":{"stt":99999}}`)))
	if w.Code != 422 {
		t.Fatalf("bad ports value = %d, want 422", w.Code)
	}
	w = do(t, s, authedRequest("PUT", "/api/v1/system/settings", strings.NewReader(`{"listen":"not-an-address"}`)))
	if w.Code != 422 {
		t.Fatalf("bad listen address = %d, want 422", w.Code)
	}
}

func TestUISettingsGetPut(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	// help_button/nfs_shares fields were removed — confirm the endpoint
	// rejects them as unknown fields (DisallowUnknownFields).
	w := do(t, s, authedRequest("PUT", "/api/v1/ui/settings",
		strings.NewReader(`{"label":"? Storage","title":"Storage shares","nfs_shares":[{"name":"models"}]}`)))
	if w.Code != 422 {
		t.Fatalf("PUT ui/settings with removed fields = %d, want 422 (unknown field rejected)", w.Code)
	}

	// Empty body succeeds.
	w = do(t, s, authedRequest("PUT", "/api/v1/ui/settings", strings.NewReader(`{}`)))
	if w.Code != 200 {
		t.Fatalf("PUT ui/settings empty = %d, body=%s", w.Code, w.Body)
	}
}

func TestRouterSettingsExtendedFields(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	w := do(t, s, authedRequest("GET", "/api/v1/router/settings", nil))
	var resp routerSettingsResponse
	decodeJSON(t, w.Body, &resp)
	if resp.BusyMode != "wait" || !resp.InjectStreamUsage || resp.CompressorLocalEnabled {
		t.Errorf("defaults = %+v, want wait/true/false", resp)
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/router/settings",
		strings.NewReader(`{"inject_stream_usage":false,"compressor_local_enabled":true}`)))
	if w.Code != 200 {
		t.Fatalf("PUT router/settings (extended) = %d, body=%s", w.Code, w.Body)
	}
	decodeJSON(t, w.Body, &resp)
	if resp.BusyMode != "wait" {
		t.Errorf("BusyMode = %q, want untouched \"wait\"", resp.BusyMode)
	}
	if resp.InjectStreamUsage {
		t.Error("InjectStreamUsage = true, want false after PUT")
	}
	if !resp.CompressorLocalEnabled {
		t.Error("CompressorLocalEnabled = false, want true after PUT")
	}

	raw, err := set.Get(context.Background(), "usage.inject_stream_usage")
	if err != nil || string(raw) != "false" {
		t.Errorf("usage.inject_stream_usage stored = %q err=%v, want \"false\"", raw, err)
	}
}

func TestAuthConfigForwardAuthHeaderProviderConfig(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	w := do(t, s, authedRequest("GET", "/api/v1/auth/config", nil))
	var resp authConfigResponse
	decodeJSON(t, w.Body, &resp)
	if resp.ProviderConfig["header_name"] != "X-Auth-Request-User" {
		t.Errorf("default header_name = %v, want X-Auth-Request-User", resp.ProviderConfig["header_name"])
	}
	if resp.ProviderConfig["trusted_cidrs"] != "127.0.0.0/8" {
		t.Errorf("default trusted_cidrs = %v, want 127.0.0.0/8", resp.ProviderConfig["trusted_cidrs"])
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/auth/config",
		strings.NewReader(`{"provider_config":{"header_name":"X-Forwarded-User","trusted_cidrs":"10.0.0.0/8, 192.168.1.0/24"}}`)))
	if w.Code != 200 {
		t.Fatalf("PUT auth/config (forward_auth_header) = %d, body=%s", w.Code, w.Body)
	}
	decodeJSON(t, w.Body, &resp)
	if resp.ProviderConfig["header_name"] != "X-Forwarded-User" {
		t.Errorf("header_name after PUT = %v", resp.ProviderConfig["header_name"])
	}

	// A malformed CIDR entry in the list must be rejected, not silently
	// dropped (authz.ParseCIDRs would otherwise drop it and narrow the
	// trust list without the operator noticing — the exact lockout risk
	// this validation exists for).
	w = do(t, s, authedRequest("PUT", "/api/v1/auth/config",
		strings.NewReader(`{"provider_config":{"trusted_cidrs":"10.0.0.0/8, not-a-cidr"}}`)))
	if w.Code != 422 {
		t.Fatalf("malformed CIDR entry = %d, want 422", w.Code)
	}
}

func TestRestartRequiredClearedAtBoot(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	do(t, s, authedRequest("PUT", "/api/v1/monitor/settings", strings.NewReader(`{}`)))
	do(t, s, authedRequest("PUT", "/api/v1/router/config", strings.NewReader(`{"health_ttl_s":10}`)))
	if info := s.restartRequired(context.Background()); info == nil {
		t.Fatal("expected restart_required to be set")
	}

	s.ClearRestartRequired(context.Background())
	if info := s.restartRequired(context.Background()); info != nil {
		t.Errorf("expected restart_required cleared, got %+v", info)
	}

	// The signal also surfaces on /status.
	do(t, s, authedRequest("PUT", "/api/v1/router/config", strings.NewReader(`{"health_ttl_s":11}`)))
	w := do(t, s, authedRequest("GET", "/api/v1/status", nil))
	var status statusResponse
	decodeJSON(t, w.Body, &status)
	if status.RestartRequired == nil {
		t.Error("GET /status should surface restart_required once a restart-mode field changed")
	}
}

// TestSchedulerSeedGetPutIsSeedNotLiveOrRestart — Phase 6. infra.scheduler is
// a distinct store key from scheduler.config (the live sched.Config): a
// scheduler/seed PUT must never mark restart_required (it isn't "restart" —
// the running daemon never re-reads it at all) and must never be visible
// through GET /api/v1/scheduler/config (that reads scheduler.config, which
// this endpoint never touches).
func TestSchedulerSeedGetPutIsSeedNotLiveOrRestart(t *testing.T) {
	set := newFakeSettings()
	s := serverWithSettings(t, set)

	w := do(t, s, authedRequest("GET", "/api/v1/scheduler/seed", nil))
	if w.Code != 200 {
		t.Fatalf("GET scheduler/seed = %d, body=%s", w.Code, w.Body)
	}
	var got schedulerConfigResponse
	decodeJSON(t, w.Body, &got)
	if got.IdleUnloadS != schedulerSeedDefaultIdleUnloadS || got.ReservationSoonMin != schedulerSeedDefaultReservationSoonMin {
		t.Errorf("defaults = %+v", got)
	}

	w = do(t, s, authedRequest("PUT", "/api/v1/scheduler/seed", strings.NewReader(`{"idle_unload_s":300}`)))
	if w.Code != 200 {
		t.Fatalf("PUT scheduler/seed = %d, body=%s", w.Code, w.Body)
	}
	var putResp schedulerConfigResponse
	decodeJSON(t, w.Body, &putResp)
	if putResp.IdleUnloadS != 300 {
		t.Errorf("IdleUnloadS = %d, want 300", putResp.IdleUnloadS)
	}
	// Untouched fields keep their defaults, not zero — proves the partial
	// merge, same guarantee as every other group in this file.
	if putResp.SmallJobTokenThreshold != schedulerSeedDefaultSmallJobTokenThreshold {
		t.Errorf("SmallJobTokenThreshold = %d, want untouched default %d", putResp.SmallJobTokenThreshold, schedulerSeedDefaultSmallJobTokenThreshold)
	}

	// Seed mode: never marks restart_required.
	if info := s.restartRequired(context.Background()); info != nil {
		t.Errorf("scheduler seed PUT should not mark restart_required, got %+v", info)
	}

	// A separate key from scheduler.config — GET /scheduler/config must
	// still read its own defaults, unaffected by the seed write above.
	w = do(t, s, authedRequest("GET", "/api/v1/scheduler/config", nil))
	var live schedulerConfigResponse
	decodeJSON(t, w.Body, &live)
	if live.IdleUnloadS != 180 {
		t.Errorf("scheduler/config IdleUnloadS = %d, want untouched default 180 (seed write must not leak into the live key)", live.IdleUnloadS)
	}

	// Out-of-range rejected.
	w = do(t, s, authedRequest("PUT", "/api/v1/scheduler/seed", strings.NewReader(`{"priority_jump_cap":-1}`)))
	if w.Code != 422 {
		t.Fatalf("negative priority_jump_cap = %d, want 422", w.Code)
	}
}
