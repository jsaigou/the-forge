// settings/panels/Billing.tsx — Sprint 12 (was H) Phase 5. Merges the
// former separate Currency and Cost & power sections into one "Billing"
// section, two cards — presentational only, both components moved out of
// pages/Settings.tsx unedited (their own eyebrow+card chrome, own query/
// mutation hooks, own 700ms delayed-close pattern all untouched).
import { useState } from "react";
import { SaveButton } from "../../components/SaveButton";
import { apiErrorMessage } from "../../lib/api";
import { localeCurrencyGuess } from "../../lib/format";
import { useBillingSettings, useCostSettings, useUpdateBillingSettings, useUpdateCostSettings } from "../../lib/queries";
import type { BillingSettings, CostSettings } from "../../lib/types";
import { CURRENCIES } from "../constants";

function Currency({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useBillingSettings();
  const update = useUpdateBillingSettings();
  const [draft, setDraft] = useState<BillingSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const active = draft ?? cfg.data;

  function save() {
    if (!draft) return;
    setError(null);
    update.mutate(draft, {
      // Sprint K: delayed close so SaveButton's flash has time to paint —
      // see the matching comment on ProviderKeys.tsx's submitEdit.
      onSuccess: () => setTimeout(() => setDraft(null), 700),
      onError: (e) => setError(apiErrorMessage(e)),
    });
  }

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow" id="billing-currency">Currency</div>
        <div className="card"><div className="empty-note">Operator role required to view billing settings.</div></div>
      </>
    );
  }
  if (!active) {
    return (
      <>
        <div className="eyebrow" id="billing-currency">Currency</div>
        <div className="card"><div className="empty-note">Loading billing settings…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow" id="billing-currency">Currency</div>
      <div className="card">
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-grid">
          <label className="form-row">Display currency (ISO 4217)
            <select
              value={active.display_currency}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, display_currency: e.target.value })}
            >
              {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
            </select>
          </label>
          <label className="form-row">FX source URL (optional)
            <input
              value={active.fx_source_url ?? ""}
              placeholder="https://open.er-api.com/v6/latest/USD"
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, fx_source_url: e.target.value })}
            />
          </label>
          <label className="form-row">FX refresh (minutes, optional)
            <input
              type="number"
              min={1}
              value={active.fx_refresh_min ?? ""}
              placeholder="60"
              disabled={!canAdmin}
              onChange={(e) => {
                const v = e.target.value;
                setDraft({ ...active, fx_refresh_min: v === "" ? undefined : Number(v) });
              }}
            />
          </label>
        </div>
        {canAdmin && draft && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>Reset</button>
            <SaveButton pending={update.isPending} isError={update.isError} onClick={save} />
          </div>
        )}
      </div>
    </>
  );
}

// Cost & power (Dashboard follow-up round 2) — moved here from the
// Dashboard's Cost tab, following the exact pattern Currency (above) uses:
// own query/mutation hooks, canAdmin as a prop, isError/loading branches
// that repeat the eyebrow so the section header never disappears.
function CostSettingsPanel({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useCostSettings();
  const update = useUpdateCostSettings();
  const [draft, setDraft] = useState<CostSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const active = draft ?? cfg.data;
  const localeGuess = localeCurrencyGuess();

  function save() {
    if (!draft) return;
    setError(null);
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    update.mutate(draft, { onSuccess: () => setTimeout(() => setDraft(null), 700), onError: (e) => setError(apiErrorMessage(e)) });
  }

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow" id="billing-cost">Cost &amp; power</div>
        <div className="card"><div className="empty-note">Operator role required to view cost settings.</div></div>
      </>
    );
  }
  if (!active) {
    return (
      <>
        <div className="eyebrow" id="billing-cost">Cost &amp; power</div>
        <div className="card"><div className="empty-note">Loading cost settings…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow" id="billing-cost">Cost &amp; power</div>
      <div className="card">
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-grid">
          <label className="form-row">Electricity rate (per kWh)
            <input
              type="number" step="0.01" min={0}
              value={active.rate_per_kwh}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, rate_per_kwh: Number(e.target.value) })}
            />
          </label>
          <label className="form-row">
            <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
              Rate currency (ISO 4217)
              {canAdmin && localeGuess && localeGuess !== active.rate_currency && (
                <button
                  type="button"
                  className="chip"
                  style={{ cursor: "pointer", fontSize: 10 }}
                  title="Set from your browser's locale"
                  onClick={() => setDraft({ ...active, rate_currency: localeGuess })}
                >
                  use {localeGuess}
                </button>
              )}
            </span>
            <input
              value={active.rate_currency}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, rate_currency: e.target.value.toUpperCase() })}
            />
          </label>
          <label className="form-row" title="Added to measured package watts, then divided by PSU efficiency to get wall watts.">Rest-of-system overhead (W)
            <input
              type="number" step="1" min={0}
              value={active.overhead_w}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, overhead_w: Number(e.target.value) })}
            />
          </label>
          <label className="form-row" title="Wall watts = (package watts + overhead) ÷ PSU efficiency. Uncalibrated estimate until checked against a plug meter.">PSU efficiency (0–1]
            <input
              type="number" step="0.01" min={0.01} max={1}
              value={active.psu_efficiency}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, psu_efficiency: Number(e.target.value) })}
            />
          </label>
          <label className="form-row" title="Scales the Overview power chart/tile against a real hardware limit only — never feeds cost math.">Overview power-chart ceiling (W, package max draw)
            <input
              type="number" step="1" min={0}
              value={active.max_power_w}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, max_power_w: Number(e.target.value) })}
            />
          </label>
          <label className="form-row" title="Flat 'if this model ran alone, flat-out' hypothetical rate used only for model/config card power estimates — does not feed measured-energy figures.">Per-model card constant (kW, counterfactual)
            <input
              type="number" step="0.01" min={0}
              value={active.power_kw}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, power_kw: Number(e.target.value) })}
            />
          </label>
        </div>
        {canAdmin && draft && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>Reset</button>
            <SaveButton pending={update.isPending} isError={update.isError} onClick={save} />
          </div>
        )}
      </div>
    </>
  );
}

export function Billing({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <Currency canAdmin={canAdmin} />
      <CostSettingsPanel canAdmin={canAdmin} />
    </>
  );
}
