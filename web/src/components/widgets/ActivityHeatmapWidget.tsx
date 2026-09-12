import { useEffect, useState } from "react";
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

// Auto-cycle (operator feedback 2026-09-13): steps through All → Local →
// External → All on its own, TRUE cross-fading over FADE_MS — not a
// fade-out-then-fade-in. Two "slots" (index 0 and 1) each hold one scope's
// data; exactly one is active (opacity 1) at rest. Every CYCLE_MS
// (= PAUSE_MS + FADE_MS — the interval timer fires once per pause-then-fade
// cycle, not once per fade alone) the INACTIVE slot's data is swapped to
// the next scope (invisible at that instant, so the swap itself is never
// seen) and the two opacities flip simultaneously, so the outgoing scope's
// cells and the incoming scope's cells visibly overlap and cross-fade for
// the whole FADE_MS, holding static for PAUSE_MS in between. A manual
// RangeToggle click overwrites the active slot's data directly (no opacity
// change, so no transition — an instant jump) and the cycle just continues
// forward from there.
const PAUSE_MS = 3000;
const FADE_MS = 3000;
const CYCLE_MS = PAUSE_MS + FADE_MS;

type CycleState = { active: 0 | 1; scopes: [HeatmapScopeKey, HeatmapScopeKey] };

function nextScopeAfter(scope: HeatmapScopeKey): HeatmapScopeKey {
  const idx = HEATMAP_SCOPES.findIndex((s) => s.key === scope);
  return HEATMAP_SCOPES[(idx + 1) % HEATMAP_SCOPES.length].key;
}

// Widget "activity-heatmap" (see lib/widgetRegistry.ts). Extracted verbatim
// from Dashboard's Overview tab, Phase 5 (2026-08-12).
export function ActivityHeatmapWidget() {
  const heatmap = useUsageHeatmap("365d");
  const [cycle, setCycle] = useState<CycleState>({ active: 0, scopes: ["all", "all"] });

  useEffect(() => {
    const intervalId = setInterval(() => {
      setCycle((prev) => {
        const nextActive: 0 | 1 = prev.active === 0 ? 1 : 0;
        const scopes: [HeatmapScopeKey, HeatmapScopeKey] = [...prev.scopes];
        scopes[nextActive] = nextScopeAfter(prev.scopes[prev.active]);
        return { active: nextActive, scopes };
      });
    }, CYCLE_MS);
    return () => clearInterval(intervalId);
  }, []);

  function handleManualChange(scope: HeatmapScopeKey) {
    setCycle((prev) => {
      const scopes: [HeatmapScopeKey, HeatmapScopeKey] = [...prev.scopes];
      scopes[prev.active] = scope;
      return { ...prev, scopes };
    });
  }

  function daysForScope(scope: HeatmapScopeKey) {
    return (heatmap.data?.days ?? []).map((d) => ({
      date: d.date,
      tokens: scope === "local" ? d.tokens_local : scope === "external" ? d.tokens_external : d.tokens,
      requests: scope === "local" ? d.requests_local : scope === "external" ? d.requests_external : d.requests,
    }));
  }
  function colorsForScope(scope: HeatmapScopeKey) {
    const hex = HEATMAP_SCOPE_BASE_HEX[scope];
    return hex ? sequentialRamp(hex) : undefined;
  }

  const activeScope = cycle.scopes[cycle.active];
  const activeScopeLabel = HEATMAP_SCOPES.find((sc) => sc.key === activeScope)!.label;
  const hasData = (heatmap.data?.days.length ?? 0) > 0;
  const layers = [0, 1].map((i) => {
    const scope = cycle.scopes[i as 0 | 1];
    return {
      days: daysForScope(scope),
      colors: colorsForScope(scope),
      opacity: cycle.active === i ? 1 : 0,
    };
  });

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <span>Token activity · last year</span>
        <RangeToggle options={HEATMAP_SCOPES} value={activeScope} onChange={handleManualChange} />
      </div>
      <div className="card heatmap-card">
        {heatmap.isLoading ? (
          <div className="empty-note">Loading activity…</div>
        ) : hasData ? (
          <ActivityHeatmap layers={layers} stretch fadeMs={FADE_MS} ariaLabel={`Token activity by day, last year — ${activeScopeLabel}`} />
        ) : (
          <div className="empty-note">No activity data yet.</div>
        )}
      </div>
    </>
  );
}
