// Concentric multi-ring resource gauge (Sprint L), replacing the 3 separate
// ResourceDial donuts. All rings share the same angular sweep (0-100% maps
// to the same start/end angle on every radius) — arc *length* grows with
// radius, so equal values at different radii would draw unequal-looking arcs
// if each ring re-normalized independently; a shared sweep keeps angle the
// comparable channel. Rings can't each carry an interior label at this size,
// so identity comes from the legend list beside the cluster, not ring
// position alone.
//
// Pre-release feedback round, 2026-08-06: dropped to 3 rings (VRAM/GPU/Temp
// — Storage/CPU/RAM removed, out of scope for a GPU-inference dashboard),
// each ring now gets a fixed identity color from the app's categorical
// palette instead of a status-threshold color (green/yellow/red) — an
// explicit simplification, not a semantic-color mismatch: there's no more
// "this ring means danger" meaning to preserve. Start angle moved from top
// (12 o'clock) to left (9 o'clock), and stroke width doubled.
//
// Same-day follow-up: reordered to Temp (innermost) / VRAM (middle) / GPU
// (outermost), and stroke width increased another 50% (8px -> 12px). The
// app's 5-color categorical palette (FFBE0B/FB5607/FF006E/8338EC/3A86FF) has
// no green in it, and Temp was asked to read as green — rather than force a
// mismatched hue onto it, one new color (#0AFF85) was added, computed to sit
// at the same saturation/lightness class as the other five so it reads as
// part of the same family rather than a bolted-on outlier.
//
// Pre-release feedback round, 2026-08-12: whole cluster +20% (radius/
// stroke/padding) and legend text +25%, for legibility at the Console's
// normal viewing distance. Also **reverses** the "Temp reads fixed-green"
// decision immediately above: unlike VRAM/GPU (identity-colored, no
// threshold meaning — there's nothing dangerous about high VRAM use),
// temperature inherently carries a magnitude the operator wants visible at
// a glance. Temp is now rendered as a fully solid disc (no more partial-arc
// fill scaled against TEMP_MAX_C) whose *color* interpolates
// green -> fuchsia (#FF006E) with the live reading instead — fill-length
// and color have swapped which one encodes the value. The green endpoint
// is re-picked from #0AFF85 to ~#00CC76 (see TEMP_COOL_HSL below) for light-mode
// legibility; the ramp concept and hot endpoint are unchanged.

export interface RingMetric {
  key: string;
  label: string;
  // Raw value: a 0-100 percentage for every ring except temp (°C).
  value: number | null;
  // 0-100 fill fraction for the arc. Unused (see solidDisc) for temp, which
  // encodes its magnitude via `color` instead — see tempRampColor.
  fillPct: number | null;
  color: string;
  format: (v: number) => string;
  // True for the temp ring only: renders as one filled circle covering the
  // whole inner area (no hollow center), not a stroked ring — see
  // file-header comment for why temp reads solid while VRAM/GPU stay
  // stroked arcs.
  solidDisc?: boolean;
}

// +20% (2026-08-12) over the prior R0=14/STROKE=12/SIZE_PAD=6.
const SCALE = 1.2;
const STROKE = 12 * SCALE; // base stroke width
const TEMP_DISC_R = 10 * SCALE; // temp solid disc radius (25% smaller than original)
const VRAM_STROKE = STROKE * 0.75; // VRAM ring stroke (narrower than GPU)
const GPU_STROKE = STROKE * 1.25; // GPU ring stroke (25% thicker)
const GAP = 2 * SCALE; // constant visual gap between ring edges
const SIZE_PAD = 6 * SCALE;

// Legend text +25% (2026-08-12) over the prior 11px/8px/46px.
const LABEL_FONT_SIZE = 11 * 1.25;
const LEGEND_DOT_SIZE = 8 * 1.25;
const LABEL_WIDTH = 46 * 1.25;

// Fixed identity colors for VRAM/GPU (innermost → outermost order below adds
// Temp, whose color is computed per-reading — see tempRampColor). From the
// 5-color categorical palette used across this feedback round.
// VRAM identity color is theme-aware (steel blue #6FA3C8 dark / #33658A
// light) via the --ring-vram token defined per-theme in theme.css. The
// operator's mint palette was tried first but collided with the temp ramp's
// cool endpoint (both ~h155 green); the previously-rejected saturated blue
// #3A86FF likewise read poorly, so a muted steel blue sits between.
const RING_COLORS = { vram: "var(--ring-vram)", gpu: "#FB5607" };

// Board auto-shuts-down near 90°C on this hardware and never reaches 100 —
// operator-confirmed. 90 is the honest full-scale ceiling the temp ring's
// color ramp scales against (see tempRampColor — fill is always solid now,
// color carries the magnitude instead).
const TEMP_MAX_C = 90;

// Temp ring color ramp endpoints: cool green -> hot fuchsia, validated via
// the dataviz skill's validate_palette.js against both theme panels
// (dark #121d21, light #fbfcfc). The original app-palette green (#0AFF85)
// read at only 1.31:1 contrast on the light panel — nearly invisible — so
// the cool end is darkened to ~#00CC76 (h155/s100/l40 below). The light-
// mode contrast still WARNs at the coolest readings; that's covered by the
// ring's own always-visible numeric label (rendered in text tokens, never
// the series color) — the validator's required mitigation for a contrast
// WARN. Hot end (#FF006E, h334/s100/l50) is unchanged, already in the
// app's categorical palette.
//
// Interpolated in HSL, not RGB: a plain per-channel RGB blend between two
// near-complementary hues (green and fuchsia sit ~180° apart) passes
// through a desaturated gray trough at the midpoint — confirmed live, the
// ring at a real 34°C reading rendered as a muddy olive-gray, not a
// legible color. HSL keeps saturation/lightness essentially constant and
// only rotates hue, so the ramp instead travels the *warm* path — green ->
// chartreuse -> yellow -> orange -> red -> magenta — which both stays
// vivid throughout and reads as a more intuitive cool->hot progression
// than the RGB version's incidental detour through blue-violet. Hue
// interpolation is hand-coded to decrease through 0° (not a generic
// shortest-path lerp) specifically to force that warm route, since the
// two endpoints are close enough to 180° apart that "shortest path" is
// ambiguous and could as easily pick the cold direction.
const TEMP_COOL_HSL = { h: 155, l: 40 };
const TEMP_HOT_HSL = { h: 334 - 360, l: 50 }; // -26, continues decreasing past 0

function tempRampColor(tempC: number): string {
  const t = Math.max(0, Math.min(1, tempC / TEMP_MAX_C));
  const h = ((TEMP_COOL_HSL.h + (TEMP_HOT_HSL.h - TEMP_COOL_HSL.h) * t) % 360 + 360) % 360;
  const l = TEMP_COOL_HSL.l + (TEMP_HOT_HSL.l - TEMP_COOL_HSL.l) * t;
  return `hsl(${h}, 100%, ${l}%)`;
}

export function RingCluster({
  vramPct,
  gpuPct,
  tempC,
}: {
  vramPct: number | null;
  gpuPct: number | null;
  tempC: number | null;
}) {
  // Innermost → outermost.
  const rings: RingMetric[] = [
    {
      key: "temp",
      label: "Temp",
      value: tempC,
      fillPct: null,
      color: tempC != null ? tempRampColor(tempC) : "var(--border)",
      format: (v) => `${Math.round(v)}°C`,
      solidDisc: true,
    },
    { key: "vram", label: "VRAM", value: vramPct, fillPct: vramPct, color: RING_COLORS.vram, format: (v) => `${Math.round(v)}%` },
    { key: "gpu", label: "GPU", value: gpuPct, fillPct: gpuPct, color: RING_COLORS.gpu, format: (v) => `${Math.round(v)}%` },
  ];

  // Compute radii so the visual gap between ring edges is constant (GAP),
  // regardless of individual stroke widths. Each ring's inner edge sits
  // GAP pixels outside the previous ring's outer edge.
  const vramR = TEMP_DISC_R + GAP + VRAM_STROKE / 2;
  const gpuR = vramR + VRAM_STROKE / 2 + GAP + GPU_STROKE / 2;
  const ringR = [0, vramR, gpuR]; // temp uses disc radius, not ring radius
  const ringStroke = [0, VRAM_STROKE, GPU_STROKE];

  const outerR = gpuR;
  const size = (outerR + GPU_STROKE / 2 + SIZE_PAD) * 2;
  const c = size / 2;

  return (
    <div style={{ display: "flex", alignItems: "center", gap: 14, flexWrap: "wrap" }}>
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} style={{ display: "block", flex: "0 0 auto" }} role="img" aria-label="System resource usage">
        {rings.map((ring, i) => {
          const r = ringR[i];

          if (ring.solidDisc) {
            // Filled circle — temp disc, 25% smaller than the original ring radius.
            return (
              <circle key={ring.key} cx={c} cy={c} r={TEMP_DISC_R} fill={ring.color} opacity={ring.value == null ? 0.5 : 1}>
                <title>{`${ring.label}: ${ring.value != null ? ring.format(ring.value) : "unknown"}`}</title>
              </circle>
            );
          }

          const sw = ringStroke[i];
          const circumference = 2 * Math.PI * r;
          const pct = ring.fillPct != null ? Math.max(0, Math.min(100, ring.fillPct)) : 0;
          const dash = (pct / 100) * circumference;
          return (
            <g key={ring.key}>
              <circle cx={c} cy={c} r={r} fill="none" stroke="var(--border)" strokeWidth={sw} opacity={ring.value == null ? 0.5 : 1} />
              {ring.value != null && (
                <circle
                  cx={c}
                  cy={c}
                  r={r}
                  fill="none"
                  stroke={ring.color}
                  strokeWidth={sw}
                  strokeLinecap="round"
                  strokeDasharray={`${dash} ${circumference - dash}`}
                  transform={`rotate(-180 ${c} ${c})`}
                >
                  <title>{`${ring.label}: ${ring.format(ring.value)}`}</title>
                </circle>
              )}
            </g>
          );
        })}
      </svg>
      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        {rings.map((ring) => (
          <div key={ring.key} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: LABEL_FONT_SIZE }}>
            <span
              style={{
                width: LEGEND_DOT_SIZE,
                height: LEGEND_DOT_SIZE,
                borderRadius: "50%",
                background: ring.value != null ? ring.color : "var(--border)",
                flex: "0 0 auto",
              }}
            />
            <span style={{ color: "var(--text-dim)", width: LABEL_WIDTH }}>{ring.label}</span>
            <span style={{ fontFamily: "var(--mono)", color: "var(--text)" }}>{ring.value != null ? ring.format(ring.value) : "—"}</span>
          </div>
        ))}
      </div>
    </div>
  );
}
