export function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(Math.round(n));
}

// formatGB renders a byte count as GB (GiB) for display. A1 bytes retrofit:
// input is bytes (was MB); divide by 1024³. Returns "—" for null/undefined.
export function formatGB(bytes: number | null | undefined, digits = 0): string {
  if (bytes == null) return "—";
  return (bytes / (1024 * 1024 * 1024)).toFixed(digits);
}

export function formatUsd(n: number | null | undefined): string {
  if (n == null) return "—";
  return `$${n.toFixed(2)}`;
}

// Sprint 0 §0.2 — display-currency formatting. All money on the wire is now
// *_display in UsageResponse.display_currency; formatCurrency renders a value
// in that ISO 4217 code via Intl.NumberFormat (currency-aware symbol + scale).
// Falls back to a plain 2-decimal number when the currency code is empty/unknown
// so a missing display currency never throws.
const _ccyCache = new Map<string, Intl.NumberFormat>();

function ccyFormatter(currency: string): Intl.NumberFormat {
  let f = _ccyCache.get(currency);
  if (f) return f;
  try {
    f = new Intl.NumberFormat(undefined, {
      style: "currency",
      currency,
      maximumFractionDigits: 2,
    });
  } catch {
    // Invalid / empty ISO code — fall back to a neutral 2-decimal render.
    f = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 });
  }
  _ccyCache.set(currency, f);
  return f;
}

export function formatCurrency(n: number | null | undefined, currency: string): string {
  if (n == null) return "—";
  return ccyFormatter(currency).format(n);
}

// formatCurrencyPrecise is formatCurrency for figures that can be genuinely
// tiny (Compressor savings chips, 2026-07-31: a real 0.000133 USD compression
// saving rounded to a misleading "$0.00" at the usual 2 fraction digits, and
// converting to a 0-decimal currency like JPY made it worse, not better).
// Widens fraction digits only when the value would otherwise round to zero —
// everyday amounts still render exactly like formatCurrency.
const _ccyPreciseCache = new Map<string, Intl.NumberFormat>();

function ccyPreciseFormatter(currency: string, digits: number): Intl.NumberFormat {
  const key = `${currency}:${digits}`;
  let f = _ccyPreciseCache.get(key);
  if (f) return f;
  try {
    f = new Intl.NumberFormat(undefined, {
      style: "currency",
      currency,
      minimumFractionDigits: digits,
      maximumFractionDigits: digits,
    });
  } catch {
    f = new Intl.NumberFormat(undefined, { minimumFractionDigits: digits, maximumFractionDigits: digits });
  }
  _ccyPreciseCache.set(key, f);
  return f;
}

export function formatCurrencyPrecise(n: number | null | undefined, currency: string): string {
  if (n == null) return "—";
  if (n === 0) return formatCurrency(0, currency);
  const abs = Math.abs(n);
  let digits = 2;
  while (digits < 8 && Math.round(abs * 10 ** digits) === 0) digits++;
  return ccyPreciseFormatter(currency, digits).format(n);
}

// Compact "38s" / "6m" / "2.1h" duration, for the Compressor local-savings chip
// (time saved this window). formatUptime/formatIdle above serve different
// shapes (device uptime, a live countdown) — this one is deliberately terse.
export function formatDurationShort(seconds: number | null | undefined): string {
  if (seconds == null) return "—";
  const abs = Math.abs(seconds);
  if (abs < 60) return `${Math.round(seconds)}s`;
  if (abs < 3600) return `${Math.round(seconds / 60)}m`;
  return `${(seconds / 3600).toFixed(1)}h`;
}

// Compact money for strip/card totals (e.g. $1.2k, $3.4M). Stays in the
// display currency's symbol; useful where a full-precision figure is noise.
const _ccyCompactCache = new Map<string, Intl.NumberFormat>();

function ccyCompactFormatter(currency: string): Intl.NumberFormat {
  let f = _ccyCompactCache.get(currency);
  if (f) return f;
  try {
    f = new Intl.NumberFormat(undefined, { style: "currency", currency, maximumFractionDigits: 1 });
  } catch {
    f = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
  }
  _ccyCompactCache.set(currency, f);
  return f;
}

export function formatCurrencyCompact(n: number | null | undefined, currency: string): string {
  if (n == null) return "—";
  const abs = Math.abs(n);
  const f = ccyCompactFormatter(currency);
  if (abs >= 1_000_000) return f.format(n / 1_000_000).replace(/\.0$/, "") + "M";
  if (abs >= 1_000) return f.format(n / 1_000).replace(/\.0$/, "") + "k";
  return formatCurrency(n, currency);
}

// A best-effort, non-exhaustive region→currency table for the "use my
// browser locale" convenience on the electricity-rate currency field
// (Dashboard cost/savings sprint, follow-up). Intl has no built-in
// region→currency lookup, so this is a small hand-maintained table covering
// common regions — falls back to null (caller keeps whatever's configured)
// rather than guessing for an unlisted region.
const REGION_CURRENCY: Record<string, string> = {
  US: "USD", JP: "JPY", GB: "GBP", DE: "EUR", FR: "EUR", IT: "EUR", ES: "EUR", NL: "EUR",
  BE: "EUR", PT: "EUR", IE: "EUR", AT: "EUR", FI: "EUR", GR: "EUR",
  CA: "CAD", AU: "AUD", NZ: "NZD", CN: "CNY", KR: "KRW", IN: "INR", BR: "BRL", MX: "MXN",
  RU: "RUB", CH: "CHF", SE: "SEK", NO: "NOK", DK: "DKK", PL: "PLN", SG: "SGD", HK: "HKD",
  TW: "TWD", TH: "THB", ID: "IDR", MY: "MYR", PH: "PHP", VN: "VND", ZA: "ZAR", TR: "TRY",
  AE: "AED", SA: "SAR", IL: "ILS",
};

export function localeCurrencyGuess(): string | null {
  try {
    const region = new Intl.Locale(navigator.language).maximize().region;
    return region ? (REGION_CURRENCY[region] ?? null) : null;
  } catch {
    return null;
  }
}

export function formatPct(n: number | null | undefined): string {
  if (n == null) return "—";
  return `${Math.round(n)}%`;
}

export function formatWatts(v: number | null | undefined): string {
  if (v == null) return "—";
  return `${v.toFixed(0)} W`;
}

// Phase 5 (2026-08-12): the Resources dashboard's Network gauge/trend series
// — /proc/net/dev byte-rate counters, diffed server-side (null on the first
// collector cycle or a counter reset, never a false 0 — see
// go/internal/collector/proc.go's NetDev).
export function formatBytesPerSec(bytesPerSec: number | null | undefined): string {
  if (bytesPerSec == null) return "—";
  const abs = Math.abs(bytesPerSec);
  if (abs >= 1024 * 1024) return `${(bytesPerSec / (1024 * 1024)).toFixed(1)} MB/s`;
  if (abs >= 1024) return `${(bytesPerSec / 1024).toFixed(1)} KB/s`;
  return `${bytesPerSec.toFixed(0)} B/s`;
}

export function formatUptime(seconds: number | null | undefined): string {
  if (seconds == null) return "—";
  const days = Math.floor(seconds / 86400);
  if (days >= 1) return `${days}d`;
  const hours = Math.floor(seconds / 3600);
  if (hours >= 1) return `${hours}h`;
  return `${Math.floor(seconds / 60)}m`;
}

export function formatIdle(seconds: number | null | undefined): string {
  if (seconds == null) return "—";
  const m = Math.floor(seconds / 60);
  const s = Math.floor(seconds % 60);
  return `${m}:${String(s).padStart(2, "0")}`;
}

export function formatClock(iso: string): string {
  const d = new Date(iso);
  return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

export function dayLabel(date: Date): { dn: string; dd: number } {
  return {
    dn: date.toLocaleDateString([], { weekday: "short" }),
    dd: date.getDate(),
  };
}

export function isSameDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

// formatRelativeTime renders a unix-seconds timestamp as "just now" / "Nm
// ago" / "Nh ago" / "Nd ago" (product/QA sprint, 2026-07-29 — Dashboard
// notifications panel).
export function formatRelativeTime(epochSec: number): string {
  const diffS = Math.max(0, Date.now() / 1000 - epochSec);
  if (diffS < 60) return "just now";
  if (diffS < 3600) return `${Math.floor(diffS / 60)}m ago`;
  if (diffS < 86400) return `${Math.floor(diffS / 3600)}h ago`;
  return `${Math.floor(diffS / 86400)}d ago`;
}

// countryFlag maps a 2-letter ISO country code to its emoji flag (regional
// indicator symbols). "" on anything else — callers render no chip then.
// Shared by RemoteOfferings + the Settings Providers panel (multi-provider
// sprint, 2026-08-06; moved here from RemoteOfferings' local copy).
export function countryFlag(countryCode: string): string {
  if (!countryCode || countryCode.length !== 2) return "";
  const codePoints = countryCode.toUpperCase().split("").map((c) => 0x1f1e6 + c.charCodeAt(0) - 65);
  return String.fromCodePoint(...codePoints);
}
