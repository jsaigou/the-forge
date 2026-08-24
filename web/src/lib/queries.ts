import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { api, setCsrfToken } from "./api";
import type {
  APIKeyCreateRequest,
  AuthConfigPutRequest,
  AuthPolicyPutRequest,
  BillingSettings,
  CatalogBenchmark,
  CatalogConfig,
  CatalogFamily,
  CatalogGenealogy,
  CatalogModel,
  CatalogNote,
  CatalogOffering,
  CatalogService,
  CatalogVariant,
  CostSettingsUpdate,
  DashboardLayout,
  FavoritesResponse,
  IdentityLinkCreateRequest,
  MetricsSettingsUpdate,
  MonitorSettingsUpdate,
  ProviderCreateRequest,
  ProviderKeyRequest,
  ProviderUpdateRequest,
  Reservation,
  RouterConfigUpdate,
  RouterSettingsUpdate,
  SchedulerConfig,
  SchedulerJobInput,
  SmithActionKind,
  SmithActionRisk,
  SmithChatContext,
  SmithChatRequest,
  SmithPendingAsk,
  SmithSettings,
  SmithAutonomyPolicy,
  SmithToolActivityEvent,
  SystemSettingsUpdate,
  TotpConfirmRequest,
  UISettingsUpdate,
  WebAuthnFinishAssertRequest,
  WebAuthnFinishRegisterRequest,
} from "./types";

// Sprint 13 (Sprint 12 Phase 9, plan risk 12) — Settings mounts one panel at
// a time, so with the 5s telemetry default every section switch refetched
// and flashed a loading state. Settings data is configuration, not
// telemetry: it only changes when an operator saves, and every save
// invalidates its own key — so these hooks get a generous staleTime and the
// telemetry hooks keep the client default.
export const SETTINGS_STALE_MS = 5 * 60_000;

export const qk = {
  status: ["status"] as const,
  metrics: ["metrics"] as const,
  schedulerStatus: ["scheduler", "status"] as const,
  notifications: (includeDismissed = false) => ["notifications", includeDismissed] as const,
  infraServices: ["infra-services"] as const,
  maintenance: ["maintenance"] as const,
  usage: (w: string) => ["usage", w] as const,
  usageEvents: (w: string) => ["usage", "events", w] as const,
  usageHeatmap: (w: string, tz: string) => ["usage", "heatmap", w, tz] as const,
  modelCards: (w: string) => ["models", "cards", w] as const,
  configCards: (w: string) => ["configs", "cards", w] as const,
  reservations: ["reservations"] as const,
  schedulerJobs: ["scheduler", "jobs"] as const,
  schedulerConfig: ["scheduler", "config"] as const,
  compressor: ["compressor"] as const,
  compressorSummary: (w: string) => ["compressor", "summary", w] as const,
  costSummary: (w: string) => ["cost", "summary", w] as const,
  costEnergyHistory: (w: string, res: string) => ["cost", "energy-history", w, res] as const,
  costSettings: ["cost", "settings"] as const,
  routerSettings: ["router", "settings"] as const,
  // Sprint 12 (was H) Phase 6 — the typed infra.*/metrics.*/ui.* groups
  // Phase 2 built with no frontend caller until the new Settings panels.
  routerConfig: ["router", "config"] as const,
  monitorSettings: ["monitor", "settings"] as const,
  metricsSettings: ["metrics", "settings"] as const,
  uiSettings: ["ui", "settings"] as const,
  dashboardLayout: ["dashboard", "layout"] as const,
  systemSettings: ["system", "settings"] as const,
  schedulerSeed: ["scheduler", "seed"] as const,
  providers: ["providers"] as const,
  routingPreview: (model: string, assumeDown: string[], assumeDisabled: string[]) =>
    ["routing", "preview", model, assumeDown, assumeDisabled] as const,
  billingSettings: ["billing", "settings"] as const,
  metricsHistory: (w: string, series: string[], res: string) => ["metrics", "history", w, series, res] as const,
  modes: ["modes"] as const,
  profiles: ["profiles"] as const,
  profile: (mode: string) => ["profile", mode] as const,
  profileProgress: ["profile", "progress"] as const,
  profileActive: ["profile", "active"] as const,
  authPolicy: ["auth", "policy"] as const,
  authConfig: ["auth", "config"] as const,
  webauthnCredentials: ["auth", "webauthn", "credentials"] as const,
  identityLinks: ["auth", "identity-links"] as const,
  keys: (kind?: string) => ["keys", kind ?? "all"] as const,
  recoveryCodes: ["auth", "recovery-codes"] as const,
  // ── Model Catalog ──
  catalogModels: ["catalog", "models"] as const,
  catalogVariants: ["catalog", "variants"] as const,
  catalogVariantsForModel: (modelId: number) => ["catalog", "variants", "model", modelId] as const,
  catalogArtifacts: ["catalog", "artifacts"] as const,
  catalogArtifactsForVariant: (variantId: number) => ["catalog", "artifacts", "variant", variantId] as const,
  catalogConfigs: ["catalog", "configs"] as const,
  catalogConfigsForVariant: (variantId: number) => ["catalog", "configs", "variant", variantId] as const,
  catalogOfferings: ["catalog", "offerings"] as const,
  catalogOfferingsForModel: (modelId: number) => ["catalog", "offerings", "model", modelId] as const,
  catalogBenchmarks: (subjectType?: string, subjectId?: number) =>
    ["catalog", "benchmarks", subjectType ?? "all", subjectId ?? "all"] as const,
  catalogNotes: (subjectType?: string, subjectId?: number) =>
    ["catalog", "notes", subjectType ?? "all", subjectId ?? "all"] as const,
  catalogServices: ["catalog", "services"] as const,
  catalogFamilies: ["catalog", "families"] as const,
  catalogGenealogies: ["catalog", "genealogies"] as const,
  catalogEngines: ["catalog", "engines"] as const,
  catalogBuilds: ["catalog", "builds"] as const,
  catalogQuantizations: ["catalog", "quantizations"] as const,
  catalogFormats: ["catalog", "formats"] as const,
  modelFiles: ["models", "files"] as const,
  favorites: (subjectType = "config") => ["favorites", subjectType] as const,
  // ── Smith (Wave 2 — docs/v5-smith-wave2.md §3) ──
  smith: {
    status: ["smith", "status"] as const,
    findings: (since?: string, severity?: string) => ["smith", "findings", since, severity] as const,
    checks: ["smith", "checks"] as const,
    investigations: (status?: string) => ["smith", "investigations", status] as const,
    investigation: (id: number) => ["smith", "investigation", id] as const,
    // Wave 3 / P2 — action model + handoff (track W3-C).
    actions: (status?: string, investigationId?: number) => ["smith", "actions", status, investigationId] as const,
    action: (id: number) => ["smith", "action", id] as const,
    // P3 — reasoning tier.
    conversations: ["smith", "conversations"] as const,
    conversation: (id: number) => ["smith", "conversation", id] as const,
    settings: ["smith", "settings"] as const,
    // P4 — knowledge base.
    kbRef: (ref: string) => ["smith", "kb", "ref", ref] as const,
    kbBlocked: ["smith", "kb", "blocked"] as const,
    // Sprint 2/3 — procedure engine. procedurePreview is a plain server
    // fetch keyed by the SOURCE action id (never cached across actions);
    // procedureRun is the checkpoint UI's live-polled run state, invalidated
    // by smith:procedure_step (lib/sse.ts).
    procedurePreview: (actionId: number) => ["smith", "procedurePreview", actionId] as const,
    procedureRun: (actionId: number) => ["smith", "procedureRun", actionId] as const,
    // Sprint 4 — supervision & evaluation harness. procedureRuns is the
    // history list (invalidated by smith:procedure_step like procedureRun
    // above, since a step/checkpoint/finish on ANY run can change the list's
    // most-recent-first ordering or a row's status); procedureScorecard is
    // keyed per action like procedureRun.
    procedureRuns: ["smith", "procedureRuns"] as const,
    procedureScorecard: (actionId: number) => ["smith", "procedureScorecard", actionId] as const,
    // Sprint 5 — standing autonomy policy.
    autonomy: ["smith", "autonomy"] as const,
    // Client-only (never fetched from the server): the in-flight streamed
    // text for one message, written by lib/sse.ts's smith:token listener and
    // cleared on smith:message_done once the persisted row is authoritative.
    streaming: (messageId: number) => ["smith", "streaming", messageId] as const,
    // P7 — client-only, same shape as streaming above: live tool-round
    // activity for one in-flight message, written by smith:tool_call and
    // cleared on smith:message_done (the persisted tool_call rows in the
    // conversation fetch are what's authoritative afterward).
    toolActivity: (messageId: number) => ["smith", "toolActivity", messageId] as const,
    // Sprint S3-Web — client-only messaging channel for "Ask smith"
    // affordances on error rows (Console alerts, Diagnostics notifications/
    // findings/investigations). One slot at a time: clicking an affordance
    // overwrites the previous pending ask (the operator's latest click wins,
    // same UX as prefilling the input). AskSmith reads + clears it on
    // becoming the active tab, then sends a context-seeded chat turn.
    pendingAsk: ["smith", "pendingAsk"] as const,
  },
};

export function useStatus() {
  return useQuery({ queryKey: qk.status, queryFn: api.status, refetchInterval: 15_000 });
}

export function useMetrics() {
  return useQuery({ queryKey: qk.metrics, queryFn: api.metrics, refetchInterval: 8_000 });
}

export function useSchedulerStatus() {
  return useQuery({ queryKey: qk.schedulerStatus, queryFn: api.schedulerStatus, refetchInterval: 10_000 });
}

export function useInfraServices() {
  return useQuery({ queryKey: qk.infraServices, queryFn: api.infraServices, refetchInterval: 15_000 });
}

// ── Notifications (product/QA sprint, 2026-07-29 — Dashboard panel) ─────────

export function useNotifications(includeDismissed = false) {
  return useQuery({
    queryKey: qk.notifications(includeDismissed),
    queryFn: () => api.notifications(includeDismissed),
    refetchInterval: 15_000,
  });
}

function invalidateNotifications(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ["notifications"] });
}

export function useAcknowledgeNotification() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.acknowledgeNotification(id),
    onSuccess: () => invalidateNotifications(qc),
  });
}

export function useDismissNotification() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.dismissNotification(id),
    onSuccess: () => invalidateNotifications(qc),
  });
}

export function useAcknowledgeAllNotifications() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.acknowledgeAllNotifications(),
    onSuccess: () => invalidateNotifications(qc),
  });
}

export function useUsage(window_: string) {
  return useQuery({ queryKey: qk.usage(window_), queryFn: () => api.usage(window_) });
}

export function useUsageEvents(window_: string) {
  return useQuery({ queryKey: qk.usageEvents(window_), queryFn: () => api.usageEvents(window_) });
}

// Sprint L — Dashboard Overview activity heatmap. tz defaults to the
// browser's own IANA zone so bucket boundaries match what the operator
// actually sees as "today" (see the backend's local-calendar-day bucketing).
export function useUsageHeatmap(window_: string, tz = Intl.DateTimeFormat().resolvedOptions().timeZone) {
  return useQuery({ queryKey: qk.usageHeatmap(window_, tz), queryFn: () => api.usageHeatmap(window_, tz) });
}

// Mobile F6: the cards payloads are the app's largest fetches (~700-900 KB)
// and every ModelCardView remounts these hooks, so with the default
// staleTime 0 each staggered mount refetched (4× cards fetches on one fresh
// Console load). 30s staleTime makes remounts cache hits; explicit
// invalidations (profile:done) still force refetches regardless.
const CARDS_STALE_MS = 30_000;

export function useModelCards(window_: string) {
  return useQuery({ queryKey: qk.modelCards(window_), queryFn: () => api.modelCards(window_), staleTime: CARDS_STALE_MS });
}

export function useConfigCards(window_: string) {
  return useQuery({ queryKey: qk.configCards(window_), queryFn: () => api.configCards(window_), staleTime: CARDS_STALE_MS });
}

export function useReservations() {
  return useQuery({ queryKey: qk.reservations, queryFn: api.reservations });
}

export function useSchedulerConfig() {
  return useQuery({ queryKey: qk.schedulerConfig, queryFn: api.schedulerConfig, staleTime: SETTINGS_STALE_MS });
}

export function useCompressorConfig() {
  return useQuery({ queryKey: qk.compressor, queryFn: api.compressorConfig, staleTime: SETTINGS_STALE_MS });
}

// Cost/savings sprint Phase 3 — per-proxy cache-hit/latency/time-saved summary.
export function useCompressorSummary(window_: string) {
  return useQuery({ queryKey: qk.compressorSummary(window_), queryFn: () => api.compressorSummary(window_) });
}

// ── Cost (Phase 2) ────────────────────────────────────────────────────────

export function useCostSummary(window_: string) {
  return useQuery({ queryKey: qk.costSummary(window_), queryFn: () => api.costSummary(window_) });
}

export function useCostEnergyHistory(window_: string, res = "auto") {
  return useQuery({
    queryKey: qk.costEnergyHistory(window_, res),
    queryFn: () => api.costEnergyHistory(window_, res),
  });
}

export function useCostSettings() {
  return useQuery({ queryKey: qk.costSettings, queryFn: api.costSettings, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateCostSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (cfg: CostSettingsUpdate) => api.updateCostSettings(cfg),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.costSettings });
      qc.invalidateQueries({ queryKey: ["cost"] });
    },
  });
}

export function useRouterSettings() {
  return useQuery({ queryKey: qk.routerSettings, queryFn: api.routerSettings, staleTime: SETTINGS_STALE_MS });
}

// Phase 6: PUT /api/v1/router/settings's first frontend caller (see
// RouterSettingsUpdate's doc comment in types.ts). apply="immediate" — no
// qk.status invalidation needed, unlike the restart-mode mutations below.
export function useUpdateRouterSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: RouterSettingsUpdate) => api.updateRouterSettings(patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.routerSettings }),
  });
}

// ── Sprint 12 (was H) Phase 6 — infra.*/metrics.*/ui.* groups ──────────────
// Every mutation here that can flip apply="restart" also invalidates
// qk.status, so the restart-required banner/topbar pip (Status.restart_required)
// appears immediately after Save instead of waiting for useStatus's own
// 15s poll.

export function useRouterConfig() {
  return useQuery({ queryKey: qk.routerConfig, queryFn: api.routerConfig, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateRouterConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: RouterConfigUpdate) => api.updateRouterConfig(patch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.routerConfig });
      qc.invalidateQueries({ queryKey: qk.status });
    },
  });
}

export function useMonitorSettings() {
  return useQuery({ queryKey: qk.monitorSettings, queryFn: api.monitorSettings, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateMonitorSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: MonitorSettingsUpdate) => api.updateMonitorSettings(patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.monitorSettings }),
  });
}

export function useMetricsSettings() {
  return useQuery({ queryKey: qk.metricsSettings, queryFn: api.metricsSettings, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateMetricsSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: MetricsSettingsUpdate) => api.updateMetricsSettings(patch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.metricsSettings });
      qc.invalidateQueries({ queryKey: qk.status }); // sample_interval_s is restart-mode
    },
  });
}

export function useUiSettings() {
  return useQuery({ queryKey: qk.uiSettings, queryFn: api.uiSettings, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateUiSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: UISettingsUpdate) => api.updateUiSettings(patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.uiSettings }),
  });
}

// ADR-0011: custom dashboard pages — system-wide layout, full-replace PUT.
export function useDashboardLayout() {
  return useQuery({ queryKey: qk.dashboardLayout, queryFn: api.dashboardLayout, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateDashboardLayout() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (layout: DashboardLayout) => api.updateDashboardLayout(layout),
    onSuccess: (data) => qc.setQueryData(qk.dashboardLayout, data),
  });
}

// Read-only, admin-gated on the backend — General panel's daemon strip.
// `enabled` lets callers skip the request entirely for non-admins rather
// than firing a request that's guaranteed to 403.
export function useSystemSettings(enabled: boolean) {
  return useQuery({ queryKey: qk.systemSettings, queryFn: api.systemSettings, enabled, staleTime: SETTINGS_STALE_MS });
}

// Sprint 12 Phase 7 — the Danger Zone's mutations. Preflight is a plain
// useMutation (not a query): it's an on-demand dry-run action, never cached
// or refetched, and its result is only ever the return value of the one
// call that triggered it.
export function useUpdateSystemSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: SystemSettingsUpdate) => api.updateSystemSettings(patch),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.systemSettings });
      qc.invalidateQueries({ queryKey: qk.status });
    },
  });
}

export function useSystemPreflight() {
  return useMutation({ mutationFn: (patch: SystemSettingsUpdate) => api.systemPreflight(patch) });
}

export function useSystemRestart() {
  return useMutation({ mutationFn: () => api.systemRestart() });
}

export function useSchedulerSeed() {
  return useQuery({ queryKey: qk.schedulerSeed, queryFn: api.schedulerSeed, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateSchedulerSeed() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (cfg: SchedulerConfig) => api.updateSchedulerSeed(cfg),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedulerSeed }),
  });
}

export function useLoadModel() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ mode, slot }: { mode: string; slot: string }) => api.loadModel(mode, slot),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    },
  });
}

export function useUnloadSlot() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (slot: string) => api.unloadSlot(slot),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    },
  });
}

export function useSwitchMode() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (mode: string) => api.switchMode(mode),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    },
  });
}

export function useCreateReservation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (r: Parameters<typeof api.createReservation>[0]) => api.createReservation(r),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.reservations }),
  });
}

export function useCancelReservation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (label: string) => api.cancelReservation(label),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.reservations }),
  });
}

// ── Scheduler jobs (P3 — cron-style forced loads) ─────────────────────────

export function useSchedulerJobs() {
  return useQuery({ queryKey: qk.schedulerJobs, queryFn: api.schedulerJobs, staleTime: SETTINGS_STALE_MS });
}

export function useCreateSchedulerJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (j: SchedulerJobInput) => api.createSchedulerJob(j),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedulerJobs }),
  });
}

export function useUpdateSchedulerJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, job }: { id: number; job: SchedulerJobInput }) => api.updateSchedulerJob(id, job),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedulerJobs }),
  });
}

export function useDeleteSchedulerJob() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.deleteSchedulerJob(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedulerJobs }),
  });
}

export function useRunSchedulerJobNow() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.runSchedulerJobNow(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.schedulerJobs });
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    },
  });
}

export function useUpdateSchedulerConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (cfg: SchedulerConfig) => api.updateSchedulerConfig(cfg),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.schedulerConfig }),
  });
}

export function useCompressorRestart() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (service: string) => api.compressorRestart(service),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.compressor }),
  });
}

export function useCompressorTeardown() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (service: string) => api.compressorTeardown(service),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.compressor }),
  });
}

export function useCompressorCreateProxy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { service: string; label: string; target_url: string }) => api.compressorCreateProxy(body),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.compressor });
      qc.invalidateQueries({ queryKey: qk.providers });
    },
  });
}

export function useCompressorPassthrough() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ scope, enabled, service }: { scope: "all" | "proxy"; enabled: boolean; service?: string }) =>
      api.compressorSetPassthrough(scope, enabled, service),
    // Also invalidate infraServices (2026-07-29): the services strip's A0 row
    // and Compressor proxy rows derive their "compressing"/"bypassed" detail
    // from passthrough state — refetch so the toggle reads as applied
    // immediately instead of waiting for the next poll.
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.compressor });
      qc.invalidateQueries({ queryKey: qk.infraServices });
    },
  });
}

export function useServiceModeToggle() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ name, action }: { name: string; action: "start" | "stop" }) =>
      action === "start" ? api.serviceModeStart(name) : api.serviceModeStop(name),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.infraServices });
    },
  });
}

export function useTtsToggle() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (action: "start" | "stop") => (action === "start" ? api.ttsStart() : api.ttsStop()),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.infraServices });
    },
  });
}

// ── Providers (Sprint 0 §0.3 / §0.9) ──────────────────────────────────────────

export function useProviders() {
  return useQuery({ queryKey: qk.providers, queryFn: api.providers, refetchInterval: 30_000 });
}

export function useCreateProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: ProviderCreateRequest) => api.createProvider(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.providers }),
  });
}

// Phase 7: keyed by id (the stable handle across a rename), not name —
// see api.ts's doc comment on the same change.
export function useUpdateProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, req }: { id: number; req: ProviderUpdateRequest }) => api.updateProvider(id, req),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.providers }),
  });
}

export function useSetProviderKey() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, req }: { id: number; req: ProviderKeyRequest }) => api.setProviderKey(id, req),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.providers }),
  });
}

export function useDeleteProvider() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.deleteProvider(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.providers }),
  });
}

// product/QA sprint, 2026-07-29 — billing-API auto-discovery.
export function useDiscoverProviderBilling() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.discoverProviderBilling(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.providers }),
  });
}

// ── Routing preview (Phase 7) ─────────────────────────────────────────────────
// Read-only, no side effects (never loads a local model) — safe to fire on
// every keystroke/toggle, so this is a plain query, not a mutation. Disabled
// until a model is actually picked (enabled: !!model).
export function useRoutingPreview(model: string, opts?: { assumeDown?: string[]; assumeDisabled?: string[] }) {
  return useQuery({
    queryKey: qk.routingPreview(model, opts?.assumeDown ?? [], opts?.assumeDisabled ?? []),
    queryFn: ({ signal }: { signal?: AbortSignal }) => api.routingPreview(model, opts, signal),
    enabled: !!model,
    placeholderData: keepPreviousData,
  });
}

// ── Favorites (product/QA sprint, 2026-07-29 — Console config-card starring) ─

export function useFavorites(subjectType = "config") {
  return useQuery({ queryKey: qk.favorites(subjectType), queryFn: () => api.favorites(subjectType) });
}

// Sprint K (2026-08-05): optimistic updates for the star toggle. Before
// this, the glyph didn't flip until invalidateQueries's refetch landed —
// the button just sat disabled showing the OLD state for a full round
// trip, which is what made the color-morph animation worth adding in the
// first place (there was nothing to morph in response to). onMutate
// flips the cached favorites list immediately; onError rolls it back to
// the pre-mutation snapshot rather than leaving a stale optimistic value
// if the request actually fails.
export function useAddFavorite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ subjectType, id }: { subjectType: string; id: number }) => api.addFavorite(subjectType, id),
    onMutate: async ({ subjectType, id }) => {
      await qc.cancelQueries({ queryKey: qk.favorites(subjectType) });
      const prev = qc.getQueryData<FavoritesResponse>(qk.favorites(subjectType));
      if (prev && !prev.subject_ids.includes(id)) {
        qc.setQueryData<FavoritesResponse>(qk.favorites(subjectType), { ...prev, subject_ids: [...prev.subject_ids, id] });
      }
      return { prev, subjectType };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.prev) qc.setQueryData(qk.favorites(ctx.subjectType), ctx.prev);
    },
    onSettled: (_data, _err, { subjectType }) => qc.invalidateQueries({ queryKey: qk.favorites(subjectType) }),
  });
}

export function useRemoveFavorite() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ subjectType, id }: { subjectType: string; id: number }) => api.removeFavorite(subjectType, id),
    onMutate: async ({ subjectType, id }) => {
      await qc.cancelQueries({ queryKey: qk.favorites(subjectType) });
      const prev = qc.getQueryData<FavoritesResponse>(qk.favorites(subjectType));
      if (prev) {
        qc.setQueryData<FavoritesResponse>(qk.favorites(subjectType), {
          ...prev,
          subject_ids: prev.subject_ids.filter((sid: number) => sid !== id),
        });
      }
      return { prev, subjectType };
    },
    onError: (_err, _vars, ctx) => {
      if (ctx?.prev) qc.setQueryData(qk.favorites(ctx.subjectType), ctx.prev);
    },
    onSettled: (_data, _err, { subjectType }) => qc.invalidateQueries({ queryKey: qk.favorites(subjectType) }),
  });
}

// ── Billing settings (Sprint 0 §0.9) ─────────────────────────────────────────

export function useBillingSettings() {
  return useQuery({ queryKey: qk.billingSettings, queryFn: api.billingSettings, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateBillingSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (cfg: BillingSettings) => api.updateBillingSettings(cfg),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.billingSettings }),
  });
}

// ── Metrics history + export (Sprint 0 §0.4) ──────────────────────────────────

export function useMetricsHistory(window_: string, series: string[], res = "auto") {
  return useQuery({
    queryKey: qk.metricsHistory(window_, series, res),
    queryFn: () => api.metricsHistory(window_, series, res),
  });
}

// useMetricsExport is a mutation (not a cached query) — it fetches the
// full-resolution CSV/JSON dump on demand and triggers a browser download.
export function useMetricsExport() {
  return useMutation({
    mutationFn: ({ format, window_ }: { format: "csv" | "json"; window_?: string }) =>
      api.metricsExport(format, window_),
    onSuccess: (text, { format }) => {
      const ext = format === "csv" ? "csv" : "json";
      const mime = format === "csv" ? "text/csv" : "application/json";
      const blob = new Blob([text], { type: mime });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `forge-metrics-${Date.now()}.${ext}`;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(url);
    },
  });
}

// Re-exported for the reservation form, which builds this shape directly.
export type { Reservation };

// ── Modes (Sprint 0 §0.9 — read-only) ─────────────────────────────────────────

export function useModes() {
  return useQuery({ queryKey: qk.modes, queryFn: api.modes });
}

// ── Profiling (PROFILE track — docs/v5-profiling-benchmarks.md) ──────────────

export function useProfiles() {
  return useQuery({ queryKey: qk.profiles, queryFn: api.profiles });
}

// useProfile polls GET /api/v1/profile/{mode} every 2s while `poll` is true.
// This is the primary, SSE-independent source of "is a run in progress /
// did it just finish" feedback (docs/v5-profiling-benchmarks.md §10 root
// cause #4) — the `running` field comes straight from the backend's
// runner.IsRunning(), so it works even if the profile:* SSE events never
// reach the browser. SSE (see sse.ts) layers richer phase detail on top via
// qk.profileProgress when it does arrive, but nothing depends on it.
export function useProfile(mode: string | null, opts?: { poll?: boolean }) {
  return useQuery({
    queryKey: mode ? qk.profile(mode) : ["profile", "none"],
    queryFn: () => api.profile(mode!),
    enabled: !!mode,
    refetchInterval: opts?.poll ? 2000 : false,
  });
}

export function useProfileRun() {
  return useMutation({
    mutationFn: (req: { mode: string }) => api.profileRun(req),
  });
}

// useProfileProgress reads the SSE-driven phase-detail cache (evicting /
// loading / filling / measuring / benchmarking, with intermediate measured
// values). It is decoration only — ProfileRunCard's visibility and
// running/done/failed state come from the useProfile() poll above, not from
// this. Returns null until the first profile:* event of the current run
// arrives.
export function useProfileProgress(): ProfileProgressState | null {
  const { data } = useQuery<ProfileProgressState | null>({
    queryKey: qk.profileProgress,
    queryFn: () => null as ProfileProgressState | null,
    staleTime: Infinity,
  });
  return data ?? null;
}

// useProfileActive reads which mode (if any) currently has a profile run
// being tracked. Stored in the query cache (not component state) so it
// survives ProfileRunCard unmounting when the user switches away from the
// Settings tab mid-run.
//
// `startedAt` (client clock, ms) is the freshness gate used by
// useProfileRunTracker below: a poll response is only treated as
// authoritative once it was fetched *after* startedAt. This replaces an
// earlier "everRunning" heuristic (wait until we've observed running:true
// at least once before trusting running:false) that had a real bug — a run
// that fails synchronously, before the network round-trip for the very
// first poll completes (e.g. a vLLM-backend mode rejected instantly by
// Run()), would never be observed as running, so everRunning would never
// flip true, and the UI would show "in progress" forever with no way out.
// Gating on fetch freshness instead of "have we seen X" is correct
// regardless of how fast the run finishes.
export interface ProfileActiveState {
  mode: string;
  startedAt: number;
}

export function useProfileActive() {
  return useQuery<ProfileActiveState | null>({
    queryKey: qk.profileActive,
    queryFn: () => null as ProfileActiveState | null,
    staleTime: Infinity,
  });
}

// useProfileRunTracker is the single global driver of "has the active
// profile run finished yet." Mount it once, at the app root (App.tsx), so
// it keeps polling and finalizing state regardless of which tab is open —
// tying this to the profiling UI's own mount lifetime (the original
// version of this fix, back when it lived in the now-deleted
// ProfilingPanel.tsx) meant navigating away from Settings mid-run silently
// stopped the poll, leaving the topbar throbber stuck on stale SSE state
// indefinitely. ProfileRunCard (Phase 8 — profiling merged into
// Benchmarks) itself only *reads* useProfileActive() / useProfileProgress()
// and *writes* useProfileActive() on submit; this hook owns the poll and
// the finalization logic.
// A real run-in-progress banner disappearing seconds early was caught live
// (Phase 8, pre-release feedback sprint) — a genuine race, not a UI glitch:
// POST /api/v1/profile/run (handleProfileRun) writes its 202 response and
// THEN spawns the goroutine that calls Runner.Run, which is what actually
// sets running=true (under its own mutex, at the top of Run). The gap
// between "202 returned to the client" and "goroutine's first statement
// executes" is normally sub-millisecond, but this component's very first
// poll can land inside it, reading running:false — a value that passes the
// existing dataUpdatedAt >= startedAt freshness check (it WAS fetched after
// startedAt) even though it predates the run actually starting server-side.
// Closing the race properly means the backend marking itself running
// synchronously before responding; done here instead, on the read side,
// since it doesn't touch Run()'s locking or its existing test coverage: a
// running:false read is only trusted once it arrives comfortably after
// startedAt, not merely after it. Goroutine dispatch is near-instant on an
// idle Go runtime, so this buffer is pure safety margin, not a real delay
// an operator would notice.
const PROFILE_START_RACE_GUARD_MS = 1500;

export function useProfileRunTracker(): void {
  const qc = useQueryClient();
  const { data: active } = useProfileActive();
  const mode = active?.mode ?? null;
  const profileStatus = useProfile(mode, { poll: true });

  useEffect(() => {
    if (!active || !profileStatus.data) return;
    // Ignore cache reads that predate this run starting (e.g. a stale
    // cached GET from before the operator clicked "Evict & Profile") OR
    // that merely arrived soon after — see PROFILE_START_RACE_GUARD_MS.
    if (profileStatus.dataUpdatedAt < active.startedAt + PROFILE_START_RACE_GUARD_MS) return;
    if (profileStatus.data.running) return; // still running — keep polling

    // A fresh read confirms the run is no longer running. This fires
    // whether the run took 200ms (instant rejection) or 3 minutes.
    const data = profileStatus.data;
    const failed = !!data.last_error;
    qc.setQueryData(qk.profileProgress, {
      phase: failed ? "failed" : "done",
      running: false,
      mode: active.mode,
      error: data.last_error,
    });
    qc.invalidateQueries({ queryKey: qk.profiles });
    qc.invalidateQueries({ queryKey: qk.modelCards("7d") });
    qc.invalidateQueries({ queryKey: qk.configCards("7d") });
    qc.invalidateQueries({ queryKey: qk.status });
    qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    qc.setQueryData(qk.profileActive, null);
  }, [qc, active, profileStatus.data, profileStatus.dataUpdatedAt]);
}

export interface ProfileProgressState {
  phase: string;
  running: boolean;
  mode?: string;
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
  error?: string;
}

export function useStepUp() {
  return useMutation({
    mutationFn: (req: { factor: "password" | "totp"; password?: string; code?: string }) =>
      api.stepUp(req),
    onSuccess: async () => {
      // The backend rotates the session ID + CSRF token on step-up
      // (fixation prevention). Re-fetch the session to pick up the new
      // CSRF token so subsequent API calls don't fail with CSRF mismatch.
      try {
        const session = await api.session();
        setCsrfToken(session.csrf_token);
      } catch { /* best-effort — the retry may still work via the new cookie */ }
    },
  });
}

// ── FE-6 / Auth v2 (docs/v5-sprint0-auth-design.md §6) ──────────────────────

export function useAuthPolicy() {
  return useQuery({ queryKey: qk.authPolicy, queryFn: api.authPolicy, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateAuthPolicy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: AuthPolicyPutRequest) => api.updateAuthPolicy(req),
    onSuccess: (data) => qc.setQueryData(qk.authPolicy, data),
  });
}

export function useAuthConfig() {
  return useQuery({ queryKey: qk.authConfig, queryFn: api.authConfig, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateAuthConfig() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: Partial<AuthConfigPutRequest>) => api.updateAuthConfig(req),
    onSuccess: (data) => qc.setQueryData(qk.authConfig, data),
  });
}

export function useWebAuthnCredentials() {
  return useQuery({ queryKey: qk.webauthnCredentials, queryFn: api.webauthnCredentials, staleTime: SETTINGS_STALE_MS });
}

export function useWebAuthnRegister() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (label: string) => {
      const begin = await api.webauthnRegisterBegin();
      const cred = await navigator.credentials.create({ publicKey: begin.options as unknown as PublicKeyCredentialCreationOptions }) as PublicKeyCredential;
      const resp = cred.response as AuthenticatorAttestationResponse;
      const finishReq: WebAuthnFinishRegisterRequest = {
        label,
        response: {
          id: cred.id,
          rawId: btoa(cred.id),
          type: "public-key",
          response: {
            clientDataJSON: btoa(String.fromCharCode(...new Uint8Array(resp.clientDataJSON))),
            attestationObject: btoa(String.fromCharCode(...new Uint8Array(resp.attestationObject))),
          },
        },
      };
      return api.webauthnRegisterFinish(finishReq);
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.webauthnCredentials }),
  });
}

export function useWebAuthnAssert() {
  return useMutation({
    mutationFn: async () => {
      const begin = await api.webauthnAssertBegin();
      const cred = await navigator.credentials.get({ publicKey: begin.options as unknown as PublicKeyCredentialRequestOptions }) as PublicKeyCredential;
      const resp = cred.response as AuthenticatorAssertionResponse;
      const finishReq: WebAuthnFinishAssertRequest = {
        response: {
          id: cred.id,
          rawId: btoa(cred.id),
          type: "public-key",
          response: {
            clientDataJSON: btoa(String.fromCharCode(...new Uint8Array(resp.clientDataJSON))),
            authenticatorData: btoa(String.fromCharCode(...new Uint8Array(resp.authenticatorData))),
            signature: btoa(String.fromCharCode(...new Uint8Array(resp.signature))),
            userHandle: resp.userHandle ? btoa(String.fromCharCode(...new Uint8Array(resp.userHandle))) : undefined,
          },
        },
      };
      return api.webauthnAssertFinish(finishReq);
    },
  });
}

export function useWebAuthnCredentialDelete() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => api.webauthnCredentialDelete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.webauthnCredentials }),
  });
}

export function useTOTPEnroll() {
  return useMutation({ mutationFn: () => api.totpEnroll() });
}

export function useTOTPConfirm() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: TotpConfirmRequest) => api.totpConfirm(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.webauthnCredentials }),
  });
}

export function useTOTPDelete() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.totpDelete(),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.webauthnCredentials }),
  });
}

export function useIdentityLinks() {
  return useQuery({ queryKey: qk.identityLinks, queryFn: api.identityLinks, staleTime: SETTINGS_STALE_MS });
}

export function useIdentityLinkCreate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: IdentityLinkCreateRequest) => api.identityLinkCreate(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.identityLinks }),
  });
}

export function useIdentityLinkDelete() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ provider, principal }: { provider: string; principal: string }) =>
      api.identityLinkDelete(provider, principal),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.identityLinks }),
  });
}

export function useKeys(kind?: string) {
  return useQuery({ queryKey: qk.keys(kind), queryFn: () => api.keys(kind), staleTime: SETTINGS_STALE_MS });
}

export function useKeyCreate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (req: APIKeyCreateRequest) => api.keyCreate(req),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["keys"] }),
  });
}

export function useKeyRevoke() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (keyid: string) => api.keyRevoke(keyid),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["keys"] }),
  });
}

export function useRecoveryCodesStatus() {
  return useQuery({ queryKey: qk.recoveryCodes, queryFn: api.recoveryCodesStatus, staleTime: SETTINGS_STALE_MS });
}

export function useRecoveryCodesGenerate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.recoveryCodesGenerate(),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.recoveryCodes }),
  });
}

// ── Model Catalog (MODEL CATALOG sprint Phase 3 — docs/v5-modes-config-editable.md)

export function useCatalogModels() {
  return useQuery({ queryKey: qk.catalogModels, queryFn: api.catalogModels, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogVariants(modelId?: number) {
  return useQuery({
    queryKey: modelId ? qk.catalogVariantsForModel(modelId) : qk.catalogVariants,
    queryFn: () => api.catalogVariants(modelId),
    staleTime: SETTINGS_STALE_MS,
  });
}
export function useCatalogArtifacts(variantId?: number) {
  return useQuery({
    queryKey: variantId ? qk.catalogArtifactsForVariant(variantId) : qk.catalogArtifacts,
    queryFn: () => api.catalogArtifacts(variantId),
    staleTime: SETTINGS_STALE_MS,
  });
}
export function useCatalogConfigs(variantId?: number) {
  return useQuery({
    queryKey: variantId ? qk.catalogConfigsForVariant(variantId) : qk.catalogConfigs,
    queryFn: () => api.catalogConfigs(variantId),
    staleTime: SETTINGS_STALE_MS,
  });
}
export function useCatalogOfferings(modelId?: number) {
  return useQuery({
    queryKey: modelId ? qk.catalogOfferingsForModel(modelId) : qk.catalogOfferings,
    queryFn: () => api.catalogOfferings(modelId),
    staleTime: SETTINGS_STALE_MS,
  });
}
export function useCatalogBenchmarks(subjectType?: string, subjectId?: number) {
  return useQuery({
    queryKey: qk.catalogBenchmarks(subjectType, subjectId),
    queryFn: () => api.catalogBenchmarks(subjectType, subjectId),
    staleTime: SETTINGS_STALE_MS,
  });
}
export function useCatalogNotes(subjectType?: string, subjectId?: number) {
  return useQuery({
    queryKey: qk.catalogNotes(subjectType, subjectId),
    queryFn: () => api.catalogNotes(subjectType, subjectId),
    staleTime: SETTINGS_STALE_MS,
  });
}
export function useCatalogServices() {
  return useQuery({ queryKey: qk.catalogServices, queryFn: api.catalogServices, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogFamilies() {
  return useQuery({ queryKey: qk.catalogFamilies, queryFn: api.catalogFamilies, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogGenealogies() {
  return useQuery({ queryKey: qk.catalogGenealogies, queryFn: api.catalogGenealogies, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogEngines() {
  return useQuery({ queryKey: qk.catalogEngines, queryFn: api.catalogEngines, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogBuilds() {
  return useQuery({ queryKey: qk.catalogBuilds, queryFn: api.catalogBuilds, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogQuantizations() {
  return useQuery({ queryKey: qk.catalogQuantizations, queryFn: api.catalogQuantizations, staleTime: SETTINGS_STALE_MS });
}
export function useCatalogFormats() {
  return useQuery({ queryKey: qk.catalogFormats, queryFn: api.catalogFormats, staleTime: SETTINGS_STALE_MS });
}
export function useModelFiles() {
  return useQuery({ queryKey: qk.modelFiles, queryFn: api.modelFiles });
}

function useCatalogMutations() {
  const qc = useQueryClient();
  const invalidateAll = () => {
    qc.invalidateQueries({ queryKey: ["catalog"] });
    qc.invalidateQueries({ queryKey: qk.modes });
    qc.invalidateQueries({ queryKey: qk.status });
    qc.invalidateQueries({ queryKey: qk.modelCards("7d") });
    qc.invalidateQueries({ queryKey: qk.configCards("7d") });
  };
  return { invalidateAll };
}

// Sprint I — Genealogy/Family were previously either entirely unwrapped
// (genealogy) or list-only (family) despite the backend already supporting
// full CRUD. Mirrors useCreateCatalogModel/Update/Delete/UploadIcon exactly.
export function useCreateCatalogGenealogy() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (g: Partial<CatalogGenealogy>) => api.createCatalogGenealogy(g),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogGenealogy() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, g }: { id: number; g: Partial<CatalogGenealogy> }) => api.updateCatalogGenealogy(id, g),
    onSuccess: invalidateAll,
  });
}
export function useDeleteCatalogGenealogy() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogGenealogy(id),
    onSuccess: invalidateAll,
  });
}
export function useUploadGenealogyIcon() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, file, dark }: { id: number; file: File; dark?: boolean }) => api.uploadGenealogyIcon(id, file, dark),
    onSuccess: invalidateAll,
  });
}

export function useCreateCatalogFamily() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (f: Partial<CatalogFamily>) => api.createCatalogFamily(f),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogFamily() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, f }: { id: number; f: Partial<CatalogFamily> }) => api.updateCatalogFamily(id, f),
    onSuccess: invalidateAll,
  });
}
export function useDeleteCatalogFamily() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogFamily(id),
    onSuccess: invalidateAll,
  });
}
export function useUploadFamilyIcon() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, file, dark }: { id: number; file: File; dark?: boolean }) => api.uploadFamilyIcon(id, file, dark),
    onSuccess: invalidateAll,
  });
}

export function useCreateCatalogModel() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (m: Partial<CatalogModel>) => api.createCatalogModel(m),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogModel() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, m, reason }: { id: number; m: Partial<CatalogModel>; reason?: string }) =>
      api.updateCatalogModel(id, m, reason),
    onSuccess: invalidateAll,
  });
}
export function useDeleteCatalogModel() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogModel(id),
    onSuccess: invalidateAll,
  });
}
export function useUploadModelIcon() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, file, dark }: { id: number; file: File; dark?: boolean }) => api.uploadModelIcon(id, file, dark),
    onSuccess: invalidateAll,
  });
}

export function useCreateCatalogVariant() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (v: Partial<CatalogVariant>) => api.createCatalogVariant(v),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogVariant() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, v }: { id: number; v: Partial<CatalogVariant> }) => api.updateCatalogVariant(id, v),
    onSuccess: invalidateAll,
  });
}
export function useDeleteCatalogVariant() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogVariant(id),
    onSuccess: invalidateAll,
  });
}

export function useCreateCatalogConfig() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (c: Partial<CatalogConfig>) => api.createCatalogConfig(c),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogConfig() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, c, reason }: { id: number; c: Partial<CatalogConfig>; reason?: string }) =>
      api.updateCatalogConfig(id, c, reason),
    onSuccess: invalidateAll,
  });
}
export function useUploadConfigIcon() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, file, dark }: { id: number; file: File; dark?: boolean }) => api.uploadConfigIcon(id, file, dark),
    onSuccess: invalidateAll,
  });
}

// GET /api/v1/audit (Sprint C) — used by the config/model editors' "Change
// history" section. action_prefix scopes to one entity kind
// (catalog_config_/catalog_model_) so target (which collides across kinds)
// is unambiguous — see AuditListResponse's doc comment in lib/types.ts.
export function useAuditLog(actionPrefix: string, target: string, limit = 20) {
  return useQuery({
    queryKey: ["audit", actionPrefix, target, limit],
    queryFn: () => api.auditLog(actionPrefix, target, limit),
    enabled: target !== "",
  });
}
export function useDeleteCatalogConfig() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, force }: { id: number; force?: boolean }) => api.deleteCatalogConfig(id, force),
    onSuccess: invalidateAll,
  });
}

export function useCreateCatalogOffering() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (o: Partial<CatalogOffering>) => api.createCatalogOffering(o),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogOffering() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, o }: { id: number; o: Partial<CatalogOffering> }) => api.updateCatalogOffering(id, o),
    onSuccess: invalidateAll,
  });
}
export function useDeleteCatalogOffering() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogOffering(id),
    onSuccess: invalidateAll,
  });
}

// invalidateBenchmarkConsumers refreshes the admin catalog view plus the two
// card queries that actually render benchmark data (CapabilityBar's
// capabilities and the Performance block) — Sprint D found these mutations
// only ever invalidated ["catalog"], so a benchmark added/edited from a
// model/config detail view (or the admin Benchmarks tab) never refreshed a
// visible card without a manual reload.
function invalidateBenchmarkConsumers(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ["catalog"] });
  qc.invalidateQueries({ queryKey: ["models"] });
  qc.invalidateQueries({ queryKey: ["configs"] });
}
export function useCreateCatalogBenchmark() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (b: Partial<CatalogBenchmark>) => api.createCatalogBenchmark(b),
    onSuccess: () => invalidateBenchmarkConsumers(qc),
  });
}
export function useUpdateCatalogBenchmark() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, b }: { id: number; b: Partial<CatalogBenchmark> }) => api.updateCatalogBenchmark(id, b),
    onSuccess: () => invalidateBenchmarkConsumers(qc),
  });
}
export function useDeleteCatalogBenchmark() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogBenchmark(id),
    onSuccess: () => invalidateBenchmarkConsumers(qc),
  });
}

export function useCreateCatalogNote() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (n: Partial<CatalogNote>) => api.createCatalogNote(n),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["catalog"] }),
  });
}
// useUpdateCatalogNote — Phase 8 (pre-release feedback sprint): the PUT
// handler (catalog_handlers.go's handleCatalogNoteUpdate) already existed
// server-side, but no FE call site ever used it — NotesSection only ever
// created/deleted. Needed now that notes are editable inline on detail
// views (NotesInline.tsx).
export function useUpdateCatalogNote() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, n }: { id: number; n: Partial<CatalogNote> }) => api.updateCatalogNote(id, n),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["catalog"] }),
  });
}
export function useDeleteCatalogNote() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogNote(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["catalog"] }),
  });
}

export function useCreateCatalogService() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (s: Partial<CatalogService>) => api.createCatalogService(s),
    onSuccess: invalidateAll,
  });
}
export function useUpdateCatalogService() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: ({ id, s }: { id: number; s: Partial<CatalogService> }) => api.updateCatalogService(id, s),
    onSuccess: invalidateAll,
  });
}
export function useDeleteCatalogService() {
  const { invalidateAll } = useCatalogMutations();
  return useMutation({
    mutationFn: (id: number) => api.deleteCatalogService(id),
    onSuccess: invalidateAll,
  });
}

// ── Smith Diagnostics (Wave 2 — docs/v5-smith-wave2.md §3 track W2-C) ───────
// No SSE this wave — mutations invalidate their affected queries so the next
// render refetches the updated data.

export function useSmithStatus() {
  return useQuery({ queryKey: qk.smith.status, queryFn: api.smithStatus, refetchInterval: 30_000 });
}

export function useSmithFindings(severity?: string) {
  return useQuery({
    queryKey: qk.smith.findings(undefined, severity),
    queryFn: () => api.smithFindings(undefined, severity),
  });
}

export function useSmithFindingsPurge() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (maxAge: "all" | "72h" | "168h") => api.smithFindingsPurge(maxAge),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["smith", "findings"] }),
  });
}

export function useSmithChecks() {
  return useQuery({ queryKey: qk.smith.checks, queryFn: api.smithChecks, staleTime: SETTINGS_STALE_MS });
}

export function useSmithInvestigations(status?: string) {
  return useQuery({
    queryKey: qk.smith.investigations(status),
    queryFn: () => api.smithInvestigations(status),
  });
}

export function useSmithInvestigation(id: number | null) {
  return useQuery({
    queryKey: id ? qk.smith.investigation(id) : ["smith", "investigation", "none"],
    queryFn: () => api.smithInvestigation(id!),
    enabled: !!id,
  });
}

export function useSmithChecksRun() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ scope, checkIds }: { scope?: string; checkIds?: string[] }) =>
      api.smithChecksRun(scope ?? "quick", checkIds),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["smith", "findings"] }),
  });
}

export function useSmithInvestigationCreate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (summary: string) => api.smithInvestigationCreate(summary),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["smith", "investigations"] }),
  });
}

export function useSmithInvestigationChecks(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ checkIds, scope }: { checkIds?: string[]; scope?: string }) =>
      api.smithInvestigationChecks(id, checkIds, scope),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.smith.investigation(id) });
      qc.invalidateQueries({ queryKey: ["smith", "findings"] });
    },
  });
}

export function useSmithInvestigationResolve(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (status: string) => api.smithInvestigationResolve(id, status),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: qk.smith.investigation(id) });
      qc.invalidateQueries({ queryKey: ["smith", "investigations"] });
    },
  });
}

// ── Smith actions + handoff (Wave 3 / P2 — track W3-C) ──────────────────
// No manual polling: smith:action_update / smith:handoff_update (lib/sse.ts)
// invalidate ["smith","actions"] + qk.smith.action(id) on every state change,
// so these hooks stay plain useQuery/useMutation with react-query's own
// refetch-on-invalidate doing the work.

export function useSmithActions(status?: string, investigationId?: number) {
  return useQuery({
    queryKey: qk.smith.actions(status, investigationId),
    queryFn: () => api.smithActionsList(status, investigationId),
  });
}

// useSmithAction — Sprint S3-Web: the singular action fetch a transcript
// ActionCard resolves live state through (evidence {"action_id": N} → GET
// /actions/{id}). smith:action_update SSE (lib/sse.ts) invalidates this key
// on every status change so the card re-renders without a manual refetch.
export function useSmithAction(id: number | null) {
  return useQuery({
    queryKey: id ? qk.smith.action(id) : ["smith", "action", "none"],
    queryFn: () => api.smithActionGet(id!).then((r) => r.action),
    enabled: !!id,
  });
}

export function useSmithActionCreate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { kind: SmithActionKind; title: string; detail: Record<string, unknown>; risk: SmithActionRisk; investigation_id?: number }) =>
      api.smithActionCreate(body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["smith", "actions"] }),
  });
}

export function useSmithActionApprove(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithActionApprove(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(id) });
    },
  });
}

export function useSmithActionReject(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithActionReject(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(id) });
    },
  });
}

// Sprint S4 (§5.5) — re-run a "done — I ran it myself" runbook's source
// check(s). Invalidates the action, its list, and the investigations list
// (a clean re-check for an investigation-attached runbook resolves that
// investigation).
export function useSmithActionRecheck(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithActionRecheck(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(id) });
      qc.invalidateQueries({ queryKey: ["smith", "investigations"] });
    },
  });
}

export function useSmithActionHandoff(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (resolution: "runbook" | "acknowledge" | "remote" | "cancel") => api.smithActionHandoff(id, resolution),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(id) });
    },
  });
}

// "Let smith fix it" (autonomous-remediation Sprint 3, docs/v5-smith.md
// §13). procedurePreview is the downtime-disclosure modal's read; enabled
// only while the modal is actually open (the caller passes id=null
// otherwise) so opening ActionCard's menu doesn't fire a preview fetch for
// every pending action on screen.
export function useSmithActionProcedurePreview(id: number | null) {
  return useQuery({
    queryKey: id ? qk.smith.procedurePreview(id) : ["smith", "procedurePreview", "none"],
    queryFn: () => api.smithActionProcedurePreview(id!).then((r) => r.preview),
    enabled: !!id,
  });
}

export function useSmithActionProcedurize(id: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithActionProcedurize(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(id) });
    },
  });
}

// Procedure runs (Sprint 2) — the checkpoint UI's live state. No manual
// polling: smith:procedure_step (lib/sse.ts) invalidates this key on every
// step/status change.
export function useSmithProcedureRun(actionId: number | null) {
  return useQuery({
    queryKey: actionId ? qk.smith.procedureRun(actionId) : ["smith", "procedureRun", "none"],
    queryFn: () => api.smithProcedureRun(actionId!).then((r) => r.run),
    enabled: !!actionId,
  });
}

export function useSmithProcedureCheckpointApprove(actionId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithProcedureCheckpointApprove(actionId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(actionId) });
      qc.invalidateQueries({ queryKey: qk.smith.procedureRun(actionId) });
    },
  });
}

export function useSmithProcedureCheckpointAbort(actionId: number) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithProcedureCheckpointAbort(actionId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["smith", "actions"] });
      qc.invalidateQueries({ queryKey: qk.smith.action(actionId) });
      qc.invalidateQueries({ queryKey: qk.smith.procedureRun(actionId) });
    },
  });
}

// Supervision & evaluation harness (autonomous-remediation Sprint 4,
// docs/v5-smith.md §13). Both are plain reads; no manual polling —
// smith:procedure_step (lib/sse.ts) invalidates the run-history list on
// every run's step/status change, same convention as procedureRun above.
// The scorecard is derived from the run + action rows on the server, so
// invalidating procedureRun's own key (already wired into the checkpoint
// mutations and the SSE handler above) is what keeps it fresh — it has no
// separate invalidation of its own beyond a direct refetch.
export function useSmithProcedureRunsList(limit?: number) {
  return useQuery({
    queryKey: qk.smith.procedureRuns,
    queryFn: () => api.smithProcedureRunsList(limit).then((r) => r.runs),
  });
}

export function useSmithProcedureScorecard(actionId: number | null) {
  return useQuery({
    queryKey: actionId ? qk.smith.procedureScorecard(actionId) : ["smith", "procedureScorecard", "none"],
    queryFn: () => api.smithProcedureScorecard(actionId!).then((r) => r.scorecard),
    enabled: !!actionId,
  });
}

// ── Maintenance mode (autonomous-remediation plan, Sprint 1). No manual
// polling: maintenance:changed (lib/sse.ts) invalidates qk.maintenance on
// every state transition (entered/exited/expired), same convention as the
// action-model hooks above.

export function useMaintenance() {
  return useQuery({
    queryKey: qk.maintenance,
    queryFn: () => api.maintenance(),
  });
}

export function useMaintenanceEnter() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: { reason: string; duration_minutes?: number; affected_slots?: string[]; affected_services?: string[] }) =>
      api.maintenanceEnter(body),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.maintenance }),
  });
}

export function useMaintenanceExit() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.maintenanceExit(),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.maintenance }),
  });
}

// ── Smith: reasoning tier (P3 — docs/v5-smith.md §4.3/§5). No manual
// polling for the conversation list/detail: lib/sse.ts's
// smith:message_done listener invalidates qk.smith.conversation(id) (and
// the list, since it's recency-sorted) once a turn finishes, matching the
// action-model hooks' convention above.

export function useSmithConversations() {
  return useQuery({ queryKey: qk.smith.conversations, queryFn: api.smithConversations });
}

export function useSmithConversation(id: number | null) {
  return useQuery({
    queryKey: id ? qk.smith.conversation(id) : ["smith", "conversation", "none"],
    queryFn: () => api.smithConversation(id!),
    enabled: !!id,
  });
}

export function useSmithConversationDelete() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.smithConversationDelete(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.smith.conversations }),
  });
}

export function useSmithChat() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: SmithChatRequest) => api.smithChat(body),
    onSuccess: (resp) => {
      qc.invalidateQueries({ queryKey: qk.smith.conversations });
      qc.invalidateQueries({ queryKey: qk.smith.conversation(resp.conversation_id) });
    },
  });
}

export function useSmithInvestigationAnalyze() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.smithInvestigationAnalyze(id),
    onSuccess: (resp, investigationId) => {
      qc.invalidateQueries({ queryKey: qk.smith.conversation(resp.conversation_id) });
      qc.invalidateQueries({ queryKey: qk.smith.investigation(investigationId) });
    },
  });
}

export function useSmithSettings() {
  return useQuery({ queryKey: qk.smith.settings, queryFn: api.smithSettings, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateSmithSettings() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (patch: Partial<SmithSettings>) => api.updateSmithSettings(patch),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.smith.settings }),
  });
}

// Standing autonomy policy (autonomous-remediation Sprint 5,
// docs/v5-smith.md §13.3) — a separate endpoint from smith/settings above,
// with its own conditional (escalation-only) step-up gate handled at the
// call site via useStepUpGate, same pattern as PolicyMatrix.
export function useSmithAutonomy() {
  return useQuery({ queryKey: qk.smith.autonomy, queryFn: api.smithAutonomy, staleTime: SETTINGS_STALE_MS });
}

export function useUpdateSmithAutonomy() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (policy: SmithAutonomyPolicy) => api.updateSmithAutonomy(policy),
    onSuccess: (data) => qc.setQueryData(qk.smith.autonomy, data),
  });
}

// useSmithWebProbe — the Settings "Re-probe now" button (P5). Invalidates
// status (the source of truth Console/Diagnostics read) rather than
// settings, since reachability isn't a settings field.
export function useSmithWebProbe() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => api.smithWebProbe(),
    onSuccess: () => qc.invalidateQueries({ queryKey: qk.smith.status }),
  });
}

// useSmithSourcingEvaluate — P6 FR4. No cache/invalidation: a one-shot
// research call whose result the caller holds in local state (SourcingCard),
// same as any other "run this and show me the result" action.
export function useSmithSourcingEvaluate() {
  return useMutation({
    mutationFn: ({ hfRepo, budgetBytes }: { hfRepo: string; budgetBytes?: number }) =>
      api.smithSourcingEvaluate(hfRepo, budgetBytes),
  });
}

// ── P4 — knowledge base ─────────────────────────────────────────────────────
// No dedicated search hook: KBSearch results feed Tier 2 chat context
// server-side (reasoning.go's kbBlock) — there's no operator-facing search
// box (operator call, docs/v5-smith.md §4.7's "no search UI" scope note).
// The FE only ever needs one ref at a time (a finding card's kb_refs chip)
// or the whole blocked-work list.

// useSmithKBRef resolves one KBRef to its chunk, lazily — Diagnostics only
// fetches on expand (`enabled: !!ref`), not for every kb_refs chip on the
// page up front. Corpus chunks are embedded in the binary (versioned with
// it, never change without a redeploy), so this can cache indefinitely.
export function useSmithKBRef(ref: string | null) {
  return useQuery({
    queryKey: qk.smith.kbRef(ref ?? ""),
    queryFn: () => api.smithKBRef(ref as string),
    enabled: !!ref,
    staleTime: Infinity,
  });
}

export function useSmithBlockedWork() {
  return useQuery({ queryKey: qk.smith.kbBlocked, queryFn: api.smithKBBlocked, staleTime: SETTINGS_STALE_MS });
}

// useSmithStreamingText reads the SSE-driven in-flight text for one
// assistant message (lib/sse.ts's smith:token listener writes it via
// setQueryData; smith:message_done clears it). Same "client-only cache
// slot, queryFn never really runs" pattern as useProfileProgress above —
// staleTime: Infinity means this never refetches on its own, only updates
// when SSE writes into it. Returns "" (never streamed, or already cleared)
// rather than undefined so callers don't need an extra null-check.
export function useSmithStreamingText(messageId: number | null): string {
  const { data } = useQuery<string>({
    queryKey: messageId ? qk.smith.streaming(messageId) : ["smith", "streaming", "none"],
    queryFn: () => "",
    enabled: !!messageId,
    staleTime: Infinity,
  });
  return data ?? "";
}

// useSmithToolActivity mirrors useSmithStreamingText exactly, for P7's
// smith:tool_call events — a live "smith is checking disk space…" list
// while a tool round is otherwise silent (the round gate withholds its
// content from smith:token until the round commits to prose).
export function useSmithToolActivity(messageId: number | null): SmithToolActivityEvent[] {
  const { data } = useQuery<SmithToolActivityEvent[]>({
    queryKey: messageId ? qk.smith.toolActivity(messageId) : ["smith", "toolActivity", "none"],
    queryFn: () => [],
    enabled: !!messageId,
    staleTime: Infinity,
  });
  return data ?? [];
}

// ── Sprint S3-Web — "Ask smith" affordance + resolution banner ────────────
// The pending-ask channel is a client-only cache slot (never fetched from
// the server), same precedent as qk.smith.streaming / toolActivity. The
// affordance writes into it; AskSmith reads + clears it on mount/route-in
// and sends a context-seeded chat turn (text="" + context). One slot at a
// time — the operator's latest click wins.

// useAskSmithAffordance returns a function an error row can call to route
// the operator to #help/smith and seed the pending ask. The route change is
// what makes the AskSmith component the active tab; the cache write is what
// hands it the context. Both happen together so there's no race window
// where AskSmith mounts, finds nothing to ask, and then the ask lands.
export function useAskSmithAffordance() {
  const qc = useQueryClient();
  return (context: SmithChatContext[]) => {
    if (context.length === 0) return;
    const ask: SmithPendingAsk = { context };
    qc.setQueryData<SmithPendingAsk | null>(qk.smith.pendingAsk, ask);
    // Route to #help/smith — the hash router (App.tsx) makes Help the active
    // tab and AskSmith the active sub-tab. Push (not replace) so Back
    // returns to the row the operator came from.
    location.hash = "#help/smith";
  };
}

// useSmithPendingAsk is read by AskSmith on becoming the active tab. Returns
// the pending ask (or null) and a clear function. Same client-only shape as
// useSmithStreamingText — staleTime: Infinity means this never refetches on
// its own, only updates when useAskSmithAffordance writes into it.
export function useSmithPendingAsk(): {
  pending: SmithPendingAsk | null;
  clear: () => void;
} {
  const qc = useQueryClient();
  const { data } = useQuery<SmithPendingAsk | null>({
    queryKey: qk.smith.pendingAsk,
    queryFn: () => null as SmithPendingAsk | null,
    staleTime: Infinity,
  });
  return {
    pending: data ?? null,
    clear: () => qc.setQueryData(qk.smith.pendingAsk, null),
  };
}
