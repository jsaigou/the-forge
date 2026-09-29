import { useTranslation } from "react-i18next";
import { formatCurrency, formatPct, formatWatts } from "../../lib/format";
import { useCostEnergyHistory, useCostSummary } from "../../lib/queries";
import { rangeLabel } from "../../lib/rangeLabels";
import type { CostEnergyHistoryPoint } from "../../lib/types";
import { TrendChart, type TrendSeriesDef } from "../charts/TrendChart";

// Widget "electricity-breakdown" (see lib/widgetRegistry.ts). Relocated
// verbatim from Dashboard's now-deleted Trends tab — Phase 5 (2026-08-12).
// ADR-0012: rangeLabel is derived from window_ internally rather than passed
// as a separate prop.
export function ElectricityBreakdownWidget({ window_ }: { window_: string }) {
  const { t } = useTranslation("dashboard");
  const costSummary = useCostSummary(window_);
  const energyHistory = useCostEnergyHistory(window_);
  const energy = costSummary.data?.energy;

  // Single series only: cost_display = wall_wh_est × a constant rate within
  // any one window, so plotting both normalized-to-own-max produces two
  // perfectly overlapping curves — the second draws directly on top of the
  // first and silently hides it (see feedback_dont_chart_correlated_series).
  // The stat tiles above already show the exact cost figure; the trend
  // line's job is just the energy shape over time.
  const energySeries: TrendSeriesDef<CostEnergyHistoryPoint>[] = [
    { key: "wall", label: t("electricity_breakdown.series_wall"), get: (p) => p.wall_wh_est, color: "var(--heat-deep)", format: (v) => `${v.toFixed(1)} Wh` },
  ];

  return (
    <>
      <div className="eyebrow">{t("electricity_breakdown.title", { range: rangeLabel(window_, t) })}</div>
      <div className="card">
        {energy ? (
          <>
            <div className="stats" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))", marginBottom: 12 }}>
              <div className="stat">
                <div className="k">{t("electricity_breakdown.cost_k")}</div>
                <div className="v">{formatCurrency(energy.cost_display, costSummary.data!.display_currency)}</div>
                <div className="d">{t("electricity_breakdown.cost_d", { rate: energy.rate_per_kwh, currency: energy.rate_currency })}</div>
              </div>
              <div className="stat">
                <div className="k">{t("electricity_breakdown.package_k")}</div>
                <div className="v">{energy.package_wh.toFixed(0)} Wh</div>
                <div className="d">{t("electricity_breakdown.package_d")}</div>
              </div>
              <div className="stat">
                <div className="k">{t("electricity_breakdown.wall_k")}</div>
                <div className="v">{energy.wall_wh_est.toFixed(0)} Wh</div>
                <div className="d">{t("electricity_breakdown.wall_d", { overhead: energy.overhead_w, psu: energy.psu_efficiency })}</div>
              </div>
              <div className="stat">
                <div className="k">{t("electricity_breakdown.coverage_k")}</div>
                <div className="v" style={{ color: energy.coverage_pct < 90 ? "var(--warn)" : undefined }}>{formatPct(energy.coverage_pct)}</div>
                <div className="d">{t("electricity_breakdown.coverage_d", { gap: Math.round(energy.gap_seconds / 60), unmeasured: Math.round(energy.unmeasured_seconds / 60) })}</div>
              </div>
              <div className="stat">
                <div className="k">{t("electricity_breakdown.idle_k")}</div>
                <div className="v">{formatWatts(energy.idle_baseline_w)}</div>
                <div className="d">{t("electricity_breakdown.idle_d")}</div>
              </div>
              <div className="stat">
                <div className="k">{t("electricity_breakdown.attributable_k")}</div>
                <div className="v">{energy.attributable_wh_est.toFixed(0)} Wh</div>
                <div className="d">{t("electricity_breakdown.attributable_d")}</div>
              </div>
            </div>
            {energyHistory.data && energyHistory.data.points.length > 1 ? (
              <TrendChart<CostEnergyHistoryPoint> points={energyHistory.data.points} series={energySeries} ariaLabel={t("electricity_breakdown.chart_aria")} />
            ) : (
              <div className="empty-note">
                {t("electricity_breakdown.no_trend")}
              </div>
            )}
          </>
        ) : (
          <div className="empty-note">{t("electricity_breakdown.loading")}</div>
        )}
      </div>
    </>
  );
}
