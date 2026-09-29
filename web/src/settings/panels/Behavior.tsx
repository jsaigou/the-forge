import { useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { ConfirmButton } from "../../components/ConfirmButton";
import { SaveButton } from "../../components/SaveButton";
import type { ConfigWritePayload } from "../../lib/configPayload";
import { useErrorState } from "../../lib/useErrorState";
import {
  useCatalogConfigs,
  useCatalogModelAliases,
  useCatalogCapabilityTiers,
  useCatalogVirtualModels,
  useCreateCatalogModelAlias,
  useCreateCatalogCapabilityTier,
  useCreateCatalogVirtualModel,
  useDeleteCatalogModelAlias,
  useDeleteCatalogCapabilityTier,
  useDeleteCatalogVirtualModel,
  useUpdateCatalogConfig,
  useUpdateCatalogModelAlias,
  useUpdateCatalogCapabilityTier,
  useUpdateCatalogVirtualModel,
} from "../../lib/queries";
import type { CatalogConfig, CatalogModelAlias, CatalogCapabilityTier, CatalogVirtualModel } from "../../lib/types";

// settings/panels/Behavior.tsx (2026-09-14) — answers one question: which
// config actually serves a request, and how does it think? Three features
// decide that, each shipped its own day with its own settings coverage
// bolted on separately: model aliases (T3) got a full Settings section;
// capability-tier substitution (Part 1) got one too; the reasoning_effort/
// chat_template_caps_override pair (Part 2's T1+T2) got NONE —
// chat_template_caps_override had no edit surface anywhere in the app at
// all (a comment in ConfigEditView.tsx claiming otherwise was false), and
// reasoning_effort_default was reachable only by opening one specific
// config's edit form, never documented as a "thinking control" feature. See
// ~/.claude/plans/indexed-prancing-toucan.md for the full investigation.
//
// Originally its own top-level "Model Behavior" Settings section; folded
// into Routing & Compressor the same day at the operator's call ("makes
// more logical sense to combine this with the routing and compressor
// section") — capability-tier substitution's `routing.capability_substitution` toggle
// already lives on that page's Router behavior card, immediately above
// where this renders, and everything here is downstream of the same
// request-serving decision that card makes. Rendered from Routing.tsx as
// ModelBehaviorSection, same pattern as Compression.tsx's CompressorModeCard/
// CompressorProxiesCard — a component that lives in this file but isn't its
// own SectionKey.
//
// CapabilityTiersSection/CapabilityTierForm and ModelAliasesSection/ModelAliasForm
// below are moved verbatim from CatalogPanel.tsx's old Taxonomy sub-tab —
// neither is really a taxonomy concept (that tab is genuinely just
// genealogy/family lineage and icon inheritance). ConfigBehaviorMatrix is
// new: a per-config overview so an operator doesn't have to open every
// config's edit form one at a time to see how the three features currently
// apply to it. Field parity with the two dedicated config editors
// (ConfigEditView.tsx / CatalogPanel.tsx's ConfigForm) is maintained by
// construction — every write here goes through ConfigWritePayload, the same
// full-replace-safe type they use (lib/configPayload.ts).
export function ModelBehaviorSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  return (
    <>
      <div className="eyebrow" id="behavior" style={{ marginTop: 22 }}>{t("behavior.heading")}</div>
      <div className="card" style={{ marginBottom: 12 }}>
        <div style={{ fontSize: 12, color: "var(--text-dim)", lineHeight: 1.55 }}>
          <Trans i18nKey="behavior.intro" ns="settings" components={{ b: <b />, code: <code /> }} />
        </div>
      </div>

      <div className="eyebrow" id="behavior-configs" style={{ marginTop: 22 }}>{t("behavior.configs_heading")}</div>
      <div className="card" style={{ marginBottom: 12 }}>
        <div style={{ fontSize: 11.5, color: "var(--text-mute)", marginBottom: 10 }}>
          <Trans i18nKey="behavior.configs_note" ns="settings" components={{ code: <code /> }} />
        </div>
        <ConfigBehaviorMatrix canAdmin={canAdmin} />
      </div>

      <div className="eyebrow" id="behavior-capability-tiers" style={{ marginTop: 22 }}>{t("behavior.tiers_heading")}</div>
      <div className="card" style={{ marginBottom: 12 }}>
        <div style={{ fontSize: 11.5, color: "var(--text-mute)", marginBottom: 10 }}>
          {t("behavior.tiers_note")}
        </div>
        <CapabilityTiersSection canAdmin={canAdmin} />
      </div>

      <div className="eyebrow" id="behavior-aliases" style={{ marginTop: 22 }}>{t("behavior.aliases_heading")}</div>
      <div className="card">
        <div style={{ fontSize: 11.5, color: "var(--text-mute)", marginBottom: 10 }}>
          {t("behavior.aliases_note")}
        </div>
        <ModelAliasesSection canAdmin={canAdmin} />
      </div>

      <div className="eyebrow" id="behavior-virtual-models" style={{ marginTop: 22 }}>{t("behavior.virtual_models_heading")}</div>
      <div className="card">
        <div style={{ fontSize: 11.5, color: "var(--text-mute)", marginBottom: 10 }}>
          <Trans i18nKey="behavior.virtual_models_note" ns="settings" components={{ b: <b /> }} />
        </div>
        <VirtualModelsSection canAdmin={canAdmin} />
      </div>
    </>
  );
}

// ── Per-config matrix ────────────────────────────────────────────────────────

type ThinkingOverrideState = "auto" | "on" | "off";

function ConfigBehaviorMatrix({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const configs = useCatalogConfigs();
  const capabilityTiers = useCatalogCapabilityTiers();
  const aliases = useCatalogModelAliases();
  const update = useUpdateCatalogConfig();
  const { error, showError, clearError } = useErrorState();

  const configList = configs.data ?? [];
  const capabilityTierList = capabilityTiers.data ?? [];
  const aliasList = aliases.data ?? [];

  // Every write round-trips the full record it read (ConfigWritePayload
  // permits that — see lib/configPayload.ts) with just the touched field(s)
  // replaced, so a matrix edit can never wipe a field this table doesn't
  // show, the exact bug this whole section exists to stop happening again.
  // id/chat_template_caps/chat_template_caps_probed_at are stripped before
  // the body: id travels in the URL (update.mutate's own `id: cfg.id`), and
  // configBody's decode rejects unknown fields (DisallowUnknownFields) — a
  // body carrying any of the three 400s as "unknown field" (found live
  // 2026-09-14 verifying this exact panel, twice — see
  // lib/configPayload.ts's doc comment for why the type alone can't catch
  // this at a spread site; the id half of the fix mirrors Routing.tsx's
  // submitPatch, which documents the same trap for offerings).
  function patch(cfg: CatalogConfig, fields: Partial<ConfigWritePayload>) {
    clearError();
    const { id, chat_template_caps: _caps, chat_template_caps_probed_at: _probedAt, ...rest } = cfg;
    const payload: ConfigWritePayload = { ...rest, ...fields };
    update.mutate({ id, c: payload }, { onError: showError });
  }

  if (configs.isError) {
    return <div className="empty-note">{t("behavior.matrix.catalog_unavailable")}</div>;
  }
  if (configList.length === 0) {
    return <div className="empty-note">{t("behavior.matrix.no_configs")}</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      <div style={{ overflowX: "auto" }}>
        <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 12 }}>
          <thead>
            <tr style={{ textAlign: "left", color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".05em" }}>
              <th style={{ padding: "4px 8px 4px 0" }}>{t("behavior.matrix.col_config")}</th>
              <th style={{ padding: "4px 8px" }}>{t("behavior.matrix.col_tier")}</th>
              <th style={{ padding: "4px 8px" }}>{t("behavior.matrix.col_reasoning")}</th>
              <th style={{ padding: "4px 8px" }}>{t("behavior.matrix.col_thinking")}</th>
              <th style={{ padding: "4px 8px" }}>{t("behavior.matrix.col_aliases")}</th>
            </tr>
          </thead>
          <tbody>
            {configList.map((c) => {
              const configAliases = aliasList.filter((a) => a.config_id === c.id);
              const nativeReasoning = !!c.chat_template_caps.supports_reasoning_effort;
              const thinkingOverride = c.chat_template_caps_override?.supports_enable_thinking;
              const thinkingState: ThinkingOverrideState = thinkingOverride === undefined ? "auto" : thinkingOverride ? "on" : "off";
              return (
                <tr key={c.id} style={{ borderTop: "1px solid var(--border)" }}>
                  <td style={{ padding: "6px 8px 6px 0", fontFamily: "var(--mono)", fontWeight: 600, whiteSpace: "nowrap" }}>{c.name}</td>
                  <td style={{ padding: "6px 8px" }}>
                    {canAdmin ? (
                      <div style={{ display: "flex", gap: 4, alignItems: "center" }}>
                        <select
                          value={c.capability_tier_id}
                          onChange={(e) => patch(c, { capability_tier_id: Number(e.target.value), capability_rank: 0 })}
                        >
                          <option value={0}>{t("behavior.matrix.tier_none")}</option>
                          {capabilityTierList.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
                        </select>
                        {c.capability_tier_id !== 0 && (
                          <input
                            type="number"
                            title={t("behavior.matrix.rank_title")}
                            value={c.capability_rank}
                            style={{ width: 48 }}
                            onChange={(e) => patch(c, { capability_rank: Number(e.target.value) })}
                          />
                        )}
                      </div>
                    ) : (
                      `${capabilityTierList.find((p) => p.id === c.capability_tier_id)?.name ?? "—"}${c.capability_tier_id !== 0 ? t("behavior.matrix.tier_rank_suffix", { rank: c.capability_rank }) : ""}`
                    )}
                  </td>
                  <td style={{ padding: "6px 8px" }}>
                    {canAdmin ? (
                      <select
                        value={c.reasoning_effort_default}
                        onChange={(e) => patch(c, { reasoning_effort_default: e.target.value as CatalogConfig["reasoning_effort_default"] })}
                      >
                        <option value="">{t("behavior.matrix.reasoning_build_default")}</option>
                        <option value="none">none</option>
                        <option value="low">low</option>
                        <option value="medium">medium</option>
                        <option value="high">high</option>
                      </select>
                    ) : (
                      c.reasoning_effort_default || "—"
                    )}
                  </td>
                  <td style={{ padding: "6px 8px" }}>
                    {nativeReasoning ? (
                      <span className="chip" title={t("behavior.matrix.thinking_native_title")}>
                        {t("behavior.matrix.thinking_native")}
                      </span>
                    ) : canAdmin ? (
                      <select
                        value={thinkingState}
                        onChange={(e) => {
                          const state = e.target.value as ThinkingOverrideState;
                          const next = { ...(c.chat_template_caps_override ?? {}) };
                          if (state === "auto") delete next.supports_enable_thinking;
                          else next.supports_enable_thinking = state === "on";
                          patch(c, { chat_template_caps_override: Object.keys(next).length > 0 ? next : undefined });
                        }}
                      >
                        <option value="auto">{t("behavior.matrix.thinking_auto")}</option>
                        <option value="on">{t("behavior.matrix.thinking_force_on")}</option>
                        <option value="off">{t("behavior.matrix.thinking_force_off")}</option>
                      </select>
                    ) : (
                      thinkingOverride === undefined ? "—" : thinkingOverride ? t("behavior.matrix.thinking_forced_on") : t("behavior.matrix.thinking_forced_off")
                    )}
                  </td>
                  <td style={{ padding: "6px 8px", color: "var(--text-mute)" }}>
                    {configAliases.length > 0 ? configAliases.map((a) => a.name).join(", ") : "—"}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </>
  );
}

// ── Capability tiers ──────────────────────────────────────────────────────
// Moved verbatim from CatalogPanel.tsx's Taxonomy sub-tab (Sprint P1,
// 2026-09-13) — mirrors GenealogiesSection's structure minus the icon
// plumbing (a capability tier is an operator-only routing label, never rendered
// in the model gallery). See store.CapabilityTier's doc comment.
function CapabilityTiersSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const capabilityTiers = useCatalogCapabilityTiers();
  const configs = useCatalogConfigs();
  const create = useCreateCatalogCapabilityTier();
  const update = useUpdateCatalogCapabilityTier();
  const remove = useDeleteCatalogCapabilityTier();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = capabilityTiers.data ?? [];
  const configList = configs.data ?? [];
  const editingCapabilityTier = editing === "new" ? null : list.find((p) => p.id === editing);

  function handleSubmit(draft: Partial<CatalogCapabilityTier>, id?: number) {
    clearError();
    if (id) {
      update.mutate({ id, p: draft }, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    }
  }

  function deleteWarning(id: number, name: string) {
    const dependents = configList.filter((c) => c.capability_tier_id === id).length;
    return dependents > 0
      ? t("behavior.tiers.delete_confirm_dependents", { name, count: dependents })
      : t("behavior.tiers.delete_confirm_simple", { name });
  }
  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (capabilityTiers.isError) {
    return <div className="empty-note">{t("behavior.tiers.catalog_unavailable")}</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {editing !== null ? (
        <CapabilityTierForm
          key={editing === "new" ? "new" : editing}
          existing={editingCapabilityTier ?? undefined}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          {list.length === 0 && <div className="empty-note">{t("behavior.tiers.none_yet")}</div>}
          {list.map((p) => (
            <div className="qrow" key={p.id}>
              <span style={{ fontSize: 12.5, fontWeight: 600, flex: 1 }}>{p.name}</span>
              {p.mode && <span className="chip" style={{ fontSize: 10.5 }}>{p.mode}</span>}
              <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
                {t("behavior.tiers.configs_count", { count: configList.filter((c) => c.capability_tier_id === p.id).length })}
              </span>
              {canAdmin && (
                <div className="actions" style={{ marginLeft: 12, display: "flex", gap: 6 }}>
                  <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(p.id); clearError(); }}>{t("behavior.tiers.edit")}</button>
                  <ConfirmButton
                    className="btn"
                    style={{ fontSize: 11, padding: "4px 8px" }}
                    pending={remove.isPending}
                    onConfirm={() => handleDelete(p.id)}
                    warning={deleteWarning(p.id, p.name)}
                  />
                </div>
              )}
            </div>
          ))}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              {t("behavior.tiers.new_tier")}
            </button>
          )}
        </>
      )}
    </>
  );
}

function CapabilityTierForm({
  existing,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogCapabilityTier;
  onSubmit: (draft: Partial<CatalogCapabilityTier>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const { t } = useTranslation("settings");
  const [name, setName] = useState(existing?.name ?? "");
  const [mode, setMode] = useState(existing?.mode ?? "");
  const [notes, setNotes] = useState(existing?.notes ?? "");

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">{t("behavior.tiers.form.name_label")}
        <input value={name} placeholder={t("behavior.tiers.form.name_placeholder")} onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="form-row">{t("behavior.tiers.form.mode_label")}
        <select value={mode} onChange={(e) => setMode(e.target.value)}>
          <option value="">{t("behavior.tiers.form.mode_inherit")}</option>
          <option value="off">{t("behavior.tiers.form.mode_off")}</option>
          <option value="fallback_only">{t("behavior.tiers.form.mode_fallback_only")}</option>
          <option value="prefer_smarter">{t("behavior.tiers.form.mode_prefer_smarter")}</option>
        </select>
      </label>
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("behavior.tiers.form.notes_label")}
        <textarea rows={2} value={notes} onChange={(e) => setNotes(e.target.value)} />
      </label>
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>{t("behavior.tiers.form.cancel")}</button>
        <SaveButton
          className="go"
          pending={pending}
          isError={isError}
          disabled={pending || !name}
          onClick={() => onSubmit({ name, mode, notes }, existing?.id)}
        />
      </div>
    </div>
  );
}

// ── Model aliases ────────────────────────────────────────────────────────────
// Moved verbatim from CatalogPanel.tsx's Taxonomy sub-tab (per-request
// thinking control, Sprint T3, 2026-09-14) — mirrors CapabilityTiersSection's
// structure. Unlike a CapabilityTier (an operator-only routing label), an alias
// IS a real model name a caller sends — the list makes that mapping and
// what it forces visible, since there's nowhere else in the catalog UI a
// "second name for this config" would otherwise show up.
function ModelAliasesSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const aliases = useCatalogModelAliases();
  const configs = useCatalogConfigs();
  const create = useCreateCatalogModelAlias();
  const update = useUpdateCatalogModelAlias();
  const remove = useDeleteCatalogModelAlias();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = aliases.data ?? [];
  const configList = configs.data ?? [];
  const configName = (id: number) => configList.find((c) => c.id === id)?.name ?? t("behavior.aliases.config_fallback", { id });
  const editingAlias = editing === "new" ? null : list.find((a) => a.id === editing);

  function handleSubmit(draft: Partial<CatalogModelAlias>, id?: number) {
    clearError();
    if (id) {
      update.mutate({ id, a: draft }, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    }
  }
  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (aliases.isError) {
    return <div className="empty-note">{t("behavior.aliases.catalog_unavailable")}</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {editing !== null ? (
        <ModelAliasForm
          key={editing === "new" ? "new" : editing}
          existing={editingAlias ?? undefined}
          configs={configList}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          {list.length === 0 && <div className="empty-note">{t("behavior.aliases.none_yet")}</div>}
          {list.map((a) => {
            const forcedEffort = typeof a.request_defaults.reasoning_effort === "string" ? a.request_defaults.reasoning_effort : "";
            return (
              <div className="qrow" key={a.id}>
                <span style={{ fontSize: 12.5, fontWeight: 600 }}>{a.name}</span>
                <span style={{ fontSize: 11, color: "var(--text-mute)" }}>→ {configName(a.config_id)}</span>
                {forcedEffort && <span className="chip" style={{ fontSize: 10.5 }}>{t("behavior.aliases.forces_reasoning", { effort: forcedEffort })}</span>}
                {a.visibility === "hidden" && <span className="chip" style={{ fontSize: 10.5 }}>{t("behavior.aliases.hidden_chip")}</span>}
                <span style={{ flex: 1 }} />
                {canAdmin && (
                  <div className="actions" style={{ marginLeft: 12, display: "flex", gap: 6 }}>
                    <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(a.id); clearError(); }}>{t("behavior.aliases.edit")}</button>
                    <ConfirmButton
                      className="btn"
                      style={{ fontSize: 11, padding: "4px 8px" }}
                      pending={remove.isPending}
                      onConfirm={() => handleDelete(a.id)}
                      warning={t("behavior.aliases.delete_confirm", { name: a.name })}
                    />
                  </div>
                )}
              </div>
            );
          })}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              {t("behavior.aliases.new_alias")}
            </button>
          )}
        </>
      )}
    </>
  );
}

function ModelAliasForm({
  existing,
  configs,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogModelAlias;
  configs: CatalogConfig[];
  onSubmit: (draft: Partial<CatalogModelAlias>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const { t } = useTranslation("settings");
  const [name, setName] = useState(existing?.name ?? "");
  const [configId, setConfigId] = useState(existing?.config_id ?? 0);
  const existingEffort = typeof existing?.request_defaults.reasoning_effort === "string" ? existing.request_defaults.reasoning_effort : "";
  const [reasoningEffort, setReasoningEffort] = useState(existingEffort);
  const [hidden, setHidden] = useState(existing?.visibility === "hidden");

  function submit() {
    // Preserve any other forced field already on this alias (set via the
    // API, no dedicated control here yet) — only reasoning_effort is ever
    // added/removed/changed by this form, same "never silently drop what
    // you didn't expose a control for" rule ConfigEditView follows.
    const requestDefaults: Record<string, unknown> = { ...(existing?.request_defaults ?? {}) };
    if (reasoningEffort) {
      requestDefaults.reasoning_effort = reasoningEffort;
    } else {
      delete requestDefaults.reasoning_effort;
    }
    onSubmit({ name, config_id: configId, request_defaults: requestDefaults, visibility: hidden ? "hidden" : "visible" }, existing?.id);
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">{t("behavior.aliases.form.name_label")}
        <input value={name} placeholder={t("behavior.aliases.form.name_placeholder")} onChange={(e) => setName(e.target.value)} />
        <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
          {t("behavior.aliases.form.name_hint")}
        </span>
      </label>
      <label className="form-row">{t("behavior.aliases.form.routes_to_label")}
        <select value={configId} onChange={(e) => setConfigId(Number(e.target.value))}>
          <option value={0}>{t("behavior.aliases.form.select_placeholder")}</option>
          {configs.map((c) => <option key={c.id} value={c.id}>{c.name}</option>)}
        </select>
      </label>
      <label className="form-row">{t("behavior.aliases.form.force_effort_label")}
        <select value={reasoningEffort} onChange={(e) => setReasoningEffort(e.target.value)}>
          <option value="">{t("behavior.aliases.form.dont_force")}</option>
          <option value="none">{t("behavior.aliases.form.effort_none")}</option>
          <option value="low">{t("behavior.aliases.form.effort_low")}</option>
          <option value="medium">{t("behavior.aliases.form.effort_medium")}</option>
          <option value="high">{t("behavior.aliases.form.effort_high")}</option>
        </select>
        <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
          {t("behavior.aliases.form.force_effort_hint")}
        </span>
      </label>
      <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8, gridColumn: "1 / -1" }}>
        <input type="checkbox" checked={hidden} onChange={(e) => setHidden(e.target.checked)} />
        {t("behavior.aliases.form.hidden_label")}
      </label>
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>{t("behavior.aliases.form.cancel")}</button>
        <SaveButton
          className="go"
          pending={pending}
          isError={isError}
          disabled={pending || !name || !configId}
          onClick={submit}
        />
      </div>
    </div>
  );
}

// ── Virtual models ───────────────────────────────────────────────────────────
// Mirrors ModelAliasesSection/ModelAliasForm's structure (2026-09-15). Unlike
// an alias (a fixed config_id), the target here is chosen dynamically at
// request time by virtual_models.go — this UI only manages which tier (or
// "real throughput data, no tier") a name resolves against.
function VirtualModelsSection({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const virtualModels = useCatalogVirtualModels();
  const capabilityTiers = useCatalogCapabilityTiers();
  const create = useCreateCatalogVirtualModel();
  const update = useUpdateCatalogVirtualModel();
  const remove = useDeleteCatalogVirtualModel();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = virtualModels.data ?? [];
  const tierList = capabilityTiers.data ?? [];
  const tierName = (id: number) => tierList.find((p) => p.id === id)?.name ?? t("behavior.virtual_models.tier_fallback", { id });
  const editingVirtualModel = editing === "new" ? null : list.find((m) => m.id === editing);

  function handleSubmit(draft: Partial<CatalogVirtualModel>, id?: number) {
    clearError();
    if (id) {
      update.mutate({ id, m: draft }, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    }
  }
  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (virtualModels.isError) {
    return <div className="empty-note">{t("behavior.virtual_models.catalog_unavailable")}</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {editing !== null ? (
        <VirtualModelForm
          key={editing === "new" ? "new" : editing}
          existing={editingVirtualModel ?? undefined}
          capabilityTiers={tierList}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          {list.length === 0 && <div className="empty-note">{t("behavior.virtual_models.none_yet")}</div>}
          {list.map((m) => (
            <div className="qrow" key={m.id}>
              <span style={{ fontSize: 12.5, fontWeight: 600 }}>{m.name}</span>
              <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
                {m.kind === "capability_tier" ? t("behavior.virtual_models.resolves_tier", { tier: tierName(m.capability_tier_id) }) : t("behavior.virtual_models.resolves_throughput")}
              </span>
              {m.visibility === "hidden" && <span className="chip" style={{ fontSize: 10.5 }}>{t("behavior.virtual_models.hidden_chip")}</span>}
              <span style={{ flex: 1 }} />
              {canAdmin && (
                <div className="actions" style={{ marginLeft: 12, display: "flex", gap: 6 }}>
                  <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(m.id); clearError(); }}>{t("behavior.virtual_models.edit")}</button>
                  <ConfirmButton
                    className="btn"
                    style={{ fontSize: 11, padding: "4px 8px" }}
                    pending={remove.isPending}
                    onConfirm={() => handleDelete(m.id)}
                    warning={t("behavior.virtual_models.delete_confirm", { name: m.name })}
                  />
                </div>
              )}
            </div>
          ))}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              {t("behavior.virtual_models.new_virtual_model")}
            </button>
          )}
        </>
      )}
    </>
  );
}

function VirtualModelForm({
  existing,
  capabilityTiers,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogVirtualModel;
  capabilityTiers: CatalogCapabilityTier[];
  onSubmit: (draft: Partial<CatalogVirtualModel>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const { t } = useTranslation("settings");
  const [name, setName] = useState(existing?.name ?? "");
  const [kind, setKind] = useState<CatalogVirtualModel["kind"]>(existing?.kind ?? "capability_tier");
  const [capabilityTierId, setCapabilityTierId] = useState(existing?.capability_tier_id ?? 0);
  const [hidden, setHidden] = useState(existing?.visibility === "hidden");
  const [notes, setNotes] = useState(existing?.notes ?? "");

  function submit() {
    onSubmit({
      name,
      kind,
      capability_tier_id: kind === "capability_tier" ? capabilityTierId : 0,
      visibility: hidden ? "hidden" : "visible",
      notes,
    }, existing?.id);
  }

  const canSave = !!name && (kind === "throughput" || capabilityTierId !== 0);

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">{t("behavior.virtual_models.form.name_label")}
        <input value={name} placeholder={t("behavior.virtual_models.form.name_placeholder")} onChange={(e) => setName(e.target.value)} />
        <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
          {t("behavior.virtual_models.form.name_hint")}
        </span>
      </label>
      <label className="form-row">{t("behavior.virtual_models.form.resolves_by_label")}
        <select value={kind} onChange={(e) => setKind(e.target.value as CatalogVirtualModel["kind"])}>
          <option value="capability_tier">{t("behavior.virtual_models.form.kind_tier")}</option>
          <option value="throughput">{t("behavior.virtual_models.form.kind_throughput")}</option>
        </select>
      </label>
      {kind === "capability_tier" && (
        <label className="form-row">{t("behavior.virtual_models.form.tier_label")}
          <select value={capabilityTierId} onChange={(e) => setCapabilityTierId(Number(e.target.value))}>
            <option value={0}>{t("behavior.virtual_models.form.select_placeholder")}</option>
            {capabilityTiers.map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
          </select>
        </label>
      )}
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("behavior.virtual_models.form.notes_label")}
        <textarea rows={2} value={notes} onChange={(e) => setNotes(e.target.value)} />
      </label>
      <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8, gridColumn: "1 / -1" }}>
        <input type="checkbox" checked={hidden} onChange={(e) => setHidden(e.target.checked)} />
        {t("behavior.virtual_models.form.hidden_label")}
      </label>
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>{t("behavior.virtual_models.form.cancel")}</button>
        <SaveButton
          className="go"
          pending={pending}
          isError={isError}
          disabled={pending || !canSave}
          onClick={submit}
        />
      </div>
    </div>
  );
}
