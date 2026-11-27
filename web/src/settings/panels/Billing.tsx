// settings/panels/Billing.tsx — Sprint 12 (was H) Phase 5 origin: the former
// separate Currency and Cost & power sections, merged into one "Billing"
// section, two cards — both components moved out of pages/Settings.tsx
// unedited at the time. Sprint 2 Stream 2D
// (docs/ui-remediation-plan-2026-11.md; review §A2 "Two draft/save
// patterns") ended the "moved unedited" status: both cards now draft/save/
// reset through useSettingsGroup, the shared machinery every Phase 6 panel
// uses — including its step-up gate, which these PUTs really are behind
// (requireAssurance(page.settings), httpapi.go; the old hand-rolled save
// surfaced those 403s as raw error notes with no retry path). The
// <Field>+fields.ts registry move (labels/help/apply badges as BILLING_*
// records) landed in Sprint 3 / 3C: BILLING_FIELDS in fields.ts carries the
// text, units and validation and 7 of 9 controls render through <Field>.
// rate_currency (locale-guess chip) and fx_refresh_min (blank = unset, not 0)
// stay bespoke, but take their label/help from the same records.
import { useTranslation } from "react-i18next";
import { InfoTip } from "../../components/InfoTip";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { localeCurrencyGuess } from "../../lib/format";
import { useBillingSettings, useCostSettings, useUpdateBillingSettings, useUpdateCostSettings } from "../../lib/queries";
import { Field, fieldHelp, fieldLabel } from "../Field";
import { BILLING_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";

const F = Object.fromEntries(BILLING_FIELDS.map((f) => [f.id, f]));

function Currency({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useBillingSettings();
  const update = useUpdateBillingSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow" id="billing-currency">{t("billing.currency_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: t("billing.resource_billing") })}</div></div>
      </>
    );
  }
  if (!g.active) {
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
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["billing.display_currency"]} value={g.active.display_currency} disabled={!canAdmin}
            onChange={(v) => g.setField("display_currency", String(v))} />
          <Field rec={F["billing.fx_source_url"]} value={g.active.fx_source_url ?? ""} disabled={!canAdmin}
            onChange={(v) => g.setField("fx_source_url", String(v))} />
          <Field rec={F["billing.fx_refresh_min"]} value={g.active.fx_refresh_min ?? ""} disabled={!canAdmin}
            onChange={(v) => g.setField("fx_refresh_min", v === "" ? undefined : Number(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

// Cost & power (Dashboard follow-up round 2) — moved here from the
// Dashboard's Cost tab, following the pattern Currency (above) uses:
// own eyebrow+card chrome, query/mutation hooks (useSettingsGroup owns the
// draft/save since 2D), canAdmin as a prop, isError/loading branches that
// repeat the eyebrow so the section header never disappears.
function CostSettingsPanel({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useCostSettings();
  const update = useUpdateCostSettings();
  const g = useSettingsGroup(cfg.data, update);
  const localeGuess = localeCurrencyGuess();

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow" id="billing-cost">{t("billing.cost_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: t("billing.resource_cost") })}</div></div>
      </>
    );
  }
  if (!g.active) {
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
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["billing.rate_per_kwh"]} value={g.active.rate_per_kwh} disabled={!canAdmin}
            onChange={(v) => g.setField("rate_per_kwh", Number(v))} />
          <label className="form-row" id="billing.rate_currency" htmlFor="billing-rate-currency-input">
            <span className="field-label-line">
              {fieldLabel(t, F["billing.rate_currency"])}
              <InfoTip text={fieldHelp(t, F["billing.rate_currency"])} />
              {canAdmin && localeGuess && localeGuess !== g.active.rate_currency && (
                <button
                  type="button"
                  className="chip billing-locale-chip"
                  title={t("billing.use_locale_title")}
                  onClick={() => g.setField("rate_currency", localeGuess)}
                >
                  {t("billing.use_locale_button", { code: localeGuess })}
                </button>
              )}
            </span>
            <input
              id="billing-rate-currency-input"
              value={g.active.rate_currency}
              disabled={!canAdmin}
              onChange={(e) => g.setField("rate_currency", e.target.value.toUpperCase())}
            />
          </label>
          <Field rec={F["billing.overhead_w"]} value={g.active.overhead_w} disabled={!canAdmin}
            onChange={(v) => g.setField("overhead_w", Number(v))} />
          <Field rec={F["billing.psu_efficiency"]} value={g.active.psu_efficiency} disabled={!canAdmin}
            onChange={(v) => g.setField("psu_efficiency", Number(v))} />
          <Field rec={F["billing.max_power_w"]} value={g.active.max_power_w} disabled={!canAdmin}
            onChange={(v) => g.setField("max_power_w", Number(v))} />
          <Field rec={F["billing.power_kw"]} value={g.active.power_kw} disabled={!canAdmin}
            onChange={(v) => g.setField("power_kw", Number(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
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
