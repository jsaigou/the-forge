import { formatTokens } from "../../lib/format";
import type { UsageHeatmapDay } from "../../lib/types";

// Callers (the Dashboard's ALL/Local/External toggle) project down to just
// these three fields before passing in — the grid itself only ever reads
// date/tokens/requests, regardless of which scope produced them.
type HeatmapCell = Pick<UsageHeatmapDay, "date" | "tokens" | "requests">;

// GitHub-contribution-style token-activity grid (Sprint L). Inline SVG, no
// chart library (CSP-strict PWA ships none) — same hand-rolled house style
// as TrendChart.tsx. Cells are real calendar weeks (Sun→Sat rows), not raw
// 7-day chunks from the window start, so the grid reads the way a reader
// expects a contribution graph to read.

const CELL = 11;
const GAP = 3;
const STEP = CELL + GAP;
const LEFT_PAD = 28; // room for the Mon/Wed/Fri weekday labels
const TOP_PAD = 12; // room for month labels
// "All" scope's ramp — the app's existing themed --usage-1..4 sequential
// vars (theme.css), unchanged.
const DEFAULT_COLORS = ["var(--border)", "var(--usage-1)", "var(--usage-2)", "var(--usage-3)", "var(--usage-4)"];
const WEEKDAY_LABELS: Record<number, string> = { 1: "Mon", 3: "Wed", 5: "Fri" };
const MONTH_NAMES = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

// Builds a one-hue sequential ramp (dataviz skill: light→dark, one step per
// level) from a base hex the same way theme.css's --usage-1..4 are built —
// color-mix toward the card surface at 52/68/84/100%, so a scope-specific
// ramp (Local/External) tracks the active theme automatically instead of
// needing its own light/dark CSS vars.
export function sequentialRamp(hex: string): string[] {
  return [
    "var(--border)",
    `color-mix(in srgb, ${hex} 52%, var(--panel))`,
    `color-mix(in srgb, ${hex} 68%, var(--panel))`,
    `color-mix(in srgb, ${hex} 84%, var(--panel))`,
    hex,
  ];
}

// Parsed as a UTC date-only value — `date` is already the correct calendar
// day in the window's requested tz; re-parsing in the browser's local tz
// would risk shifting it a day in either direction.
function parseDay(date: string): Date {
  const [y, m, d] = date.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d));
}

function levelFor(tokens: number, max: number): number {
  if (tokens <= 0 || max <= 0) return 0;
  const ratio = tokens / max;
  if (ratio > 0.75) return 4;
  if (ratio > 0.5) return 3;
  if (ratio > 0.25) return 2;
  return 1;
}

// One cross-fadeable data source for the grid — ActivityHeatmapWidget's
// auto-cycle passes two of these (the outgoing scope at opacity 1→0, the
// incoming scope at opacity 0→1) so the swap is a true crossfade, not a
// fade-to-empty-then-fade-in. A caller not cross-fading just passes one.
export interface HeatmapLayer {
  days: HeatmapCell[];
  colors?: string[];
  opacity: number;
}

export function ActivityHeatmap({
  layers,
  ariaLabel = "Token activity by day",
  stretch = false,
  fadeMs = 250,
}: {
  // Grid geometry (dates, columns, month labels) is derived from
  // layers[0].days — every layer is expected to share the same date range
  // (true for the widget's All/Local/External scopes, which all come from
  // the same window, just different per-day fields), only cell colors
  // differ per layer.
  layers: HeatmapLayer[];
  ariaLabel?: string;
  // stretch (operator feedback 2026-09-13): the grid's natural pixel width
  // (LEFT_PAD + columns*STEP) is normally just a ceiling — maxWidth:100%
  // only ever shrinks it to fit a narrower card, never grows it to fill a
  // wider one, so a card sized for the widget's own section reads as mostly
  // empty. width:100% instead always fills the container; height:auto plus
  // the SVG's own viewBox (no preserveAspectRatio override) keeps every
  // cell square and proportional either way — this only changes whether the
  // grid is allowed to scale UP, not how cells are laid out.
  stretch?: boolean;
  // fadeMs: how long each layer's own opacity transition takes. With two
  // layers cross-fading simultaneously (one 1→0, one 0→1 at the same time)
  // this is the whole transition's duration, not half of it.
  fadeMs?: number;
}) {
  const referenceDays = layers[0]?.days ?? [];
  if (referenceDays.length === 0) {
    return <div className="empty-note">No activity data yet.</div>;
  }

  const firstDate = parseDay(referenceDays[0].date);
  const firstSunday = new Date(firstDate);
  firstSunday.setUTCDate(firstSunday.getUTCDate() - firstSunday.getUTCDay());

  type Cell = { x: number; y: number; day: HeatmapCell; date: Date };
  function buildCells(days: HeatmapCell[]): Cell[] {
    return days.map((day) => {
      const date = parseDay(day.date);
      const weekday = date.getUTCDay();
      const col = Math.round((date.getTime() - firstSunday.getTime()) / (7 * 86400000));
      return { x: LEFT_PAD + col * STEP, y: TOP_PAD + weekday * STEP, day, date };
    });
  }

  // Month labels + overall size come from the reference layer only — every
  // layer shares the same dates, so this never needs to be computed twice.
  let maxCol = 0;
  let lastMonthLabeled = -1;
  const monthLabels: { x: number; text: string }[] = [];
  for (const day of referenceDays) {
    const date = parseDay(day.date);
    const weekday = date.getUTCDay();
    const col = Math.round((date.getTime() - firstSunday.getTime()) / (7 * 86400000));
    maxCol = Math.max(maxCol, col);
    if (weekday === 0 && date.getUTCMonth() !== lastMonthLabeled) {
      lastMonthLabeled = date.getUTCMonth();
      monthLabels.push({ x: LEFT_PAD + col * STEP, text: MONTH_NAMES[date.getUTCMonth()] });
    }
  }

  const width = LEFT_PAD + (maxCol + 1) * STEP;
  const height = TOP_PAD + 7 * STEP + 2;

  return (
    <>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        width={width}
        height={height}
        style={stretch ? { width: "100%", height: "auto", display: "block" } : { maxWidth: "100%", height: "auto", display: "block" }}
        role="img"
        aria-label={ariaLabel}
      >
        {monthLabels.map((m) => (
          <text key={m.x} x={m.x} y={TOP_PAD - 4} style={{ fill: "var(--text-mute)", fontSize: 9, fontFamily: "var(--mono)" }}>
            {m.text}
          </text>
        ))}
        {Object.entries(WEEKDAY_LABELS).map(([row, label]) => (
          <text
            key={row}
            x={LEFT_PAD - 6}
            y={TOP_PAD + Number(row) * STEP + CELL - 2}
            textAnchor="end"
            style={{ fill: "var(--text-mute)", fontSize: 9, fontFamily: "var(--mono)" }}
          >
            {label}
          </text>
        ))}
        {layers.map((layer, i) => {
          const cells = buildCells(layer.days);
          const colors = layer.colors ?? DEFAULT_COLORS;
          const maxTokens = Math.max(0, ...layer.days.map((d) => d.tokens));
          return (
            <g key={i} style={{ opacity: layer.opacity, transition: `opacity ${fadeMs}ms ease` }}>
              {cells.map((c) => {
                const level = levelFor(c.day.tokens, maxTokens);
                const dateLabel = c.date.toLocaleDateString([], { month: "short", day: "numeric", year: "numeric", timeZone: "UTC" });
                return (
                  <rect key={c.day.date} x={c.x} y={c.y} width={CELL} height={CELL} rx={2} fill={colors[level]}>
                    <title>
                      {dateLabel}: {formatTokens(c.day.tokens)} tokens, {c.day.requests} request{c.day.requests === 1 ? "" : "s"}
                    </title>
                  </rect>
                );
              })}
            </g>
          );
        })}
      </svg>
    </>
  );
}
