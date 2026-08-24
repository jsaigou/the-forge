// Inline SVG multi-series trend line, extracted from Dashboard.tsx (Sprint 0
// §0.4) during the Dashboard cost/savings sprint (Phase 5) so it can be
// shared across tabs (Trends' memory/GPU/storage/power series today; Cost's
// energy-history sparkline). Each series is normalized to its own max over
// the window so differently-scaled units share one panel; absolute current
// values are in the legend. Callers server-downsample (§0.4), so `points` is
// already ~hundreds, not ~10k. No external charting dependency (CSP-strict
// PWA; the repo ships none) — see docs/dataviz conventions: one axis, color
// follows the entity, thin non-scaling-stroke lines.
export interface TrendSeriesDef<P> {
  key: string;
  label: string;
  get: (p: P) => number | null | undefined;
  color: string;
  format: (v: number) => string;
  // Secondary encoding for a hue that's otherwise close to a sibling series
  // in this app's fixed (non-categorical) accent palette — see Dashboard's
  // "power" series, which shares the warm/orange family with mem/gpu/disk.
  dashed?: boolean;
  // Dashboard follow-up round 2: a fixed ceiling to scale against instead of
  // the window's own observed max — e.g. Overview's power series scales
  // against a real hardware power limit, not whatever the highest reading
  // in the selected window happened to be (a tiny blip would otherwise look
  // like it maxed out the chart). Omit to keep the default per-series
  // auto-scaling behavior.
  max?: number;
  // Composed-chart mark type (pre-release feedback round, 2026-08-06):
  // false draws this series as a plain line with no fill, so two series in
  // different units sharing one panel (e.g. watts vs. a %) can look like
  // "one filled area + one line overlay" rather than two overlapping
  // fills. Omit/true keeps the original area+line-per-series behavior.
  area?: boolean;
}

const CHART_W = 800;
const CHART_H = 180;
const CHART_PAD = { top: 8, right: 12, bottom: 20, left: 12 };
const CHART_IW = CHART_W - CHART_PAD.left - CHART_PAD.right;
const CHART_IH = CHART_H - CHART_PAD.top - CHART_PAD.bottom;

function lastValue<P>(points: P[], get: TrendSeriesDef<P>["get"]): number | null {
  for (let i = points.length - 1; i >= 0; i--) {
    const v = get(points[i]);
    if (v != null) return v;
  }
  return null;
}

// Builds one array of {x,y} pixel points per contiguous non-null run, so a
// gap in the data breaks both the line and its fill instead of joining or
// filling across missing samples.
function buildSegments<P extends { ts: number }>(
  points: P[],
  get: TrendSeriesDef<P>["get"],
  max: number,
  tMin: number,
  tRange: number,
): { x: number; y: number }[][] {
  const segments: { x: number; y: number }[][] = [];
  let cur: { x: number; y: number }[] = [];
  for (const p of points) {
    const v = get(p);
    if (v == null) {
      if (cur.length > 0) segments.push(cur);
      cur = [];
      continue;
    }
    const x = CHART_PAD.left + ((p.ts - tMin) / tRange) * CHART_IW;
    const y = CHART_PAD.top + CHART_IH - (v / max) * CHART_IH;
    cur.push({ x, y });
  }
  if (cur.length > 0) segments.push(cur);
  return segments;
}

function linePath(segments: { x: number; y: number }[][]): string {
  return segments
    .map((seg) => seg.map((pt, i) => `${i === 0 ? "M" : "L"}${pt.x.toFixed(1)},${pt.y.toFixed(1)}`).join(" "))
    .join(" ");
}

// Closes each segment down to the chart baseline and back, so the fill only
// covers the area actually under a drawn line — never across a gap.
const BASE_Y = CHART_PAD.top + CHART_IH;
function areaPath(segments: { x: number; y: number }[][]): string {
  return segments
    .map((seg) => {
      if (seg.length === 0) return "";
      const line = seg.map((pt, i) => `${i === 0 ? "M" : "L"}${pt.x.toFixed(1)},${pt.y.toFixed(1)}`).join(" ");
      const last = seg[seg.length - 1];
      const first = seg[0];
      return `${line} L${last.x.toFixed(1)},${BASE_Y} L${first.x.toFixed(1)},${BASE_Y} Z`;
    })
    .join(" ");
}

export function TrendChart<P extends { ts: number }>({
  points,
  series,
  ariaLabel,
}: {
  points: P[];
  series: TrendSeriesDef<P>[];
  ariaLabel: string;
}) {
  const tMin = points[0].ts;
  const tMax = points[points.length - 1].ts;
  const tRange = tMax - tMin || 1;

  const startLabel = new Date(tMin * 1000).toLocaleDateString([], { month: "short", day: "numeric" });
  const midLabel = new Date(((tMin + tMax) / 2) * 1000).toLocaleDateString([], { month: "short", day: "numeric" });

  return (
    <>
      <div style={{ display: "flex", gap: 16, flexWrap: "wrap", marginBottom: 8 }}>
        {series.map((s) => {
          const v = lastValue(points, s.get);
          return (
            <div key={s.key} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12 }}>
              <span style={{ width: 10, height: 10, borderRadius: 2, background: s.color, flex: "0 0 auto" }} />
              <span style={{ color: "var(--text-dim)" }}>{s.label}</span>
              <span style={{ fontFamily: "var(--mono)", color: "var(--text)" }}>{v != null ? s.format(v) : "—"}</span>
            </div>
          );
        })}
      </div>
      <svg
        viewBox={`0 0 ${CHART_W} ${CHART_H}`}
        style={{ width: "100%", height: "auto", display: "block" }}
        role="img"
        aria-label={ariaLabel}
      >
        {[0, 0.5, 1].map((f) => (
          <line
            key={f}
            x1={CHART_PAD.left}
            x2={CHART_W - CHART_PAD.right}
            y1={CHART_PAD.top + CHART_IH * (1 - f)}
            y2={CHART_PAD.top + CHART_IH * (1 - f)}
            style={{ stroke: "var(--border)", strokeWidth: 1 }}
          />
        ))}
        {series.map((s) => {
          const vals = points.map((p) => s.get(p)).filter((v): v is number => v != null);
          if (vals.length === 0) return null;
          const max = s.max ?? Math.max(1, ...vals);
          const segments = buildSegments(points, s.get, max, tMin, tRange);
          return (
            <g key={s.key}>
              {/* Translucent fill under the line, opaque line on top — the
                  line is the data; the fill is just a legibility aid. */}
              {s.area !== false && <path d={areaPath(segments)} fill={s.color} fillOpacity={0.15} stroke="none" />}
              <path
                d={linePath(segments)}
                fill="none"
                vectorEffect="non-scaling-stroke"
                style={{ stroke: s.color, strokeWidth: 1.6, strokeDasharray: s.dashed ? "4 3" : undefined }}
              />
            </g>
          );
        })}
        <text x={CHART_PAD.left} y={CHART_H - 5} style={{ fill: "var(--text-mute)", fontSize: 10, fontFamily: "var(--mono)" }}>
          {startLabel}
        </text>
        <text x={CHART_W / 2} y={CHART_H - 5} textAnchor="middle" style={{ fill: "var(--text-mute)", fontSize: 10, fontFamily: "var(--mono)" }}>
          {midLabel}
        </text>
        <text x={CHART_W - CHART_PAD.right} y={CHART_H - 5} textAnchor="end" style={{ fill: "var(--text-mute)", fontSize: 10, fontFamily: "var(--mono)" }}>
          now
        </text>
      </svg>
    </>
  );
}
