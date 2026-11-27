// lib/peakWindows.ts — pure helpers behind the provider peak-windows builder
// (Settings → Providers). The stored value stays the backend's JSON
// ({"tz":..., "windows":[{"days":[1,2],"start":"01:00","end":"04:00"}]});
// the builder edits `windows` as rows and these helpers convert both ways.
// Erasable TypeScript so scripts/peakwindows.test.mjs can load it directly.

export type PeakWindowRow = { days: number[]; start: string; end: string };

const HHMM = /^([01]\d|2[0-3]):[0-5]\d$/;

/** Parse the windows JSON into builder rows. Returns null when the text is
    not representable as rows (bad JSON, not an array, a row of the wrong
    shape, a day outside 0–6, a time not HH:MM) — the caller then keeps the
    raw editor open so nothing is silently rewritten. Empty text = no rows. */
export function parseWindowRows(json: string): PeakWindowRow[] | null {
  if (json.trim() === "") return [];
  let v: unknown;
  try {
    v = JSON.parse(json);
  } catch {
    return null;
  }
  if (!Array.isArray(v)) return null;
  const rows: PeakWindowRow[] = [];
  for (const w of v) {
    if (typeof w !== "object" || w === null) return null;
    const { days, start, end } = w as Record<string, unknown>;
    if (!Array.isArray(days) || !days.every((d) => Number.isInteger(d) && d >= 0 && d <= 6)) return null;
    if (typeof start !== "string" || typeof end !== "string" || !HHMM.test(start) || !HHMM.test(end)) return null;
    rows.push({ days: [...(days as number[])].sort((a, b) => a - b), start, end });
  }
  return rows;
}

/** Rows back to the windows JSON text ("" when there are none, matching the
    box being left empty = no peak concept). */
export function serializeWindowRows(rows: PeakWindowRow[]): string {
  return rows.length === 0 ? "" : JSON.stringify(rows);
}

export function toggleDay(days: number[], day: number): number[] {
  return days.includes(day) ? days.filter((d) => d !== day) : [...days, day].sort((a, b) => a - b);
}

/** A sensible starting row for "Add window": weekday working hours. */
export function newWindowRow(): PeakWindowRow {
  return { days: [1, 2, 3, 4, 5], start: "09:00", end: "17:00" };
}
