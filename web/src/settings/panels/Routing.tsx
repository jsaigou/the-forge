// settings/panels/Routing.tsx — Sprint 12 (was H) Phase 6. Two cards, two
// distinct settings groups sharing one section by consequence, not
// endpoint: "Router behavior" is router/settings (independent immediate-mode
// KV toggles — PUT /api/v1/router/settings was an orphaned endpoint before
// this, see RouterSettingsUpdate's doc comment in types.ts); "Router config"
// is router/config (infra.router, all restart-mode — router.Deps.Cfg is
// built once in main.go and never re-read). listen_port is deliberately
// absent from both — a confirmed dead field the router never binds to (see
// infra_handlers.go).
//
// Phase 7 (2026-08-13): gained a model→provider routing map + a routing
// preview tool, and absorbed the Compressor section (the merged-page ask —
// routing and Compressor are two halves of one request path). The model map
// edits only enabled/priority — the routing subset that used to be "only
// editable buried in Catalog → Offerings" — and always spreads the full
// fetched offering before PUTting (handleCatalogOfferingUpdate is a
// full-replace endpoint; see feedback_fullreplace_curl_verification_hazard).
// Full-record editing (pricing/wire_model/context/variant) stays in
// Catalog → Offerings, one deep-link away.
//
// Phase 8 (2026-08-14): the routing preview simulation form was replaced
// with a read-only provider→model dendrogram (RoutingTree). ModelRoutingSection
// moved to the top of the panel as the most-accessed card. ConfigCard gained
// a single section-level restart banner (per-field badges suppressed via
// Field's hideApplyBadge prop) and Save / Save-and-Restart / Restart buttons.
//
// Sprint 10 (2026-08-20, docs/v5-headroom-replacement.md): RoutingTree moved
// to components/RoutingTree.tsx (shared with the Dashboard's read-only copy)
// and grew the real compressor state × health link matrix; this panel keeps
// the admin-only long-press enable/disable interaction.
//
// Sprint 2 Stream 2D (docs/ui-remediation-plan-2026-11.md): ConfigCard's
// restart footer + health poll moved out of the card body into the local
// RestartFooter component below (behavior-preserving extraction — the one
// "pending restart changes" summary pattern's prototype for S3; see its
// doc comment for the one wiring fix it carries).
import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Icon } from "../../components/Icon";
import { RestartOverlay } from "../../components/RestartOverlay";
import { RoutingTree } from "../../components/RoutingTree";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { api, apiErrorMessage } from "../../lib/api";
import { providerIconSlug } from "../../lib/providerPresets";
import { countryFlag } from "../../lib/format";
import { groupOfferingsByModel, preferredOfferingIds } from "../../lib/offeringPreference";
import {
  useCatalogModels,
  useCatalogOfferings,
  useProviders,
  useRouterConfig,
  useRouterSettings,
  useStatus,
  useSystemRestart,
  useUpdateCatalogOffering,
  useUpdateRouterConfig,
  useUpdateRouterSettings,
} from "../../lib/queries";
import { useStepUpGate } from "../../lib/useStepUpGate";
import type { CatalogOffering } from "../../lib/types";
import { Field } from "../Field";
import { ROUTING_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";
import { ModelBehaviorSection } from "./Behavior";
import { CompressorModeCard, CompressorProxiesCard } from "./Compression";

const F = Object.fromEntries(ROUTING_FIELDS.map((f) => [f.id, f]));

function Behavior({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useRouterSettings();
  const update = useUpdateRouterSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">{t("routing.behavior_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: t("routing.resource_router") })}</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">{t("routing.behavior_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.loading", { resource: t("routing.resource_router") })}</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">{t("routing.behavior_title")}</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["routing.busy_mode"]} value={g.active.busy_mode} disabled={!canAdmin}
            onChange={(v) => g.setField("busy_mode", String(v))} />
          <Field rec={F["routing.inject_stream_usage"]} value={g.active.inject_stream_usage} disabled={!canAdmin}
            onChange={(v) => g.setField("inject_stream_usage", Boolean(v))} />
          <Field rec={F["routing.provider_failover"]} value={g.active.provider_failover} disabled={!canAdmin}
            onChange={(v) => g.setField("provider_failover", Boolean(v))} />
          <Field rec={F["routing.capability_substitution"]} value={g.active.capability_substitution} disabled={!canAdmin}
            onChange={(v) => g.setField("capability_substitution", String(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset} title={t("routing.discard_title")}>{t("routing.discard")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// RestartFooter — Sprint 2 Stream 2D: the router-config card's restart-aware
// save frame, extracted from ConfigCard where it lived inline (the one
// "pending restart changes" summary pattern's prototype, review §A3 "Restart
// UX varies per section"). Owns everything restart-shaped in the card:
// restartRequired (useStatus), the restart mutation + health poll (the same
// pollToken guard idiom as Danger.tsx's RestartAction), the Save /
// Save-and-Restart / Restart footer, and the restart gate's StepUpModal.
// The card's fields ride as children so the restart error note keeps its
// pre-extraction position above them; per-field apply badges stay
// suppressed on those fields (hideApplyBadge) — this footer is the card's
// one restart surface. One deliberate wiring fix carried by the extraction,
// flagged here so it's not silent: the pre-extraction ConfigCard called
// gate.handle on restart errors but never rendered that gate's modal, so a
// step-up-required 403 (POST /system/restart is
// requireAssurance(action.system.restart)-gated) silently no-oped; this
// follows RestartAction's working idiom instead — modal shows, retry runs.
function RestartFooter({
  canAdmin,
  dirty,
  pending,
  isError,
  save,
  reset,
  children,
}: {
  canAdmin: boolean;
  dirty: boolean;
  pending: boolean;
  isError: boolean;
  save: () => Promise<void>;
  reset: () => void;
  children: ReactNode;
}) {
  const { t } = useTranslation("settings");
  const restart = useSystemRestart();
  const status = useStatus();
  const qc = useQueryClient();
  const gate = useStepUpGate();
  const [restarting, setRestarting] = useState(false);
  const [restartError, setRestartError] = useState<string | null>(null);
  const pollToken = useRef(0);

  const restartRequired = !!status.data?.restart_required;

  async function pollHealth(token: number) {
    await new Promise((r) => setTimeout(r, 900));
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      if (pollToken.current !== token) return;
      if (await api.health()) {
        if (pollToken.current === token) {
          setRestarting(false);
          qc.invalidateQueries();
        }
        return;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
    if (pollToken.current === token) {
      setRestarting(false);
      setRestartError(t("danger.restart_timeout_error"));
    }
  }

  function triggerRestart() {
    setRestartError(null);
    const attempt = () =>
      restart.mutate(undefined, {
        onSuccess: () => {
          const token = ++pollToken.current;
          setRestarting(true);
          void pollHealth(token);
        },
        onError: (e) => {
          if (!gate.handle(e, attempt)) setRestartError(apiErrorMessage(e));
        },
      });
    attempt();
  }

  function dismissRestart() {
    pollToken.current++;
    setRestarting(false);
  }

  async function saveAndRestart() {
    try {
      await save();
      triggerRestart();
    } catch {
      // save already surfaced its error via useSettingsGroup (g.error)
    }
  }

  return (
    <>
      {restartError && <div className="error-note" style={{ marginBottom: 12 }}>{restartError}</div>}
      {children}
      {canAdmin && (
        <div className="form-actions" style={{ marginTop: 12, gap: 8 }}>
          {dirty ? (
            <>
              <button className="btn" onClick={reset} title={t("routing.discard_title")}>{t("routing.discard")}</button>
              <SaveButton pending={pending} isError={isError} onClick={save} />
              <button className="btn primary" disabled={pending || restarting} onClick={saveAndRestart}>
                {pending ? t("routing.saving") : t("routing.save_and_restart")}
              </button>
            </>
          ) : restartRequired ? (
            <button className="btn primary" disabled={restart.isPending || restarting} onClick={triggerRestart}>
              {restart.isPending ? t("danger.requesting") : t("routing.restart_button")}
            </button>
          ) : null}
        </div>
      )}
      {restarting && <RestartOverlay onDone={dismissRestart} />}
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

function ConfigCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useRouterConfig();
  const update = useUpdateRouterConfig();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">{t("routing.config_title")}</div>
        <div className="card"><div className="empty-note">{t("routing.config_role_required")}</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">{t("routing.config_title")}</div>
        <div className="card"><div className="empty-note">{t("routing.config_loading")}</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">{t("routing.config_title")}</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12 }}>
          {t("routing.config_note")}
        </div>
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <RestartFooter canAdmin={canAdmin} dirty={g.dirty} pending={g.pending} isError={g.isError} save={g.save} reset={g.reset}>
          <div className="form-grid">
            <Field rec={F["routing.connect_timeout_s"]} value={g.active.connect_timeout_s} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("connect_timeout_s", Number(v))} />
            <Field rec={F["routing.request_timeout_s"]} value={g.active.request_timeout_s} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("request_timeout_s", Number(v))} />
            <Field rec={F["routing.health_ttl_s"]} value={g.active.health_ttl_s} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("health_ttl_s", Number(v))} />
            <Field rec={F["routing.max_retries_per_backend"]} value={g.active.max_retries_per_backend} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("max_retries_per_backend", Number(v))} />
            <Field rec={F["routing.ensure_loaded_timeout_s"]} value={g.active.ensure_loaded_timeout_s} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("ensure_loaded_timeout_s", Number(v))} />
          </div>
          <div className="form-grid wide">
            <Field rec={F["routing.embedding_url"]} value={g.active.embedding_url} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("embedding_url", String(v))} />
            <Field rec={F["routing.tts_url"]} value={g.active.tts_url} disabled={!canAdmin} hideApplyBadge
              onChange={(v) => g.setField("tts_url", String(v))} />
          </div>
        </RestartFooter>
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// ModelRoutingSection — the model→provider map. Backing fields
// (offerings.priority, offerings.enabled, router_providers.enabled)
// already existed; this is the first UI outside Catalog → Offerings that
// surfaces them, scoped to just the routing-relevant subset.
function ModelRoutingSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const models = useCatalogModels();
  const offerings = useCatalogOfferings();
  const providers = useProviders();
  const update = useUpdateCatalogOffering();
  const [error, setError] = useState<string | null>(null);

  if (offerings.isError) {
    return (
      <>
        <div className="eyebrow" id="routing-models">{t("routing.model_routing_title")}</div>
        <div className="card"><div className="empty-note">{t("routing.catalog_unavailable")}</div></div>
      </>
    );
  }

  const offeringList = offerings.data ?? [];
  const modelList = models.data ?? [];
  const providerByName = new Map((providers.data?.providers ?? []).map((p) => [p.name, p]));
  const groups = groupOfferingsByModel(offeringList);
  const preferredIds = preferredOfferingIds(offeringList, (name) => providerByName.get(name)?.enabled ?? true);

  function submitPatch(o: CatalogOffering, patch: Partial<CatalogOffering>) {
    setError(null);
    // Full-replace endpoint — spread the whole fetched offering, never a
    // partial body (see this file's header comment). id is excluded: it
    // travels in the URL, and offeringBody's decode rejects unknown fields
    // (DisallowUnknownFields), so a body carrying id 400s as "unknown field".
    const { id, ...rest } = o;
    update.mutate({ id, o: { ...rest, ...patch } }, { onError: (e) => setError(apiErrorMessage(e)) });
  }

  return (
    <>
      <div className="eyebrow" id="routing-models">{t("routing.model_routing_title")}</div>
      <div className="card">
        <RoutingTree />
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          {t("routing.routing_description_before")}{" "}
          <a href="#settings/catalog/offerings">{t("routing.catalog_offerings_link")}</a>.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {offeringList.length === 0 && <div className="empty-note">{t("routing.no_offerings")}</div>}
        {[...groups.entries()].map(([modelId, group]) => {
          const model = modelList.find((m) => m.id === modelId);
          return (
            <div key={modelId} style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4, display: "flex", alignItems: "center", gap: 6 }}>
                {model?.logo && <Icon slug={model.logo} name={model.name} sm />}
                {model?.name ?? t("common:routing_tree.model_fallback_name", { id: modelId })}
              </div>
              <div className="hoom">
                {group.map((o) => {
                  const prov = providerByName.get(o.provider);
                  const providerDisabled = prov !== undefined && !prov.enabled;
                  return (
                    <div className="prox" key={o.id} style={{ opacity: providerDisabled || !o.enabled ? 0.55 : undefined }}>
                      <Icon slug={providerIconSlug(o.provider)} name={o.provider} sm />
                      <div>
                        <div className="pn">
                          {o.provider}
                          {prov?.country && <span style={{ marginLeft: 6, fontSize: 13 }}>{countryFlag(prov.country)}</span>}
                          {preferredIds.has(o.id) && (
                            <span className="chip" style={{ marginLeft: 6, color: "var(--ok)" }} title={t("routing.preferred_title")}>
                              {t("routing.preferred_chip")}
                            </span>
                          )}
                          {providerDisabled && <span className="chip" style={{ marginLeft: 4, color: "var(--warn)" }}>{t("routing.provider_off_chip")}</span>}
                        </div>
                        <div className="pu" style={{ fontFamily: "var(--mono)" }}>{o.wire_model}</div>
                      </div>
                      {canAdmin ? (
                        <div className="actions" style={{ marginLeft: "auto", display: "flex", alignItems: "center", gap: 8 }}>
                          <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 11 }}>
                            {t("routing.priority_label")}
                            <input
                              type="number"
                              value={o.priority}
                              style={{ width: 60 }}
                              disabled={update.isPending}
                              onChange={(e) => {
                                const v = Number(e.target.value);
                                if (!Number.isNaN(v)) submitPatch(o, { priority: v });
                              }}
                            />
                          </label>
                          <button
                            disabled={update.isPending}
                            title={o.enabled ? t("routing.disable_route_title") : t("routing.enable_route_title")}
                            onClick={() => submitPatch(o, { enabled: !o.enabled })}
                          >
                            {o.enabled ? t("routing.disable_button") : t("routing.enable_button")}
                          </button>
                        </div>
                      ) : (
                        <div className="pu" style={{ marginLeft: "auto" }}>
                          {t("routing.priority_status", { priority: o.priority, status: o.enabled ? t("routing.status_enabled") : t("routing.status_disabled") })}
                        </div>
                      )}
                    </div>
                  );
                })}
              </div>
            </div>
          );
        })}
      </div>
    </>
  );
}

export function Routing({ canAdmin, canOperate }: { canAdmin: boolean; canOperate: boolean }) {
  return (
    <>
      <ModelRoutingSection canAdmin={canAdmin} />
      <Behavior canAdmin={canAdmin} />
      <ModelBehaviorSection canAdmin={canAdmin} />
      <ConfigCard canAdmin={canAdmin} />
      <CompressorModeCard canOperate={canOperate} />
      <CompressorProxiesCard canOperate={canOperate} canAdmin={canAdmin} />
    </>
  );
}
