// settings/panels/ProviderKeys.tsx — Sprint 12 (was H) Phase 5. Moved out of
// pages/Settings.tsx verbatim (zero logic edits — see the sprint plan's
// panel-adaptation table). CRUD via /api/v1/providers; the list shows the
// masked `api_key_masked` form, the API key itself is write-only
// (PUT /api/v1/providers/{name}/key) and never echoed back by the server.
import { useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { ConfirmButton } from "../../components/ConfirmButton";
import { Icon } from "../../components/Icon";
import { SaveButton } from "../../components/SaveButton";
import { apiErrorMessage } from "../../lib/api";
import { countryFlag } from "../../lib/format";
import { translatedDataResidencyGroup, translatedProviderNote } from "../../lib/providerPresetI18n";
import { PROVIDER_PRESETS, providerIconSlug } from "../../lib/providerPresets";
import { useCreateProvider, useDeleteProvider, useDiscoverProviderBilling, useProviders, useSetProviderKey, useUpdateProvider } from "../../lib/queries";
import type { ProviderCreateRequest, ProviderUpdateRequest } from "../../lib/types";
import { CURRENCIES } from "../constants";

const EMPTY_CREATE: ProviderCreateRequest = {
  name: "",
  bill_currency: "USD",
  target_url: "",
  status_url: "",
  credits_url: "",
  org_id: "",
  billing_console_url: "",
};

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
  // Phase 7: keyed by id, not name — a rename must not invalidate whichever
  // row is mid-edit.
  const [editing, setEditing] = useState<number | null>(null);
  const [editDraft, setEditDraft] = useState<ProviderUpdateRequest>({});
  const [keyFor, setKeyFor] = useState<number | null>(null);
  const [newKey, setNewKey] = useState("");
  const [error, setError] = useState<string | null>(null);
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

  function submitEdit(id: number) {
    setError(null);
    update.mutate(
      { id, req: editDraft },
      {
        // Sprint K: SaveButton's "✓ Saved" flash needs a moment to actually
        // render before the form it lives on unmounts — closing
        // synchronously in the same onSuccess (the old behavior) meant
        // update.isPending flips false and the component unmounts in the
        // same React commit, so the flash would never paint. 700ms is long
        // enough to register, short of the 1400ms the flash itself holds.
        onSuccess: () => {
          setTimeout(() => {
            setEditing(null);
            setEditDraft({});
          }, 700);
        },
        onError: showError,
      },
    );
  }

  function submitKey(id: number) {
    setError(null);
    setKey.mutate(
      { id, req: { api_key: newKey } },
      {
        onSuccess: () => {
          setTimeout(() => {
            setKeyFor(null);
            setNewKey("");
          }, 700);
        },
        onError: showError,
      },
    );
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
            const isEditing = editing === p.id;
            const isKeying = keyFor === p.id;
            const modelsShown = expandedModels === p.id;
            // Rename preview: the provider icon is resolved from the name
            // slug, so a live preview here shows the consequence before
            // saving (renaming off a known vendor slug drops the mark).
            const previewName = isEditing ? (editDraft.name ?? p.name) : p.name;
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
                          setEditing(p.id);
                          setEditDraft({
                            name: p.name,
                            bill_currency: p.bill_currency,
                            billing_enabled: p.billing_enabled,
                            billing_console_url: p.billing_console_url ?? "",
                            target_url: p.target_url ?? "",
                            status_url: p.status_url ?? "",
                            credits_url: p.credits_url ?? "",
                            org_id: p.org_id ?? "",
                          });
                          setKeyFor(null);
                        }}
                      >
                        {t("provider_keys.edit_button")}
                      </button>
                      <button disabled={isKeying || setKey.isPending} onClick={() => { setKeyFor(p.id); setNewKey(""); setEditing(null); }}>{t("provider_keys.set_key_button")}</button>
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
                      <input value={editDraft.name ?? ""} onChange={(e) => setEditDraft({ ...editDraft, name: e.target.value })} />
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
                        {t("provider_keys.name_hint")}
                      </span>
                      {editDraft.name && /[^\p{L}\p{N} &_.-]/u.test(editDraft.name) && (
                        <span style={{ fontSize: 11, color: "var(--warn)" }}>
                          {t("provider_keys.name_invalid_chars")}
                        </span>
                      )}
                    </label>
                    <label className="form-row">{t("provider_keys.bill_currency_label")}
                      <select value={editDraft.bill_currency ?? ""} onChange={(e) => setEditDraft({ ...editDraft, bill_currency: e.target.value })}>
                        {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
                      </select>
                    </label>
                    <label className="form-row">{t("compression.target_url_label")}
                      <input value={editDraft.target_url ?? ""} placeholder="https://api.example.com/v1" onChange={(e) => setEditDraft({ ...editDraft, target_url: e.target.value })} />
                    </label>
                    <label className="form-row">{t("provider_keys.status_url_label")}
                      <input value={editDraft.status_url ?? ""} placeholder="https://deepseek.statuspage.io/api/v2/summary.json" onChange={(e) => setEditDraft({ ...editDraft, status_url: e.target.value })} />
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>{t("provider_keys.status_url_hint_edit")}</span>
                    </label>
                    <label className="form-row">{t("provider_keys.credits_url_label")}
                      <input value={editDraft.credits_url ?? ""} placeholder="https://api.example.com/user/balance" onChange={(e) => setEditDraft({ ...editDraft, credits_url: e.target.value })} />
                    </label>
                    <label className="form-row">{t("provider_keys.org_id_label")}
                      <input value={editDraft.org_id ?? ""} placeholder="required by some providers' credits/analytics APIs (e.g. AI&)" onChange={(e) => setEditDraft({ ...editDraft, org_id: e.target.value })} />
                    </label>
                    <label className="form-row">{t("provider_keys.billing_console_url_label")}
                      <input value={editDraft.billing_console_url ?? ""} placeholder="https://platform.example.com/usage" onChange={(e) => setEditDraft({ ...editDraft, billing_console_url: e.target.value })} />
                    </label>
                    <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
                      <input
                        type="checkbox"
                        checked={editDraft.billing_enabled ?? true}
                        onChange={(e) => setEditDraft({ ...editDraft, billing_enabled: e.target.checked })}
                      />
                      {t("provider_keys.billing_api_enabled")}
                    </label>
                    <PeakWindowsEditor
                      value={editDraft.peak_windows ?? p.peak_windows ?? ""}
                      peakActiveNow={p.peak_active_now}
                      onChange={(v) => setEditDraft({ ...editDraft, peak_windows: v })}
                    />
                    <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                      <button className="btn" onClick={() => { setEditing(null); setEditDraft({}); }}>{t("provider_keys.cancel")}</button>
                      <SaveButton pending={update.isPending} isError={update.isError} onClick={() => submitEdit(p.id)} />
                    </div>
                  </div>
                )}

                {isKeying && (
                  <div style={{ display: "flex", gap: 8, alignItems: "flex-end", flexWrap: "wrap", marginTop: 4 }}>
                    <label className="form-row" style={{ flex: "1 1 320px" }}>{t("provider_keys.new_api_key_label")}
                      <input
                        type="password"
                        value={newKey}
                        placeholder="sk-…"
                        onChange={(e) => setNewKey(e.target.value)}
                        autoComplete="off"
                        spellCheck={false}
                      />
                    </label>
                    <div className="form-actions">
                      <button className="btn" onClick={() => { setKeyFor(null); setNewKey(""); }}>{t("provider_keys.cancel")}</button>
                      <SaveButton
                        pending={setKey.isPending}
                        isError={setKey.isError}
                        disabled={setKey.isPending || !newKey}
                        onClick={() => submitKey(p.id)}
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

type PeakWindowRow = { days: number[]; start: string; end: string };
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
// 2026-09-13) — a labeled Timezone picker plus a validated JSON textarea
// for a provider's recurring peak-hours windows (e.g. DeepSeek's weekday
// 01:00-04:00 + 06:00-10:00 UTC). The windows LIST stays JSON — there's no
// precedent anywhere in this codebase for a bespoke hour-range picker (the
// closest is SchedulerJobs' raw cron text input) and it's edited rarely —
// but the timezone is a single flat value a provider almost always quotes
// in one local zone ("9am-5pm Pacific"), and typing "tz":"..." correctly
// inside hand-written JSON is exactly the kind of error a labeled field
// with a curated dropdown avoids. The server converts using the real IANA
// database (internal/pricing), so entering local hours + a zone here is
// enough — no manual UTC math, and it stays correct across DST automatically.
// The summary below only reformats the parsed JSON for readability — it
// never evaluates whether a window is currently active; that's
// peakActiveNow, computed server-side and passed in, never re-derived here.
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

  function update(nextTz: string, nextWindowsJson: string) {
    setTz(nextTz);
    setWindowsJson(nextWindowsJson);
    onChange(composeStored(nextTz, nextWindowsJson));
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
    <div style={{ gridColumn: "1 / -1", display: "grid", gap: 10 }}>
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
            style={{ fontFamily: "var(--mono)" }}
            onChange={(e) => update(e.target.value, windowsJson)}
          />
        )}
        <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
          {t("provider_keys.peak_windows.timezone_hint")}
        </span>
      </label>
      <label className="form-row">
        {t("provider_keys.peak_windows.windows_label")}
        <textarea
          rows={2}
          value={windowsJson}
          placeholder={PEAK_WINDOWS_JSON_PLACEHOLDER}
          style={{ fontFamily: "var(--mono)", fontSize: 11 }}
          onChange={(e) => update(tz, e.target.value)}
        />
        <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
          {t("provider_keys.peak_windows.windows_hint", { format: `[{"days":[0-6, 0=Sun],"start":"HH:MM","end":"HH:MM"}]` })}
        </span>
        {localParseError && <span style={{ fontSize: 11, color: "var(--warn)" }}>{localParseError}</span>}
        {summary && !localParseError && (
          <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
            {summary}
            {peakActiveNow && <span className="chip" style={{ marginLeft: 6, color: "var(--warn)" }}>{t("provider_keys.peak_windows.peak_now_chip")}</span>}
          </span>
        )}
      </label>
    </div>
  );
}
