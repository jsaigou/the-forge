import type {
  APIKeyCreateRequest,
  APIKeyCreateResponse,
  APIKeysResponse,
  AuditListResponse,
  AuthConfig,
  AuthConfigPutRequest,
  AuthPolicyPutRequest,
  AuthPolicyResponse,
  BillingSettings,
  CostEnergyHistoryResponse,
  CostSettings,
  CostSettingsUpdate,
  CostSummary,
  DashboardLayout,
  CompressorSummaryResponse,
  CatalogArtifact,
  CatalogBenchmark,
  CatalogBuild,
  CatalogConfig,
  CatalogEngine,
  CatalogFamily,
  CatalogGenealogy,
  CatalogModel,
  CatalogModelFile,
  CatalogNote,
  CatalogOffering,
  CatalogService,
  CatalogVariant,
  ConfigCard,
  FavoritesResponse,
  CompressorConfig,
  HFDownload,
  HFDownloadStartBody,
  HFPreflightFile,
  HFPreflightReport,
  HFSearchResponse,
  HFTokenResponse,
  HFTreeResponse,
  IdentityLink,
  IdentityLinkCreateRequest,
  IdentityLinksResponse,
  InfraService,
  MaintenanceState,
  Metrics,
  MetricsHistoryResponse,
  MetricsSettings,
  MetricsSettingsUpdate,
  ModelCard,
  MonitorSettings,
  MonitorSettingsUpdate,
  NotificationsResponse,
  ModesListResponse,
  PreflightResult,
  ProfileResult,
  ProfileRunRequest,
  ProfilesListResponse,
  Provider,
  ProviderCreateRequest,
  ProviderDiscoverBillingResponse,
  ProviderKeyRequest,
  ProviderUpdateRequest,
  ProvidersResponse,
  RecoveryCodesGenerateResponse,
  RecoveryCodesStatusResponse,
  Reservation,
  RouterConfig,
  RouterConfigUpdate,
  RouterSettings,
  RouterSettingsUpdate,
  RoutingPreviewResponse,
  SchedulerConfig,
  SchedulerJobInput,
  SchedulerJobsResponse,
  SchedulerStatus,
  SessionInfo,
  SmithAction,
  SmithActionKind,
  SmithActionRisk,
  SmithActionsResponse,
  SmithChatRequest,
  SmithChatResponse,
  SmithChecksResponse,
  SmithChecksRunResponse,
  SmithConversationDetail,
  SmithConversationsResponse,
  SmithFindingsResponse,
  SmithInvestigation,
  SmithInvestigationChecksResponse,
  SmithInvestigationDetail,
  SmithInvestigationsResponse,
  SmithKBBlockedResponse,
  SmithKBRefResponse,
  SmithProcedurePreview,
  SmithProcedureRun,
  SmithProcedureRunSummary,
  SmithProcedureScorecard,
  SmithSettings,
  SmithAutonomyPolicy,
  SmithAutonomyResponse,
  SmithSourcingEvaluation,
  SmithStatus,
  SmithWebProviderStatus,
  Status,
  StepUpRequest,
  StepUpResponse,
  SystemRestartResponse,
  SystemSettings,
  SystemSettingsUpdate,
  TotpConfirmRequest,
  TotpConfirmResponse,
  TotpEnrollResponse,
  UISettings,
  UISettingsUpdate,
  UsageEvent,
  UsageHeatmapResponse,
  UsageResponse,
  VoiceListResponse,
  VoiceSettings,
  VoiceSettingsUpdate,
  WebAuthnCredentialsResponse,
  WebAuthnFinishAssertRequest,
  WebAuthnFinishAssertResponse,
  WebAuthnFinishRegisterRequest,
  WebAuthnFinishRegisterResponse,
  WebAuthnBeginAssertResponse,
  WebAuthnBeginRegisterResponse,
} from "./types";

let csrfToken: string | null = null;

export function setCsrfToken(token: string): void {
  csrfToken = token;
}

class ApiError extends Error {
  status: number;
  body: unknown;
  constructor(status: number, body: unknown) {
    super(`API ${status}`);
    this.status = status;
    this.body = body;
  }
}

// Server error bodies are either {error, fields} (pydantic 422s, see
// validators.format_validation_error) or {error} (everything else).
export function apiErrorMessage(e: unknown): string {
  if (e instanceof ApiError && e.body && typeof e.body === "object") {
    const body = e.body as { error?: string; fields?: Record<string, string>; message?: string };
    if (body.fields && Object.keys(body.fields).length > 0) {
      return Object.entries(body.fields).map(([k, v]) => `${k}: ${v}`).join("; ");
    }
    if (body.message) return body.message;
    if (body.error) return body.error;
  }
  return e instanceof Error ? e.message : "Request failed";
}

async function request<T>(path: string, init?: RequestInit, signal?: AbortSignal): Promise<T> {
  const headers: Record<string, string> = { ...(init?.headers as Record<string, string>) };
  const method = (init?.method || "GET").toUpperCase();
  if (method !== "GET" && csrfToken) {
    headers["X-CSRF-Token"] = csrfToken;
  }
  // Never set Content-Type on FormData: the browser must generate the
  // multipart boundary itself. Forcing application/json here clobbers the
  // boundary and the server's ParseMultipartForm rejects every upload
  // (Sprint A1 — the "icon upload fails silently" root cause, 2026-07-31).
  if (init?.body && !(init.body instanceof FormData) && !headers["Content-Type"]) {
    headers["Content-Type"] = "application/json";
  }
  const resp = await fetch(path, { ...init, headers, credentials: "include", signal });
  if (resp.status === 401) {
    window.location.href = "/login?next=" + encodeURIComponent(window.location.pathname);
    throw new ApiError(401, null);
  }
  const text = await resp.text();
  const data = text ? JSON.parse(text) : null;
  if (!resp.ok) {
    throw new ApiError(resp.status, data);
  }
  return data as T;
}

const get = <T,>(path: string, signal?: AbortSignal) => request<T>(path, undefined, signal);
const post = <T,>(path: string, body?: unknown, signal?: AbortSignal) =>
  request<T>(path, { method: "POST", body: body !== undefined ? JSON.stringify(body) : undefined }, signal);
const put = <T,>(path: string, body?: unknown, signal?: AbortSignal) =>
  request<T>(path, { method: "PUT", body: body !== undefined ? JSON.stringify(body) : undefined }, signal);
const del = <T,>(path: string, body?: unknown, signal?: AbortSignal) =>
  request<T>(path, { method: "DELETE", body: body !== undefined ? JSON.stringify(body) : undefined }, signal);

export const api = {
  session: () => get<SessionInfo>("/api/v1/session"),
  status: () => get<Status>("/api/v1/status"),
  metrics: () => get<Metrics>("/api/v1/metrics"),
  schedulerStatus: () => get<SchedulerStatus>("/api/v1/scheduler/status"),
  // product/QA sprint, 2026-07-29 — Dashboard notifications panel.
  notifications: (includeDismissed = false) =>
    get<NotificationsResponse>(`/api/v1/notifications${includeDismissed ? "?include_dismissed=1" : ""}`),
  acknowledgeNotification: (id: number) => post<{ success: boolean }>(`/api/v1/notifications/${id}/ack`),
  dismissNotification: (id: number) => post<{ success: boolean }>(`/api/v1/notifications/${id}/dismiss`),
  acknowledgeAllNotifications: () => post<{ success: boolean }>("/api/v1/notifications/ack-all"),
  infraServices: () => get<{ services: InfraService[] }>("/api/v1/infra-services"),
  usage: (window_: string) => get<UsageResponse>(`/api/v1/usage?window=${encodeURIComponent(window_)}`),
  usageEvents: (window_: string, limit = 200) =>
    get<UsageEvent[]>(`/api/v1/usage/events?window=${encodeURIComponent(window_)}&limit=${limit}`),
  usageHeatmap: (window_: string, tz: string) =>
    get<UsageHeatmapResponse>(`/api/v1/usage/heatmap?window=${encodeURIComponent(window_)}&tz=${encodeURIComponent(tz)}`),
  modelCards: (window_: string) =>
    get<{ cards: ModelCard[]; window: string; display_currency: string }>(`/api/v1/models/cards?window=${encodeURIComponent(window_)}`),
  configCards: (window_: string) =>
    get<{ cards: ConfigCard[]; window: string; display_currency: string }>(`/api/v1/configs/cards?window=${encodeURIComponent(window_)}`),

  switchMode: (mode: string) => post<{ ok?: boolean; success?: boolean; message?: string }>(`/api/v1/switch/${encodeURIComponent(mode)}`),
  loadModel: (mode: string, slot: string) => post<{ success: boolean; message?: string }>("/api/v1/load", { mode, slot }),
  unloadSlot: (slot: string) => post<{ success: boolean; message?: string }>("/api/v1/unload", { slot }),

  reservations: () => get<{ reservations: Reservation[]; total: number }>("/api/v1/reservations"),
  createReservation: (r: Omit<Reservation, "allow_agent_reschedule" | "allow_agent_cancellation"> & Partial<Pick<Reservation, "allow_agent_reschedule" | "allow_agent_cancellation">>) =>
    post<{ ok: boolean }>("/api/v1/reservations", r),
  cancelReservation: (label: string) => del<{ ok: boolean }>(`/api/v1/reservations/${encodeURIComponent(label)}`),

  schedulerConfig: () => get<SchedulerConfig>("/api/v1/scheduler/config"),
  updateSchedulerConfig: (cfg: SchedulerConfig) => put<{ ok: boolean }>("/api/v1/scheduler/config", cfg),

  // P3 scheduler jobs — cron-style forced loads (forge/p3sched track).
  schedulerJobs: () => get<SchedulerJobsResponse>("/api/v1/scheduler/jobs"),
  createSchedulerJob: (j: SchedulerJobInput) =>
    post<{ ok: boolean; id: number }>("/api/v1/scheduler/jobs", j),
  updateSchedulerJob: (id: number, j: SchedulerJobInput) =>
    put<{ ok: boolean }>(`/api/v1/scheduler/jobs/${id}`, j),
  deleteSchedulerJob: (id: number) => del<{ ok: boolean }>(`/api/v1/scheduler/jobs/${id}`),
  runSchedulerJobNow: (id: number) => post<{ ok: boolean; message?: string }>(`/api/v1/scheduler/jobs/${id}/run-now`),

  // Sprint 12 (was H) Phase 6 — infra.scheduler, the boot seed sched.Config
  // falls back to when scheduler.config is unset. Distinct store key and
  // endpoint from schedulerConfig above; see types.ts's RouterConfig-adjacent
  // comments for the apply-mode rationale.
  schedulerSeed: () => get<SchedulerConfig>("/api/v1/scheduler/seed"),
  updateSchedulerSeed: (cfg: SchedulerConfig) => put<SchedulerConfig>("/api/v1/scheduler/seed", cfg),

  compressorConfig: () => get<CompressorConfig>("/api/v1/compressor/config"),
  // Cost/savings sprint Phase 3 — per-proxy cache-hit/latency/time-saved summary.
  compressorSummary: (window_: string) =>
    get<CompressorSummaryResponse>(`/api/v1/compressor/summary?window=${encodeURIComponent(window_)}`),
  compressorRestart: (service: string) => post<{ ok: boolean }>("/api/v1/compressor/restart", { service }),
  compressorTeardown: (service: string) => post<{ ok: boolean }>("/api/v1/compressor/proxy/teardown", { service }),
  compressorCreateProxy: (body: { service: string; label: string; target_url: string }) =>
    post<{ ok: boolean; service: string; port: number; unit: string }>("/api/v1/compressor/proxy/create", body),
  compressorSetPassthrough: (scope: "all" | "proxy", enabled: boolean, service?: string) =>
    put<{ ok: boolean; passthrough_all: boolean; passthrough_services: string[] }>(
      "/api/v1/compressor/passthrough",
      scope === "all" ? { scope, enabled } : { scope, enabled, service },
    ),

  routerSettings: () => get<RouterSettings>("/api/v1/router/settings"),
  // Phase 6: the PUT counterpart — this endpoint existed since C1-Q5 with
  // zero frontend callers until now (see RouterSettingsUpdate's doc comment).
  updateRouterSettings: (patch: RouterSettingsUpdate) => put<RouterSettings>("/api/v1/router/settings", patch),

  // Sprint 12 Phase 6 — the four typed groups Phase 2 built with no frontend
  // caller until the Settings "routing"/"monitoring"/"general" panels.
  routerConfig: () => get<RouterConfig>("/api/v1/router/config"),
  updateRouterConfig: (patch: RouterConfigUpdate) => put<RouterConfig>("/api/v1/router/config", patch),
  monitorSettings: () => get<MonitorSettings>("/api/v1/monitor/settings"),
  updateMonitorSettings: (patch: MonitorSettingsUpdate) => put<MonitorSettings>("/api/v1/monitor/settings", patch),
  metricsSettings: () => get<MetricsSettings>("/api/v1/metrics/settings"),
  updateMetricsSettings: (patch: MetricsSettingsUpdate) => put<MetricsSettings>("/api/v1/metrics/settings", patch),
  uiSettings: () => get<UISettings>("/api/v1/ui/settings"),
  updateUiSettings: (patch: UISettingsUpdate) => put<UISettings>("/api/v1/ui/settings", patch),
  voiceSettings: () => get<VoiceSettings>("/api/v1/voice/settings"),
  updateVoiceSettings: (patch: VoiceSettingsUpdate) => put<VoiceSettings>("/api/v1/voice/settings", patch),
  voiceList: () => get<VoiceListResponse>("/api/v1/voice/list"),
  startInfraService: (name: "stt" | "embedding" | "aligner" | "tts") => post<void>(`/api/v1/${name}/start`, {}),
  stopInfraService: (name: "stt" | "embedding" | "aligner" | "tts") => post<void>(`/api/v1/${name}/stop`, {}),
  dashboardLayout: () => get<DashboardLayout>("/api/v1/dashboard/layout"),
  updateDashboardLayout: (layout: DashboardLayout) => put<DashboardLayout>("/api/v1/dashboard/layout", layout),
  // Admin-only on the backend — callers must gate the query on canAdmin
  // themselves. Read-only in Phase 6 (General's daemon strip); editable via
  // the Danger Zone as of Phase 7.
  systemSettings: () => get<SystemSettings>("/api/v1/system/settings"),
  updateSystemSettings: (patch: SystemSettingsUpdate) => put<SystemSettings>("/api/v1/system/settings", patch),
  // Dry-run checklist — writes nothing, same candidate-building logic the
  // PUT above runs for real. A failing PUT 422s with the identical
  // {error:"preflight_failed", fields, checks} shape; ApiError.body carries
  // it, parsed by the Danger Zone directly rather than through this call.
  systemPreflight: (patch: SystemSettingsUpdate) => post<PreflightResult>("/api/v1/system/preflight", patch),
  // 202 Accepted — the real success signal is the health-poll reconnect
  // afterward (see DangerZone.tsx), not this response.
  systemRestart: () => post<SystemRestartResponse>("/api/v1/system/restart"),
  // Unauthenticated liveness probe, used only for the post-restart poll —
  // deliberately not run through request<T> (no CSRF/401-redirect dance
  // wanted while the daemon is mid-restart and may 5xx/refuse transiently).
  health: async (): Promise<boolean> => {
    try {
      const resp = await fetch("/api/v1/health", { credentials: "include" });
      return resp.ok;
    } catch {
      return false;
    }
  },

  // Sprint 0 §0.3 / §0.9 — providers (taxonomy, credits, status, CRUD).
  // GET masks keys; PUT .../key is write-only and never echoes the secret.
  // Phase 7: mutations key off id, not name — {ref} dual-accepts either
  // (resolveProviderRef, since 0042), but id is the stable handle a rename
  // doesn't disturb, so it's the one the UI should hold onto across an edit.
  providers: () => get<ProvidersResponse>("/api/v1/providers"),
  createProvider: (req: ProviderCreateRequest) => post<Provider>("/api/v1/providers", req),
  updateProvider: (id: number, req: ProviderUpdateRequest) => put<Provider>(`/api/v1/providers/${id}`, req),
  setProviderKey: (id: number, req: ProviderKeyRequest) => put<{ ok: boolean }>(`/api/v1/providers/${id}/key`, req),
  deleteProvider: (id: number) => del<{ ok: boolean }>(`/api/v1/providers/${id}`),
  discoverProviderBilling: (id: number) =>
    post<ProviderDiscoverBillingResponse>(`/api/v1/providers/${id}/discover-billing`),
  // Phase 7 — read-only routing-resolution simulation, sharing the router's
  // exact selection rule (never simulate assumeDown/assumeDisabled as
  // request-body mutations — this endpoint is GET-only and has no
  // side effects, including no scheduler load for local models).
  routingPreview: (model: string, opts?: { assumeDown?: string[]; assumeDisabled?: string[] }, signal?: AbortSignal) => {
    const params = new URLSearchParams({ model });
    if (opts?.assumeDown?.length) params.set("assume_down", opts.assumeDown.join(","));
    if (opts?.assumeDisabled?.length) params.set("assume_disabled", opts.assumeDisabled.join(","));
    return get<RoutingPreviewResponse>(`/api/v1/routing/preview?${params.toString()}`, signal);
  },

  // Sprint 0 §0.9 — billing/currency settings (display currency + FX source).
  billingSettings: () => get<BillingSettings>("/api/v1/billing/settings"),
  updateBillingSettings: (cfg: BillingSettings) => put<BillingSettings>("/api/v1/billing/settings", cfg),

  // product/QA sprint, 2026-07-29 — Console config-card starring.
  favorites: (subjectType = "config") =>
    get<FavoritesResponse>(`/api/v1/favorites?subject_type=${encodeURIComponent(subjectType)}`),
  addFavorite: (subjectType: string, id: number) =>
    put<{ ok: boolean }>(`/api/v1/favorites/${encodeURIComponent(subjectType)}/${id}`),
  removeFavorite: (subjectType: string, id: number) =>
    del<{ ok: boolean }>(`/api/v1/favorites/${encodeURIComponent(subjectType)}/${id}`),

  // Sprint 0 §0.4 — metrics time-series history (server-downsampled) + export.
  metricsHistory: (window_: string, series: string[], res = "auto") =>
    get<MetricsHistoryResponse>(
      `/api/v1/metrics/history?window=${encodeURIComponent(window_)}&series=${encodeURIComponent(series.join(","))}&res=${encodeURIComponent(res)}`,
    ),
  metricsExport: async (format: "csv" | "json", window_ = "90d"): Promise<string> => {
    // Export returns a CSV/JSON text body (not the usual JSON envelope), so it
    // bypasses request<T> which JSON-parses. GET ⇒ no CSRF header needed.
    const resp = await fetch(
      `/api/v1/metrics/export?format=${encodeURIComponent(format)}&window=${encodeURIComponent(window_)}`,
      { credentials: "include" },
    );
    if (resp.status === 401) {
      window.location.href = "/login?next=" + encodeURIComponent(window.location.pathname);
      throw new ApiError(401, null);
    }
    if (!resp.ok) throw new ApiError(resp.status, await resp.text());
    return resp.text();
  },

  // Cost/savings sprint Phase 2 — measured electricity cost + wall-power settings.
  costSummary: (window_: string) => get<CostSummary>(`/api/v1/cost/summary?window=${encodeURIComponent(window_)}`),
  costEnergyHistory: (window_: string, res = "auto") =>
    get<CostEnergyHistoryResponse>(
      `/api/v1/cost/energy-history?window=${encodeURIComponent(window_)}&res=${encodeURIComponent(res)}`,
    ),
  costSettings: () => get<CostSettings>("/api/v1/cost/settings"),
  updateCostSettings: (cfg: CostSettingsUpdate) => put<CostSettings>("/api/v1/cost/settings", cfg),

  serviceModeStart: (name: string) => post<{ ok?: boolean; success?: boolean }>(`/api/v1/service-mode/${encodeURIComponent(name)}/start`),
  serviceModeStop: (name: string) => post<{ ok?: boolean; success?: boolean }>(`/api/v1/service-mode/${encodeURIComponent(name)}/stop`),
  ttsStart: () => post<{ ok?: boolean; success?: boolean }>("/api/v1/tts/start"),
  ttsStop: () => post<{ ok?: boolean; success?: boolean }>("/api/v1/tts/stop"),

  // Sprint 0 §0.9 — read-only modes list (config-defined; mutations are 501
  // by design — C1-Q3 WON'T-DO). FE-4 Settings renders this read-only.
  modes: () => get<ModesListResponse>("/api/v1/modes"),

  // PROFILE track — model profiling + benchmarks (docs/v5-profiling-benchmarks.md).
  // POST /run is admin + step-up (destructive: evicts all A1–A4).
  profileRun: (req: ProfileRunRequest) => post<{ success: boolean; message?: string }>("/api/v1/profile/run", req),
  profile: (mode: string) => get<ProfileResult>(`/api/v1/profile/${encodeURIComponent(mode)}`),
  profiles: () => get<ProfilesListResponse>("/api/v1/profile"),
  stepUp: (req: StepUpRequest) => post<StepUpResponse>("/api/v1/auth/step-up", req),

  // Smith self-diagnosis (Wave 2 — docs/v5-smith.md §4.1). GET /smith/status
  // returns the deterministic SelfContext picture (brain resolution, tier,
  // schedule, check catalog metadata).
  smithStatus: () => get<SmithStatus>("/api/v1/smith/status"),

  // Wave 2 §3 track W2-C — sweep controls, findings, investigations.
  smithChecksRun: (scope: string, checkIds?: string[]) =>
    post<SmithChecksRunResponse>("/api/v1/smith/checks/run", checkIds && checkIds.length > 0 ? { check_ids: checkIds } : { scope }),
  smithFindings: (since?: string, severity?: string) => {
    const params = new URLSearchParams();
    if (since) params.set("since", since);
    if (severity) params.set("severity", severity);
    const qs = params.toString();
    return get<SmithFindingsResponse>(`/api/v1/smith/findings${qs ? `?${qs}` : ""}`);
  },
  smithFindingsPurge: (maxAge: "all" | "72h" | "168h") =>
    post<{ deleted: number }>("/api/v1/smith/findings/purge", { max_age: maxAge }),
  smithChecks: () => get<SmithChecksResponse>("/api/v1/smith/checks"),
  smithInvestigations: (status?: string) =>
    get<SmithInvestigationsResponse>(`/api/v1/smith/investigations${status ? `?status=${encodeURIComponent(status)}` : ""}`),
  smithInvestigationCreate: (summary: string) =>
    post<SmithInvestigation>("/api/v1/smith/investigations", { trigger: "manual", summary }),
  smithInvestigation: (id: number) =>
    get<SmithInvestigationDetail>(`/api/v1/smith/investigations/${id}`),
  smithInvestigationChecks: (id: number, checkIds?: string[], scope?: string) =>
    post<SmithInvestigationChecksResponse>(`/api/v1/smith/investigations/${id}/checks`, checkIds && checkIds.length > 0 ? { check_ids: checkIds } : { scope: scope ?? "quick" }),
  smithInvestigationResolve: (id: number, status: string) =>
    request<SmithInvestigation>(`/api/v1/smith/investigations/${id}`, { method: "PATCH", body: JSON.stringify({ status }) }),

  // Wave 3 / P2 — the action model + self-eviction handoff (track W3-C;
  // backend lands in W3-A/W3-B). POST /smith/actions and POST
  // .../approve are the two step-up-gated calls (action.smith.execute) —
  // wire both through useStepUpGate() like ProfilingPanel's profileRun.
  smithActionsList: (status?: string, investigationId?: number) => {
    const params = new URLSearchParams();
    if (status) params.set("status", status);
    if (investigationId != null) params.set("investigation_id", String(investigationId));
    const qs = params.toString();
    return get<SmithActionsResponse>(`/api/v1/smith/actions${qs ? `?${qs}` : ""}`);
  },
  smithActionCreate: (body: { kind: SmithActionKind; title: string; detail: Record<string, unknown>; risk: SmithActionRisk; investigation_id?: number }) =>
    post<{ action: SmithAction }>("/api/v1/smith/actions", body),
  // Sprint S3-Web — the singular GET /actions/{id} a transcript ActionCard
  // resolves live state through (evidence {"action_id": N} → fetch + SSE).
  smithActionGet: (id: number) => get<{ action: SmithAction }>(`/api/v1/smith/actions/${id}`),
  smithActionApprove: (id: number) => post<{ action: SmithAction }>(`/api/v1/smith/actions/${id}/approve`, {}),
  smithActionReject: (id: number) => post<{ action: SmithAction }>(`/api/v1/smith/actions/${id}/reject`, {}),
  // Sprint S4 (§5.5) — re-run a "done — I ran it myself" runbook's source
  // check(s); a clean re-check for an investigation-attached runbook flows
  // into the resolution loop.
  smithActionRecheck: (id: number) => post<{ action: SmithAction }>(`/api/v1/smith/actions/${id}/recheck`, {}),
  // S7-followup smith UX sprint — "check now" for a PENDING runbook, the
  // replacement for the removed self-attestation "done — I ran it myself"
  // button: smith re-verifies the underlying condition itself. A clean
  // result closes the proposal (action comes back superseded); a
  // still-failing one is NOT an error — the action is left pending and
  // `still_failing` names the check(s) that are still failing.
  smithActionCheckNow: (id: number) =>
    post<{ action: SmithAction; still_failing?: string[] }>(`/api/v1/smith/actions/${id}/check-now`, {}),
  smithActionHandoff: (id: number, resolution: "runbook" | "acknowledge" | "remote" | "cancel") =>
    post<{ action: SmithAction }>(`/api/v1/smith/actions/${id}/handoff`, { resolution }),

  // "Let smith fix it" (autonomous-remediation Sprint 3, docs/v5-smith.md
  // §13) — converts a pending atomic action into its equivalent procedure.
  // procedurize carries the same step-up gate as approve (wire through
  // useStepUpGate() like approve does).
  smithActionProcedurePreview: (id: number) => get<{ preview: SmithProcedurePreview }>(`/api/v1/smith/actions/${id}/procedure_preview`),
  smithActionProcedurize: (id: number) => post<{ action: SmithAction }>(`/api/v1/smith/actions/${id}/procedurize`, {}),
  // Procedure runs (Sprint 2) — the checkpoint UI's data + controls.
  smithProcedureRun: (actionId: number) => get<{ run: SmithProcedureRun }>(`/api/v1/smith/actions/${actionId}/procedure`),
  smithProcedureCheckpointApprove: (actionId: number) =>
    post<{ action: SmithAction }>(`/api/v1/smith/actions/${actionId}/procedure/checkpoint/approve`, {}),
  smithProcedureCheckpointAbort: (actionId: number) =>
    post<{ action: SmithAction }>(`/api/v1/smith/actions/${actionId}/procedure/checkpoint/abort`, {}),

  // Supervision & evaluation harness (autonomous-remediation Sprint 4,
  // docs/v5-smith.md §13) — the run history list + per-run scorecard.
  smithProcedureRunsList: (limit?: number) =>
    get<{ runs: SmithProcedureRunSummary[] }>(`/api/v1/smith/procedures/runs${limit ? `?limit=${limit}` : ""}`),
  smithProcedureScorecard: (actionId: number) =>
    get<{ scorecard: SmithProcedureScorecard }>(`/api/v1/smith/actions/${actionId}/procedure/scorecard`),

  // Maintenance mode (autonomous-remediation plan, Sprint 1). Not under
  // /smith/ — a general operational gate the operator can also drive by
  // hand. POST/DELETE are the same step-up gate as the action model.
  maintenance: () => get<MaintenanceState>("/api/v1/maintenance"),
  maintenanceEnter: (body: { reason: string; duration_minutes?: number; affected_slots?: string[]; affected_services?: string[] }) =>
    post<MaintenanceState>("/api/v1/maintenance", body),
  maintenanceExit: () => del<MaintenanceState>("/api/v1/maintenance"),

  // P3 — the reasoning tier (docs/v5-smith.md §4.3/§5). POST /chat returns
  // 202 immediately; the answer streams over the existing SSE connection as
  // smith:token deltas keyed by message_id, then smith:message_done — api.ts
  // stays JSON-only, no fetch streaming (see lib/sse.ts).
  smithConversations: () => get<SmithConversationsResponse>("/api/v1/smith/conversations"),
  smithConversation: (id: number) => get<SmithConversationDetail>(`/api/v1/smith/conversations/${id}`),
  smithConversationDelete: (id: number) =>
    request<{ deleted: boolean }>(`/api/v1/smith/conversations/${id}`, { method: "DELETE" }),
  smithChat: (body: SmithChatRequest) => post<SmithChatResponse>("/api/v1/smith/chat", body),
  smithSettings: () => get<SmithSettings>("/api/v1/smith/settings"),
  updateSmithSettings: (body: Partial<SmithSettings>) => request<SmithSettings>("/api/v1/smith/settings", { method: "PUT", body: JSON.stringify(body) }),

  // Standing autonomy policy (autonomous-remediation Sprint 5,
  // docs/v5-smith.md §13.3) — a separate endpoint from smith/settings above
  // since PUT carries its own conditional (escalation-only) step-up gate.
  smithAutonomy: () => get<SmithAutonomyResponse>("/api/v1/smith/autonomy"),
  updateSmithAutonomy: (body: SmithAutonomyPolicy) => put<SmithAutonomyResponse>("/api/v1/smith/autonomy", body),
  smithInvestigationAnalyze: (id: number) => post<SmithChatResponse>(`/api/v1/smith/investigations/${id}/analyze`, {}),
  smithWebProbe: () => post<{ providers: SmithWebProviderStatus[] }>("/api/v1/smith/web/probe", {}),

  // P6 FR4 — model sourcing (docs/v5-smith.md §4.9): read-only HF repo
  // evaluation, same no-mutation posture as smithWebProbe above.
  smithSourcingEvaluate: (hfRepo: string, budgetBytes?: number) =>
    post<{ evaluation: SmithSourcingEvaluation }>("/api/v1/smith/sourcing/evaluate", {
      hf_repo: hfRepo,
      budget_bytes: budgetBytes ?? 0,
    }),

  // P4 — knowledge base (docs/v5-smith.md §4.7/§5): one KBRef's chunk (the
  // finding-card expansion) and the parsed docs/investigations.md
  // "externally blocked work" list.
  smithKBRef: (ref: string) => get<SmithKBRefResponse>(`/api/v1/smith/kb/${encodeURIComponent(ref)}`),
  smithKBBlocked: () => get<SmithKBBlockedResponse>("/api/v1/smith/kb/blocked"),

  // FE-6 / Auth v2 (docs/v5-sprint0-auth-design.md §6). All endpoints are
  // implemented in auth_handlers.go; mutations are CSRF-checked + audit-logged.
  // Policy + config
  authPolicy: () => get<AuthPolicyResponse>("/api/v1/auth/policy"),
  updateAuthPolicy: (req: AuthPolicyPutRequest) => put<AuthPolicyResponse>("/api/v1/auth/policy", req),
  authConfig: () => get<AuthConfig>("/api/v1/auth/config"),
  updateAuthConfig: (req: Partial<AuthConfigPutRequest>) => put<AuthConfig>("/api/v1/auth/config", req),

  // TOTP
  totpEnroll: () => post<TotpEnrollResponse>("/api/v1/auth/totp/enroll"),
  totpConfirm: (req: TotpConfirmRequest) => post<TotpConfirmResponse>("/api/v1/auth/totp/confirm", req),
  totpDelete: () => del<{ ok: boolean }>("/api/v1/auth/totp"),

  // WebAuthn
  webauthnRegisterBegin: () => post<WebAuthnBeginRegisterResponse>("/api/v1/auth/webauthn/register/begin"),
  webauthnRegisterFinish: (req: WebAuthnFinishRegisterRequest) =>
    post<WebAuthnFinishRegisterResponse>("/api/v1/auth/webauthn/register/finish", req),
  webauthnAssertBegin: () => post<WebAuthnBeginAssertResponse>("/api/v1/auth/webauthn/assert/begin"),
  webauthnAssertFinish: (req: WebAuthnFinishAssertRequest) =>
    post<WebAuthnFinishAssertResponse>("/api/v1/auth/webauthn/assert/finish", req),
  webauthnCredentials: () => get<WebAuthnCredentialsResponse>("/api/v1/auth/webauthn/credentials"),
  webauthnCredentialDelete: (id: string) => del<{ ok: boolean }>(`/api/v1/auth/webauthn/credentials/${encodeURIComponent(id)}`),

  // Identity linking
  identityLinks: () => get<IdentityLinksResponse>("/api/v1/auth/identity-links"),
  identityLinkCreate: (req: IdentityLinkCreateRequest) =>
    post<IdentityLink>("/api/v1/auth/identity-links", req),
  identityLinkDelete: (provider: string, principal: string) =>
    del<{ ok: boolean }>(`/api/v1/auth/identity-links/${encodeURIComponent(provider)}/${encodeURIComponent(principal)}`),

  // API-key management
  keys: (kind?: string) => get<APIKeysResponse>(`/api/v1/keys${kind ? `?kind=${encodeURIComponent(kind)}` : ""}`),
  keyCreate: (req: APIKeyCreateRequest) => post<APIKeyCreateResponse>("/api/v1/keys", req),
  keyRevoke: (keyid: string) => del<{ ok: boolean }>(`/api/v1/keys/${encodeURIComponent(keyid)}`),

  // Recovery codes (Phase C)
  recoveryCodesStatus: () => get<RecoveryCodesStatusResponse>("/api/v1/auth/recovery-codes"),
  recoveryCodesGenerate: () => post<RecoveryCodesGenerateResponse>("/api/v1/auth/recovery-codes/generate"),

  // ── Model Catalog (MODEL CATALOG sprint Phase 3 — docs/v5-modes-config-editable.md)
  // All under /api/v1/catalog/* except GET /api/v1/models/files.
  // Read endpoints are role-agnostic; mutations require admin + page.settings.

  // Genealogy + Family CRUD (Sprint I adds the mutations/icon uploads —
  // families were previously list-only in the FE client despite the
  // backend already supporting full CRUD; genealogy was entirely unwrapped).
  catalogGenealogies: () => get<CatalogGenealogy[]>("/api/v1/catalog/genealogies"),
  createCatalogGenealogy: (g: Partial<CatalogGenealogy>) => post<CatalogGenealogy>("/api/v1/catalog/genealogies", g),
  updateCatalogGenealogy: (id: number, g: Partial<CatalogGenealogy>) =>
    put<CatalogGenealogy>(`/api/v1/catalog/genealogies/${id}`, g),
  deleteCatalogGenealogy: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/genealogies/${id}`),
  uploadGenealogyIcon: (id: number, file: File, dark = false) => {
    const form = new FormData();
    form.append("file", file);
    const qs = dark ? "?variant=dark" : "";
    return request<{ logo?: string; logo_dark?: string }>(`/api/v1/catalog/genealogies/${id}/icon${qs}`, { method: "PUT", body: form });
  },

  catalogFamilies: () => get<CatalogFamily[]>("/api/v1/catalog/families"),
  createCatalogFamily: (f: Partial<CatalogFamily>) => post<CatalogFamily>("/api/v1/catalog/families", f),
  updateCatalogFamily: (id: number, f: Partial<CatalogFamily>) => put<CatalogFamily>(`/api/v1/catalog/families/${id}`, f),
  deleteCatalogFamily: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/families/${id}`),
  uploadFamilyIcon: (id: number, file: File, dark = false) => {
    const form = new FormData();
    form.append("file", file);
    const qs = dark ? "?variant=dark" : "";
    return request<{ logo?: string; logo_dark?: string }>(`/api/v1/catalog/families/${id}/icon${qs}`, { method: "PUT", body: form });
  },

  catalogQuantizations: () => get<{ id: number; name: string }[]>("/api/v1/catalog/quantizations"),
  catalogFormats: () => get<{ id: number; name: string }[]>("/api/v1/catalog/formats"),
  catalogEngines: () => get<CatalogEngine[]>("/api/v1/catalog/engines"),
  catalogBuilds: () => get<CatalogBuild[]>("/api/v1/catalog/builds"),
  catalogArtifacts: (variantId?: number) =>
    get<CatalogArtifact[]>(`/api/v1/catalog/artifacts${variantId ? `?variant_id=${variantId}` : ""}`),
  modelFiles: () => get<CatalogModelFile[]>("/api/v1/models/files"),

  catalogModels: () => get<CatalogModel[]>("/api/v1/catalog/models"),
  createCatalogModel: (m: Partial<CatalogModel>) => post<CatalogModel>("/api/v1/catalog/models", m),
  updateCatalogModel: (id: number, m: Partial<CatalogModel>, reason?: string) =>
    put<CatalogModel>(`/api/v1/catalog/models/${id}`, { ...m, reason }),
  deleteCatalogModel: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/models/${id}`),
  uploadModelIcon: (id: number, file: File, dark = false) => {
    const form = new FormData();
    form.append("file", file);
    const qs = dark ? "?variant=dark" : "";
    return request<{ logo?: string; logo_dark?: string }>(`/api/v1/catalog/models/${id}/icon${qs}`, {
      method: "PUT",
      body: form,
    });
  },

  catalogVariants: (modelId?: number) =>
    get<CatalogVariant[]>(`/api/v1/catalog/variants${modelId ? `?model_id=${modelId}` : ""}`),
  createCatalogVariant: (v: Partial<CatalogVariant>) => post<CatalogVariant>("/api/v1/catalog/variants", v),
  updateCatalogVariant: (id: number, v: Partial<CatalogVariant>) => put<CatalogVariant>(`/api/v1/catalog/variants/${id}`, v),
  deleteCatalogVariant: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/variants/${id}`),

  catalogConfigs: (variantId?: number) =>
    get<CatalogConfig[]>(`/api/v1/catalog/configs${variantId ? `?variant_id=${variantId}` : ""}`),
  createCatalogConfig: (c: Partial<CatalogConfig>) => post<CatalogConfig>("/api/v1/catalog/configs", c),
  updateCatalogConfig: (id: number, c: Partial<CatalogConfig>, reason?: string) =>
    put<CatalogConfig>(`/api/v1/catalog/configs/${id}`, { ...c, reason }),
  uploadConfigIcon: (id: number, file: File, dark = false) => {
    const form = new FormData();
    form.append("file", file);
    const qs = dark ? "?variant=dark" : "";
    return request<{ logo?: string; logo_dark?: string }>(`/api/v1/catalog/configs/${id}/icon${qs}`, { method: "PUT", body: form });
  },

  auditLog: (actionPrefix: string, target: string, limit = 20) =>
    get<AuditListResponse>(
      `/api/v1/audit?action_prefix=${encodeURIComponent(actionPrefix)}&target=${encodeURIComponent(target)}&limit=${limit}`,
    ),
  deleteCatalogConfig: (id: number, force?: boolean) =>
    del<{ ok: boolean }>(`/api/v1/catalog/configs/${id}${force ? "?force=true" : ""}`),

  catalogOfferings: (modelId?: number) =>
    get<CatalogOffering[]>(`/api/v1/catalog/offerings${modelId ? `?model_id=${modelId}` : ""}`),
  createCatalogOffering: (o: Partial<CatalogOffering>) => post<CatalogOffering>("/api/v1/catalog/offerings", o),
  updateCatalogOffering: (id: number, o: Partial<CatalogOffering>) => put<CatalogOffering>(`/api/v1/catalog/offerings/${id}`, o),
  deleteCatalogOffering: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/offerings/${id}`),

  catalogBenchmarks: (subjectType?: string, subjectId?: number) =>
    get<CatalogBenchmark[]>(
      `/api/v1/catalog/benchmarks${subjectType && subjectId ? `?subject_type=${subjectType}&subject_id=${subjectId}` : ""}`,
    ),
  createCatalogBenchmark: (b: Partial<CatalogBenchmark>) => post<CatalogBenchmark>("/api/v1/catalog/benchmarks", b),
  updateCatalogBenchmark: (id: number, b: Partial<CatalogBenchmark>) => put<CatalogBenchmark>(`/api/v1/catalog/benchmarks/${id}`, b),
  deleteCatalogBenchmark: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/benchmarks/${id}`),

  catalogNotes: (subjectType?: string, subjectId?: number) =>
    get<CatalogNote[]>(
      `/api/v1/catalog/notes${subjectType && subjectId ? `?subject_type=${subjectType}&subject_id=${subjectId}` : ""}`,
    ),
  createCatalogNote: (n: Partial<CatalogNote>) => post<CatalogNote>("/api/v1/catalog/notes", n),
  updateCatalogNote: (id: number, n: Partial<CatalogNote>) => put<CatalogNote>(`/api/v1/catalog/notes/${id}`, n),
  deleteCatalogNote: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/notes/${id}`),

  catalogServices: () => get<CatalogService[]>("/api/v1/catalog/services"),
  createCatalogService: (s: Partial<CatalogService>) => post<CatalogService>("/api/v1/catalog/services", s),
  updateCatalogService: (id: number, s: Partial<CatalogService>) => put<CatalogService>(`/api/v1/catalog/services/${id}`, s),
  deleteCatalogService: (id: number) => del<{ ok: boolean }>(`/api/v1/catalog/services/${id}`),

  // HF model acquisition (go/internal/hfdownload) — search, recursive
  // tree/rank, pre-flight, and the download job queue.
  hfSearch: (q: string, limit?: number) =>
    get<HFSearchResponse>(`/api/v1/hf/search?q=${encodeURIComponent(q)}${limit ? `&limit=${limit}` : ""}`),
  hfTree: (repo: string, revision?: string, budgetBytes?: number) =>
    get<HFTreeResponse>(
      `/api/v1/hf/tree?repo=${encodeURIComponent(repo)}${revision ? `&revision=${encodeURIComponent(revision)}` : ""}${budgetBytes ? `&budget_bytes=${budgetBytes}` : ""}`,
    ),
  hfPreflight: (repo: string, files: HFPreflightFile[], destDir?: string) =>
    post<HFPreflightReport>("/api/v1/hf/preflight", { repo, files, dest_dir: destDir }),
  hfDownloads: () => get<{ downloads: HFDownload[] }>("/api/v1/hf/downloads"),
  hfDownload: (id: number) => get<HFDownload>(`/api/v1/hf/downloads/${id}`),
  hfDownloadStart: (body: HFDownloadStartBody) => post<HFDownload>("/api/v1/hf/downloads", body),
  hfDownloadApprove: (id: number) => post<{ success: boolean }>(`/api/v1/hf/downloads/${id}/approve`),
  hfDownloadPause: (id: number) => post<{ success: boolean; was_running: boolean }>(`/api/v1/hf/downloads/${id}/pause`),
  hfDownloadResume: (id: number) => post<{ success: boolean }>(`/api/v1/hf/downloads/${id}/resume`),
  hfDownloadCancel: (id: number) => post<{ success: boolean }>(`/api/v1/hf/downloads/${id}/cancel`),
  hfDownloadDelete: (id: number) => del<{ success: boolean }>(`/api/v1/hf/downloads/${id}`),
  hfTokenGet: () => get<HFTokenResponse>("/api/v1/hf/token"),
  hfTokenPut: (token: string) => put<HFTokenResponse>("/api/v1/hf/token", { token }),
};

export { ApiError };
