// Shared window→label map for widgets that accept a window_ prop.
// ADR-0012: rangeLabel is derived from window_, not passed as a separate
// prop. Each widget's configSchema declares which windows it supports; this
// map gives the "dashboard" translation namespace key (ranges.*, same
// catalog Dashboard.tsx's own range toggles use) for any given window string.

const RANGE_LABEL_KEYS: Record<string, string> = {
  "24h": "ranges.24h",
  "72h": "ranges.72h",
  "7d": "ranges.1w",
  "30d": "ranges.1m",
  "180d": "ranges.6mo",
  "365d": "ranges.1y",
  "3650d": "ranges.all",
};

// t must be a "dashboard"-namespaced translate function (useTranslation("dashboard")).
export function rangeLabel(window_: string, t: (key: string) => string): string {
  const key = RANGE_LABEL_KEYS[window_];
  return key ? t(key) : window_;
}
