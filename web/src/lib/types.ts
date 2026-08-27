// Mirrors the JSON shapes returned by foundry/app.py — see docs/scheduler.md,
// docs/rewrite-plan.md Phase 4/5/6, and the route handlers themselves for the
// authoritative definitions. Kept intentionally loose (optional fields) where
// the backend already documents a field as "may be null."

export type Role = "viewer" | "operator" | "admin";

export interface SessionInfo {
  csrf_token: string;
  username: string;
  role: Role;
  assurance: AuthFactor;
  network_principal: string | null;
  policy: Record<string, AuthFactor>;
}

export interface SlotLoadingState {
  in_progress: boolean;
  mode: string | null;
  started_at: number | null;
}

export interface ModeAvailable {
  label: string;
  description: string;
  family: string;
  tags: string[];
  color: string;
  icon: string;
  context: number;
  slot_capable: boolean;
  backend: string;
}

export interface ServiceModeInfo {
  label: string;
  icon: string;
  unit: string | null;
  active: boolean;
}

export interface Status {
  mode: string;
  description: string;
  services: Record<string, string>;
  ports: Record<string, boolean>;
  slots: Record<string, string | null>;
  slot_labels: Record<string, string>;
  modes_available: Record<string, ModeAvailable>;
  service_modes: Record<string, ServiceModeInfo>;
  ui: Record<string, unknown>;
  hostname: string;
  version: string;
  tts_active: boolean;
  switch: {
    in_progress: boolean;
    target: string | null;
    started_at: number | null;
    last_result: { success: boolean; message: string } | null;
  };
  slot_loading: Record<string, SlotLoadingState>;
  slot_unloading: Record<string, SlotLoadingState>;
  // alerts is superseded by GET /api/v1/notifications (product/QA sprint,
  // 2026-07-29) — that endpoint persists + dedupes the same collector
  // signal with ack/dismiss, which this bare array never had. Kept on the
  // wire for backward compat; the FE reads notifications, not this.
  alerts?: Array<{ code: string; msg: string; port?: number }>;
  // Sprint K (2026-08-05): true while a loaded slot has requests_processing
  // > 0 right now. Absent key == not loaded / not scraped yet, same
  // convention as `slots`. This is the poll/reconnect source of truth; the
  // low-latency slot:activity SSE event (lib/sse.ts) is the push between
  // polls, merged into this same field rather than replacing it.
  slot_activity: Record<string, boolean>;
  // Sprint S-B: per-slot consumer attribution for active generations —
  // "Examplehost (OpenCode)" style labels; "SMITH" when smith's brain holds the
  // slot. Empty/absent when idle or unattributed.
  slot_consumers?: Record<string, string>;
  // Additive, omitted entirely when no PROFILE run is in progress — lets
  // Bay.tsx show which slots a run is holding correctly on a fresh page
  // load / SSE reconnect, not just while profile:progress events are live.
  profiling?: { running: boolean; mode?: string };
  // Sprint 12 (was H): non-nil once any apply="restart" settings group has
  // been saved since the daemon last booted — cleared server-side right
  // after config.LoadFromStore on every fresh start. `keys` are the raw
  // settings-store keys touched (e.g. "infra.router"), not field names.
  restart_required?: RestartRequiredInfo;
}

export interface RestartRequiredInfo {
  keys: string[];
  since: string; // ISO 8601
  by: string;
}

// GET /api/v1/notifications (product/QA sprint, 2026-07-29 — Dashboard
// notifications panel). Severity vocabulary: info | warn | crit.
export interface NotificationItem {
  id: number;
  code: string;
  severity: "info" | "warn" | "crit";
  subject?: string;
  message: string;
  first_seen: number;
  last_seen: number;
  occurrences: number;
  acknowledged_at?: number;
  dismissed_at?: number;
}

export interface NotificationsResponse {
  notifications: NotificationItem[];
}

export interface Metrics {
  mode: string;
  memory: { total_bytes: number; used_bytes: number; avail_bytes: number; pct: number };
  cpu: { load1: number; pct: number };
  disk: { total_bytes: number; free_bytes: number; used_bytes: number; pct: number }; // Sprint 0 §0.4
  gpu_use_pct: number | null;
  gtt_used_bytes: number | null;
  gtt_total_bytes: number | null;
  temp_celsius: number | null;
  uptime_seconds: number | null;
  inference_rss_bytes: number | null;
  model_weights_bytes: number | null;
  kv_cache_bytes: number | null;
  // Cost/savings sprint, 2026-07-30: amdgpu PPT package rail in watts (nil =
  // sensor absent). NOT wall power — see CostSummary.energy.wall_wh_est for
  // the calibrated estimate.
  package_power_w: number | null;
  // Phase 4 collector metrics (2026-08-12): net_rx/tx are a diffed
  // /proc/net/dev rate (null on the first cycle or a probe failure — never a
  // false 0). storage is per-mount, additive alongside the original single-
  // volume `disk` above. gpu_junction/cpu_package/nvme are additional hwmon
  // channels beyond temp_celsius (GPU edge) — any of them can be genuinely
  // null when the hardware exposes no such sensor (e.g. this host has no
  // junction/hotspot channel), not a probe bug.
  net_rx_bytes_per_sec: number | null;
  net_tx_bytes_per_sec: number | null;
  storage: StorageMount[];
  gpu_junction_temp_celsius: number | null;
  cpu_package_temp_celsius: number | null;
  nvme_temp_celsius: number | null;
}

// Phase 4 collector metrics (2026-08-12): mirrors go's metricsStorageMount.
export interface StorageMount {
  name: string;
  path: string;
  total_bytes: number;
  free_bytes: number;
  used_bytes: number;
  pct: number;
}

// Sprint 0 §0.4: GET /api/v1/metrics/history — server-downsampled time series
// for the Dashboard 7-day graph. Fields on each point are optional (only the
// requested series are populated). A1 bytes retrofit: series are bytes.
// Cost/savings sprint Phase 1/2: package_power_w/wall_power_w_est require
// `series=power` explicitly — never in the default series set.
export interface MetricsHistoryPoint {
  ts: number;
  gtt_used_bytes?: number;
  gtt_total_bytes?: number;
  gpu_use_pct?: number;
  mem_used_bytes?: number;
  mem_total_bytes?: number;
  disk_used_bytes?: number;
  disk_total_bytes?: number;
  package_power_w?: number;
  wall_power_w_est?: number;
  // Phase 4 collector metrics (2026-08-12): additive, `?series=cpu`/
  // `?series=network` only — same not-in-the-default-set pattern as power
  // above.
  cpu_pct?: number;
  net_rx_bytes_per_sec?: number;
  net_tx_bytes_per_sec?: number;
}

export interface MetricsHistoryResponse {
  window: string;
  resolution_s: number;
  points: MetricsHistoryPoint[];
}

export interface SchedulerStatus {
  slots: Record<string, string | null>;
  slot_labels: Record<string, string>;
  idle_seconds: Record<string, number | null>;
  // Each occupied slot's real live GPU footprint (VRAM+GTT via
  // /proc/<pid>/fdinfo — includes KV cache, not the curated weights-only
  // estimate configCards' derived.memory_req_bytes carries). Absent/0 for
  // empty slots or when the probe couldn't read it.
  slot_memory_bytes: Record<string, number>;
  // NON-slot watched units' real GPU footprints by unit name (ComfyUI,
  // always-on services, compressor proxies — whoever holds GTT while no
  // slot does). Additive S2 attribution; slot units never appear here.
  unit_memory_bytes: Record<string, number>;
  memory_budget: { total_bytes: number; used_bytes: number; free_bytes: number };
  queue: QueueTicket[];
}

export interface QueueTicket {
  ticket_id: string;
  model: string;
  requested_by: string;
  target_slot?: string | null;
  status: string; // "queued" | "loading" | ...
  small_job?: boolean;
  enqueued_at: number;
}

// Sprint 0 §0.2 (billing/currency freeze): local models carry an electricity
// "power estimate" in display currency; external models carry native + display
// cost. All money is *_display in UsageResponse.display_currency.
export interface UsageModelRow {
  model: string;
  prompt_tokens: number;
  predicted_tokens: number;
  power_cost_display: number; // electricity est, in display currency
  loads_ok: number;
  load_failures: number;
  inference_hangs: number;
  kfd_evictions: number;
  unloads: number;
}

export interface UsageExternalRow {
  provider: string;
  model: string;
  prompt_tokens: number;
  completion_tokens: number;
  requests: number;
  cost_native: number; // as billed, in native_currency
  native_currency: string; // provider's bill currency
  cost_display: number; // FX-converted to display currency
  // Phase 4 (cost/savings sprint): included in `requests`, contributes 0 to
  // every token/cost figure above — the provider response had no parseable
  // usage object. A visible known-unknown, not a silent zero.
  requests_unmetered: number;
}

export interface UsageResponse {
  window: string;
  display_currency: string; // Sprint 0 §0.2
  fx_as_of: number | null; // epoch of FX used; null if 1:1
  fx_stale: boolean;
  models: UsageModelRow[];
  external: UsageExternalRow[];
  compressor: CompressorSavingsRow[];
  totals: {
    local_cost_display: number;
    external_cost_display: number;
    total_cost_display: number;
    compressor_saved_tokens: number;
  };
}

export interface UsageEvent {
  ts: number;
  kind: string;
  model?: string;
  slot?: string;
  detail?: string;
}

// Sprint L — Dashboard Overview activity heatmap. Dense: every calendar day
// in [window ago, today] gets a row, even ones with zero traffic, so the
// client never has to gap-fill a GitHub-style grid.
export interface UsageHeatmapDay {
  date: string; // YYYY-MM-DD, calendar day in `tz`
  tokens: number;
  requests: number;
  // Local/external split for the ALL/Local/External toggle. Additive to
  // tokens/requests above (local + external == the combined total).
  tokens_local: number;
  tokens_external: number;
  requests_local: number;
  requests_external: number;
}

export interface UsageHeatmapResponse {
  window: string;
  tz: string;
  days: UsageHeatmapDay[];
}

export interface ModelCapability {
  id: string;
  label: string;
  score: number;
  benchmark: string;
}

// Sprint 0 §0.7: normalized, derived badge (inline icon + tooltip). icon is an
// icon-system slug (§0.8); "" ⇒ generic text badge.
export interface Badge {
  id: string;
  label: string;
  icon: string;
}

// GET /api/v1/favorites (product/QA sprint, 2026-07-29 — Console config-
// card starring). subject_ids are config ids (ConfigCard.id).
export interface FavoritesResponse {
  subject_ids: number[];
}

export interface ModelCard {
  id: string;
  name: string;
  creator: string;
  license_name: string;
  license_url: string;
  description: string;
  key_features: string[];
  badges: Badge[]; // Sprint 0 §0.7
  // Sprint J1: what this model's architecture supports (subset of
  // "text" | "vision" | "audio"), independent of any one config's ability
  // to deliver it — see ConfigCard.modalities for the narrowed, config-
  // scoped view.
  modalities: string[];
  logo: string;
  logo_dark: string; // dark-theme variant (Phase 3); "" = same as logo
  hf_repo: string;
  family: string;
  // product/QA sprint, 2026-07-29 — the level above family (a vendor's own
  // release lineage, e.g. Nemotron, of which "Nemotron 3" is one family/
  // generation). "" when the model's family has none set.
  genealogy: string;
  modes: string[];
  capabilities: ModelCapability[];
  performance: {
    measured_ts: number | null;
    // prefill_ts (Compressor local-savings prefill sprint, 2026-08-06): the
    // curated catalog prefill_tps benchmark, when present. Distinct from
    // measured_ts (decode_tps) — the two aren't reliably ordered on this
    // hardware, never substitute one for the other.
    prefill_ts: number | null;
    memory_req_bytes: number | null;
    power_est_per_1m: number | null; // electricity est per 1M tokens, FX-converted to display_currency (profiling/pricing sprint 2026-08-07: was "USD/1M"; now display-currency-denominated)
    power_cost_per_1k: number; // deprecated; kept for back-compat
  };
  // Sprint 0 §0.7 / decision 3: stability_score dropped from card output.
  quality: {
    is_abliterated: boolean | null;
    abliteration_quality: string;
  };
  derived: {
    arch: string | null;
    trained_ctx: number | null;
    file_size_bytes: number | null;
    memory_req_bytes: number | null;
    history: {
      last_result: string | null;
      ctx_reduction_rate: number;
      avg_load_time_s: number | null;
      trained_ctx: number | null;
    } | null;
    reliability: {
      loads_ok: number;
      load_failures: number;
      inference_hangs: number;
      kfd_evictions: number;
    } | null;
  };
}

// Phase B (B1): config-scoped card — one per Config. GET /api/v1/configs/cards.
// Denormalizes model identity + variant quality + performance benchmarks +
// live-derived data (GGUF metadata, file size, history, reliability) for
// the console "Choose a config" gallery.
export interface ConfigCard {
  id: number;
  name: string;
  model_id: string;
  n_ctx: number;
  status: string; // "unverified" | "verified"
  visibility: string; // "visible" | "hidden"
  is_default: boolean;
  created_at: number; // unix seconds

  // Model identity (denormalized).
  model_name: string;
  creator: string;
  license_name: string;
  license_url: string;
  description: string;
  key_features: string[];
  badges: Badge[];
  // Sprint J1: what THIS config can actually deliver — the model's
  // modalities narrowed by mmproj presence and any explicit override.
  // modalities_unavailable names modalities the model supports that this
  // config can't, with a human reason (missing mmproj, mmproj file missing
  // on disk, or an explicit override) — render as a visible, fixable gap
  // rather than omitting silently.
  modalities: string[];
  modalities_unavailable: { id: string; reason: string }[];
  logo: string;
  logo_dark: string; // dark-theme variant (Phase 3); "" = same as logo
  hf_repo: string;
  family: string;
  capabilities: ModelCapability[];

  // Sprint B: the load recipe (config expanded view's "load options" list).
  // extra_args is the flat llama.cpp argv token array — the only source of
  // truth for --parallel and friends (store's `parallel` column is a dead
  // field, never read by the launcher — see llamaFlags.ts). backend is
  // rocm|vulkan|vllm from the linked Build.
  extra_args: string[];
  backend: string;
  variant_name: string;

  // Variant quality.
  quality: {
    is_abliterated: boolean | null;
    abliteration_quality: string;
  };

  // Performance (from benchmarks: decode_tps, prefill_tps, safe_memory_bytes).
  performance: {
    measured_ts: number | null;
    prefill_ts: number | null; // see ConfigCard.performance.prefill_ts
    memory_req_bytes: number | null;
    power_est_per_1m: number | null; // electricity est per 1M tokens, display-currency-denominated (2026-08-07)
    power_cost_per_1k: number; // deprecated, always 0
  };

  // Derived (live).
  derived: {
    arch: string | null;
    trained_ctx: number | null;
    file_size_bytes: number | null;
    memory_req_bytes: number | null;
    history: {
      last_result: string | null;
      ctx_reduction_rate: number;
      avg_load_time_s: number | null;
      trained_ctx: number | null;
    } | null;
    reliability: {
      loads_ok: number;
      load_failures: number;
      inference_hangs: number;
      kfd_evictions: number;
    } | null;
  };
}

export interface Reservation {
  label: string;
  model: string;
  start: string;
  end: string;
  scope: "bay" | "whole_box" | "comfyui";
  bay: string | null;
  created_by: string;
  allow_agent_reschedule: boolean;
  allow_agent_cancellation: boolean;
}

export interface SchedulerConfig {
  idle_unload_s: number;
  small_job_token_threshold: number;
  priority_jump_cap: number;
  reservation_soon_min: number;
}

// P3 scheduler jobs: cron-style forced loads (forge/p3sched track).
export interface SchedulerJob {
  id: number;
  name: string;
  cron: string;
  config_name: string;
  slot: string | null; // null = scheduler chooses
  enabled: boolean;
  last_run_at: string | null; // ISO; null = never fired
  next_run_at: string | null; // ISO; null = unscheduled
  created_by: string | null;
  created_at: string | null;
}

export interface SchedulerJobsResponse {
  jobs: SchedulerJob[];
  total: number;
}

export interface SchedulerJobInput {
  name: string;
  cron: string;
  config_name: string;
  slot?: string | null;
  enabled?: boolean;
}

export interface CompressorProxy {
  id: number;
  service: string;
  label: string;
  port: number;
  target_url: string;
  unit: string;
  active: boolean;
  orphaned?: boolean;
  orphaned_days_left?: number;
  passthrough: boolean; // Phase 8: true if bypassed (global or this proxy individually)
}

export interface RouterProvider {
  id: number;
  name: string;
  api_key: string;
  target_url: string;
  compressor_proxy: string;
  model: string;
  model2: string;
}

export interface CompressorConfig {
  proxies: CompressorProxy[];
  providers: RouterProvider[];
  passthrough_all: boolean; // Phase 8: global master bypass switch
  // Sprint 10 (docs/v5-headroom-replacement.md): the live
  // compressor.external_enabled setting. providers[].compressor_proxy only
  // carries DEDICATED links, so without this flag the routing diagram cannot
  // compute a provider's effective proxy (dedicated vs shared "external"
  // fallback vs direct) the way the router's remoteCompressorBaseURL does.
  external_enabled: boolean;
}

export interface CompressorSavingsRow {
  proxy: string;
  tokens_in: number;
  saved: number;
}

export interface InfraService {
  name: string;
  unit: string | null; // still on the wire; FE may hide it
  port: number | null; // still on the wire; FE hides it (Sprint 0 §0.5)
  active: boolean;
  kind: "systemd" | "service_mode";
  mode_key: string | null; // config.toml [modes.<mode_key>] key, when kind === "service_mode"
  detail: string | null; // Sprint 0 §0.5: short status line, e.g. "passthrough on"
  compressor_passthrough: boolean | null; // Sprint 0 §0.5: only meaningful on the A0/LLM-Proxy row
  // Icon manifest slug (web/src/assets/icons/manifest.ts) for the model
  // actually backing this service, null when none is known. Console
  // service-chip polish, 2026-07-31.
  logo: string | null;
  // Set only on "Compressor (<service>)" rows — lets the frontend aggregate
  // per-proxy health into one indicator without parsing `detail`'s prose.
  // "" (absent) on every other row.
  compressor_state?: "compressing" | "bypassed" | "idle" | "down" | "";
  // Sprint 4 (resource bounding + monitoring): sibling to compressor_state,
  // deliberately not merged — compressor_state is routing POSTURE
  // (compressing/bypassed/idle/down), compressor_resource_health is process RESOURCE
  // health (a bypassed proxy can still be restart-looping). "" (absent) on
  // every non-Compressor row.
  compressor_resource_health?: "unknown" | "ok" | "restarting" | "memory_growth" | "";
  // Back the tooltip — the compressor process's latest observed RSS and
  // systemd's lifetime restart count for its unit. null/undefined when no
  // compressor_samples row exists yet.
  compressor_rss_bytes?: number | null;
  compressor_restarts?: number | null;
}

// RouterSettings mirrors go/internal/httpapi/shapes.go's
// routerSettingsResponse — independent settings-KV toggles, all
// apply="immediate" (read fresh per request by internal/router). Sprint 12
// (was H) Phase 2 added inject_stream_usage/compressor_local_enabled on the
// backend; this type only grew the matching fields in Phase 6, when the
// Settings "routing" panel became the endpoint's first real frontend caller
// (PUT /api/v1/router/settings was an orphaned endpoint before this — the
// backend worked, nothing called it). provider_failover (multi-provider
// routing sprint, 2026-08-06) is the same shape: when true, a remote
// request whose provider errors fails over to the next offering of the same
// model in priority order.
export interface RouterSettings {
  busy_mode: string;
  inject_stream_usage: boolean;
  compressor_local_enabled: boolean;
  provider_failover: boolean;
}

export type RouterSettingsUpdate = Partial<RouterSettings>;

// RouterConfig mirrors routerConfigResponse (GET/PUT /api/v1/router/config,
// infra.router) — all apply="restart", since router.Deps.Cfg is constructed
// once in main.go and never re-read (see the sprint plan's apply-modes
// table). Deliberately excludes listen_port: a real dead field the router
// never binds to (see infra_handlers.go's doc comment on why).
export interface RouterConfig {
  connect_timeout_s: number;
  request_timeout_s: number; // 0 = unbounded (laguna-s-21 fix) — a real value, not "unset"
  health_ttl_s: number;
  max_retries_per_backend: number;
  ensure_loaded_timeout_s: number;
  embedding_url: string;
  tts_url: string;
}

export type RouterConfigUpdate = Partial<RouterConfig>;

// RoutingPreviewCandidate/RoutingPreviewResponse mirror
// routingPreviewCandidate/routingPreviewResponse (GET /api/v1/routing/preview,
// Phase 7) — a read-only simulation of what a0 would resolve a model to,
// sharing the exact selection rule live routing uses
// (router.SelectOfferingChain) so this can never show a chain live routing
// wouldn't actually pick. health_consulted is always false today — live
// routing never consults provider health at all; a "down" provider is only
// skipped when provider_failover is on AND it actually returns a transport
// error/5xx. assumed_down only changes selection when provider_failover is
// on; assumed_disabled always does (it's a faithful simulation of the real
// Enabled toggle, not a hypothetical about behavior that doesn't exist).
export interface RoutingPreviewCandidate {
  provider: string;
  wire_model: string;
  priority: number;
  offering_enabled: boolean;
  provider_enabled: boolean; // real state, unaffected by assume_disabled
  assumed_disabled: boolean;
  assumed_down: boolean;
  selected: boolean;
  reason: string; // why skipped; "" when selected
}

export interface RoutingPreviewResponse {
  model: string;
  kind: "local" | "remote" | "not_found";
  health_consulted: boolean;
  provider_failover: boolean;
  candidates: RoutingPreviewCandidate[];
  note?: string;
}

// MonitorSettings mirrors monitorSettingsResponse (GET/PUT
// /api/v1/monitor/settings, infra.monitor) — all apply="live" (ReloadConfig
// picks it up; the collector re-reads *config.Config every poll cycle).
export interface MonitorSettings {
  poll_interval_s: number;
  hang_tps_thousandth: number;
  hang_sustain_s: number;
  switch_cooldown_s: number;
  gtt_warn_pct: number;
}

export type MonitorSettingsUpdate = Partial<MonitorSettings>;

// MetricsSettings mirrors metricsSettingsResponse (GET/PUT
// /api/v1/metrics/settings) — the two fields have DIFFERENT apply modes on
// the same endpoint: retention_days is "live" (store.RunRetention re-reads
// it as a func value every prune tick), sample_interval_s is "restart"
// (baked into a time.NewTicker once inside metricsSamplerOnce.Do). Do not
// badge them the same in the UI.
export interface MetricsSettings {
  retention_days: number;
  sample_interval_s: number;
}

export type MetricsSettingsUpdate = Partial<MetricsSettings>;

// UISettings mirrors uiSettingsResponse (GET/PUT /api/v1/ui/settings).
// ui.help_button and nfs.shares were removed as orphaned settings (no V5
// frontend component renders them).
export interface UISettings {}

// VoiceSettings mirrors GET/PUT /api/v1/voice/settings (Tier 1 Sprint 2,
// Voice & Speech settings, 2026-08-27) — the bare tts.engines shape, no
// wrapper object (same unwrapped convention as MonitorSettings above).
// Keyed to match go/internal/ttsctl.Engines exactly: customvoice/voicedesign/
// base mirror tts.VoiceMode's three constants; kokoro gets its own field
// since it isn't a VoiceMode at all (dual_engine routes to it by voice-id
// namespace, never through the Qwen engine).
export type VoiceEngineMode = "resident" | "available" | "disabled";

export interface VoiceEngineConfig {
  enabled: boolean;
  mode: VoiceEngineMode;
  /** Systemd unit to start/stop for resident mode. "" = daemon-unmanaged. */
  unit: string;
  /** Resident backend URL. "" = not configured (falls back to CLI/omitted). */
  url: string;
}

export interface VoiceSettings {
  kokoro: VoiceEngineConfig;
  customvoice: VoiceEngineConfig;
  voicedesign: VoiceEngineConfig;
  base: VoiceEngineConfig;
}

export type VoiceSettingsUpdate = VoiceSettings; // always a full replace, never a partial patch

// VoiceListResponse mirrors GET /api/v1/voice/list (Sprint 1 UI papercuts,
// 2026-08-27) — forge-tts's live voice registry, passed through with an
// added `engine` field (one of the VoiceSettings keys) per entry so the
// Settings UI can group by engine without re-deriving forge-tts's own Type
// taxonomy ("design"/"clone" vs. the engine keys "voicedesign"/"base").
export interface VoiceListEntry {
  id: string;
  name: string;
  type: string;
  engine: keyof VoiceSettings | string;
  language: string;
  tier?: string;
  builtin: boolean;
  has_sample: boolean;
  sample_text?: string;
}

export interface VoiceListResponse {
  voices: VoiceListEntry[];
}

export type UISettingsUpdate = Partial<UISettings>;

// DashboardLayout mirrors dashboardLayoutResponse (GET/PUT /api/v1/dashboard/layout).
// ADR-0011: system-wide settings key "dashboard.pages", schema-evolvable for
// per-user owner scope (additive `owner` field per page, not yet populated).
// The store holds only custom pages; the 3 default tabs (overview/cost/
// resources) are bespoke React code, not data.
export interface WidgetInstance {
  slug: string;
  props: Record<string, string | number | boolean>;
}

export interface DashboardPage {
  id: string;
  name: string;
  widgets: WidgetInstance[];
}

export interface DashboardLayout {
  pages: DashboardPage[];
}

// SystemSettings mirrors systemSettingsResponse (GET /api/v1/system/settings,
// admin + area.settings.system) — infra.server/paths/ports/tailscale, every
// field apply="restart" and boot-critical. Read-only display in the General
// panel's daemon strip (Phase 6); editable in the Danger Zone (Phase 7),
// gated behind arm+step-up+preflight.
export interface SystemSettings {
  listen: string;
  router_listen: string;
  mcp_listen: string;
  db_path: string;
  tts_unit: string;
  models_dir: string;
  sysconfig_dir: string;
  state_dir: string;
  icons_dir: string;
  vulkan_bin: string;
  rocm_bin: string;
  ports: Record<string, number>;
  hostname: string;
  rp_id: string;
  // Secure attribute on the session + WebAuthn challenge cookies (issue
  // #27, sprint 4). Defaults true; false is the tailscale-serve-only
  // opt-out (this process speaks plain HTTP behind a TLS-terminating
  // proxy on the same host). Unlike every other field in this group, this
  // one is genuinely live off ReloadConfig — no restart needed — though
  // the save still raises the shared restart-required banner for the
  // group as a whole.
  cookie_secure: boolean;
}

// SystemSettingsUpdate mirrors systemSettingsBody exactly — NOT a bare
// Partial<SystemSettings>. The backend decodes with DisallowUnknownFields,
// and systemSettingsBody deliberately has no rp_id field at all (reserved/
// unused, shown for transparency on GET, never offered as a PUT target) —
// including it here would make every PUT carrying it 400 on "unknown field
// rp_id" the moment a caller spread the full SystemSettings object in.
export interface SystemSettingsUpdate {
  listen?: string;
  router_listen?: string;
  mcp_listen?: string;
  db_path?: string;
  tts_unit?: string;
  models_dir?: string;
  sysconfig_dir?: string;
  state_dir?: string;
  icons_dir?: string;
  vulkan_bin?: string;
  rocm_bin?: string;
  // Whole-map replace when present (never a per-key patch) — see
  // infra_handlers.go's systemSettingsBody doc comment.
  ports?: Record<string, number>;
  hostname?: string;
  cookie_secure?: boolean;
}

// PreflightCheck/PreflightResult mirror preflight.go's preflightCheck +
// the {ok, checks} POST /system/preflight response shape. A PUT that fails
// preflight 422s with {error:"preflight_failed", fields, checks} — the
// `fields` half reuses the ordinary validation-error shape (apiErrorMessage
// renders it with zero extra work); `checks` is the richer per-row view the
// Danger Zone's checklist renders, parsed straight off ApiError.body.
export interface PreflightCheck {
  field: string;
  level: "ok" | "warn" | "error";
  message: string;
  detail?: string;
}

export interface PreflightResult {
  ok: boolean;
  checks: PreflightCheck[];
}

export interface SystemRestartResponse {
  ok: boolean;
  unit: string;
}

// ── Providers (Sprint 0 §0.3, GET /api/v1/providers) ─────────────────────────
// One provider has many models; External Models is grouped by provider. Health
// + credits are daemon-fetched and cached. "idle" is dropped.

// ProviderModel — Phase 7 (2026-08-13): offerings-derived, not the old
// provider_models catalog (dropped, migration 0043 — 0 rows on the live
// deployment, no write path ever existed for it). model_id is the
// offering's wire_model; catalog_model_id links to Catalog → Offerings for
// the full record (pricing/wire_model/context/variant are edited there —
// see Settings → Routing's model→provider map for the routing subset:
// enabled/priority). currency is the offering's own (not inherited from
// the provider's bill_currency — providers can offer models priced in
// different currencies, e.g. aiand's JPY offerings).
export interface ProviderModel {
  model_id: string;
  catalog_model_id: number;
  display_name: string;
  logo: string; // icon slug (§0.8)
  price_in_per_1m: number;
  price_out_per_1m: number;
  currency: string;
  priority: number;
  enabled: boolean;
  compressor_proxy: string | null; // the PROVIDER's linked proxy, if any
  passthrough: boolean | null; // proxy bypass state, if linked
}

export interface ProviderHealth {
  state: "reachable" | "degraded" | "down" | "unknown";
  as_of: number | null;
  source: "status_page" | "live_probe" | "none";
  detail: string | null; // e.g. incident title
}

export interface ProviderCredits {
  balance_native: number | null;
  currency: string | null;
  as_of: number | null;
  supported: boolean; // false ⇒ provider API has no balance endpoint
  // BE-3 F4 fix: some providers (AI&, confirmed — no queryable balance
  // exists at all, credits purchased via web checkout only) have no
  // balance concept but do expose real period spend via a usage/cost
  // analytics API. spend_period_label is e.g. "24h spend"; null when
  // balance_native is the relevant figure instead.
  spend_period: number | null;
  spend_period_label: string | null;
}

export interface Provider {
  // id is the surrogate PK (Phase 6 surrogate-key migration) — the stable
  // handle a rename doesn't disturb. PUT/DELETE /api/v1/providers/{ref}
  // dual-accepts either id or name; the UI still keys off name today (the
  // rename UI itself is Phase 7 — this field is wired through in advance).
  id: number;
  name: string;
  api_key_masked: string; // never the full secret
  bill_currency: string;
  health: ProviderHealth;
  credits: ProviderCredits;
  models: ProviderModel[];
  // product/QA sprint, 2026-07-29: billing_enabled gates the credits API
  // (false → the Console chip's credits/spend region renders blank rather
  // than stale). billing_console_url is the human billing-dashboard link
  // (e.g. https://platform.deepseek.com/usage), distinct from credits_url
  // (the machine balance-API endpoint used server-side).
  billing_enabled: boolean;
  billing_console_url?: string;
  credits_url?: string;
  // Surfaced so the Edit form can pre-fill these — they were omitted from
  // the GET response by oversight, leaving every Edit field blank.
  target_url?: string;
  status_url?: string;
  org_id?: string;
  // Multi-provider routing sprint, 2026-08-06: enabled=false disables the
  // provider without deleting it — the router skips its offerings and the
  // health/credits poller stops refreshing it; everything else (keys,
  // offerings, linked proxy) is preserved for re-enabling.
  enabled: boolean;
  // Provider-level data residency ("" / undefined = unknown).
  country?: string;
  data_residency_group?: string;
}

export interface ProvidersResponse {
  providers: Provider[];
}

// GET/PUT /api/v1/billing/settings (Sprint 0 §0.9).
export interface BillingSettings {
  display_currency: string;
  fx_source_url?: string;
  fx_refresh_min?: number;
}

// ── Provider CRUD request bodies (Sprint 0 §0.9, FE-1b/FE-4) ─────────────────
// POST /api/v1/providers and PUT /api/v1/providers/{name} share these shapes;
// the response is the full Provider (§0.3) in either case. The API key is
// write-only (PUT .../key) and never echoed back by the server.

export interface ProviderCreateRequest {
  name: string;
  bill_currency: string;
  target_url: string;
  status_url: string;
  credits_url: string;
  // X-Org-ID header value some providers' credits/analytics APIs require
  // alongside the API key (confirmed for AI&). "" = not configured.
  org_id: string;
  // product/QA sprint, 2026-07-29. billing_enabled omitted/undefined
  // defaults to true server-side — only send it to explicitly disable.
  billing_enabled?: boolean;
  billing_console_url?: string;
  // Multi-provider routing sprint, 2026-08-06: prefilled from the curated
  // preset so residency is stored ON the provider row (pre-0032 it lived
  // only in the preset table and hand-created providers had none).
  country?: string;
  data_residency_group?: string;
  // Operator feedback 2026-08-14: when true, the server auto-provisions a
  // Compressor proxy linked to the new provider (safe name derived from the
  // provider name, label = provider display name). The create form defaults
  // this on; omitted/undefined is treated as false server-side.
  create_proxy?: boolean;
}

export interface ProviderUpdateRequest {
  // Phase 7 (2026-08-13): a rename, now that router_providers has a real
  // surrogate PK (0042) — id is the stable handle a rename doesn't disturb,
  // PUT /api/v1/providers/{ref} dual-accepts either.
  name?: string;
  bill_currency?: string;
  target_url?: string;
  status_url?: string;
  credits_url?: string;
  org_id?: string;
  compressor_proxy?: string;
  // product/QA sprint, 2026-07-29.
  billing_enabled?: boolean;
  billing_console_url?: string;
  // Multi-provider routing sprint, 2026-08-06: the disable-without-delete
  // toggle + residency fields.
  enabled?: boolean;
  country?: string;
  data_residency_group?: string;
}

// POST /api/v1/providers/{name}/discover-billing response (product/QA
// sprint, 2026-07-29). tried lists every candidate URL probed, in order —
// the operator sees what was checked even when nothing was found.
export interface ProviderDiscoverBillingResponse {
  found: boolean;
  url: string;
  saved: boolean;
  tried: { url: string; parsed: boolean }[];
}

export interface ProviderKeyRequest {
  api_key: string;
}

// ── Auth v2 (Sprint 0-AUTH, docs/v5-sprint0-auth-design.md §6) ─────────────
// Frozen request/response shapes for the new policy + step-up + WebAuthn/TOTP
// + identity linking + API-key management endpoints. Endpoints return 501 in
// this design gate; BE-AUTH Phase A/B/C will implement them.

export type AuthFactor = "network" | "password" | "totp" | "passkey";
export type AuthNetworkProvider = "tailscale" | "forward_auth_header" | "none";

export interface StepUpRequest {
  factor: AuthFactor;
  password?: string;
  code?: string;
}

export interface StepUpResponse {
  assurance: AuthFactor;
  assurance_at: number | null;
}

// WebAuthn — shapes mirror the WebAuthn JSON serialization spec so the FE can
// pass options directly to navigator.credentials.create/get.

export interface WebAuthnCredential {
  id: string;
  label: string;
  transports: string[];
  created_at: number;
  last_used_at: number | null;
}

export interface WebAuthnCredentialsResponse {
  credentials: WebAuthnCredential[];
}

export interface PublicKeyCredentialDescriptorJSON {
  id: string;
  type: "public-key";
  transports?: string[];
}

export interface PubKeyCredParam {
  type: "public-key";
  alg: number;
}

export interface WebAuthnRelyingParty {
  id: string;
  name: string;
}

export interface WebAuthnUser {
  id: string;
  name: string;
  display_name: string;
}

export interface AuthenticatorSelectionCriteria {
  authenticator_attachment?: "platform" | "cross-platform";
  resident_key?: "required" | "preferred" | "discouraged";
  user_verification?: "required" | "preferred" | "discouraged";
}

export interface PublicKeyCredentialCreationOptionsJSON {
  rp: WebAuthnRelyingParty;
  user: WebAuthnUser;
  challenge: string;
  pubKeyCredParams: PubKeyCredParam[];
  timeout?: number;
  excludeCredentials?: PublicKeyCredentialDescriptorJSON[];
  authenticatorSelection?: AuthenticatorSelectionCriteria;
  attestation?: "none" | "indirect" | "direct";
}

export interface WebAuthnBeginRegisterResponse {
  options: PublicKeyCredentialCreationOptionsJSON;
}

export interface WebAuthnRegResponseData {
  clientDataJSON: string;
  attestationObject: string;
  authenticatorData?: string;
  transports?: string[];
}

export interface WebAuthnRegistrationResponseJSON {
  id: string;
  rawId: string;
  type: "public-key";
  response: WebAuthnRegResponseData;
  clientExtensionResults?: Record<string, unknown>;
}

export interface WebAuthnFinishRegisterRequest {
  response: WebAuthnRegistrationResponseJSON;
  label: string;
}

export interface WebAuthnFinishRegisterResponse {
  credential: WebAuthnCredential;
}

export interface PublicKeyCredentialRequestOptionsJSON {
  challenge: string;
  timeout?: number;
  rpId?: string;
  allowCredentials?: PublicKeyCredentialDescriptorJSON[];
  userVerification?: "required" | "preferred" | "discouraged";
  extensions?: Record<string, unknown>;
}

export interface WebAuthnBeginAssertResponse {
  options: PublicKeyCredentialRequestOptionsJSON;
}

export interface WebAuthnAuthResponseData {
  clientDataJSON: string;
  authenticatorData: string;
  signature: string;
  userHandle?: string;
}

export interface WebAuthnAuthenticationResponseJSON {
  id: string;
  rawId: string;
  type: "public-key";
  response: WebAuthnAuthResponseData;
  clientExtensionResults?: Record<string, unknown>;
}

export interface WebAuthnFinishAssertRequest {
  response: WebAuthnAuthenticationResponseJSON;
}

export interface WebAuthnFinishAssertResponse {
  verified: boolean;
  assurance?: AuthFactor;
}

// TOTP

export interface TotpEnrollResponse {
  secret: string;
  otpauth_uri: string;
}

export interface TotpConfirmRequest {
  code: string;
}

export interface TotpConfirmResponse {
  active: boolean;
}

// Recovery codes (Phase C, §8) — GET /api/v1/auth/recovery-codes +
// POST /api/v1/auth/recovery-codes/generate.

export interface RecoveryCodesStatusResponse {
  has_codes: boolean;
  unused: number;
  total: number;
}

export interface RecoveryCodesGenerateResponse {
  codes: string[];
  total: number;
}

// Identity linking

export interface IdentityLink {
  provider: AuthNetworkProvider;
  principal: string;
  user_id: number;
  created_at: number;
}

export interface IdentityLinksResponse {
  links: IdentityLink[];
}

export interface IdentityLinkCreateRequest {
  provider: AuthNetworkProvider;
  principal: string;
  user_id: number;
}

// API-key management

export type APIKeyKind = "forge" | "router" | "mcp";

export interface APIKey {
  keyid: string;
  kind: APIKeyKind;
  name: string;
  role?: Role;
  /** Operator's preferred consumer label ("" = derive at request time). */
  display_name?: string;
  created_at: number;
  last_used_at: number | null;
  revoked_at: number | null;
}

export interface APIKeysResponse {
  keys: APIKey[];
}

export interface APIKeyCreateRequest {
  kind: APIKeyKind;
  name: string;
  role?: Role;
  /** Preferred consumer label shown on slot attribution when set. */
  display_name?: string;
}

export interface APIKeyCreateResponse {
  token: string;
  key: APIKey;
}

// Policy + config

export interface AuthPolicyResponse {
  policy: Record<string, AuthFactor>;
}

export interface AuthPolicyPutRequest {
  policy: Record<string, AuthFactor>;
}

export interface AuthConfig {
  network_provider: AuthNetworkProvider;
  provider_config?: Record<string, unknown>;
  webauthn_rp_id?: string;
  webauthn_rp_name?: string;
  step_up_ttl_min?: number;
  network_default_role?: Role;
  a0_tailnet_bypass?: boolean;
}

export interface AuthConfigPutRequest extends AuthConfig {}

// ── Modes (GET /api/v1/modes — read-only; Sprint 0 §0.9) ──────────────────────
// Mirrors the frozen modesListResponse shape (go/internal/httpapi/shapes.go:343)
// and the per-mode entry built in handlers.go:handleModesList. "services" is the
// raw config.Service[] catalog — opaque to FE-4, which renders only the
// display fields (label/family/description/type/default/tags/icon/color).
export interface ModeConfig {
  label: string;
  family: string;
  description: string;
  color: string;
  icon: string;
  tags: string[];
  default: boolean;
  type: string; // "" (inference) or "service"
  services: unknown[];
}

export interface ModesListResponse {
  modes: Record<string, ModeConfig>;
}

// ── Profiling (PROFILE track — docs/v5-profiling-benchmarks.md) ──────────────
// GET /api/v1/profile/{mode} + POST /api/v1/profile/run + SSE profile:* events.

// DepthBenchmark is one point in the profiling depth-sweep curve (product/QA
// sprint, 2026-07-29): index 0 (empty context) is TYPICAL — the same figure
// as prefill_tps/decode_tps above; the last entry (~full context) is WORST
// CASE. depth_tokens is how full the KV cache was when this row was measured.
export interface DepthBenchmark {
  depth_tokens: number;
  pp2048_tps: number;
  tg128_tps: number;
}

export interface ProfileResult {
  mode: string;
  // config_id (Phase 8, pre-release feedback sprint): the catalog config
  // this profile belongs to — real since the Phase 6 surrogate-key
  // migration gave model_profiles a config_id column. Prefer this over
  // matching on mode/name; see lib/profileJoin.ts.
  config_id: number;
  model_id: string;
  n_ctx: number;
  actual_n_ctx: number;
  backend: string;
  parallel: number;
  safe_memory_bytes: number;
  prefill_tps: number;
  decode_tps: number;
  fingerprint: string;
  stale: boolean;
  profiled: boolean;
  measured_at: number; // unix seconds
  running: boolean;
  depth_benchmarks: DepthBenchmark[];
  // Set when the most recent run for this mode failed and no later run has
  // succeeded since. Lets a polling client (ProfilingPanel) surface a
  // failure without depending on the profile:failed SSE event.
  last_error?: string;
  last_error_at?: number;
}

export interface ProfileListItem {
  mode: string;
  config_id: number;
  model_id: string;
  n_ctx: number;
  backend: string;
  safe_memory_bytes: number;
  prefill_tps: number;
  decode_tps: number;
  stale: boolean;
  measured_at: number;
  depth_benchmarks: DepthBenchmark[];
}

export interface ProfilesListResponse {
  profiles: ProfileListItem[];
}

export interface ProfileRunRequest {
  mode: string;
}

// SSE profile:progress payload — phase + optional measured values.
//
// Sprint K (2026-08-05): the backend (internal/profile/profile.go's
// publishProgress calls) already sends per-phase stage detail — this type
// used to omit most of it, which is why ProfilingPanel's phase text read as
// static/generic despite the backend having real numbers to show. Adding
// the missing fields is a pure frontend fix, no backend change needed.
export interface ProfileProgressEvent {
  mode: string;
  phase: "evicting" | "loading" | "verifying" | "filling" | "measuring" | "benchmarking";
  target_slot?: string;
  already_loaded?: boolean;
  slot?: string;
  actual_n_ctx?: number;
  target_n_ctx?: number;
  depth_target?: number;
  depth_tokens?: number;
  peak_bytes?: number;
  safe_bytes?: number;
  pp2048_tps?: number;
  tg128_tps?: number;
  prefill_tps?: number;
  decode_tps?: number;
}

// SSE profile:done payload.
export interface ProfileDoneEvent {
  result: ProfileResult;
}

// SSE profile:failed payload.
export interface ProfileFailedEvent {
  mode: string;
  phase: string;
  error: string;
}

// ── Model Catalog (MODEL CATALOG sprint, Phase 3 API — docs/v5-modes-config-editable.md)
// Mirrors the JSON shapes from go/internal/httpapi/catalog_handlers.go.
// Read endpoints are role-agnostic; mutations require admin + page.settings.

// CatalogGenealogy is the level above Family (product/QA sprint,
// 2026-07-29) — a vendor's own release lineage (Qwen, Gemma, Nemotron, …).
// Sprint I: logo is the top of the icon inheritance chain (see
// registry.resolveLogo) — inherited by every family/model/config under it
// that doesn't set its own.
export interface CatalogGenealogy {
  id: number;
  name: string;
  logo: string;
  // Phase 3 — dark-theme variant override; "" falls back to logo. Same
  // inheritance level as logo (see registry.resolveLogos).
  logo_dark: string;
}

export interface CatalogFamily {
  id: number;
  name: string;
  genealogy_id: number;
  // Sprint I — icon inheritance; see CatalogGenealogy's doc comment.
  logo: string;
  logo_dark: string;
}

export interface CatalogModel {
  id: number;
  family_id: number;
  name: string;
  architecture: string;
  parameter_count: string;
  description: string;
  creator: string;
  license_name: string;
  license_url: string;
  hf_repo: string;
  logo: string;
  logo_dark: string;
  key_features: string[];
  // Sprint J1 — subset of "text" | "vision" | "audio".
  modalities: string[];
  // Model-level decommission flag (0062): "visible" | "hidden". Hidden
  // models are excluded from the Models gallery but stay in Settings.
  visibility: string;
}

export interface CatalogVariant {
  id: number;
  model_id: number;
  name: string;
  derivation_type: string;
  source_variant_id: number;
  trained_ctx: number;
  is_abliterated: boolean;
  abliteration_quality: string;
}

export interface CatalogArtifact {
  id: number;
  variant_id: number;
  quantization_id: number;
  format_id: number;
  file_path: string;
  shard_set_id: string;
  is_auxiliary: boolean;
  artifact_type: string;
  missing: boolean;
  sha256: string;
  file_size_bytes: number;
  gguf_arch: string;
  gguf_trained_ctx: number;
  gguf_parameter_count: string;
  gguf_quant_type: string;
}

export interface CatalogEngine {
  id: number;
  name: string;
}

export interface CatalogBuild {
  id: number;
  engine_id: number;
  name: string;
  binary_path: string;
  reason: string;
}

// GET /api/v1/audit (Sprint C — audit_log's first read surface; the table
// itself has existed since the MODEL CATALOG sprint). action_prefix+target
// must be combined, never target alone — target ids collide across entity
// types (a config #7 and a model #7 are both the string "7").
export interface AuditEntry {
  id: number;
  ts: string;
  actor: string;
  action: string;
  target: string;
  detail: string;
  remote_addr: string;
}

export interface AuditListResponse {
  entries: AuditEntry[];
}

export interface CatalogConfig {
  id: number;
  name: string;
  variant_id: number;
  weight_artifact_id: number;
  engine_id: number;
  build_id: number;
  mmproj_artifact_id: number;
  n_ctx: number;
  parallel: number;
  extra_args: string[];
  status: string;
  visibility: string;
  is_default: boolean;
  fingerprint: string;
  // Sprint I — config-level icon override; "" inherits via the
  // model/family/genealogy chain. See CatalogGenealogy's doc comment.
  logo: string;
  logo_dark: string;
  // Sprint J1: overrides the model's default modalities when this config
  // can't deliver everything the model architecturally supports. undefined
  // (field omitted by the server, via `omitzero`) = derive; an explicit
  // array (even []) = use verbatim.
  modalities?: string[];
}

export interface CatalogOffering {
  id: number;
  model_id: number;
  variant_id: number;
  provider: string;
  wire_model: string;
  price_in_per_1m: number;
  price_out_per_1m: number;
  // The provider's discounted cache-hit input rate (e.g. DeepSeek's ~10x
  // cheaper cached-token price); undefined/null = unmodelled — the router
  // then prices cached tokens at the full price_in_per_1m rate.
  price_cached_in_per_1m?: number | null;
  currency: string;
  context_length: number;
  enabled: boolean;
  // Operator preference among the offerings of one model (multi-provider
  // routing sprint, 2026-08-06): LOWEST value wins; default 100 = "no
  // preference" (ties break by provider name).
  priority: number;
}

export interface CatalogBenchmark {
  id: number;
  metric: string;
  value: string;
  source: string;
  source_url: string;
  source_date: string;
  subject_type: string;
  subject_id: number;
  notes: string;
}

export interface CatalogNote {
  id: number;
  subject_type: string;
  subject_id: number;
  author: string;
  body: string;
  created_at: string;
  updated_at: string;
}

export interface CatalogService {
  id: number;
  name: string;
  label: string;
  description: string;
  icon: string;
  color: string;
  unit: string;
  health_check: string;
}

export interface CatalogModelFile {
  path: string;
  size_bytes: number;
  arch: string;
  trained_ctx: number;
  is_shard_set: boolean;
}

// ── Cost / savings sprint, Phase 5 (docs: joyful-splashing-moonbeam.md) ──────
// Mirrors go/internal/httpapi/cost_handlers.go + compressor_summary_handlers.go.

export interface CostCalibration {
  single_slot_active_wall_w_p50: number | null; // null unless >=10 qualifying samples
  single_slot_active_wall_w_p95: number | null;
  samples: number;
}

export interface CostEnergy {
  method: string; // "trapezoid" (only value so far)
  measured_seconds: number;
  gap_seconds: number;
  unmeasured_seconds: number;
  coverage_pct: number;
  package_wh: number;
  wall_wh_est: number;
  overhead_w: number;
  psu_efficiency: number;
  rate_per_kwh: number;
  rate_currency: string;
  cost_display: number;
  idle_wh_est: number;
  active_wh_est: number;
  attributable_wh_est: number;
  idle_baseline_w: number;
  active_seconds: number;
  single_slot_seconds: number;
  multi_slot_seconds: number;
  calibration: CostCalibration;
}

export interface CostSummary {
  window: string;
  display_currency: string;
  fx_as_of: number | null;
  fx_stale: boolean;
  energy: CostEnergy;
}

export interface CostEnergyHistoryPoint {
  ts: number; // bucket start, unix seconds
  package_wh: number;
  wall_wh_est: number;
  cost_display: number;
  coverage_pct: number;
}

export interface CostEnergyHistoryResponse {
  window: string;
  resolution_s: number;
  points: CostEnergyHistoryPoint[];
}

export interface CostSettings {
  power_kw: number;
  rate_per_kwh: number;
  rate_currency: string;
  overhead_w: number;
  psu_efficiency: number;
  // Dashboard follow-up round 2: package-level power ceiling used only to
  // scale the Overview power chart/tile against a real hardware limit —
  // never used for cost math.
  max_power_w: number;
}

// PUT body — every field optional, partial merge server-side.
export type CostSettingsUpdate = Partial<CostSettings>;

// GET /api/v1/compressor/summary?window=. `*_since_start` fields are Compressor-
// process LIFETIME gauges, never window statistics — only `*_mean_ms` is a
// real per-window figure. The five time/money-saved fields are entirely
// absent (not null) when not computable — see docs/v5-headroom-topology.md
// and joyful-splashing-moonbeam.md Phase 3.
export interface CompressorSummaryProxy {
  proxy: string;
  kind: "local" | "remote";

  tokens_in: number;
  tokens_out: number;
  // Compression savings. Usually 0 — Compressor runs --lossless — but not
  // pinned to 0: a real 950-token delta was recorded live on the deepseek
  // proxy 2026-07-31. Priced into compression_saved_native below.
  tokens_saved: number;
  requests: number;
  requests_cached: number;
  requests_failed: number;
  requests_rate_limited: number;
  cache_hit_rate_pct: number | null; // null when requests === 0

  ttfb_mean_ms: number | null;
  ttfb_min_ms_since_start: number | null;
  ttfb_max_ms_since_start: number | null;

  latency_mean_ms: number | null;
  latency_min_ms_since_start: number | null;
  latency_max_ms_since_start: number | null;

  overhead_mean_ms: number | null;
  overhead_min_ms_since_start: number | null;
  overhead_max_ms_since_start: number | null;

  requests_by_provider?: Record<string, number>;
  requests_by_model?: Record<string, number>;

  // Provider cache token metrics (from compress_cache_read_tokens_total,
  // compress_uncached_input_tokens_total, etc. — scraped as of 0.35.0).
  cache_read_tokens?: number;
  uncached_tokens?: number;
  cache_busts?: number;
  cache_bust_tokens_lost?: number;
  cache_read_tokens_by_provider?: Record<string, number>;
  uncached_tokens_by_provider?: Record<string, number>;
  provider_cache_requests?: Record<string, number>;
  provider_cache_hit_requests?: Record<string, number>;

  // Per-compressor timing (from compressor_transform_timing_ms_{sum,count}).
  transform_timing_ms?: Record<string, number>;
  transform_timing_count?: Record<string, number>;

  // Local-only, TPS-independent: cached-request tokens that avoided a full
  // re-prefill (requests_cached × this window's own avg tokens/request).
  // Present whenever anything was cached this window.
  tokens_saved_est?: number;

  // Local-only estimate; time_saved/money_saved omitted entirely for remote
  // proxies, when nothing cacheable was available this window, or (2026-08-06
  // local-savings prefill sprint) when NO model contributing cached requests
  // had a resolvable real prefill TPS — there is deliberately no flat-rate
  // fallback anymore, so this can legitimately be absent even though
  // tokens_saved_est is present. That combination is an anomaly (logged
  // server-side), not routine — see prefill_breakdown.
  time_saved_seconds_est?: number;
  money_saved_est?: number;
  money_saved_currency?: string;
  // money_saved_est FX-converted to the response's display_currency
  // (2026-07-31 fix — this endpoint used to never convert).
  money_saved_display?: number;
  // tps_source/tps_mode describe the SINGLE LARGEST contributor to
  // time_saved_seconds_est (by share of cached requests this window) — see
  // prefill_breakdown for the full per-model accounting. Every source here
  // is a real measurement; "fallback" no longer exists.
  tps_source?: "profile_depth_curve" | "observed" | "profile_scalar" | "catalog" | "live";
  tps_mode?: string;
  // prefill_breakdown is the full per-model accounting behind
  // time_saved_seconds_est: every model that contributed cached requests
  // this window AND had a resolvable real prefill TPS, sorted by share
  // descending. Local-only.
  prefill_breakdown?: {
    mode: string;
    share: number; // 0-1, this model's share of the proxy's requests this window
    tps: number;
    source: "profile_depth_curve" | "observed" | "profile_scalar" | "catalog" | "live";
  }[];

  // Remote-only. cached_prompt_tokens is EVERY cached-prompt token the
  // provider reported this window (real, not estimated). cache_discount_*
  // is the priced SUBSET of that — only cached tokens whose offering has a
  // modelled cached rate contribute money; cache_discount_saved_native/
  // currency are omitted when nothing was priced. The two token counts can
  // differ when pricing is incomplete.
  cached_prompt_tokens?: number;
  cache_discount_saved_native?: number;
  cache_discount_saved_currency?: string;
  cache_discount_tokens?: number;
  // cache_discount_saved_native FX-converted to the response's
  // display_currency (2026-07-31 fix).
  cache_discount_saved_display?: number;

  // Remote-only, REAL (not estimated): tokens_saved above, priced at this
  // window's blended input rate for the provider. Distinct from
  // cache_discount_saved_native — that prices the PROVIDER's own prompt-cache
  // discount (applies with or without Compressor in the path); this prices
  // Compressor's own compression saving, the actual Compressor saving. Omitted
  // when tokens_saved is 0 or nothing this window matched a priced offering.
  compression_saved_native?: number;
  compression_saved_currency?: string;
  compression_rate_per_1m?: number;
  // compression_saved_native FX-converted to the response's display_currency
  // (2026-07-31 fix — this was the field the operator saw as a stuck "$0.00"
  // while billing.display_currency was set to JPY).
  compression_saved_display?: number;
}

// display_currency/fx_as_of/fx_stale mirror the same fields on UsageResponse
// / CostSummary — every *_native money field above has a matching
// *_display field converted into this currency.
export interface CompressorSummaryResponse {
  window: string;
  display_currency: string;
  fx_as_of: number | null;
  fx_stale: boolean;
  proxies: CompressorSummaryProxy[];
}

// ── Smith self-diagnosis (GET /api/v1/smith/status, docs/v5-smith.md §4.1)
// Mirrors go/internal/smith.SelfContext — snake_case JSON tags are frozen in
// shapes_freeze_test.go. The no-LLM status picture: no model is involved in
// assembling it (the chat brain lands in P3).
export interface SmithBrainResolution {
  resolution: string;
  model?: string;
  slot?: string;
  provider?: string;
  detail: string;
}

export interface SmithSchedule {
  quick: string;
  deep: string;
  enabled: boolean;
}

export interface SmithStatus {
  hostname: string;
  snapshot_taken_at: number;
  snapshot_age_s: number;
  metrics: {
    mode: string;
    mem_total_bytes: number;
    mem_used_bytes: number;
    mem_avail_bytes: number;
    mem_pct: number;
    gtt_used_bytes: number | null;
    gtt_total_bytes: number | null;
    gpu_use_pct: number | null;
    temp_celsius: number | null;
    uptime_seconds: number | null;
    package_power_w: number | null;
    disk_total_bytes: number;
    disk_free_bytes: number;
    disk_used_bytes: number;
    disk_pct: number;
    inference_rss_bytes: number | null;
  } | null;
  alerts: { code: string; msg: string; port?: number; unit?: string }[];
  slots: Record<string, {
    mode: string;
    label: string;
    memory_bytes: number;
    idle_seconds: number | null;
  }>;
  memory_budget: { total_bytes: number; used_bytes: number; free_bytes: number };
  brain: SmithBrainResolution;
  tier: string;
  check_count: number;
  fast_check_count: number;
  schedule: SmithSchedule;
  web: SmithWebStatus;
  tools: SmithToolsStatus;
  retention: SmithRetentionStatus;
  self_review: SmithSelfReviewStatus;
  brain_residency: SmithBrainResidencyStatus;
  // S2 — missed-pattern ledger (docs/v5-smith-experience.md §3.7). The capped
  // list of questions the fast path couldn't answer and the reasoning tier
  // did. omitempty on the wire so the key is absent when no patterns exist
  // yet — additive to the frozen shape, existing consumers unaffected.
  missed_patterns?: SmithMissedPattern[];
}

// SmithMissedPattern mirrors go/internal/smith.MissedPattern — one recorded
// question the fast path missed (redacted text + the tools the reasoning
// turn used). Surfaces on GET /smith/status for operators to file follow-up
// catalog entries; promotion into the fast path is always a reviewed code
// change, never auto-learned (§3.7).
export interface SmithMissedPattern {
  text: string; // redacted question
  tools_used: string[]; // tool IDs the reasoning turn used
  at: number; // unix seconds
}

// SmithRetentionStatus mirrors go/internal/smith.RetentionStatus —
// last_run_at is null until the first scheduled prune completes.
export interface SmithRetentionStatus {
  enabled: boolean;
  last_run_at: number | null;
  deleted_findings: number;
  deleted_web_cache: number;
}

// SmithSelfReviewStatus mirrors go/internal/smith.SelfReviewStatus (Thread
// C's periodic self-review sweep) — last_run_at is null until the first
// scheduled sweep completes.
export interface SmithSelfReviewStatus {
  enabled: boolean;
  last_run_at: number | null;
  actions_promoted: number;
  actions_superseded: number;
  investigations_proposed: number;
}

// SmithBrainResidencyStatus mirrors go/internal/smith.BrainResidencyStatus
// (brain_residency.go) — last_attempt_at is null until the first on-demand
// or periodic load attempt completes.
export interface SmithBrainResidencyStatus {
  stay_resident: boolean;
  last_attempt_at: number | null;
  last_loaded: boolean;
  last_slot?: string;
  last_error?: string;
}

// ── Smith: read-only tool loop (P7 — docs/v5-smith.md §9). Mirrors
// go/internal/smith.ToolsStatus/ToolsConfig/Retention.

// resolved_mode is "" until a turn has run for this model — that's the
// whole point of surfacing it: run one turn on a candidate brain, then
// this chip is the answer to "did it end up native or fenced".
export interface SmithToolsStatus {
  enabled: boolean;
  mode: "auto" | "native" | "fenced" | "off";
  resolved_mode: string;
  model: string;
  count: number;
}

// ── Smith: web research (P5 — docs/v5-smith.md §4.8). Mirrors
// go/internal/smith/web.ProviderStatus + smith.WebStatus.

export interface SmithWebProviderStatus {
  name: string;
  role: "search" | "fetch";
  configured: boolean;
  enabled: boolean;
  reachable: boolean;
  detail: string;
  checked_at: string | null; // null = never probed
  latency_ms: number;
}

export interface SmithWebStatus {
  enabled: boolean;
  providers: SmithWebProviderStatus[];
}

// ── Smith: investigations, findings, checks (Wave 2 — docs/v5-smith-wave2.md §3)
// Mirrors go/internal/smith.Finding, smith.StoredFinding, smith.Investigation,
// smith.CheckMeta + the httpapi response wrappers. snake_case per the frozen
// shapes in shapes_freeze_test.go.

// Finding is one check's outcome from a sweep (POST /checks/run or
// /investigations/{id}/checks). Evidence is a JSON object on the wire
// (map[string]any in Go).
export interface SmithFinding {
  check_id: string;
  severity: string;
  summary: string;
  evidence: Record<string, unknown>;
  proposal_ids: string[];
  kb_refs: string[];
  // confidence/confidence_note (Tier 1 Sprint 4): derived from evidence
  // completeness, never guessed by a model. "high" when unset.
  confidence: "high" | "medium" | "low";
  confidence_note?: string;
}

// StoredFinding is a persisted finding row (GET /findings, investigation
// detail). Evidence is raw JSON text (string in Go) — parse + pretty-print.
export interface SmithStoredFinding {
  id: number;
  investigation_id: number | null;
  check_id: string;
  severity: string;
  summary: string;
  evidence: string;
  sweep_kind: string;
  created_at: string;
  kb_refs: string[];
  repeat_count: number;
  confidence: "high" | "medium" | "low";
  confidence_note?: string;
}

export interface SmithInvestigation {
  id: number;
  trigger: string;
  status: string;
  opened_at: number;
  closed_at: number | null;
  summary: string;
  conversation_id: number | null;
  // Sprint S3 §2.4.1 — stamped when an approved action's post-verify comes
  // back clean and closes this investigation. Additive (omitempty on the
  // wire); absent on older rows, decodes to undefined. The FE renders a
  // "resolved by" line pointing at this action id.
  resolved_by_action_id?: number | null;
}

export interface SmithCheckMeta {
  id: string;
  name: string;
  category: string;
  fast: boolean;
}

export interface SmithChecksRunResponse {
  sweep_kind: string;
  scope: string;
  count: number;
  worst: string;
  findings: SmithFinding[];
}

export interface SmithFindingsResponse {
  count: number;
  findings: SmithStoredFinding[];
}

export interface SmithChecksResponse {
  count: number;
  checks: SmithCheckMeta[];
}

export interface SmithInvestigationsResponse {
  count: number;
  investigations: SmithInvestigation[];
}

export interface SmithInvestigationDetail extends SmithInvestigation {
  findings: SmithStoredFinding[];
}

export interface SmithInvestigationChecksResponse {
  count: number;
  worst: string;
  findings: SmithFinding[];
}

// ── Smith: action model + handoff (Wave 3 / P2 — smith P2 plan,
// peaceful-plotting-reef.md). Mirrors go/internal/smith.Action/Handoff/
// RunbookStep/Candidate/ActionResult/VerifyResult + the httpapi response
// wrappers, frozen by TestFrozenSmithActionShapes (W3-B). snake_case per the
// same wire convention as the findings/investigations shapes above. The
// backend for these routes does not exist yet as of this track (W3-C) —
// these types are the contract W3-A/W3-B build against.

export interface SmithHandoffCandidate {
  offering_id: number;
  model: string;
  provider: string;
  healthy: boolean;
}

export interface SmithRunbookStep {
  title: string;
  command: string;
  verify: string;
  // P7 additions (docs/v5-smith.md §4.6) — absent on an older stored
  // runbook, decodes to undefined, both rendered conditionally.
  why?: string;
  verify_command?: string;
}

export type SmithHandoffState = "not_required" | "required" | "runbook_issued" | "acknowledged" | "remote_swapped";

export interface SmithHandoff {
  state: SmithHandoffState;
  reason: string;
  brain_slot: string;
  brain_model: string;
  // Probed at proposal-creation time (P3, go/internal/smith/handoff.go's
  // probeHandoffCandidates) — real health, not a placeholder. Empty when
  // Catalog has nothing to offer, never null.
  candidates: SmithHandoffCandidate[];
  runbook: SmithRunbookStep[];
  issued_at: number | null;
  acknowledged_by: string | null;
  acknowledged_at: number | null;
}

export interface SmithVerifyResult {
  check_id: string;
  severity: "ok" | "info" | "warn" | "crit";
  summary: string;
  at: number;
}

export interface SmithActionResult {
  ok: boolean;
  message: string;
  error: string;
  verify: SmithVerifyResult[];
}

export type SmithActionKind =
  | "runbook"
  | "load_config"
  | "unload_slot"
  | "restart_forge_unit"
  | "settings_change"
  | "catalog_change"
  | "delete_files"
  // Autonomous-remediation Sprint 2 (go/internal/smith/procedure.go) — a
  // registered multi-step procedure run via the procedure engine.
  | "procedure";

// P6 FR7 — one file in a delete_files action's detail.files (go/internal/
// smith/execute.go's deleteFileEntry).
export interface SmithDeleteFileEntry {
  path: string;
  folder_type: string;
  size_bytes: number;
}

// P6 FR4 — model sourcing (go/internal/smith/sourcing.go's QuantCandidate
// / SourcingEvaluation).
export interface SmithQuantCandidate {
  filename: string;
  size_bytes: number;
  quant: string;
  estimated_vram_bytes: number;
  fits_budget: boolean;
  recommended: boolean;
}

export interface SmithSourcingEvaluation {
  repo: string;
  budget_bytes: number;
  candidates: SmithQuantCandidate[];
  recommended: SmithQuantCandidate | null;
  download_steps: SmithRunbookStep[];
  cached: boolean;
}

// HF model acquisition (go/internal/hfdownload + internal/hf) — search,
// recursive tree/rank, pre-flight, and the download job queue.

export interface HFSearchResult {
  id: string;
  author: string;
  downloads: number;
  likes: number;
  tags: string[];
  gated: boolean;
  pipeline_tag: string;
  last_modified: string;
  // no_gguf marks the synthetic "true publisher, no compatible GGUF"
  // entry the search endpoint injects when nobody has quantized this
  // family yet — not a real downloadable result.
  no_gguf?: boolean;
}

export interface HFSearchResponse {
  results: HFSearchResult[];
}

// Reuses the same shape as SmithQuantCandidate — both are
// hf.RankCandidates' output, one from the FR4 sourcing endpoint, one from
// the acquisition track's own /hf/tree.
export type HFQuantCandidate = SmithQuantCandidate;

export interface HFTreeResponse {
  repo: string;
  candidates: HFQuantCandidate[];
  recommended: HFQuantCandidate | null;
}

export interface HFPreflightFile {
  filename: string;
  size_bytes: number;
}

export interface HFPreflightCheck {
  id: string;
  severity: "ok" | "warn" | "block";
  summary: string;
}

export interface HFPreflightReport {
  repo: string;
  files: HFPreflightFile[];
  total_bytes: number;
  dest_dir: string;
  blocked: boolean;
  checks: HFPreflightCheck[];
  requires_backend?: string;
}

export type HFDownloadState =
  | "pending_approval"
  | "queued"
  | "running"
  | "paused"
  | "verifying"
  | "registering"
  | "done"
  | "failed"
  | "cancelled";

export interface HFDownloadFile {
  filename: string;
  dest_rel_path: string;
  bytes_done: number;
  bytes_total: number;
  state: string;
}

export interface HFDownload {
  id: number;
  repo: string;
  revision: string;
  dest_dir: string;
  config_name?: string;
  state: HFDownloadState;
  bytes_done: number;
  bytes_total: number;
  error?: string;
  attempts: number;
  proposed_by?: string;
  created_config_id?: number;
  created_at: number;
  updated_at: number;
  files?: HFDownloadFile[];
}

export interface HFDownloadStartBody {
  repo: string;
  revision?: string;
  files: HFPreflightFile[];
  dest_dir?: string;
  config_name?: string;
}

export interface HFTokenResponse {
  token: string; // masked
  configured: boolean;
}

// SSE payloads (download:progress|state_changed|done|failed — Contract 1
// amendment, go/internal/hfdownload/events.go).
export interface HFDownloadProgressEvent {
  job_id: number;
  state: string;
  bytes_done: number;
  bytes_total: number;
  bytes_per_sec: number;
  eta_s: number;
  current_file: string;
}

export interface HFDownloadStateChangedEvent {
  job_id: number;
  state: HFDownloadState;
  error?: string;
}

export interface HFDownloadDoneEvent {
  job_id: number;
  config_id: number;
  model_id: number;
}

export interface HFDownloadFailedEvent {
  job_id: number;
  error: string;
}

export interface HFDownloadDeletedEvent {
  job_id: number;
}

export type SmithActionStatus =
  | "pending"
  | "approved"
  | "rejected"
  | "executing"
  | "done"
  | "done_unverified"
  | "failed"
  | "superseded";

export type SmithActionRisk = "info" | "low" | "high";

export interface SmithAction {
  id: number;
  investigation_id: number | null;
  conversation_id: number | null;
  finding_id: number | null;
  kind: SmithActionKind;
  title: string;
  detail: Record<string, unknown>;
  risk: SmithActionRisk;
  status: SmithActionStatus;
  self_evicting: boolean;
  handoff: SmithHandoff | null;
  dedupe_key: string;
  result: SmithActionResult | null;
  created_by: string;
  approved_by: string | null;
  audit_ref: string | null;
  created_at: number;
  executed_at: number | null;
  verified_at: number | null;
  resolved_at: number | null;
  // Server-computed (go/internal/smith/actions.go's Action.MarshalJSON) —
  // autonomous-remediation Sprint 6 replaced a hand-maintained
  // PROCEDURIZABLE_KINDS TS const (a second copy of the backend's kind map
  // with nothing cross-checking it) with this field, so ActionCard never
  // has to re-derive "is a procedure mapped, and is this action in a state
  // where that's actionable" on its own.
  procedurizable: boolean;
}

export interface SmithActionsResponse {
  count: number;
  pending_count: number;
  actions: SmithAction[];
}

// ── Procedure engine (autonomous-remediation Sprint 2/3 — go/internal/
// smith/procedure.go's ProcedureRun/ProcedureStepOutcome, and Sprint 3's
// procedurize.go's ProcedurePreview). Mirrors the Go JSON shapes verbatim.

export interface SmithProcedureStepOutcome {
  index: number;
  title: string;
  argv: string[] | null;
  ok: boolean;
  error?: string;
  exit_code: number;
  duration_ms: number;
  stdout_tail?: string;
  stderr_tail?: string;
  verify?: SmithVerifyResult[];
  at: number;
}

// "precondition_failed" (added 2026-08-27) is distinct from "failed": the
// run never executed a single step because its preconditions weren't met —
// "not applicable to this host," not a genuine attempt-and-failure.
export type SmithProcedureRunStatus = "running" | "awaiting_checkpoint" | "completed" | "failed" | "precondition_failed" | "aborted";

export interface SmithProcedureRun {
  id: number;
  action_id: number;
  procedure_id: string;
  status: SmithProcedureRunStatus;
  current_step: number;
  lease_id?: string;
  steps: SmithProcedureStepOutcome[];
  checkpoint_note?: string;
  started_at: number;
  heartbeat_at: number;
  finished_at: number | null;
}

// smith:procedure_step SSE payload (lib/sse.ts).
export interface SmithProcedureStepEvent {
  action_id: number;
  procedure_id: string;
  status: SmithProcedureRunStatus;
  current_step: number;
  event: string;
}

// GET .../procedure_preview — the downtime-disclosure modal's read-only
// data, projected from the mapped procedure's own registered Impact.
export interface SmithProcedurePreview {
  procedure_id: string;
  title: string;
  needs_maintenance: boolean;
  est_duration_sec: number;
  affected_slots?: string[];
  affected_services?: string[];
  daemon_restart: boolean;
}

// ── Supervision & evaluation harness (autonomous-remediation Sprint 4 —
// go/internal/smith/procedure.go's ProcedureRunSummary/ProcedureScorecard).
// Mirrors the Go JSON shapes verbatim.

export interface SmithProcedureRunSummary extends SmithProcedureRun {
  action_title: string;
  action_status: string;
}

export interface SmithProcedureScorecard {
  action_id: number;
  procedure_id: string;
  run_status: SmithProcedureRunStatus;
  action_status: string;
  completed: boolean;
  precondition_failed: boolean;
  unattended_completion: boolean;
  checkpoints_declared: number;
  checkpoints_reached: number;
  post_verify_passed: boolean;
  steps_total: number;
  steps_completed: number;
  needs_maintenance: boolean;
  est_duration_seconds?: number;
  actual_duration_seconds?: number;
}

// ── Maintenance mode (autonomous-remediation plan, Sprint 1 — go/internal/
// maintenance). Mirrors maintenance.State's JSON shape.
export interface MaintenanceState {
  active: boolean;
  lease_id?: string;
  reason?: string;
  entered_by?: string;
  affected_slots?: string[];
  affected_services?: string[];
  entered_at?: number;
  expires_at?: number;
}

// ── Smith: reasoning tier (P3 — docs/v5-smith.md §4.3/§5). Mirrors
// go/internal/smith.Conversation/Message + the httpapi response wrappers,
// frozen by TestFrozenSmithChatShapes.

export type SmithMessageKind =
  | "user"
  | "smith_deterministic"
  | "smith_reasoning"
  | "action"
  | "runbook"
  | "notice"
  // P7 — one tool-loop round's activity, reusing the evidence column
  // (never a new column; see go/internal/smith/conversations.go's
  // MsgKindToolCall).
  | "tool_call";

export interface SmithConversation {
  id: number;
  title: string;
  tier: "deterministic" | "reasoning";
  created_at: number;
  updated_at: number;
}

// MessageSource is one citation smith read during a web:true turn (P5,
// docs/v5-smith.md §4.8) — mirrors go/internal/smith.MessageSource.
export interface SmithMessageSource {
  provider: string;
  url: string;
  title: string;
  snippet: string;
  fetched_at: number; // unix seconds
  cached: boolean;
}

export interface SmithMessage {
  id: number;
  conversation_id: number;
  kind: SmithMessageKind;
  content: string;
  evidence: string | null; // raw JSON text, nullable
  model: string | null;
  tier: "deterministic" | "reasoning" | null;
  error: string | null;
  token_count: number | null;
  created_at: number;
  sources: SmithMessageSource[]; // never null — [] when this turn had no web research
}

export interface SmithConversationsResponse {
  count: number;
  conversations: SmithConversation[];
}

export interface SmithConversationDetail extends SmithConversation {
  messages: SmithMessage[];
}

// SmithChatContext is one attached error-context item on a chat turn
// (Sprint S3 §3.4, R5). Mirrors go/internal/smith.ChatContext — snake_case
// per the frozen wire convention. When POST /chat carries a non-empty
// `context` array and `text` is empty, the server composes the seed user
// message itself (composeContextSeedMessage) so the FE never string-formats
// evidence. Additive to the frozen smithChatBody shape.
export interface SmithChatContext {
  code: string;
  message: string;
  source: string;
  at: number; // unix seconds
}

// POST /api/v1/smith/chat request body. conversation_id omitted/0 starts a
// new conversation. escalate and web are accepted for wire compatibility but
// no longer sent by the FE — web research is always on unless disabled in
// Settings (Sprint S1), and escalation is automatic. context (Sprint S3
// §3.4) carries attached error context; when present and text is empty, the
// server composes the seed user message from it.
export interface SmithChatRequest {
  conversation_id?: number;
  text: string;
  escalate?: boolean;
  web?: boolean;
  context?: SmithChatContext[];
}

// SmithPendingAsk is a client-only cache slot (never fetched from the
// server) carrying an "Ask smith" affordance's attached context from the
// row the operator clicked (Console alert, Diagnostics notification/finding/
// investigation) to the AskSmith chat component. AskSmith reads + clears it
// on becoming the active tab, then sends a context-seeded chat turn (text=""
// + context). Same client-only-messaging-channel precedent as
// qk.smith.streaming / qk.smith.toolActivity.
export interface SmithPendingAsk {
  context: SmithChatContext[];
}

// 202 response — the answer streams over SSE (smith:token/smith:message_done
// keyed by message_id), never returned inline here.
export interface SmithChatResponse {
  conversation_id: number;
  message_id: number;
}

// SmithWebProviderSettings is one adapter's editable config. api_key is
// masked on read (httpapi.maskSecret); submitting the same masked value
// back on a PUT leaves the real key unchanged (resolveAPIKeyWrite).
export interface SmithWebProviderSettings {
  base_url: string;
  enabled: boolean;
  api_key: string;
}

export interface SmithWebSettings {
  enabled: boolean;
  provider_order: string[];
  cache_ttl: string;
  searxng: SmithWebProviderSettings;
  firecrawl: SmithWebProviderSettings;
  direct: SmithWebProviderSettings;
  customsearch: SmithWebProviderSettings;
  customfetch: SmithWebProviderSettings;
}

export interface SmithSettings {
  model: string;
  handoff_offerings: number[];
  brain_chain: string[];
  build_refresh_watchlist: string[];
  comfyui_keep_files: string[];
  schedule: SmithSchedule;
  thresholds: {
    gtt_warn_pct: number;
    gtt_crit_pct: number;
    disk_warn_pct: number;
    disk_crit_pct: number;
    device_lost_window_minutes: number;
    build_refresh_behind_n: number;
    compressor_failopen_warn_pct: number;
  };
  web: SmithWebSettings;
  tools: SmithToolsSettings;
  retention: SmithRetentionSettings;
  self_review: SmithSelfReviewSettings;
  brain_residency: SmithBrainResidencySettings;
}

// SmithToolsSettings mirrors go/internal/smith.ToolsConfig.
export interface SmithToolsSettings {
  enabled: boolean;
  mode: "auto" | "native" | "fenced" | "off";
  max_rounds: number;
}

// SmithRetentionSettings mirrors go/internal/smith.Retention. A *_days <= 0
// (or info_hours <= 0) skips that tier entirely rather than deleting
// everything.
export interface SmithRetentionSettings {
  enabled: boolean;
  ok_days: number;
  info_hours: number;
  warn_crit_days: number;
  web_cache_days: number;
  web_cache_max_rows: number;
}

// SmithSelfReviewSettings mirrors go/internal/smith.SelfReview. The sweep
// interval itself is a fixed backend constant — only enabled/grace_minutes
// are operator-tunable.
export interface SmithSelfReviewSettings {
  enabled: boolean;
  grace_minutes: number;
}

// SmithBrainResidencySettings mirrors go/internal/smith.BrainResidency —
// the opt-in "keep the brain always loaded" choice, default false.
export interface SmithBrainResidencySettings {
  stay_resident: boolean;
}

// ── Smith: standing autonomy policy (autonomous-remediation Sprint 5,
// docs/v5-smith.md §13.3). A separate endpoint from SmithSettings above,
// not a field on it — PUT carries its own conditional step-up gate.

export interface SmithAutonomyProcedurePolicy {
  enabled: boolean;
  cooldown_seconds: number;
  max_per_day: number;
}

export interface SmithAutonomyPolicy {
  enabled: boolean;
  procedures: Record<string, SmithAutonomyProcedurePolicy>;
}

export interface SmithAutonomyEligibleProcedure {
  id: string;
  title: string;
}

export interface SmithAutonomyResponse {
  policy: SmithAutonomyPolicy;
  eligible_procedures: SmithAutonomyEligibleProcedure[];
}

// ── Smith: knowledge base (P4 — docs/v5-smith.md §4.7/§5). Mirrors
// go/internal/smith.Chunk / smith.BlockedItem + the httpapi response wrappers.

// Chunk is one embedded KB entry, resolved via GET /kb/{ref} — exactly
// what a Finding's kb_refs names.
export interface SmithKBChunk {
  ref: string;
  doc: string;
  slug: string;
  title: string;
  category: string;
  source: string;
  body: string;
}

export interface SmithKBRefResponse {
  chunk: SmithKBChunk;
}

// BlockedItem is one docs/investigations.md entry (GET /kb/blocked) — the
// "externally blocked work" list on Help → Diagnostics.
export interface SmithBlockedItem {
  number: number;
  title: string;
  status: "open" | "resolved" | "closed";
  status_text: string;
  blocked_on: string;
  where_to_check: string;
  when_unblocked: string;
  last_checked: string;
  urls: string[];
}

export interface SmithKBBlockedResponse {
  count: number;
  items: SmithBlockedItem[];
}

// SSE payloads (Contract 1 amendment, docs/v5-smith.md §5). smith:token
// deltas arrive batched server-side (~120ms), not per-token.
export interface SmithTokenEvent {
  conversation_id: number;
  message_id: number;
  delta: string;
}

// smith:status (S4 phase 2, docs/v5-ops-sprints-2026-08-21.md) — a short
// human sentence about a turn's progress (e.g. "loading brain model — first
// load typically takes 20-90s"), published by Smith.publishStatus
// (reasoning.go). status is free-text prose, not a machine-readable ETA
// field — there is no numeric duration to drive a progress bar with.
export interface SmithStatusEvent {
  conversation_id: number;
  message_id: number;
  status: string;
}

export interface SmithMessageDoneEvent {
  conversation_id: number;
  message_id: number;
  tier: "deterministic" | "reasoning";
}

export interface SmithTierChangedEvent {
  conversation_id: number;
  tier: "deterministic" | "reasoning";
  reason: string;
}

// smith:tool_call (P7) — one tool-round liveness signal. "started" fires
// before the tool runs, since the round gate withholds a fenced-mode
// round's content from smith:token entirely until it's known to be prose —
// without this event, a slow run_check would look like a hang.
export interface SmithToolActivityEvent {
  conversation_id: number;
  message_id: number;
  round: number;
  name: string;
  status: "started" | "done" | "error";
  detail: string;
}

// One call inside a tool_call message's evidence (parsed from the raw JSON
// string on SmithMessage.evidence — mirrors go/internal/smith.toolCallRecord).
export interface SmithToolCallRecord {
  id: string;
  name: string;
  args: Record<string, unknown>;
  ok: boolean;
  summary: string;
  duration_ms: number;
  error: string | null;
}

export interface SmithToolCallEvidence {
  round: number;
  calls: SmithToolCallRecord[];
}

// smith:action_update (P2 / Sprint S3-Go) — emitted on every action status
// change (pending→approved→executing→done/failed, reject, handoff resolve).
// Mirrors go/internal/smith.publishActionUpdate's payload. The FE reads
// action_id to invalidate the singular qk.smith.action(id) cache so a live
// ActionCard in a transcript refetches fresh state.
export interface SmithActionUpdateEvent {
  action_id: number;
  status: string;
  kind: string;
  risk: string;
  self_evicting: boolean;
  pending_count?: number;
}
