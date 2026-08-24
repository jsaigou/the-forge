import { formatCurrency, formatWatts } from "../../lib/format";
import { useCostSettings, useCostSummary, useMetrics } from "../../lib/queries";
import { CompressorSavingsChips } from "../CompressionSavingsChips";

// Widget "right-now" (see lib/widgetRegistry.ts). Power consumption estimate,
// electricity cost, and Compressor savings — the operator's most-checked
// figures, hence Overview rather than buried in the Cost tab.
//
// ADR-0012: window_ is now a prop (default "24h") instead of hardcoded —
// previously the whole row was fixed at 24h because Overview had no range
// toggle of its own. On custom pages the window is configurable.
export function RightNowWidget({ window_ = "24h" }: { window_?: string }) {
  const metrics = useMetrics();
  const costSettings = useCostSettings();
  const costSummary = useCostSummary(window_);

  const wallWattsNow =
    metrics.data?.package_power_w != null && costSettings.data
      ? (metrics.data.package_power_w + costSettings.data.overhead_w) / costSettings.data.psu_efficiency
      : null;

  const energy = costSummary.data?.energy;
  const displayCurrency = costSummary.data?.display_currency ?? "USD";

  return (
    <>
      <div className="eyebrow">Right now</div>
      <div
        className="stats stats-hero"
        style={{ gridTemplateColumns: "repeat(auto-fit, minmax(180px, 220px))", justifyContent: "center" }}
      >
        <div className="stat">
          <div className="k">Power</div>
          <div className="v cost">{wallWattsNow != null ? formatWatts(wallWattsNow) : ""}</div>
        </div>
        <div className="stat">
          <div className="k">Electricity (24h)</div>
          <div className="v cost">{energy ? formatCurrency(energy.cost_display, displayCurrency) : ""}</div>
        </div>
        <CompressorSavingsChips window_={window_} />
      </div>
    </>
  );
}
