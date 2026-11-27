// Pure helpers for the Dashboard "Concurrency" card (a0 request concurrency). No React and no i18n import, so they run
// under `npm test` (node --test). The locale is passed in by the caller.
import type { A0InFlight, ConcurrencyResponse } from "./types";

export type ConcurrencyWindow = "1h" | "24h" | "7d" | "31d";
export const CONCURRENCY_WINDOWS: readonly ConcurrencyWindow[] = ["1h", "24h", "7d", "31d"];

const WINDOW_SECONDS: Record<ConcurrencyWindow, number> = {
  "1h": 3600,
  "24h": 86_400,
  "7d": 7 * 86_400,
  "31d": 31 * 86_400,
};

// The backend caps a span at 31 days and a request is in the window when it STARTED in it.
export function concurrencyRange(win: ConcurrencyWindow, nowSec: number): { since: number; until: number } {
  const until = Math.floor(nowSec);
  return { since: until - WINDOW_SECONDS[win], until };
}

// Models seen in the window: local models, external-provider models and anything a0 is serving right now. Sorted,
// de-duplicated, blanks dropped.
export function modelsSeen(
  local: readonly { model: string }[] | undefined,
  external: readonly { model: string }[] | undefined,
  live: A0InFlight | undefined,
): string[] {
  const set = new Set<string>();
  for (const r of local ?? []) if (r.model) set.add(r.model);
  for (const r of external ?? []) if (r.model) set.add(r.model);
  for (const r of live?.rows ?? []) if (r.model) set.add(r.model);
  return [...set].sort((a, b) => a.localeCompare(b));
}

// Live gauge for one model: the model-total row (empty consumer). Null when a0 has not seen the model recently.
export function liveFor(live: A0InFlight | undefined, model: string): { now: number; peak: number } | null {
  const row = live?.rows.find((r) => r.model === model && !r.consumer);
  return row ? { now: row.in_flight, peak: row.peak } : null;
}

export interface OutcomeCounts {
  ok: number;
  error: number;
  aborted: number;
}

// Missing outcome keys count as 0; unknown outcomes are ignored (the card labels the three the contract names).
export function outcomeCounts(o: ConcurrencyResponse["outcomes"] | undefined): OutcomeCounts {
  return { ok: o?.ok ?? 0, error: o?.error ?? 0, aborted: o?.aborted ?? 0 };
}

// 0..1 fraction -> "12%" / "0.4%" (one decimal under 10%, so a small but real overlap is not shown as 0%).
export function formatFraction(f: number, locale?: string): string {
  const pct = Math.max(0, Math.min(1, Number.isFinite(f) ? f : 0)) * 100;
  const digits = pct > 0 && pct < 10 ? 1 : 0;
  return `${new Intl.NumberFormat(locale, { minimumFractionDigits: digits, maximumFractionDigits: digits }).format(pct)}%`;
}

// Busy time as seconds, minutes or hours; the unit labels come from the caller, which owns the translation.
export function formatBusy(
  seconds: number,
  units: { s: string; min: string; h: string },
  locale?: string,
): { value: string; unit: string } {
  const s = Number.isFinite(seconds) && seconds > 0 ? seconds : 0;
  const nf = (n: number, d: number) => new Intl.NumberFormat(locale, { maximumFractionDigits: d }).format(n);
  if (s < 60) return { value: nf(s, s < 10 ? 1 : 0), unit: units.s };
  if (s < 3600) return { value: nf(s / 60, 1), unit: units.min };
  return { value: nf(s / 3600, 1), unit: units.h };
}
