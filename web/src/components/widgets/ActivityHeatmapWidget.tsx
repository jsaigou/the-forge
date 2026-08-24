import { useState } from "react";
import { useUsageHeatmap } from "../../lib/queries";
import { ActivityHeatmap, sequentialRamp } from "../charts/ActivityHeatmap";
import { RangeToggle } from "../RangeToggle";

// Local/External get their own one-hue ramp (operator-specified base hexes);
// All keeps ActivityHeatmap's default themed ramp (undefined → component
// default), left alone per the operator's explicit call.
const HEATMAP_SCOPES = [
  { key: "all", label: "All" },
  { key: "local", label: "Local" },
  { key: "external", label: "External" },
] as const;
type HeatmapScopeKey = (typeof HEATMAP_SCOPES)[number]["key"];

const HEATMAP_SCOPE_BASE_HEX: Partial<Record<HeatmapScopeKey, string>> = {
  local: "#3bf4fb",
  external: "#b100e8",
};

// Widget "activity-heatmap" (see lib/widgetRegistry.ts). Extracted verbatim
// from Dashboard's Overview tab, Phase 5 (2026-08-12).
export function ActivityHeatmapWidget() {
  const heatmap = useUsageHeatmap("84d");
  const [heatmapScope, setHeatmapScope] = useState<HeatmapScopeKey>("all");

  const heatmapDays = heatmap.data?.days.map((d) => ({
    date: d.date,
    tokens: heatmapScope === "local" ? d.tokens_local : heatmapScope === "external" ? d.tokens_external : d.tokens,
    requests: heatmapScope === "local" ? d.requests_local : heatmapScope === "external" ? d.requests_external : d.requests,
  }));
  const heatmapScopeHex = HEATMAP_SCOPE_BASE_HEX[heatmapScope];
  const heatmapColors = heatmapScopeHex ? sequentialRamp(heatmapScopeHex) : undefined;
  const heatmapScopeLabel = HEATMAP_SCOPES.find((sc) => sc.key === heatmapScope)!.label;

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <span>Token activity · last 12 weeks</span>
        <RangeToggle options={HEATMAP_SCOPES} value={heatmapScope} onChange={setHeatmapScope} />
      </div>
      <div className="card">
        {heatmap.isLoading ? (
          <div className="empty-note">Loading activity…</div>
        ) : heatmapDays && heatmapDays.length > 0 ? (
          <ActivityHeatmap days={heatmapDays} colors={heatmapColors} ariaLabel={`Token activity by day, last 12 weeks — ${heatmapScopeLabel}`} />
        ) : (
          <div className="empty-note">No activity data yet.</div>
        )}
      </div>
    </>
  );
}
