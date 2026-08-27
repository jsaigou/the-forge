// SPDX-License-Identifier: Apache-2.0

package httpapi

// This file holds the JSON response shapes for Contract 1 §3 — the
// interfaces in web/src/lib/types.ts at the freeze commit. The collector /
// sched / engine / store packages define Go types without JSON tags
// (Contract 2 types are frozen, not wire shapes); these structs translate
// between the two with the snake_case keys the PWA expects.
//
// Field-level notes from Contract 1 §3 that the TS file cannot express
// are enforced here:
//   - Status.slots null vs "" — Go map[string]string with "" = empty becomes
//     null in JSON via slotModeOrNull + slotsOrNull.
//   - SchedulerStatus.idle_seconds null for empty/unknown.
//   - Metrics nullable pointer fields emit null, not 0.
//   - Reservation.start / end are ISO-8601 strings; bay is null unless
//     scope == "bay".

import (
	"encoding/json"
	"time"

	"github.com/jsaigou/the-forge/internal/registry"
)

// sessionInfoResponse mirrors web/src/lib/types.ts SessionInfo.
//
// Sprint 0-AUTH: adds assurance level, the network principal (if this
// session was bootstrapped from a trusted network identity), and the
// effective policy map so the PWA can pre-gate navigation.
type sessionInfoResponse struct {
	CSRFToken        string            `json:"csrf_token"`
	Username         string            `json:"username"`
	Role             string            `json:"role"`
	Assurance        string            `json:"assurance"`
	NetworkPrincipal *string           `json:"network_principal"`
	Policy           map[string]string `json:"policy"`
}

// statusResponse mirrors web/src/lib/types.ts Status.
type statusResponse struct {
	Mode          string                   `json:"mode"`
	Description   string                   `json:"description"`
	Services      map[string]string        `json:"services"`
	Ports         map[string]bool          `json:"ports"`
	Slots         map[string]*string       `json:"slots"`
	SlotLabels    map[string]string        `json:"slot_labels"`
	ModesAvail    map[string]modeAvail     `json:"modes_available"`
	ServiceModes  map[string]svcModeInfo   `json:"service_modes"`
	UI            map[string]any           `json:"ui"`
	Hostname      string                   `json:"hostname"`
	Version       string                   `json:"version"`
	TTSActive     bool                     `json:"tts_active"`
	Switch        switchStateJSON          `json:"switch"`
	SlotLoading   map[string]slotStateJSON `json:"slot_loading"`
	SlotUnloading map[string]slotStateJSON `json:"slot_unloading"`
	Alerts        []alertJSON              `json:"alerts,omitempty"`
	// SlotActivity is additive (Sprint K, 2026-08-05): true while a loaded
	// slot has requests_processing > 0 right now. Sourced from
	// snap.Inference, which already carried RequestsProcessing per slot but
	// had zero readers outside the collector package before this. This is
	// what makes a fresh page load / SSE reconnect correct — the low-latency
	// slot:activity SSE event (bus.go) is the push between polls, not a
	// replacement source of truth. Absent key == not loaded / not scraped
	// yet, same convention as Slots.
	SlotActivity map[string]bool  `json:"slot_activity"`
	Profiling    *profilingStatus `json:"profiling,omitempty"`
	// RestartRequired is additive (Sprint 12, was H): non-nil once any
	// restart-mode settings group (infra.router/server/paths/tailscale,
	// metrics.sample_interval_s) has been saved since the daemon last
	// booted — see infra_handlers.go's markRestartRequired/ClearRestartRequired.
	// Surfaced on /status (already polled by every tab) rather than a new
	// endpoint.
	RestartRequired *restartRequiredInfo `json:"restart_required,omitempty"`
	// SlotConsumers is additive (per-slot consumer attribution): slot id →
	// the human-facing label of whoever most recently generated against it
	// ("Examplehost (OpenCode)", "SMITH", a tailnet IP), within the freshness
	// window (activity.ConsumerFreshness). omitempty — absent when nothing
	// is consuming.
	SlotConsumers map[string]string `json:"slot_consumers,omitempty"`
}

// profilingStatus additive (Sprint K): lets Bay.tsx show a warning-stripe
// overlay on the slots a PROFILE run is currently holding, correctly on a
// fresh page load / SSE reconnect — useProfileActive (web/src/lib/queries.ts)
// is client-cache-only and profile:progress events can be minutes apart
// during a deep depth-sweep fill, so a client that (re)connects mid-run
// would otherwise see nothing until the next progress event.
type profilingStatus struct {
	Running bool   `json:"running"`
	Mode    string `json:"mode,omitempty"`
}

type modeAvail struct {
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Family      string   `json:"family"`
	Tags        []string `json:"tags"`
	Color       string   `json:"color"`
	Icon        string   `json:"icon"`
	Context     int      `json:"context"`
	SlotCapable bool     `json:"slot_capable"`
	Backend     string   `json:"backend"`
}

type svcModeInfo struct {
	Label  string  `json:"label"`
	Icon   string  `json:"icon"`
	Unit   *string `json:"unit"`
	Active bool    `json:"active"`
}

type switchStateJSON struct {
	InProgress bool                 `json:"in_progress"`
	Target     *string              `json:"target"`
	StartedAt  *float64             `json:"started_at"`
	LastResult *lifecycleResultJSON `json:"last_result"`
}

type lifecycleResultJSON struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	NCtx    int    `json:"n_ctx,omitempty"`
}

type slotStateJSON struct {
	InProgress bool     `json:"in_progress"`
	Mode       *string  `json:"mode"`
	StartedAt  *float64 `json:"started_at"`
}

type alertJSON struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Port *int   `json:"port,omitempty"`
}

// metricsResponse mirrors web/src/lib/types.ts Metrics.
//
// Sprint 0 §0.4: Disk is the models/data volume sample (total/free/used/pct).
// Frozen here; the collector does not sample it yet (BE-1 wires that), so the
// handler emits the zero value until then.
//
// A1 (bytes retrofit, 2026-07-24): every memory field is bytes. Wire keys
// changed from *_mb to *_bytes; the PWA types move with them.
type metricsResponse struct {
	Mode              string        `json:"mode"`
	Memory            metricsMemory `json:"memory"`
	CPU               metricsCPU    `json:"cpu"`
	Disk              metricsDisk   `json:"disk"`
	GPUUsePct         *float64      `json:"gpu_use_pct"`
	GTTUsedBytes      *int64        `json:"gtt_used_bytes"`
	GTTTotalBytes     *int64        `json:"gtt_total_bytes"`
	TempCelsius       *float64      `json:"temp_celsius"`
	UptimeSeconds     *int64        `json:"uptime_seconds"`
	InferenceRSSBytes *int64        `json:"inference_rss_bytes"`
	ModelWeightsBytes *int64        `json:"model_weights_bytes"`
	KVCacheBytes      *int64        `json:"kv_cache_bytes"`

	// PackagePowerW is the amdgpu PPT rail in watts (nil = sensor absent).
	// NOT wall power — see /api/v1/cost/summary for the calibrated
	// wall-power estimate. Additive, cost/savings sprint 2026-07-30.
	PackagePowerW *float64 `json:"package_power_w"`

	// NetRxBytesPerSec/NetTxBytesPerSec are a diffed /proc/net/dev rate
	// (nil on the first cycle or if the probe failed — never a false 0).
	// Storage is per-mount (root/models/state), additive alongside the
	// original single-volume Disk above. GPUJunctionTempC/CPUPackageTempC/
	// NVMeTempC are additional hwmon channels beyond TempCelsius (GPU
	// edge). All additive, Phase 4 collector metrics, 2026-08-12.
	NetRxBytesPerSec *float64              `json:"net_rx_bytes_per_sec"`
	NetTxBytesPerSec *float64              `json:"net_tx_bytes_per_sec"`
	Storage          []metricsStorageMount `json:"storage"`
	GPUJunctionTempC *float64              `json:"gpu_junction_temp_celsius"`
	CPUPackageTempC  *float64              `json:"cpu_package_temp_celsius"`
	NVMeTempC        *float64              `json:"nvme_temp_celsius"`
}

// metricsStorageMount mirrors collector.StorageMount (Phase 4, 2026-08-12).
type metricsStorageMount struct {
	Name       string  `json:"name"`
	Path       string  `json:"path"`
	TotalBytes int64   `json:"total_bytes"`
	FreeBytes  int64   `json:"free_bytes"`
	UsedBytes  int64   `json:"used_bytes"`
	Pct        float64 `json:"pct"`
}

type metricsMemory struct {
	TotalBytes int64   `json:"total_bytes"`
	UsedBytes  int64   `json:"used_bytes"`
	AvailBytes int64   `json:"avail_bytes"`
	Pct        float64 `json:"pct"`
}

type metricsCPU struct {
	Load1 float64 `json:"load1"`
	Pct   float64 `json:"pct"`
}

type metricsDisk struct {
	TotalBytes int64   `json:"total_bytes"`
	FreeBytes  int64   `json:"free_bytes"`
	UsedBytes  int64   `json:"used_bytes"`
	Pct        float64 `json:"pct"`
}

// metricsHistoryResponse mirrors web/src/lib/types.ts MetricsHistoryResponse
// (Sprint 0 §0.4). GET /api/v1/metrics/history downsamples the
// metric_samples table (avg per bucket) so a 7-day window returns ~hundreds
// of points. BE-1 implements it; frozen here so FE-2 can build the graph
// against it. A1: series fields are bytes (*_bytes).
type metricsHistoryResponse struct {
	Window      string                `json:"window"`
	ResolutionS int                   `json:"resolution_s"`
	Points      []metricsHistoryPoint `json:"points"`
}

type metricsHistoryPoint struct {
	TS             int64    `json:"ts"`
	GTTUsedBytes   *int64   `json:"gtt_used_bytes,omitempty"`
	GTTTotalBytes  *int64   `json:"gtt_total_bytes,omitempty"`
	GPUUsePct      *float64 `json:"gpu_use_pct,omitempty"`
	MemUsedBytes   *int64   `json:"mem_used_bytes,omitempty"`
	MemTotalBytes  *int64   `json:"mem_total_bytes,omitempty"`
	DiskUsedBytes  *int64   `json:"disk_used_bytes,omitempty"`
	DiskTotalBytes *int64   `json:"disk_total_bytes,omitempty"`

	// PackagePowerW/WallPowerWEst added for ?series=power (cost/savings
	// sprint 2026-07-30). Additive — omitted entirely unless requested, so
	// every pre-existing request/response pair is byte-identical. NOT part
	// of the default series set (parseHistorySeries("")) — that's the
	// detail that keeps this change backward-compatible under the freeze.
	PackagePowerW *float64 `json:"package_power_w,omitempty"`
	WallPowerWEst *float64 `json:"wall_power_w_est,omitempty"`

	// CPUPct/NetRxBytesPerSec/NetTxBytesPerSec added for ?series=cpu and
	// ?series=network (Phase 4 collector metrics, 2026-08-12) — same
	// additive-and-not-in-the-default-set pattern as power above.
	CPUPct           *float64 `json:"cpu_pct,omitempty"`
	NetRxBytesPerSec *float64 `json:"net_rx_bytes_per_sec,omitempty"`
	NetTxBytesPerSec *float64 `json:"net_tx_bytes_per_sec,omitempty"`
}

// schedulerStatusResponse mirrors web/src/lib/types.ts SchedulerStatus.
type schedulerStatusResponse struct {
	Slots       map[string]*string  `json:"slots"`
	SlotLabels  map[string]string   `json:"slot_labels"`
	IdleSeconds map[string]*float64 `json:"idle_seconds"`
	// SlotMemoryBytes is each occupied slot's real live GPU footprint
	// (VRAM+GTT via /proc/<pid>/fdinfo — includes KV cache, not just curated
	// weight-only estimates). Absent/0 for empty slots.
	SlotMemoryBytes map[string]int64 `json:"slot_memory_bytes"`
	// UnitMemoryBytes is NON-slot watched units' real GPU footprints by unit
	// name (S2 attribution: ComfyUI, always-on services, compressor proxies
	// — whoever holds GTT while no slot does). Additive, S2 of the ops
	// sprint series. Slot units never appear here.
	UnitMemoryBytes map[string]int64  `json:"unit_memory_bytes"`
	MemoryBudget    schedBudgetJSON   `json:"memory_budget"`
	Queue           []queueTicketJSON `json:"queue"`
}

type schedBudgetJSON struct {
	TotalBytes int64 `json:"total_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
}

type queueTicketJSON struct {
	TicketID    string  `json:"ticket_id"`
	Model       string  `json:"model"`
	RequestedBy string  `json:"requested_by"`
	TargetSlot  *string `json:"target_slot"`
	Status      string  `json:"status"`
	SmallJob    bool    `json:"small_job,omitempty"`
	EnqueuedAt  float64 `json:"enqueued_at"`
}

// reservationResponse mirrors web/src/lib/types.ts Reservation.
type reservationResponse struct {
	Label                  string  `json:"label"`
	Model                  string  `json:"model"`
	Start                  string  `json:"start"`
	End                    string  `json:"end"`
	Scope                  string  `json:"scope"`
	Bay                    *string `json:"bay"`
	CreatedBy              string  `json:"created_by"`
	AllowAgentReschedule   bool    `json:"allow_agent_reschedule"`
	AllowAgentCancellation bool    `json:"allow_agent_cancellation"`
}

// schedulerConfigResponse mirrors web/src/lib/types.ts SchedulerConfig.
type schedulerConfigResponse struct {
	IdleUnloadS            int `json:"idle_unload_s"`
	SmallJobTokenThreshold int `json:"small_job_token_threshold"`
	PriorityJumpCap        int `json:"priority_jump_cap"`
	ReservationSoonMin     int `json:"reservation_soon_min"`
}

// usageResponse mirrors web/src/lib/types.ts UsageResponse.
//
// Sprint 0 §0.2 (billing & currency freeze): every money field is now
// *_display in DisplayCurrency, external rows also carry cost_native +
// native_currency (as billed), and FX provenance (fx_as_of/fx_stale) rides
// the top-level response. The pre-Sprint-0 cost_usd/local_cost_usd/
// external_cost_usd/total_cost_usd fields are removed. Until BE-2 wires the
// live FX fetch/cache, the handler treats display currency as USD 1:1
// (fx_as_of null, fx_stale false).
type usageResponse struct {
	Window          string               `json:"window"`
	DisplayCurrency string               `json:"display_currency"`
	FxAsOf          *float64             `json:"fx_as_of"` // epoch of FX used; null if 1:1
	FxStale         bool                 `json:"fx_stale"`
	Models          []usageModelRow      `json:"models"`
	External        []usageExternalRow   `json:"external"`
	Compressor      []compressorSavingsRow `json:"compressor"`
	Totals          usageTotals          `json:"totals"`
}

type usageModelRow struct {
	Model            string  `json:"model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	PredictedTokens  int64   `json:"predicted_tokens"`
	PowerCostDisplay float64 `json:"power_cost_display"` // electricity est, in display currency
	LoadsOK          int     `json:"loads_ok"`
	LoadFailures     int     `json:"load_failures"`
	InferenceHangs   int     `json:"inference_hangs"` // now real — see registry.Reliability's doc comment
	KFDEvictions     int     `json:"kfd_evictions"`   // always 0 — not currently detected, see registry.Reliability's doc comment
	Unloads          int     `json:"unloads"`
}

type usageExternalRow struct {
	Provider         string  `json:"provider"`
	Model            string  `json:"model"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int     `json:"requests"`
	CostNative       float64 `json:"cost_native"`     // as billed, in native_currency
	NativeCurrency   string  `json:"native_currency"` // provider's bill currency
	CostDisplay      float64 `json:"cost_display"`    // FX-converted to display currency
	// RequestsUnmetered (cost/savings sprint Phase 4, 2026-07-30) counts
	// requests whose provider response carried no parseable usage object —
	// included in Requests but contributing 0 to every token/cost figure
	// above, so a large RequestsUnmetered relative to Requests is the signal
	// that this row's totals understate real usage, not that usage was
	// genuinely low.
	RequestsUnmetered int `json:"requests_unmetered"`
}

type compressorSavingsRow struct {
	Proxy    string `json:"proxy"`
	TokensIn int64  `json:"tokens_in"`
	Saved    int64  `json:"saved"`
}

type usageTotals struct {
	LocalCostDisplay    float64 `json:"local_cost_display"`
	ExternalCostDisplay float64 `json:"external_cost_display"`
	TotalCostDisplay    float64 `json:"total_cost_display"`
	CompressorSavedTokens int64 `json:"compressor_saved_tokens"`
}

// usageEventResponse mirrors web/src/lib/types.ts UsageEvent.
type usageEventResponse struct {
	TS     float64 `json:"ts"`
	Kind   string  `json:"kind"`
	Model  string  `json:"model,omitempty"`
	Slot   string  `json:"slot,omitempty"`
	Detail string  `json:"detail,omitempty"`
}

// modelCardsResponse mirrors web/src/lib/types.ts {cards, window}. Gains
// display_currency (profiling/pricing sprint 2026-08-07): the currency the
// cards' power_est_per_1m values are denominated in after FX conversion from
// the electricity rate currency — mirrors UsageResponse's display_currency.
type modelCardsResponse struct {
	Cards           []registry.Card `json:"cards"`
	Window          string          `json:"window"`
	DisplayCurrency string          `json:"display_currency"`
}

// configCardsResponse is the GET /api/v1/configs/cards shape (B1: config-scoped).
// display_currency as above.
type configCardsResponse struct {
	Cards           []registry.ConfigCard `json:"cards"`
	Window          string                `json:"window"`
	DisplayCurrency string                `json:"display_currency"`
}

// infraServicesResponse mirrors web/src/lib/types.ts {services: InfraService[]}.
type infraServicesResponse struct {
	Services []infraService `json:"services"`
}

// infraService mirrors web/src/lib/types.ts InfraService.
//
// Sprint 0 §0.5: Detail is a short status line ("passthrough on"), and
// CompressorPassthrough is only meaningful on the A0/LLM-Proxy row. unit/port
// stay on the wire (FE may hide them). BE-4 fills the new fields and fixes
// the A0 active-state wiring; frozen here.
type infraService struct {
	Name                string  `json:"name"`
	Unit                *string `json:"unit"`
	Port                *int    `json:"port"`
	Active              bool    `json:"active"`
	Kind                string  `json:"kind"`
	ModeKey             *string `json:"mode_key"`
	Detail              *string `json:"detail"`
	CompressorPassthrough *bool `json:"compressor_passthrough"`
	// Logo is an Icon manifest slug (web/src/assets/icons/manifest.ts) for
	// the model actually backing this service, nil when none is known.
	// Console-polish pass, 2026-07-31.
	Logo *string `json:"logo"`
	// CompressorState is set only on "Compression (<service>)" rows — one of
	// "compressing" | "bypassed" | "idle" | "down", the same branch logic
	// compressorServiceRows already used to build Detail's prose, exposed as
	// a stable enum so the frontend can aggregate proxy health without
	// pattern-matching a human-readable string. "" on every other row.
	CompressorState string `json:"compressor_state,omitempty"`
	// CompressorResourceHealth (Sprint 4, resource bounding + monitoring) is
	// a sibling to CompressorState, deliberately not merged into it:
	// CompressorState describes routing POSTURE (compressing/bypassed/idle/
	// down); CompressorResourceHealth describes process RESOURCE health
	// (ok/restarting/memory_growth/unknown) — a bypassed proxy can still be
	// restart-looping, and folding the two into one enum would lose that.
	// "" on every non-compressor row. Distinct from smith's
	// "compressor_reachability" check (checks.go) — same name split as the
	// two smith checks it draws from (compressor_health.go vs.
	// runCompressorReachability).
	CompressorResourceHealth string `json:"compressor_resource_health,omitempty"`
	// CompressorRSSBytes / CompressorRestarts back the tooltip — the
	// compressor process's latest observed RSS and systemd's lifetime
	// restart count for its unit. nil when no compressor_samples row exists
	// yet.
	CompressorRSSBytes *int64 `json:"compressor_rss_bytes,omitempty"`
	CompressorRestarts *int64 `json:"compressor_restarts,omitempty"`
}

// compressorConfigResponse mirrors web/src/lib/types.ts CompressorConfig.
type compressorConfigResponse struct {
	Proxies        []compressorProxyJSON  `json:"proxies"`
	Providers      []routerProviderJSON `json:"providers"`
	PassthroughAll bool                 `json:"passthrough_all"`
	// ExternalEnabled reports whether the shared "external" proxy fronts
	// remote providers that have no dedicated proxy of their own
	// (compressor.external_enabled settings-KV — the same key
	// router.externalCompressorEnabled reads). Sprint 10
	// (docs/v5-headroom-replacement.md): the routing diagram needs this to
	// compute each provider's EFFECTIVE proxy — Providers[].CompressorProxy
	// only carries dedicated links, so without this flag the frontend cannot
	// tell a direct route from one that flows through the shared instance.
	ExternalEnabled bool `json:"external_enabled"`
}

type compressorProxyJSON struct {
	ID          int64  `json:"id"`
	Service     string `json:"service"`
	Label       string `json:"label"`
	Port        int    `json:"port"`
	TargetURL   string `json:"target_url"`
	Unit        string `json:"unit"`
	Active      bool   `json:"active"`
	Passthrough bool   `json:"passthrough"`
}

type routerProviderJSON struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	APIKey        string `json:"api_key"` // masked, prefix+ellipsis — never the full secret
	TargetURL     string `json:"target_url"`
	CompressorProxy string `json:"compressor_proxy"`
	Model         string `json:"model"`
	Model2        string `json:"model2"`
}

// routerSettingsResponse mirrors web/src/lib/types.ts RouterSettings. Sprint
// 12 (was H) Phase 2 added InjectStreamUsage/CompressorLocalEnabled — both
// were real settings-KV toggles read per-request by internal/router
// (usage.go's injectStreamUsageEnabled, routing.go's localCompressorEnabled)
// with no HTTP surface at all before this.
type routerSettingsResponse struct {
	BusyMode             string `json:"busy_mode"`
	InjectStreamUsage    bool   `json:"inject_stream_usage"`
	CompressorLocalEnabled bool   `json:"compressor_local_enabled"`
	// ProviderFailover (multi-provider routing sprint, 2026-08-06): when
	// true, a remote request whose provider errors (transport failure/5xx)
	// fails over to the next offering of the same model in priority order.
	ProviderFailover bool `json:"provider_failover"`
}

// routerSettingsBody is the PUT request shape — pointer fields so a partial
// body only touches the settings keys it actually names. BusyMode used to
// be the sole (required) field; a body containing only {"busy_mode":"..."}
// still works exactly as before.
type routerSettingsBody struct {
	BusyMode             *string `json:"busy_mode"`
	InjectStreamUsage    *bool   `json:"inject_stream_usage"`
	CompressorLocalEnabled *bool   `json:"compressor_local_enabled"`
	ProviderFailover     *bool   `json:"provider_failover"`
}

// modesListResponse is the GET /api/v1/modes shape (V4 returns a dict; the
// PWA does not consume this so an array of {name, mode} entries is fine for
// the parity surface — V4 returns the raw config.modes dict).
type modesListResponse struct {
	Modes map[string]any `json:"modes"`
}

// ── Providers (Sprint 0 §0.3, GET /api/v1/providers) ─────────────────────────
//
// Fixes the "provider == model" taxonomy bug: one provider has many models.
// BE-3 fills health (from a configured status page or a live /models probe)
// and credits (per-provider balance API, cached); frozen here so FE-1b builds
// the provider-grouped External-Models view against it. "idle" is dropped.

// providerModelJSON is one entry in a provider's models[] — Phase 7:
// sourced from the real `offerings` table (see providers.go's
// offeringsByProvider), not the always-empty provider_models catalog this
// used to read (dropped, migration 0043). CatalogModelID lets the FE
// deep-link into Catalog → Offerings for the full record.
type providerModelJSON struct {
	ModelID        string  `json:"model_id"` // the offering's wire_model
	CatalogModelID int64   `json:"catalog_model_id"`
	DisplayName    string  `json:"display_name"`
	Logo           string  `json:"logo"` // icon slug (§0.8)
	PriceInPer1M   float64 `json:"price_in_per_1m"`
	PriceOutPer1M  float64 `json:"price_out_per_1m"`
	Currency       string  `json:"currency"` // the offering's own currency
	Priority       int     `json:"priority"`
	Enabled        bool    `json:"enabled"`
	CompressorProxy  *string `json:"compressor_proxy"` // the PROVIDER's linked proxy, if any
	Passthrough    *bool   `json:"passthrough"`    // proxy bypass state, if linked
}

type providerHealthJSON struct {
	State  string   `json:"state"`  // "reachable" | "degraded" | "down" | "unknown"
	AsOf   *float64 `json:"as_of"`  // epoch; null if never checked
	Source string   `json:"source"` // "status_page" | "live_probe" | "none"
	Detail *string  `json:"detail"` // e.g. incident title
}

type providerCreditsJSON struct {
	BalanceNative *float64 `json:"balance_native"`
	Currency      *string  `json:"currency"`
	AsOf          *float64 `json:"as_of"`
	Supported     bool     `json:"supported"` // false ⇒ provider API has no balance endpoint
	// SpendPeriod/SpendPeriodLabel (BE-3 F4 fix, additive to the frozen
	// shape): for providers with no queryable balance but a real usage/cost
	// analytics API (AI& — confirmed, no balance endpoint exists at all;
	// credits are purchased via a web checkout only). SpendPeriodLabel is
	// e.g. "24h spend"; both null when BalanceNative is the relevant figure.
	SpendPeriod      *float64 `json:"spend_period"`
	SpendPeriodLabel *string  `json:"spend_period_label"`
}

type providerJSON struct {
	// ID is the surrogate PK (Phase 6 surrogate-key migration, 0042) — the
	// stable handle that survives a rename. Name stays the display/legacy
	// lookup key; PUT/DELETE /api/v1/providers/{ref} dual-accepts either.
	ID           int64               `json:"id"`
	Name         string              `json:"name"`
	APIKeyMasked string              `json:"api_key_masked"` // never the full secret
	BillCurrency string              `json:"bill_currency"`
	Health       providerHealthJSON  `json:"health"`
	Credits      providerCreditsJSON `json:"credits"`
	Models       []providerModelJSON `json:"models"`

	// BillingEnabled/BillingConsoleURL/CreditsURL (product/QA sprint,
	// 2026-07-29) — user-editable via PUT /api/v1/providers/{name}. See
	// store.ProviderRow's doc comments for what each means.
	BillingEnabled    bool   `json:"billing_enabled"`
	BillingConsoleURL string `json:"billing_console_url,omitempty"`
	CreditsURL        string `json:"credits_url,omitempty"`

	// Enabled (multi-provider routing sprint, 2026-08-06, additive): false =
	// disabled without deletion — the router skips this provider's offerings
	// and the health/credits poller stops refreshing it. Editable via PUT.
	Enabled bool `json:"enabled"`
	// Country/DataResidencyGroup (additive): the provider-level data
	// residency fact (0008 columns, surfaced 2026-08-06). "" when unknown.
	Country            string `json:"country,omitempty"`
	DataResidencyGroup string `json:"data_residency_group,omitempty"`

	// TargetURL/StatusURL/OrgID (additive): surfaced so the FE Edit form
	// can pre-fill them. Not secrets (the preset table publishes them);
	// their omission from the original frozen GET shape was an oversight.
	TargetURL string `json:"target_url,omitempty"`
	StatusURL string `json:"status_url,omitempty"`
	OrgID     string `json:"org_id,omitempty"`
}

type providersResponse struct {
	Providers []providerJSON `json:"providers"`
}

// billingSettingsResponse mirrors GET/PUT /api/v1/billing/settings (§0.9).
type billingSettingsResponse struct {
	DisplayCurrency string  `json:"display_currency"`
	FxSourceURL     *string `json:"fx_source_url,omitempty"`
	FxRefreshMin    *int    `json:"fx_refresh_min,omitempty"`
}

// ── Auth v2 (Sprint 0-AUTH, docs/v5-sprint0-auth-design.md §6) ───────────────
//
// All endpoints below are registered as 501 stubs in this design gate so
// FE-AUTH can build against frozen shapes while BE-AUTH Phase A/B/C lands.

// stepUpRequest mirrors web/src/lib/types.ts StepUpRequest.
type stepUpRequest struct {
	Factor   string `json:"factor"`
	Password string `json:"password,omitempty"`
	Code     string `json:"code,omitempty"`
}

// stepUpResponse mirrors web/src/lib/types.ts StepUpResponse.
type stepUpResponse struct {
	Assurance   string   `json:"assurance"`
	AssuranceAt *float64 `json:"assurance_at"`
}

// webauthnCredentialJSON mirrors web/src/lib/types.ts WebAuthnCredential.
type webauthnCredentialJSON struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Transports []string `json:"transports"`
	CreatedAt  float64  `json:"created_at"`
	LastUsedAt *float64 `json:"last_used_at,omitempty"`
}

// webauthnCredentialsResponse mirrors web/src/lib/types.ts WebAuthnCredentialsResponse.
type webauthnCredentialsResponse struct {
	Credentials []webauthnCredentialJSON `json:"credentials"`
}

// webauthnPublicKeyCredentialDescriptorJSON mirrors
// web/src/lib/types.ts PublicKeyCredentialDescriptorJSON.
type webauthnPublicKeyCredentialDescriptorJSON struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"`
	Transports []string `json:"transports,omitempty"`
}

// webauthnPubKeyCredParamJSON mirrors web/src/lib/types.ts PubKeyCredParam.
type webauthnPubKeyCredParamJSON struct {
	Type string `json:"type"`
	Alg  int    `json:"alg"`
}

// webauthnRelyingPartyJSON mirrors web/src/lib/types.ts WebAuthnRelyingParty.
type webauthnRelyingPartyJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// webauthnUserJSON mirrors web/src/lib/types.ts WebAuthnUser.
type webauthnUserJSON struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// webauthnAuthenticatorSelectionJSON mirrors
// web/src/lib/types.ts AuthenticatorSelectionCriteria.
type webauthnAuthenticatorSelectionJSON struct {
	AuthenticatorAttachment string `json:"authenticator_attachment,omitempty"`
	ResidentKey             string `json:"resident_key,omitempty"`
	UserVerification        string `json:"user_verification,omitempty"`
}

// webauthnCreationOptionsJSON mirrors
// web/src/lib/types.ts PublicKeyCredentialCreationOptionsJSON. Keys follow the
// WebAuthn JSON serialization spec (camelCase) so the PWA can pass the object
// directly to navigator.credentials.create({ publicKey: options }).
type webauthnCreationOptionsJSON struct {
	RP                     webauthnRelyingPartyJSON                    `json:"rp"`
	User                   webauthnUserJSON                            `json:"user"`
	Challenge              string                                      `json:"challenge"`
	PubKeyCredParams       []webauthnPubKeyCredParamJSON               `json:"pubKeyCredParams"`
	Timeout                int                                         `json:"timeout,omitempty"`
	ExcludeCredentials     []webauthnPublicKeyCredentialDescriptorJSON `json:"excludeCredentials,omitempty"`
	AuthenticatorSelection *webauthnAuthenticatorSelectionJSON         `json:"authenticatorSelection,omitempty"`
	Attestation            string                                      `json:"attestation,omitempty"`
}

// webauthnBeginRegisterResponse mirrors
// web/src/lib/types.ts WebAuthnBeginRegisterResponse.
type webauthnBeginRegisterResponse struct {
	Options webauthnCreationOptionsJSON `json:"options"`
}

// webauthnRegResponseDataJSON mirrors the nested response object in
// web/src/lib/types.ts WebAuthnRegistrationResponseJSON.
type webauthnRegResponseDataJSON struct {
	ClientDataJSON    string   `json:"clientDataJSON"`
	AttestationObject string   `json:"attestationObject"`
	AuthenticatorData string   `json:"authenticatorData,omitempty"`
	Transports        []string `json:"transports,omitempty"`
}

// webauthnRegistrationResponseJSON mirrors
// web/src/lib/types.ts WebAuthnRegistrationResponseJSON.
type webauthnRegistrationResponseJSON struct {
	ID                     string                      `json:"id"`
	RawID                  string                      `json:"rawId"`
	Type                   string                      `json:"type"`
	Response               webauthnRegResponseDataJSON `json:"response"`
	ClientExtensionResults map[string]any              `json:"clientExtensionResults,omitempty"`
}

// webauthnFinishRegisterRequest mirrors
// web/src/lib/types.ts WebAuthnFinishRegisterRequest.
type webauthnFinishRegisterRequest struct {
	Response webauthnRegistrationResponseJSON `json:"response"`
	Label    string                           `json:"label"`
}

// webauthnFinishRegisterResponse mirrors
// web/src/lib/types.ts WebAuthnFinishRegisterResponse.
type webauthnFinishRegisterResponse struct {
	Credential webauthnCredentialJSON `json:"credential"`
}

// webauthnRequestOptionsJSON mirrors
// web/src/lib/types.ts PublicKeyCredentialRequestOptionsJSON. Keys follow the
// WebAuthn JSON serialization spec so the PWA can pass the object to
// navigator.credentials.get({ publicKey: options }).
type webauthnRequestOptionsJSON struct {
	Challenge        string                                      `json:"challenge"`
	Timeout          int                                         `json:"timeout,omitempty"`
	RPID             string                                      `json:"rpId,omitempty"`
	AllowCredentials []webauthnPublicKeyCredentialDescriptorJSON `json:"allowCredentials,omitempty"`
	UserVerification string                                      `json:"userVerification,omitempty"`
	Extensions       map[string]any                              `json:"extensions,omitempty"`
}

// webauthnBeginAssertResponse mirrors
// web/src/lib/types.ts WebAuthnBeginAssertResponse.
type webauthnBeginAssertResponse struct {
	Options webauthnRequestOptionsJSON `json:"options"`
}

// webauthnAuthResponseDataJSON mirrors the nested response object in
// web/src/lib/types.ts WebAuthnAuthenticationResponseJSON.
type webauthnAuthResponseDataJSON struct {
	ClientDataJSON    string `json:"clientDataJSON"`
	AuthenticatorData string `json:"authenticatorData"`
	Signature         string `json:"signature"`
	UserHandle        string `json:"userHandle,omitempty"`
}

// webauthnAuthenticationResponseJSON mirrors
// web/src/lib/types.ts WebAuthnAuthenticationResponseJSON.
type webauthnAuthenticationResponseJSON struct {
	ID                     string                       `json:"id"`
	RawID                  string                       `json:"rawId"`
	Type                   string                       `json:"type"`
	Response               webauthnAuthResponseDataJSON `json:"response"`
	ClientExtensionResults map[string]any               `json:"clientExtensionResults,omitempty"`
}

// webauthnFinishAssertRequest mirrors
// web/src/lib/types.ts WebAuthnFinishAssertRequest.
type webauthnFinishAssertRequest struct {
	Response webauthnAuthenticationResponseJSON `json:"response"`
}

// webauthnFinishAssertResponse mirrors
// web/src/lib/types.ts WebAuthnFinishAssertResponse.
type webauthnFinishAssertResponse struct {
	Verified  bool   `json:"verified"`
	Assurance string `json:"assurance,omitempty"`
}

// totpEnrollResponse mirrors web/src/lib/types.ts TotpEnrollResponse.
type totpEnrollResponse struct {
	Secret     string `json:"secret"`
	OTPAuthURI string `json:"otpauth_uri"`
}

// totpConfirmRequest mirrors web/src/lib/types.ts TotpConfirmRequest.
type totpConfirmRequest struct {
	Code string `json:"code"`
}

// totpConfirmResponse mirrors web/src/lib/types.ts TotpConfirmResponse.
type totpConfirmResponse struct {
	Active bool `json:"active"`
}

// identityLinkResponse mirrors web/src/lib/types.ts IdentityLink.
type identityLinkResponse struct {
	Provider  string  `json:"provider"`
	Principal string  `json:"principal"`
	UserID    int64   `json:"user_id"`
	CreatedAt float64 `json:"created_at"`
}

// identityLinksResponse mirrors web/src/lib/types.ts IdentityLinksResponse.
type identityLinksResponse struct {
	Links []identityLinkResponse `json:"links"`
}

// identityLinkCreateRequest mirrors web/src/lib/types.ts IdentityLinkCreateRequest.
type identityLinkCreateRequest struct {
	Provider  string `json:"provider"`
	Principal string `json:"principal"`
	UserID    int64  `json:"user_id"`
}

// apiKeyResponse mirrors web/src/lib/types.ts APIKey. The secret token is never
// returned on this shape (only once on create).
type apiKeyResponse struct {
	KeyID      string   `json:"keyid"`
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	Role       string   `json:"role,omitempty"`
	// Operator's preferred consumer label ("" = derive at request time).
	DisplayName string   `json:"display_name,omitempty"`
	// BoundIP is the exact client IP this key verifies from; "" = unbound
	// (security sprint 3, #34).
	BoundIP    string   `json:"bound_ip,omitempty"`
	CreatedAt  float64  `json:"created_at"`
	LastUsedAt *float64 `json:"last_used_at,omitempty"`
	RevokedAt  *float64 `json:"revoked_at,omitempty"`
	// ExpiresAt is unset for a key that never expires (security sprint 3, #36).
	ExpiresAt *float64 `json:"expires_at,omitempty"`
}

// apiKeysResponse mirrors web/src/lib/types.ts APIKeysResponse.
type apiKeysResponse struct {
	Keys []apiKeyResponse `json:"keys"`
}

// apiKeyCreateRequest mirrors web/src/lib/types.ts APIKeyCreateRequest.
type apiKeyCreateRequest struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
	// DisplayName is the operator's preferred consumer label (optional) —
	// used verbatim in slot-consumer attribution when set.
	DisplayName string `json:"display_name,omitempty"`
	// BindToRequester binds the new key to this mint request's own resolved
	// client IP (effectiveClientIP — tailscale-serve aware); false (the
	// default when omitted) mints an unbound key. Security sprint 3, #34.
	BindToRequester bool `json:"bind_to_requester,omitempty"`
	// TTLSeconds sets the key to expire that many seconds from now; 0/omitted
	// mints a key that never expires. Security sprint 3, #36.
	TTLSeconds int64 `json:"ttl_seconds,omitempty"`
}

// apiKeyCreateResponse mirrors web/src/lib/types.ts APIKeyCreateResponse. The
// token field carries the secret exactly once; the key object never includes it.
type apiKeyCreateResponse struct {
	Token string         `json:"token"`
	Key   apiKeyResponse `json:"key"`
}

// authPolicyResponse mirrors web/src/lib/types.ts AuthPolicyResponse.
type authPolicyResponse struct {
	Policy map[string]string `json:"policy"`
}

// authPolicyPutRequest mirrors web/src/lib/types.ts AuthPolicyPutRequest.
type authPolicyPutRequest struct {
	Policy map[string]string `json:"policy"`
}

// authConfigResponse mirrors web/src/lib/types.ts AuthConfig.
type authConfigResponse struct {
	NetworkProvider    string         `json:"network_provider"`
	ProviderConfig     map[string]any `json:"provider_config,omitempty"`
	WebAuthnRPID       string         `json:"webauthn_rp_id,omitempty"`
	WebAuthnRPName     string         `json:"webauthn_rp_name,omitempty"`
	StepUpTTLMin       int            `json:"step_up_ttl_min,omitempty"`
	NetworkDefaultRole string         `json:"network_default_role,omitempty"`
	A0TailnetBypass    bool           `json:"a0_tailnet_bypass,omitempty"`
}

// authConfigPutRequest mirrors web/src/lib/types.ts AuthConfigPutRequest.
type authConfigPutRequest struct {
	NetworkProvider    string         `json:"network_provider"`
	ProviderConfig     map[string]any `json:"provider_config,omitempty"`
	WebAuthnRPID       string         `json:"webauthn_rp_id,omitempty"`
	WebAuthnRPName     string         `json:"webauthn_rp_name,omitempty"`
	StepUpTTLMin       int            `json:"step_up_ttl_min,omitempty"`
	NetworkDefaultRole string         `json:"network_default_role,omitempty"`
	A0TailnetBypass    bool           `json:"a0_tailnet_bypass,omitempty"`
}

// ── Recovery codes (Phase C, §8) ──────────────────────────────────────────────

// recoveryCodesGenerateResponse mirrors the response from
// POST /api/v1/auth/recovery-codes/generate. The codes field carries the
// plaintext codes exactly once (never returned on subsequent requests).
type recoveryCodesGenerateResponse struct {
	Codes []string `json:"codes"`
	Total int      `json:"total"`
}

// recoveryCodesStatusResponse mirrors GET /api/v1/auth/recovery-codes.
type recoveryCodesStatusResponse struct {
	HasCodes bool `json:"has_codes"`
	Unused   int  `json:"unused"`
	Total    int  `json:"total"`
}

// ── Conversion helpers ───────────────────────────────────────────────────────

// unixSeconds returns the float epoch for t (0 for zero time).
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

// isoFormat returns the ISO-8601 string for t ("" for zero time).
func isoFormat(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// slotModeOrNull returns &mode when mode != "", else nil.
// Used so that Status.slots emits null for empty slots (Contract 1 §3).
func slotModeOrNull(mode string) *string {
	if mode == "" {
		return nil
	}
	v := mode
	return &v
}

// ptrString returns &v with nil for empty.
func ptrString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// ptrInt returns &v.
func ptrInt(v int) *int { return &v }

// maskSecret returns a prefix+ellipsis mask of a secret (Contract 1 §18:
// "must not leak proxy tokens or full API keys"). Returns "" when the
// secret is empty.
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	const prefix = 4
	if len(s) <= prefix {
		return s + "…"
	}
	return s[:prefix] + "…"
}

// encodeSSEEvent renders the wire form of one SSE event:
//
//	event: <name>\n
//	data: <json>\n\n
//
// Pre-encoded (json.RawMessage) data is written as-is; other types are
// JSON-marshaled. Errors are surfaced so the SSE loop can drop the event
// rather than emit a malformed one.
func encodeSSEEvent(name string, data any) ([]byte, error) {
	var raw []byte
	if b, ok := data.(json.RawMessage); ok {
		raw = b
	} else {
		b, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return []byte("event: " + name + "\ndata: " + string(raw) + "\n\n"), nil
}
