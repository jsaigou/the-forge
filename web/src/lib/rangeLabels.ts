// Shared window→label map for widgets that accept a window_ prop.
// ADR-0012: rangeLabel is derived from window_, not passed as a separate
// prop. Each widget's configSchema declares which windows it supports;
// this map provides the human-readable label for any given window string.

export const RANGE_LABELS: Record<string, string> = {
  "24h": "24h",
  "72h": "72h",
  "7d": "1w",
  "30d": "1m",
  "180d": "6mo",
  "365d": "1y",
  "3650d": "All Time",
};

export function rangeLabel(window_: string): string {
  return RANGE_LABELS[window_] ?? window_;
}
