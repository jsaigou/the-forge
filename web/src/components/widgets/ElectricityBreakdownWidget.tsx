import { formatCurrency, formatPct, formatWatts } from "../../lib/format";
import { useCostEnergyHistory, useCostSummary } from "../../lib/queries";
import { rangeLabel } from "../../lib/rangeLabels";
import type { CostEnergyHistoryPoint } from "../../lib/types";
import { TrendChart, type TrendSeriesDef } from "../charts/TrendChart";

// Single series only: cost_display = wall_wh_est × a constant rate within any
// one window, so plotting both normalized-to-own-max produces two perfectly
// overlapping curves — the second draws directly on top of the first and
// silently hides it (see feedback_dont_chart_correlated_series). The stat
// tiles above already show the exact cost figure; the trend line's job is
// just the energy shape over time.
const ENERGY_SERIES: TrendSeriesDef<CostEnergyHistoryPoint>[] = [
  { key: "wall", label: "Est. wall energy", get: (p) => p.wall_wh_est, color: "var(--heat-deep)", format: (v) => `${v.toFixed(1)} Wh` },
];

// Widget "electricity-breakdown" (see lib/widgetRegistry.ts). Relocated
// verbatim from Dashboard's now-deleted Trends tab — Phase 5 (2026-08-12).
// ADR-0012: rangeLabel is derived from window_ internally rather than passed
// as a separate prop.
export function ElectricityBreakdownWidget({ window_ }: { window_: string }) {
  const costSummary = useCostSummary(window_);
  const energyHistory = useCostEnergyHistory(window_);
  const energy = costSummary.data?.energy;

  return (
    <>
      <div className="eyebrow">Measured electricity · {rangeLabel(window_)}</div>
      <div className="card">
        {energy ? (
          <>
            <div className="stats" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))", marginBottom: 12 }}>
              <div className="stat">
                <div className="k">Cost</div>
                <div className="v">{formatCurrency(energy.cost_display, costSummary.data!.display_currency)}</div>
                <div className="d">{energy.rate_per_kwh} / kWh ({energy.rate_currency})</div>
              </div>
              <div className="stat">
                <div className="k">Package energy</div>
                <div className="v">{energy.package_wh.toFixed(0)} Wh</div>
                <div className="d">amdgpu PPT rail</div>
              </div>
              <div className="stat">
                <div className="k">Est. wall energy</div>
                <div className="v">{energy.wall_wh_est.toFixed(0)} Wh</div>
                <div className="d">+{energy.overhead_w}W overhead, ÷{energy.psu_efficiency} PSU</div>
              </div>
              <div className="stat">
                <div className="k">Coverage</div>
                <div className="v" style={{ color: energy.coverage_pct < 90 ? "var(--warn)" : undefined }}>{formatPct(energy.coverage_pct)}</div>
                <div className="d">{Math.round(energy.gap_seconds / 60)}m gaps · {Math.round(energy.unmeasured_seconds / 60)}m unmeasured</div>
              </div>
              <div className="stat">
                <div className="k">Idle baseline</div>
                <div className="v">{formatWatts(energy.idle_baseline_w)}</div>
                <div className="d">median, idle intervals</div>
              </div>
              <div className="stat">
                <div className="k">Attributable</div>
                <div className="v">{energy.attributable_wh_est.toFixed(0)} Wh</div>
                <div className="d">active energy above idle baseline</div>
              </div>
            </div>
            {energyHistory.data && energyHistory.data.points.length > 1 ? (
              <TrendChart<CostEnergyHistoryPoint> points={energyHistory.data.points} series={ENERGY_SERIES} ariaLabel="energy trend" />
            ) : (
              <div className="empty-note">
                Buckets are 24h wide — not enough history yet for a trend line at this range; try 72h/1w/1m.
              </div>
            )}
          </>
        ) : (
          <div className="empty-note">Loading energy summary…</div>
        )}
      </div>
    </>
  );
}
