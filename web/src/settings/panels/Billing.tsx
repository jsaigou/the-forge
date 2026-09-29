// settings/panels/Billing.tsx — Sprint 12 (was H) Phase 5. Merges the
// former separate Currency and Cost & power sections into one "Billing"
// section, two cards — presentational only, both components moved out of
// pages/Settings.tsx unedited (their own eyebrow+card chrome, own query/
// mutation hooks, own 700ms delayed-close pattern all untouched).
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { SaveButton } from "../../components/SaveButton";
import { apiErrorMessage } from "../../lib/api";
import { localeCurrencyGuess } from "../../lib/format";
import { useBillingSettings, useCostSettings, useUpdateBillingSettings, useUpdateCostSettings } from "../../lib/queries";
import type { BillingSettings, CostSettings } from "../../lib/types";
import { CURRENCIES } from "../constants";

function Currency({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
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
        <div className="eyebrow" id="billing-currency">{t("billing.currency_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: t("billing.resource_billing") })}</div></div>
      </>
    );
  }
  if (!active) {
    return (
      <>
        <div className="eyebrow" id="billing-currency">{t("billing.currency_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.loading", { resource: t("billing.resource_billing") })}</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow" id="billing-currency">{t("billing.currency_title")}</div>
      <div className="card">
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-grid">
          <label className="form-row">{t("billing.display_currency_label")}
            <select
              value={active.display_currency}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, display_currency: e.target.value })}
            >
              {CURRENCIES.map((c) => <option key={c} value={c}>{c}</option>)}
            </select>
          </label>
          <label className="form-row">{t("billing.fx_source_label")}
            <input
              value={active.fx_source_url ?? ""}
              placeholder="https://open.er-api.com/v6/latest/USD"
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, fx_source_url: e.target.value })}
            />
          </label>
          <label className="form-row">{t("billing.fx_refresh_label")}
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
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>{t("shared.reset")}</button>
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
  const { t } = useTranslation("settings");
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
        <div className="eyebrow" id="billing-cost">{t("billing.cost_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: t("billing.resource_cost") })}</div></div>
      </>
    );
  }
  if (!active) {
    return (
      <>
        <div className="eyebrow" id="billing-cost">{t("billing.cost_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.loading", { resource: t("billing.resource_cost") })}</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow" id="billing-cost">{t("billing.cost_title")}</div>
      <div className="card">
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-grid">
          <label className="form-row">{t("billing.electricity_rate_label")}
            <input
              type="number" step="0.01" min={0}
              value={active.rate_per_kwh}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, rate_per_kwh: Number(e.target.value) })}
            />
          </label>
          <label className="form-row">
            <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
              {t("billing.rate_currency_label")}
              {canAdmin && localeGuess && localeGuess !== active.rate_currency && (
                <button
                  type="button"
                  className="chip"
                  style={{ cursor: "pointer", fontSize: 10 }}
                  title={t("billing.use_locale_title")}
                  onClick={() => setDraft({ ...active, rate_currency: localeGuess })}
                >
                  {t("billing.use_locale_button", { code: localeGuess })}
                </button>
              )}
            </span>
            <input
              value={active.rate_currency}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, rate_currency: e.target.value.toUpperCase() })}
            />
          </label>
          <label className="form-row" title={t("billing.overhead_title")}>{t("billing.overhead_label")}
            <input
              type="number" step="1" min={0}
              value={active.overhead_w}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, overhead_w: Number(e.target.value) })}
            />
          </label>
          <label className="form-row" title={t("billing.psu_title")}>{t("billing.psu_label")}
            <input
              type="number" step="0.01" min={0.01} max={1}
              value={active.psu_efficiency}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, psu_efficiency: Number(e.target.value) })}
            />
          </label>
          <label className="form-row" title={t("billing.ceiling_title")}>{t("billing.ceiling_label")}
            <input
              type="number" step="1" min={0}
              value={active.max_power_w}
              disabled={!canAdmin}
              onChange={(e) => setDraft({ ...active, max_power_w: Number(e.target.value) })}
            />
          </label>
          <label className="form-row" title={t("billing.power_kw_title")}>{t("billing.power_kw_label")}
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
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>{t("shared.reset")}</button>
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
