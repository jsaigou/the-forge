// settings/panels/ProviderKeys.tsx — Sprint 12 (was H) Phase 5 origin:
// moved out of pages/Settings.tsx verbatim at the time (see the sprint plan's
// panel-adaptation table). CRUD via /api/v1/providers; the list shows the
// masked `api_key_masked` form, the API key itself is write-only
// (PUT /api/v1/providers/{name}/key) and never echoed back by the server.
//
// Sprint 2 Stream 2D (docs/ui-remediation-plan-2026-11.md; review §A2 "Two
// draft/save patterns"): the per-provider edit form and the set-key form now
// draft/save through useRowEditor (below) — this file's per-entity shape of
// useSettingsGroup's semantics, which that shared hook can't express (a
// keyed {id, req} mutation and a PATCH draft seeded on open, not
// draft-??-data over a singleton). The peak-windows builder stays JSON
// (3B's work); the row's immediate actions (enable/disable, discover,
// create, delete) keep their direct-mutate + shared-banner shape.
import type { UseMutationResult } from "@tanstack/react-query";
import { useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { ConfirmButton } from "../../components/ConfirmButton";
import { Icon } from "../../components/Icon";
import { SaveButton } from "../../components/SaveButton";
import { newWindowRow, parseWindowRows, serializeWindowRows, toggleDay, type PeakWindowRow } from "../../lib/peakWindows";
import { StepUpModal } from "../../components/StepUpModal";
import { apiErrorMessage } from "../../lib/api";
import { countryFlag } from "../../lib/format";
import { translatedDataResidencyGroup, translatedProviderNote } from "../../lib/providerPresetI18n";
import { PROVIDER_PRESETS, providerIconSlug } from "../../lib/providerPresets";
import { useCreateProvider, useDeleteProvider, useDiscoverProviderBilling, useProviders, useSetProviderKey, useUpdateProvider } from "../../lib/queries";
import type { ProviderCreateRequest, ProviderKeyRequest, ProviderUpdateRequest } from "../../lib/types";
import { useStepUpGate } from "../../lib/useStepUpGate";
import { CURRENCIES } from "../constants";
import { SAVE_FLASH_MS, noteSaveSuccess } from "../useSettingsGroup";

const EMPTY_CREATE: ProviderCreateRequest = {
  name: "",
  bill_currency: "USD",
  target_url: "",
  status_url: "",
  credits_url: "",
  org_id: "",
  billing_console_url: "",
};

// useRowEditor — Sprint 2 Stream 2D. This panel's counterpart of
// useSettingsGroup for per-entity CRUD rows, which that shared hook can't
// express: its contract is draft-??-data over a singleton group with a
// whole-object PUT, while a provider row edits a PATCH seeded when the
// editor opens — merging live row data would PUT untouched fields, and
// peak_windows rides exactly that omission — through a keyed mutation
// ({id, req}). The phases match useSettingsGroup step-for-step: error
// cleared on attempt, step-up gate with automatic retry (PUT /api/v1/
// providers/* is requireAssurance-gated, httpapi.go; the panel renders the
// gate's <StepUpModal>), noteSaveSuccess + the same SAVE_FLASH_MS delayed
// close on success (the editor stays mounted through SaveButton's ✓ flash),
// and errors flow through the panel's one shared banner via the setError
// prop (apiErrorMessage applied here, single slot, latest wins — the
// pre-2D behavior). S3-C/3B can hoist this next to useSettingsGroup if a
// second per-entity surface ever appears.
function useRowEditor<D extends object, V>({
  mutation,
  makeVars,
  setError,
}: {
  mutation: UseMutationResult<unknown, unknown, V>;
  makeVars: (id: number, draft: D) => V;
  setError: (message: string | null) => void;
}) {
  const [rowId, setRowId] = useState<number | null>(null);
  const [draft, setDraft] = useState<D | null>(null);
  const gate = useStepUpGate();

  function open(id: number, seed: D) {
    setRowId(id);
    setDraft(seed);
  }

  // Cancel/dismiss: immediate, and no error touch — the pre-2D cancel
  // buttons never cleared the shared banner either.
  function close() {
    setRowId(null);
    setDraft(null);
  }

  function patch(p: Partial<D>) {
    setDraft((d) => (d === null ? d : { ...d, ...p }));
  }

  function save(id: number) {
    if (rowId !== id || draft === null) return;
    setError(null);
    const attempt = () =>
      mutation.mutate(makeVars(id, draft), {
        onSuccess: () => {
          noteSaveSuccess();
          setTimeout(close, SAVE_FLASH_MS);
        },
        onError: (e) => {
          if (!gate.handle(e, attempt)) setError(apiErrorMessage(e));
        },
      });
    attempt();
  }

  return { rowId, draft, open, close, patch, save, gate, pending: mutation.isPending, isError: mutation.isError };
}

export function ProviderKeys({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const providers = useProviders();
  const create = useCreateProvider();
  const update = useUpdateProvider();
  const setKey = useSetProviderKey();
  const remove = useDeleteProvider();
  const discoverBilling = useDiscoverProviderBilling();

  const [creating, setCreating] = useState(false);
  const [createDraft, setCreateDraft] = useState<ProviderCreateRequest>(EMPTY_CREATE);
  // Operator feedback 2026-08-14: a freshly-created provider gets a Compressor
  // proxy by default. Kept as separate state (not on createDraft) so choosing
  // a preset — which rebuilds createDraft — can't clobber the operator's
  // choice.
  const [createProxy, setCreateProxy] = useState(true);
  // Sprint E preset dropdown: "" = Custom (today's blank-form behavior).
  const [presetId, setPresetId] = useState("");
  // The one shared error banner for every action on this card — single
  // slot, latest error wins; useRowEditor routes its save errors here too.
  const [error, setError] = useState<string | null>(null);

  // Phase 7's id-keying lives on in the row editors: keyed by id, not name,
  // so a rename must not invalidate whichever row is mid-edit. The two
  // editors are mutually exclusive — each open() closes the other.
  const edit = useRowEditor<ProviderUpdateRequest, { id: number; req: ProviderUpdateRequest }>({
    mutation: update,
    makeVars: (id, draft) => ({ id, req: draft }),
    setError,
  });
  const keyEditor = useRowEditor<ProviderKeyRequest, { id: number; req: ProviderKeyRequest }>({
    mutation: setKey,
    makeVars: (id, draft) => ({ id, req: draft }),
    setError,
  });
  const [discoverResult, setDiscoverResult] = useState<{ id: number; name: string; found: boolean; url: string; saved: boolean } | null>(null);
  // Provided-models list (Phase 7): which provider's list is expanded.
  const [expandedModels, setExpandedModels] = useState<number | null>(null);

  function submitDiscover(id: number, name: string) {
    setError(null);
    setDiscoverResult(null);
    discoverBilling.mutate(id, {
      onSuccess: (resp) => setDiscoverResult({ id, name, found: resp.found, url: resp.url, saved: resp.saved }),
      onError: showError,
    });
  }

  function showError(e: unknown) {
    setError(apiErrorMessage(e));
  }

  function submitCreate() {
    setError(null);
    create.mutate({ ...createDraft, create_proxy: createProxy }, {
      onSuccess: () => {
        setCreating(false);
        setCreateDraft(EMPTY_CREATE);
        setPresetId("");
      },
      onError: showError,
    });
  }

  // Sprint E: picking a preset prefills the create draft; every field stays
  // editable afterwards (this is a starting point, not a locked template).
  // "none" credits support ships billing_enabled:false and a blank
  // credits_url so the provider list states "no balance API" honestly
  // instead of polling an endpoint that can only ever render "balance
  // unavailable" — see web/src/lib/providerPresets.ts's header comment.
  function applyPreset(id: string) {
    setPresetId(id);
    if (id === "") {
      setCreateDraft(EMPTY_CREATE);
      return;
    }
    const preset = PROVIDER_PRESETS.find((p) => p.id === id);
    if (!preset) return;
    setCreateDraft({
      name: preset.id,
      bill_currency: preset.billCurrency,
      target_url: preset.targetUrl,
      status_url: preset.statusUrl,
      credits_url: preset.creditsUrl,
      org_id: "",
      billing_enabled: preset.credits !== "none",
      billing_console_url: preset.billingConsoleUrl,
      // Residency rides on the provider row now (0032) — before this it
      // lived only in the preset table, so hand-created providers had none.
      country: preset.country,
      data_residency_group: preset.dataResidencyGroup,
    });
  }

  // Disable without deleting: flips router_providers.enabled — the router
  // stops routing this provider's offerings and /v1/models drops them, but
  // keys/offerings/linked proxies stay intact for re-enabling.
  function toggleEnabled(id: number, enabled: boolean) {
    setError(null);
    update.mutate({ id, req: { enabled } }, { onError: showError });
  }

  // Sprint K: confirm() gate moved into ConfirmButton at the call site.
  function submitDelete(id: number) {
    setError(null);
    remove.mutate(id, { onError: showError });
  }

  const list = providers.data?.providers ?? [];
  const selectedPreset = PROVIDER_PRESETS.find((p) => p.id === presetId);

  return (
    <>
      <div className="eyebrow" id="providers-keys">{t("provider_keys.title")}</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          <Trans i18nKey="provider_keys.intro" ns="settings" components={{ code: <span style={{ fontFamily: "var(--mono)" }} /> }} />
        </div>

        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

        {providers.isError && <div className="empty-note">{t("provider_keys.role_required")}</div>}
        {!providers.isError && !providers.data && <div className="empty-note">{t("provider_keys.loading")}</div>}
        {!providers.isError && providers.data && list.length === 0 && (
          <div className="empty-note">{t("provider_keys.no_providers")}</div>
        )}

        <div className="hoom">
          {list.map((p) => {
            const isEditing = edit.rowId === p.id;
            const isKeying = keyEditor.rowId === p.id;
            const modelsShown = expandedModels === p.id;
            // Rename preview: the provider icon is resolved from the name
            // slug, so a live preview here shows the consequence before
            // saving (renaming off a known vendor slug drops the mark).
            const previewName = isEditing ? (edit.draft?.name ?? p.name) : p.name;
            const healthColor = p.health.state === "reachable" ? "var(--ok)" : p.health.state === "degraded" ? "var(--warn)" : p.health.state === "down" ? "var(--crit)" : "var(--text-mute)";
            return (
              <div className="prox" key={p.id} style={{ flexDirection: "column", alignItems: "stretch", gap: 10, opacity: p.enabled ? undefined : 0.55 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
                  <Icon slug={providerIconSlug(previewName)} name={previewName} sm />
                  <div>
                    <div className="pn">
                      {p.name}
                      {!p.enabled && (
                        <span className="chip" style={{ marginLeft: 6, color: "var(--warn)", borderColor: "color-mix(in srgb, var(--warn) 40%, var(--border))" }}>
                          {t("provider_keys.disabled_chip")}
                        </span>
                      )}
                      <span className="chip" style={{ marginLeft: 6, color: healthColor, borderColor: `color-mix(in srgb, ${healthColor} 40%, var(--border))` }}>
                        {p.health.state}
                      </span>
                      {p.country && (
                        <span className="chip" style={{ marginLeft: 4, color: "var(--text-dim)" }}>
                          {countryFlag(p.country)} {p.country}{p.data_residency_group ? ` · ${translatedDataResidencyGroup(t, p.data_residency_group)}` : ""}
                        </span>
                      )}
                      {p.models.length > 0 && (
                        <button
                          className="chip"
                          style={{ marginLeft: 4, cursor: "pointer" }}
                          onClick={() => setExpandedModels(modelsShown ? null : p.id)}
                          title={t("provider_keys.show_models_title")}
                        >
                          {t("provider_keys.models_count", { count: p.models.length })} {modelsShown ? "▲" : "▼"}
                        </button>
                      )}
                    </div>
                    <div className="pu">
                      {p.api_key_masked || t("provider_keys.no_key_set")} · {p.bill_currency}
                      {p.credits.supported && p.credits.balance_native != null
                        ? t("provider_keys.balance_value", { balance: p.credits.balance_native, currency: p.credits.currency ? " " + p.credits.currency : "" })
                        : p.credits.supported
                          ? t("provider_keys.balance_unavailable")
                          : t("provider_keys.no_balance_api")}
                    </div>
                  </div>
                  {canAdmin && (
                    <div className="actions" style={{ marginLeft: "auto" }}>
                      <button
                        disabled={update.isPending}
                        title={p.enabled
                          ? t("provider_keys.disable_provider_title")
                          : t("provider_keys.enable_provider_title")}
                        onClick={() => toggleEnabled(p.id, !p.enabled)}
                      >
                        {p.enabled ? t("routing.disable_button") : t("routing.enable_button")}
                      </button>
                      <button
                        disabled={isEditing || update.isPending}
                        onClick={() => {
                          edit.open(p.id, {
                            name: p.name,
                            bill_currency: p.bill_currency,
                            billing_enabled: p.billing_enabled,
                            billing_console_url: p.billing_console_url ?? "",
                            target_url: p.target_url ?? "",
                            status_url: p.status_url ?? "",
                            credits_url: p.credits_url ?? "",
                            org_id: p.org_id ?? "",
                          });
                          keyEditor.close();
                        }}
                      >
                        {t("provider_keys.edit_button")}
                      </button>
                      <button disabled={isKeying || setKey.isPending} onClick={() => { keyEditor.open(p.id, { api_key: "" }); edit.close(); }}>{t("provider_keys.set_key_button")}</button>
                      <button
                        disabled={discoverBilling.isPending}
                        title={t("provider_keys.discover_billing_title")}
                        onClick={() => submitDiscover(p.id, p.name)}
                      >
                        {discoverBilling.isPending && discoverBilling.variables === p.id ? t("provider_keys.discovering") : t("provider_keys.discover_billing_button")}
                      </button>
                      <ConfirmButton
                        className=""
                        pending={remove.isPending}
                        onConfirm={() => submitDelete(p.id)}
                        warning={t("provider_keys.delete_warning", { name: p.name })}
                      />
                    </div>
                  )}
                </div>
                {discoverResult && discoverResult.id === p.id && (
                  <div className="d" style={{ marginTop: 2 }}>
                    {discoverResult.found
                      ? discoverResult.saved
                        ? t("provider_keys.discover_found_saved", { url: discoverResult.url })
                        : t("provider_keys.discover_found_not_saved", { url: discoverResult.url })
                      : t("provider_keys.discover_not_found")}
                  </div>
                )}

                {modelsShown && (
                  <div className="hoom" style={{ marginTop: 4, paddingLeft: 12, borderLeft: "2px solid var(--border)" }}>
                    {p.models.length === 0 && <div className="empty-note">{t("provider_keys.no_offerings_for_provider")}</div>}
                    {p.models.map((m) => (
                      <div key={m.catalog_model_id + ":" + m.model_id} className="prox" style={{ opacity: m.enabled ? undefined : 0.55 }}>
                        {m.logo && <Icon slug={m.logo} name={m.display_name} sm />}
                        <div>
                          <div className="pn">
                            {m.display_name || m.model_id}
                            {!m.enabled && <span className="chip" style={{ marginLeft: 6, color: "var(--text-mute)" }}>{t("provider_keys.model_disabled_chip")}</span>}
                          </div>
                          <div className="pu" style={{ fontFamily: "var(--mono)" }}>
                            {m.model_id} · {m.price_in_per_1m}/{m.price_out_per_1m} {m.currency} · {t("routing.priority_label")} {m.priority}
                            {m.compressor_proxy ? ` · via ${m.compressor_proxy}` : ""}
                          </div>
                        </div>
                      </div>
                    ))}
                    <a href="#settings/routing" style={{ fontSize: 11, marginTop: 4 }}>
                      {t("provider_keys.edit_routing_link")}
                    </a>
                  </div>
                )}

                {isEditing && (
                  <div className="form-grid" style={{ marginTop: 4 }}>
                    <label className="form-row">{t("provider_keys.name_label")}
                      <input value={edit.draft?.name ?? ""} onChange={(e) => edit.patch({ name: e.target.value })} />
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
                        {t("provider_keys.name_hint")}
                      </span>
                      {edit.draft?.name && /[^\p{L}\p{N} &_.-]/u.test(edit.draft.name) && (
                        <span style={{ fontSize: 11, color: "var(--warn)" }}>
                          {t("provider_keys.name_invalid_chars")}
                        </span>
                      )}
                    </label>
                    <label className="form-row">{t("provider_keys.bill_currency_label")}
                      <select value={edit.draft?.bill_currency ?? ""} onChange={(e) => edit.patch({ bill_currency: e.target.value })}>
                        {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
                      </select>
                    </label>
                    <label className="form-row">{t("compression.target_url_label")}
                      <input value={edit.draft?.target_url ?? ""} placeholder="https://api.example.com/v1" onChange={(e) => edit.patch({ target_url: e.target.value })} />
                    </label>
                    <label className="form-row">{t("provider_keys.status_url_label")}
                      <input value={edit.draft?.status_url ?? ""} placeholder="https://deepseek.statuspage.io/api/v2/summary.json" onChange={(e) => edit.patch({ status_url: e.target.value })} />
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>{t("provider_keys.status_url_hint_edit")}</span>
                    </label>
                    <label className="form-row">{t("provider_keys.credits_url_label")}
                      <input value={edit.draft?.credits_url ?? ""} placeholder="https://api.example.com/user/balance" onChange={(e) => edit.patch({ credits_url: e.target.value })} />
                    </label>
                    <label className="form-row">{t("provider_keys.org_id_label")}
                      <input value={edit.draft?.org_id ?? ""} placeholder="required by some providers' credits/analytics APIs (e.g. AI&)" onChange={(e) => edit.patch({ org_id: e.target.value })} />
                    </label>
                    <label className="form-row">{t("provider_keys.billing_console_url_label")}
                      <input value={edit.draft?.billing_console_url ?? ""} placeholder="https://platform.example.com/usage" onChange={(e) => edit.patch({ billing_console_url: e.target.value })} />
                    </label>
                    <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
                      <input
                        type="checkbox"
                        checked={edit.draft?.billing_enabled ?? true}
                        onChange={(e) => edit.patch({ billing_enabled: e.target.checked })}
                      />
                      {t("provider_keys.billing_api_enabled")}
                    </label>
                    <PeakWindowsEditor
                      value={edit.draft?.peak_windows ?? p.peak_windows ?? ""}
                      peakActiveNow={p.peak_active_now}
                      onChange={(v) => edit.patch({ peak_windows: v })}
                    />
                    <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                      <button className="btn" onClick={edit.close}>{t("provider_keys.cancel")}</button>
                      <SaveButton pending={edit.pending} isError={edit.isError} onClick={() => edit.save(p.id)} />
                    </div>
                  </div>
                )}

                {isKeying && (
                  <div style={{ display: "flex", gap: 8, alignItems: "flex-end", flexWrap: "wrap", marginTop: 4 }}>
                    <label className="form-row" style={{ flex: "1 1 320px" }}>{t("provider_keys.new_api_key_label")}
                      <input
                        type="password"
                        value={keyEditor.draft?.api_key ?? ""}
                        placeholder="sk-…"
                        onChange={(e) => keyEditor.patch({ api_key: e.target.value })}
                        autoComplete="off"
                        spellCheck={false}
                      />
                    </label>
                    <div className="form-actions">
                      <button className="btn" onClick={keyEditor.close}>{t("provider_keys.cancel")}</button>
                      <SaveButton
                        pending={keyEditor.pending}
                        isError={keyEditor.isError}
                        disabled={keyEditor.pending || !keyEditor.draft?.api_key}
                        onClick={() => keyEditor.save(p.id)}
                        label={t("provider_keys.save_key")}
                      />
                    </div>
                  </div>
                )}
              </div>
            );
          })}
        </div>

        {canAdmin && (
          creating ? (
            <div className="form-grid" style={{ marginTop: 14 }}>
              <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("provider_keys.preset_label")}
                <select value={presetId} onChange={(e) => applyPreset(e.target.value)}>
                  <option value="">{t("provider_keys.preset_custom")}</option>
                  {PROVIDER_PRESETS.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
                </select>
              </label>
              {selectedPreset?.note && (
                <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)", marginTop: -6 }}>{translatedProviderNote(t, selectedPreset)}</div>
              )}
              {selectedPreset && selectedPreset.credits === "none" && (
                <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)", marginTop: -6 }}>
                  {t("provider_keys.no_balance_api_note")}
                </div>
              )}
              <label className="form-row">{t("provider_keys.name_required_label")}
                <input value={createDraft.name} placeholder="deepseek" onChange={(e) => setCreateDraft({ ...createDraft, name: e.target.value })} />
                {createDraft.name && /[^\p{L}\p{N} &_.-]/u.test(createDraft.name) && (
                  <span style={{ fontSize: 11, color: "var(--warn)" }}>
                    {t("provider_keys.name_invalid_chars")}
                  </span>
                )}
              </label>
              <label className="form-row">{t("provider_keys.bill_currency_label")}
                <select value={createDraft.bill_currency} onChange={(e) => setCreateDraft({ ...createDraft, bill_currency: e.target.value })}>
                  {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
                </select>
              </label>
              <label className="form-row">{t("compression.target_url_label")}
                <input value={createDraft.target_url} placeholder="https://api.deepseek.com/v1" onChange={(e) => setCreateDraft({ ...createDraft, target_url: e.target.value })} />
                {selectedPreset?.targetUrlIsTemplate && (
                  <span style={{ fontSize: 11, color: "var(--text-dim)" }}>{t("provider_keys.target_url_template_hint", { preset: selectedPreset.label })}</span>
                )}
              </label>
              <label className="form-row">{t("provider_keys.status_url_label")}
                <input value={createDraft.status_url} placeholder="https://deepseek.statuspage.io/api/v2/summary.json" onChange={(e) => setCreateDraft({ ...createDraft, status_url: e.target.value })} />
                <span style={{ fontSize: 11, color: "var(--text-dim)" }}>{t("provider_keys.status_url_hint_create")}</span>
              </label>
              <label className="form-row">{t("provider_keys.credits_url_label")}
                <input value={createDraft.credits_url} placeholder="https://api.deepseek.com/user/balance" onChange={(e) => setCreateDraft({ ...createDraft, credits_url: e.target.value })} />
              </label>
              <label className="form-row">{t("provider_keys.org_id_label")}{selectedPreset?.orgIdRequired ? t("provider_keys.org_id_required_suffix") : ""}
                <input value={createDraft.org_id} placeholder="required by some providers' credits/analytics APIs (e.g. AI&)" onChange={(e) => setCreateDraft({ ...createDraft, org_id: e.target.value })} />
              </label>
              <label className="form-row">{t("provider_keys.billing_console_url_label")}
                <input value={createDraft.billing_console_url ?? ""} placeholder="https://platform.example.com/usage" onChange={(e) => setCreateDraft({ ...createDraft, billing_console_url: e.target.value })} />
              </label>
              <label className="form-row" style={{ gridColumn: "1 / -1", flexDirection: "row", alignItems: "center", gap: 8 }}>
                <input type="checkbox" checked={createProxy} onChange={(e) => setCreateProxy(e.target.checked)} style={{ width: "auto" }} />
                <span>{t("provider_keys.create_proxy_checkbox")}</span>
              </label>
              <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn" onClick={() => { setCreating(false); setCreateDraft(EMPTY_CREATE); setPresetId(""); }}>{t("provider_keys.cancel")}</button>
                <button className="btn primary" disabled={create.isPending || !createDraft.name} onClick={submitCreate}>{t("provider_keys.create_button")}</button>
              </div>
            </div>
          ) : (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setCreating(true); setCreateDraft(EMPTY_CREATE); setPresetId(""); setError(null); }}>
              {t("provider_keys.add_provider_button")}
            </button>
          )
        )}
      </div>
      {/* 2D: one modal per row-editor gate (the editors are mutually
          exclusive, so at most one is ever open — a closed StepUpModal
          renders null). */}
      <StepUpModal open={edit.gate.open} requiredFactor={edit.gate.factor} onSuccess={edit.gate.onSuccess} onClose={edit.gate.onClose} />
      <StepUpModal open={keyEditor.gate.open} requiredFactor={keyEditor.gate.factor} onSuccess={keyEditor.gate.onSuccess} onClose={keyEditor.gate.onClose} />
    </>
  );
}

const PEAK_WINDOWS_JSON_PLACEHOLDER = `[{"days":[1,2,3,4,5],"start":"01:00","end":"04:00"}]`;

const WEEKDAY_SHORT_KEYS = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"] as const;

// A short curated list, not the full ~600-name IANA database — common
// zones a provider is actually likely to publish hours in, covering every
// populated continent, plus UTC itself. "Custom…" is the escape hatch for
// anything else (the free-text input below still accepts any valid IANA
// name typed directly — this select is a convenience, not a restriction).
const COMMON_TIMEZONES = [
  "UTC",
  "America/Los_Angeles",
  "America/Denver",
  "America/Chicago",
  "America/New_York",
  "America/Sao_Paulo",
  "Europe/London",
  "Europe/Paris",
  "Europe/Moscow",
  "Africa/Cairo",
  "Africa/Johannesburg",
  "Asia/Dubai",
  "Asia/Kolkata",
  "Asia/Shanghai",
  "Asia/Tokyo",
  "Asia/Seoul",
  "Australia/Sydney",
  "Pacific/Auckland",
];

type PeakSchedule = { tz?: string; windows?: PeakWindowRow[] };

// splitStored parses the full stored peak_windows JSON (as PUT to the API)
// into its two independently-edited pieces: the timezone name and the
// windows array (kept as its own JSON text so the textarea round-trips
// exactly what the operator typed, including in-progress edits). Falls
// back to (no tz, raw text in the windows box) when the stored value isn't
// valid JSON — should only happen for hand-typed data mid-edit, since
// every value that reaches here from the server already parsed once.
function splitStored(raw: string): { tz: string; windowsJson: string } {
  if (raw.trim() === "") return { tz: "", windowsJson: "" };
  try {
    const parsed = JSON.parse(raw) as PeakSchedule;
    return { tz: parsed.tz ?? "", windowsJson: JSON.stringify(parsed.windows ?? []) };
  } catch {
    return { tz: "", windowsJson: raw };
  }
}

// composeStored is splitStored's inverse — called on every keystroke in
// either sub-field to rebuild the single JSON string the API/store expects.
// Omits "tz" entirely when empty (equivalent to UTC, matches the backend's
// omitempty convention) rather than writing {"tz":""}.
function composeStored(tz: string, windowsJson: string): string {
  if (windowsJson.trim() === "" && tz.trim() === "") return "";
  let windows: unknown = [];
  try {
    windows = JSON.parse(windowsJson || "[]");
  } catch {
    // Malformed mid-edit — still compose so `tz` isn't lost, but embed the
    // raw (invalid) text as a placeholder the server will reject with a
    // real parse error surfaced through the existing error banner.
    return `{${tz ? `"tz":${JSON.stringify(tz)},` : ""}"windows":${windowsJson || "[]"}}`;
  }
  const schedule: PeakSchedule = { windows: windows as PeakWindowRow[] };
  if (tz.trim() !== "") schedule.tz = tz.trim();
  return JSON.stringify(schedule);
}

// PeakWindowsEditor (peak pricing sprint, 2026-09-12; timezone input added
// 2026-09-13; structured builder Sprint 3 / 3B, 2026-11) — a Timezone picker
// plus a day-chip + time-range builder for a provider's recurring peak-hours
// windows (e.g. DeepSeek's weekday 01:00-04:00 + 06:00-10:00 UTC). The stored
// value is still the backend's JSON; the builder edits the `windows` array as
// rows (lib/peakWindows.ts) and the raw JSON textarea lives behind an "Edit as
// raw JSON" disclosure for anything the builder can't express — if the stored
// text doesn't parse into rows the raw editor stays open (and the builder is
// hidden) so nothing is silently rewritten. The timezone is a single flat
// value a provider almost always quotes in one local zone ("9am-5pm
// Pacific"); the server converts using the real IANA database
// (internal/pricing), so entering local hours + a zone here is enough — no
// manual UTC math, correct across DST. The summary below only reformats the
// parsed JSON for readability — it never evaluates whether a window is
// currently active; that's peakActiveNow, computed server-side and passed in,
// never re-derived here.
function PeakWindowsEditor({
  value,
  peakActiveNow,
  onChange,
}: {
  value: string;
  peakActiveNow: boolean;
  onChange: (v: string) => void;
}) {
  const { t } = useTranslation("settings");
  const [tz, setTz] = useState(() => splitStored(value).tz);
  const [windowsJson, setWindowsJson] = useState(() => splitStored(value).windowsJson);
  const [tzMode, setTzMode] = useState<"select" | "custom">(() =>
    COMMON_TIMEZONES.includes(splitStored(value).tz) || splitStored(value).tz === "" ? "select" : "custom",
  );
  const [rawOpen, setRawOpen] = useState(false);

  function update(nextTz: string, nextWindowsJson: string) {
    setTz(nextTz);
    setWindowsJson(nextWindowsJson);
    onChange(composeStored(nextTz, nextWindowsJson));
  }

  const rows = parseWindowRows(windowsJson);
  // Not representable as rows → the raw editor is the only honest view.
  const showRaw = rawOpen || rows === null;
  function updateRows(next: PeakWindowRow[]) {
    update(tz, serializeWindowRows(next));
  }

  let summary: string | null = null;
  let localParseError: string | null = null;
  if (windowsJson.trim() !== "") {
    try {
      const parsed = JSON.parse(windowsJson) as PeakWindowRow[];
      const tzLabel = tz.trim() || "UTC";
      summary = parsed
        .map((w) => `${w.days.map((d) => (WEEKDAY_SHORT_KEYS[d] ? t(`provider_keys.peak_windows.weekday_${WEEKDAY_SHORT_KEYS[d]}`) : `?${d}`)).join("/")} ${w.start}–${w.end} ${tzLabel}`)
        .join(", ") || t("provider_keys.peak_windows.no_windows");
    } catch {
      localParseError = t("provider_keys.peak_windows.parse_error");
    }
  }

  return (
    <div className="pw-editor">
      <label className="form-row">
        {t("provider_keys.peak_windows.timezone_label")}
        {tzMode === "select" ? (
          <select
            value={COMMON_TIMEZONES.includes(tz) ? tz : "UTC"}
            onChange={(e) => {
              if (e.target.value === "__custom") {
                setTzMode("custom");
                return;
              }
              update(e.target.value === "UTC" ? "" : e.target.value, windowsJson);
            }}
          >
            {COMMON_TIMEZONES.map((z) => <option key={z} value={z}>{z}</option>)}
            <option value="__custom">{t("provider_keys.peak_windows.custom_ellipsis")}</option>
          </select>
        ) : (
          <input
            value={tz}
            placeholder={t("provider_keys.peak_windows.timezone_placeholder")}
            className="pw-mono"
            onChange={(e) => update(e.target.value, windowsJson)}
          />
        )}
        <span className="pw-hint">
          {t("provider_keys.peak_windows.timezone_hint")}
        </span>
      </label>

      <div className="form-row">
        <span id="pw-windows-label">{t("provider_keys.peak_windows.windows_label")}</span>
        {!showRaw && rows && (
          <div className="pw-rows" role="group" aria-labelledby="pw-windows-label">
            {rows.length === 0 && <span className="pw-hint">{t("provider_keys.peak_windows.empty_hint")}</span>}
            {rows.map((row, i) => (
              <div key={i} className="pw-row" role="group" aria-label={t("provider_keys.peak_windows.window_n", { n: i + 1 })}>
                <span className="pw-days">
                  {WEEKDAY_SHORT_KEYS.map((k, d) => (
                    <button
                      key={k}
                      type="button"
                      className="chip pw-day"
                      aria-pressed={row.days.includes(d)}
                      onClick={() => updateRows(rows.map((r, j) => (j === i ? { ...r, days: toggleDay(r.days, d) } : r)))}
                    >
                      {t(`provider_keys.peak_windows.weekday_${k}`)}
                    </button>
                  ))}
                </span>
                <input
                  type="time"
                  value={row.start}
                  aria-label={t("provider_keys.peak_windows.start_n", { n: i + 1 })}
                  onChange={(e) => e.target.value && updateRows(rows.map((r, j) => (j === i ? { ...r, start: e.target.value } : r)))}
                />
                <span aria-hidden="true">–</span>
                <input
                  type="time"
                  value={row.end}
                  aria-label={t("provider_keys.peak_windows.end_n", { n: i + 1 })}
                  onChange={(e) => e.target.value && updateRows(rows.map((r, j) => (j === i ? { ...r, end: e.target.value } : r)))}
                />
                <button
                  type="button"
                  className="icon-btn"
                  aria-label={t("provider_keys.peak_windows.remove_window", { n: i + 1 })}
                  title={t("provider_keys.peak_windows.remove_window", { n: i + 1 })}
                  onClick={() => updateRows(rows.filter((_, j) => j !== i))}
                >
                  ✕
                </button>
                {row.days.length === 0 && <span className="pw-warn">{t("provider_keys.peak_windows.no_days_warn")}</span>}
              </div>
            ))}
            <div>
              <button type="button" className="btn" onClick={() => updateRows([...rows, newWindowRow()])}>
                {t("provider_keys.peak_windows.add_window")}
              </button>
            </div>
          </div>
        )}
        {showRaw && (
          <>
            <textarea
              rows={2}
              value={windowsJson}
              aria-labelledby="pw-windows-label"
              placeholder={PEAK_WINDOWS_JSON_PLACEHOLDER}
              className="pw-mono pw-raw"
              onChange={(e) => update(tz, e.target.value)}
            />
            <span className="pw-hint">
              {t("provider_keys.peak_windows.windows_hint", { format: `[{"days":[0-6, 0=Sun],"start":"HH:MM","end":"HH:MM"}]` })}
            </span>
          </>
        )}
        {localParseError && <span className="pw-warn">{localParseError}</span>}
        {rows === null && !localParseError && <span className="pw-hint">{t("provider_keys.peak_windows.raw_only_hint")}</span>}
        {summary && !localParseError && (
          <span className="pw-hint">
            {summary}
            {peakActiveNow && <span className="chip pw-peak-chip">{t("provider_keys.peak_windows.peak_now_chip")}</span>}
          </span>
        )}
        <button
          type="button"
          className="btn pw-raw-toggle"
          aria-expanded={showRaw}
          disabled={rows === null}
          onClick={() => setRawOpen(!rawOpen)}
        >
          {showRaw ? t("provider_keys.peak_windows.back_to_builder") : t("provider_keys.peak_windows.edit_raw")}
        </button>
      </div>
    </div>
  );
}
