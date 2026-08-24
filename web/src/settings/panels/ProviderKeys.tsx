// settings/panels/ProviderKeys.tsx — Sprint 12 (was H) Phase 5. Moved out of
// pages/Settings.tsx verbatim (zero logic edits — see the sprint plan's
// panel-adaptation table). CRUD via /api/v1/providers; the list shows the
// masked `api_key_masked` form, the API key itself is write-only
// (PUT /api/v1/providers/{name}/key) and never echoed back by the server.
import { useState } from "react";
import { ConfirmButton } from "../../components/ConfirmButton";
import { Icon } from "../../components/Icon";
import { SaveButton } from "../../components/SaveButton";
import { apiErrorMessage } from "../../lib/api";
import { countryFlag } from "../../lib/format";
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
      <div className="eyebrow" id="providers-keys">Providers</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          External providers routed via A0. Keys are stored server-side in <span style={{ fontFamily: "var(--mono)" }}>router_providers</span>;
          the masked form below is the only shape the API ever returns — new keys are write-only.
        </div>

        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

        {providers.isError && <div className="empty-note">Operator role required to view providers.</div>}
        {!providers.isError && !providers.data && <div className="empty-note">Loading providers…</div>}
        {!providers.isError && providers.data && list.length === 0 && (
          <div className="empty-note">No providers configured.</div>
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
                          disabled — not routing
                        </span>
                      )}
                      <span className="chip" style={{ marginLeft: 6, color: healthColor, borderColor: `color-mix(in srgb, ${healthColor} 40%, var(--border))` }}>
                        {p.health.state}
                      </span>
                      {p.country && (
                        <span className="chip" style={{ marginLeft: 4, color: "var(--text-dim)" }}>
                          {countryFlag(p.country)} {p.country}{p.data_residency_group ? ` · ${p.data_residency_group}` : ""}
                        </span>
                      )}
                      {p.models.length > 0 && (
                        <button
                          className="chip"
                          style={{ marginLeft: 4, cursor: "pointer" }}
                          onClick={() => setExpandedModels(modelsShown ? null : p.id)}
                          title="Show the models routed through this provider"
                        >
                          {p.models.length} model{p.models.length === 1 ? "" : "s"} {modelsShown ? "▲" : "▼"}
                        </button>
                      )}
                    </div>
                    <div className="pu">
                      {p.api_key_masked || "no key set"} · {p.bill_currency}
                      {p.credits.supported && p.credits.balance_native != null
                        ? ` · balance ${p.credits.balance_native}${p.credits.currency ? " " + p.credits.currency : ""}`
                        : p.credits.supported
                          ? " · balance unavailable"
                          : " · no balance API"}
                    </div>
                  </div>
                  {canAdmin && (
                    <div className="actions" style={{ marginLeft: "auto" }}>
                      <button
                        disabled={update.isPending}
                        title={p.enabled
                          ? "Stop routing this provider's models (keys and offerings are kept)"
                          : "Resume routing this provider's models"}
                        onClick={() => toggleEnabled(p.id, !p.enabled)}
                      >
                        {p.enabled ? "Disable" : "Enable"}
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
                        Edit
                      </button>
                      <button disabled={isKeying || setKey.isPending} onClick={() => { setKeyFor(p.id); setNewKey(""); setEditing(null); }}>Set key</button>
                      <button
                        disabled={discoverBilling.isPending}
                        title="Probe candidate billing-API URLs for this provider"
                        onClick={() => submitDiscover(p.id, p.name)}
                      >
                        {discoverBilling.isPending && discoverBilling.variables === p.id ? "Discovering…" : "Discover billing"}
                      </button>
                      <ConfirmButton
                        className=""
                        pending={remove.isPending}
                        onConfirm={() => submitDelete(p.id)}
                        warning={`Delete provider "${p.name}"? Its offerings go with it. Use Disable instead to keep everything and just stop routing.`}
                      />
                    </div>
                  )}
                </div>
                {discoverResult && discoverResult.id === p.id && (
                  <div className="d" style={{ marginTop: 2 }}>
                    {discoverResult.found
                      ? discoverResult.saved
                        ? `Found and saved: ${discoverResult.url}`
                        : `Found ${discoverResult.url} but credits_url was already set — not overwritten`
                      : "No billing endpoint found among the candidates tried"}
                  </div>
                )}

                {modelsShown && (
                  <div className="hoom" style={{ marginTop: 4, paddingLeft: 12, borderLeft: "2px solid var(--border)" }}>
                    {p.models.length === 0 && <div className="empty-note">No offerings route through this provider.</div>}
                    {p.models.map((m) => (
                      <div key={m.catalog_model_id + ":" + m.model_id} className="prox" style={{ opacity: m.enabled ? undefined : 0.55 }}>
                        {m.logo && <Icon slug={m.logo} name={m.display_name} sm />}
                        <div>
                          <div className="pn">
                            {m.display_name || m.model_id}
                            {!m.enabled && <span className="chip" style={{ marginLeft: 6, color: "var(--text-mute)" }}>disabled</span>}
                          </div>
                          <div className="pu" style={{ fontFamily: "var(--mono)" }}>
                            {m.model_id} · {m.price_in_per_1m}/{m.price_out_per_1m} {m.currency} · priority {m.priority}
                            {m.compressor_proxy ? ` · via ${m.compressor_proxy}` : ""}
                          </div>
                        </div>
                      </div>
                    ))}
                    <a href="#settings/routing" style={{ fontSize: 11, marginTop: 4 }}>
                      Edit routing / pricing in Settings → Routing →
                    </a>
                  </div>
                )}

                {isEditing && (
                  <div className="form-grid" style={{ marginTop: 4 }}>
                    <label className="form-row">Name
                      <input value={editDraft.name ?? ""} onChange={(e) => setEditDraft({ ...editDraft, name: e.target.value })} />
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
                        The icon above previews live — renaming off a recognized vendor slug (e.g. "deepseek", "qwen") drops the vendor mark.
                      </span>
                      {editDraft.name && /[^\p{L}\p{N} &_.-]/u.test(editDraft.name) && (
                        <span style={{ fontSize: 11, color: "var(--warn)" }}>
                          Names must start with a letter or number and contain only letters, numbers, spaces, &, and .-_ — other characters will be rejected by the server.
                        </span>
                      )}
                    </label>
                    <label className="form-row">Bill currency
                      <select value={editDraft.bill_currency ?? ""} onChange={(e) => setEditDraft({ ...editDraft, bill_currency: e.target.value })}>
                        {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
                      </select>
                    </label>
                    <label className="form-row">Target URL
                      <input value={editDraft.target_url ?? ""} placeholder="https://api.example.com/v1" onChange={(e) => setEditDraft({ ...editDraft, target_url: e.target.value })} />
                    </label>
                    <label className="form-row">Status URL
                      <input value={editDraft.status_url ?? ""} placeholder="https://deepseek.statuspage.io/api/v2/summary.json" onChange={(e) => setEditDraft({ ...editDraft, status_url: e.target.value })} />
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>Wants a machine-readable Statuspage /api/v2/summary.json feed, not a human status page.</span>
                    </label>
                    <label className="form-row">Credits URL
                      <input value={editDraft.credits_url ?? ""} placeholder="https://api.example.com/user/balance" onChange={(e) => setEditDraft({ ...editDraft, credits_url: e.target.value })} />
                    </label>
                    <label className="form-row">Org ID
                      <input value={editDraft.org_id ?? ""} placeholder="required by some providers' credits/analytics APIs (e.g. AI&)" onChange={(e) => setEditDraft({ ...editDraft, org_id: e.target.value })} />
                    </label>
                    <label className="form-row">Billing console URL
                      <input value={editDraft.billing_console_url ?? ""} placeholder="https://platform.example.com/usage" onChange={(e) => setEditDraft({ ...editDraft, billing_console_url: e.target.value })} />
                    </label>
                    <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
                      <input
                        type="checkbox"
                        checked={editDraft.billing_enabled ?? true}
                        onChange={(e) => setEditDraft({ ...editDraft, billing_enabled: e.target.checked })}
                      />
                      Billing API enabled
                    </label>
                    <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                      <button className="btn" onClick={() => { setEditing(null); setEditDraft({}); }}>Cancel</button>
                      <SaveButton pending={update.isPending} isError={update.isError} onClick={() => submitEdit(p.id)} />
                    </div>
                  </div>
                )}

                {isKeying && (
                  <div style={{ display: "flex", gap: 8, alignItems: "flex-end", flexWrap: "wrap", marginTop: 4 }}>
                    <label className="form-row" style={{ flex: "1 1 320px" }}>New API key (write-only — never echoed back)
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
                      <button className="btn" onClick={() => { setKeyFor(null); setNewKey(""); }}>Cancel</button>
                      <SaveButton
                        pending={setKey.isPending}
                        isError={setKey.isError}
                        disabled={setKey.isPending || !newKey}
                        onClick={() => submitKey(p.id)}
                        label="Save key"
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
              <label className="form-row" style={{ gridColumn: "1 / -1" }}>Preset
                <select value={presetId} onChange={(e) => applyPreset(e.target.value)}>
                  <option value="">Custom…</option>
                  {PROVIDER_PRESETS.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
                </select>
              </label>
              {selectedPreset?.note && (
                <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)", marginTop: -6 }}>{selectedPreset.note}</div>
              )}
              {selectedPreset && selectedPreset.credits === "none" && (
                <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)", marginTop: -6 }}>
                  No balance API for this provider — billing polling stays off (Billing console URL is still a human link).
                </div>
              )}
              <label className="form-row">Name *
                <input value={createDraft.name} placeholder="deepseek" onChange={(e) => setCreateDraft({ ...createDraft, name: e.target.value })} />
                {createDraft.name && /[^\p{L}\p{N} &_.-]/u.test(createDraft.name) && (
                  <span style={{ fontSize: 11, color: "var(--warn)" }}>
                    Names must start with a letter or number and contain only letters, numbers, spaces, &, and .-_ — other characters will be rejected by the server.
                  </span>
                )}
              </label>
              <label className="form-row">Bill currency
                <select value={createDraft.bill_currency} onChange={(e) => setCreateDraft({ ...createDraft, bill_currency: e.target.value })}>
                  {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
                </select>
              </label>
              <label className="form-row">Target URL
                <input value={createDraft.target_url} placeholder="https://api.deepseek.com/v1" onChange={(e) => setCreateDraft({ ...createDraft, target_url: e.target.value })} />
                {selectedPreset?.targetUrlIsTemplate && (
                  <span style={{ fontSize: 11, color: "var(--text-dim)" }}>Replace &lt;resource&gt; with your own — {selectedPreset.label} has no single fixed endpoint.</span>
                )}
              </label>
              <label className="form-row">Status URL
                <input value={createDraft.status_url} placeholder="https://deepseek.statuspage.io/api/v2/summary.json" onChange={(e) => setCreateDraft({ ...createDraft, status_url: e.target.value })} />
                <span style={{ fontSize: 11, color: "var(--text-dim)" }}>Wants a machine-readable Statuspage /api/v2/summary.json feed, not a human status page — an unrecognized shape falls back to a live probe of the target URL.</span>
              </label>
              <label className="form-row">Credits URL
                <input value={createDraft.credits_url} placeholder="https://api.deepseek.com/user/balance" onChange={(e) => setCreateDraft({ ...createDraft, credits_url: e.target.value })} />
              </label>
              <label className="form-row">Org ID{selectedPreset?.orgIdRequired ? " *" : ""}
                <input value={createDraft.org_id} placeholder="required by some providers' credits/analytics APIs (e.g. AI&)" onChange={(e) => setCreateDraft({ ...createDraft, org_id: e.target.value })} />
              </label>
              <label className="form-row">Billing console URL
                <input value={createDraft.billing_console_url ?? ""} placeholder="https://platform.example.com/usage" onChange={(e) => setCreateDraft({ ...createDraft, billing_console_url: e.target.value })} />
              </label>
              <label className="form-row" style={{ gridColumn: "1 / -1", flexDirection: "row", alignItems: "center", gap: 8 }}>
                <input type="checkbox" checked={createProxy} onChange={(e) => setCreateProxy(e.target.checked)} style={{ width: "auto" }} />
                <span>Create a Compressor proxy for this provider (recommended)</span>
              </label>
              <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
                <button className="btn" onClick={() => { setCreating(false); setCreateDraft(EMPTY_CREATE); setPresetId(""); }}>Cancel</button>
                <button className="btn primary" disabled={create.isPending || !createDraft.name} onClick={submitCreate}>Create</button>
              </div>
            </div>
          ) : (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setCreating(true); setCreateDraft(EMPTY_CREATE); setPresetId(""); setError(null); }}>
              + Add provider
            </button>
          )
        )}
      </div>
    </>
  );
}
