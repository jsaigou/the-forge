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
import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { Icon } from "../../components/Icon";
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
import { CompressorModeCard, CompressorProxiesCard } from "./Compression";

const F = Object.fromEntries(ROUTING_FIELDS.map((f) => [f.id, f]));

function Behavior({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useRouterSettings();
  const update = useUpdateRouterSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">Router behavior</div>
        <div className="card"><div className="empty-note">Operator role required to view router settings.</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">Router behavior</div>
        <div className="card"><div className="empty-note">Loading router settings…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">Router behavior</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["routing.busy_mode"]} value={g.active.busy_mode} disabled={!canAdmin}
            onChange={(v) => g.setField("busy_mode", String(v))} />
          <Field rec={F["routing.inject_stream_usage"]} value={g.active.inject_stream_usage} disabled={!canAdmin}
            onChange={(v) => g.setField("inject_stream_usage", Boolean(v))} />
          <Field rec={F["routing.compressor_local_enabled"]} value={g.active.compressor_local_enabled} disabled={!canAdmin}
            onChange={(v) => g.setField("compressor_local_enabled", Boolean(v))} />
          <Field rec={F["routing.provider_failover"]} value={g.active.provider_failover} disabled={!canAdmin}
            onChange={(v) => g.setField("provider_failover", Boolean(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset} title="Discard unsaved changes">Discard</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function RestartOverlay({ onDone }: { onDone: () => void }) {
  return (
    <div className="modal-backdrop">
      <div className="modal">
        <h3>Restarting forge-daemon…</h3>
        <div style={{ fontSize: 12.5, color: "var(--text-dim)", lineHeight: 1.6, marginBottom: 14 }}>
          Waiting for the daemon to come back. This does <b>not</b> unload any loaded model — A1–A4 are separate
          systemd units. It <b>does</b> drop every in-flight a0 request and SSE subscriber.
        </div>
        <div className="dot-busy" style={{ width: 8, height: 8, borderRadius: "50%", background: "var(--warn)", boxShadow: "0 0 7px var(--warn)", margin: "0 auto" }} />
        <div className="form-actions" style={{ marginTop: 16 }}>
          <button className="btn" onClick={onDone}>Give up waiting</button>
        </div>
      </div>
    </div>
  );
}

function ConfigCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useRouterConfig();
  const update = useUpdateRouterConfig();
  const restart = useSystemRestart();
  const status = useStatus();
  const qc = useQueryClient();
  const gate = useStepUpGate();
  const g = useSettingsGroup(cfg.data, update);
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
      setRestartError("Restart did not come back within 60s — check the daemon manually (systemctl status forge-daemon).");
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
      await g.save();
      triggerRestart();
    } catch {
      // g.save already set g.error via useSettingsGroup
    }
  }

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">Router config</div>
        <div className="card"><div className="empty-note">Operator role required to view router config.</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">Router config</div>
        <div className="card"><div className="empty-note">Loading router config…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">Router config</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12 }}>
          Changes don't take effect until the service is restarted.
        </div>
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        {restartError && <div className="error-note" style={{ marginBottom: 12 }}>{restartError}</div>}
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
        {canAdmin && (
          <div className="form-actions" style={{ marginTop: 12, gap: 8 }}>
            {g.dirty ? (
              <>
                <button className="btn" onClick={g.reset} title="Discard unsaved changes">Discard</button>
                <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
                <button className="btn primary" disabled={g.pending || restarting} onClick={saveAndRestart}>
                  {g.pending ? "Saving…" : "Save and Restart"}
                </button>
              </>
            ) : restartRequired ? (
              <button className="btn primary" disabled={restart.isPending || restarting} onClick={triggerRestart}>
                {restart.isPending ? "Requesting…" : "Restart"}
              </button>
            ) : null}
          </div>
        )}
      </div>
      {restarting && <RestartOverlay onDone={dismissRestart} />}
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// ModelRoutingSection — the model→provider map. Backing fields
// (offerings.priority, offerings.enabled, router_providers.enabled)
// already existed; this is the first UI outside Catalog → Offerings that
// surfaces them, scoped to just the routing-relevant subset.
function ModelRoutingSection({ canAdmin }: { canAdmin: boolean }) {
  const models = useCatalogModels();
  const offerings = useCatalogOfferings();
  const providers = useProviders();
  const update = useUpdateCatalogOffering();
  const [error, setError] = useState<string | null>(null);

  if (offerings.isError) {
    return (
      <>
        <div className="eyebrow" id="routing-models">Model → provider routing</div>
        <div className="card"><div className="empty-note">Catalog not available (store may not be wired).</div></div>
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
      <div className="eyebrow" id="routing-models">Model → provider routing</div>
      <div className="card">
        <RoutingTree />
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          Which provider a0 presents for each model, and the priority order a failover walks. Enabled/priority only —
          pricing, wire model, context length, and variant stay in{" "}
          <a href="#settings/catalog/offerings">Catalog → Offerings</a>.
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {offeringList.length === 0 && <div className="empty-note">No offerings configured — nothing routes to a remote provider yet.</div>}
        {[...groups.entries()].map(([modelId, group]) => {
          const model = modelList.find((m) => m.id === modelId);
          return (
            <div key={modelId} style={{ marginBottom: 14 }}>
              <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 4, display: "flex", alignItems: "center", gap: 6 }}>
                {model?.logo && <Icon slug={model.logo} name={model.name} sm />}
                {model?.name ?? `Model #${modelId}`}
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
                            <span className="chip" style={{ marginLeft: 6, color: "var(--ok)" }} title="The offering a0 currently presents for this model">
                              preferred
                            </span>
                          )}
                          {providerDisabled && <span className="chip" style={{ marginLeft: 4, color: "var(--warn)" }}>provider off</span>}
                        </div>
                        <div className="pu" style={{ fontFamily: "var(--mono)" }}>{o.wire_model}</div>
                      </div>
                      {canAdmin ? (
                        <div className="actions" style={{ marginLeft: "auto", display: "flex", alignItems: "center", gap: 8 }}>
                          <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 11 }}>
                            priority
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
                            title={o.enabled ? "Stop routing to this offering" : "Resume routing to this offering"}
                            onClick={() => submitPatch(o, { enabled: !o.enabled })}
                          >
                            {o.enabled ? "Disable" : "Enable"}
                          </button>
                        </div>
                      ) : (
                        <div className="pu" style={{ marginLeft: "auto" }}>priority {o.priority} · {o.enabled ? "enabled" : "disabled"}</div>
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
      <ConfigCard canAdmin={canAdmin} />
      <CompressorModeCard canOperate={canOperate} />
      <CompressorProxiesCard canOperate={canOperate} canAdmin={canAdmin} />
    </>
  );
}
