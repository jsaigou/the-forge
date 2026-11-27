// lib/duration.ts — parse/format Go duration strings for the settings
// "duration" input (number + unit select). Pure and erasable-TypeScript so
// scripts/duration.test.mjs can load it directly with node's runner.
//
// The store keeps Go duration strings ("60m", "6h", "1h30m"); the widget
// shows a number and a unit instead. A composite like "1h30m" is shown in the
// largest unit that divides it evenly ("90m"); anything the parser cannot
// represent (sub-second units, garbage) returns null so the caller can keep
// the raw text editable instead of silently rewriting stored data.

export type DurationUnit = "s" | "m" | "h";

export const DURATION_UNITS: DurationUnit[] = ["s", "m", "h"];

const UNIT_SECONDS: Record<DurationUnit, number> = { s: 1, m: 60, h: 3600 };

const PART = /(\d+(?:\.\d+)?)(h|m|s)/g;

/** Total seconds of a Go duration made only of h/m/s parts, else null. */
export function durationSeconds(raw: string): number | null {
  const s = raw.trim();
  if (!s) return null;
  if (s === "0") return 0;
  let total = 0;
  let consumed = 0;
  PART.lastIndex = 0;
  for (let m = PART.exec(s); m; m = PART.exec(s)) {
    if (m.index !== consumed) return null;
    consumed += m[0].length;
    total += Number(m[1]) * UNIT_SECONDS[m[2] as DurationUnit];
  }
  if (consumed !== s.length || !Number.isFinite(total)) return null;
  return total;
}

/** Split a Go duration into the number+unit the widget displays. */
export function parseDuration(raw: string): { n: number; unit: DurationUnit } | null {
  const secs = durationSeconds(raw);
  if (secs === null) return null;
  if (secs === 0) return { n: 0, unit: "s" };
  for (const unit of ["h", "m", "s"] as DurationUnit[]) {
    const n = secs / UNIT_SECONDS[unit];
    if (Number.isInteger(n)) return { n, unit };
  }
  // Fractional seconds (e.g. "1.5s"): keep them as seconds.
  return { n: secs, unit: "s" };
}

/** Compose the Go duration string the store expects. */
export function formatDuration(n: number, unit: DurationUnit): string {
  return `${n}${unit}`;
}
