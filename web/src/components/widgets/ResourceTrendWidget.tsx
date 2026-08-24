import { useState } from "react";
import { formatBytesPerSec, formatGB, formatPct, formatWatts } from "../../lib/format";
import { useMetricsExport, useMetricsHistory } from "../../lib/queries";
import { rangeLabel } from "../../lib/rangeLabels";
import type { MetricsHistoryPoint } from "../../lib/types";
import { TrendChart, type TrendSeriesDef } from "../charts/TrendChart";

// Widget "resource-trend" (see lib/widgetRegistry.ts). Relocated verbatim
// (plus the two new Phase 4 series, cpu/net) from Dashboard's now-deleted
// Trends tab — Phase 5 (2026-08-12). Takes `window_` from the page's range
// toggle; ADR-0012: rangeLabel is derived from window_ internally rather
// than passed as a separate prop.
const TREND_SERIES: TrendSeriesDef<MetricsHistoryPoint>[] = [
  { key: "mem", label: "Memory (GTT)", get: (p) => p.gtt_used_bytes, color: "var(--cool)", format: (v) => `${formatGB(v)} GB` },
  { key: "gpu", label: "GPU use", get: (p) => p.gpu_use_pct, color: "var(--heat)", format: (v) => formatPct(v) },
  { key: "cpu", label: "CPU use", get: (p) => p.cpu_pct, color: "var(--m2)", format: (v) => formatPct(v) },
  { key: "disk", label: "Storage", get: (p) => p.disk_used_bytes, color: "var(--heat-2)", format: (v) => `${formatGB(v)} GB` },
  { key: "power", label: "Package power", get: (p) => p.package_power_w, color: "var(--heat-deep)", format: formatWatts, dashed: true },
  { key: "net_rx", label: "Net ↓", get: (p) => p.net_rx_bytes_per_sec, color: "var(--m1)", format: (v) => formatBytesPerSec(v), dashed: true },
  { key: "net_tx", label: "Net ↑", get: (p) => p.net_tx_bytes_per_sec, color: "var(--m3)", format: (v) => formatBytesPerSec(v) },
];

export function ResourceTrendWidget({ window_ }: { window_: string }) {
  const [visibleSeries, setVisibleSeries] = useState<Set<string>>(new Set(["mem", "gpu"]));
  const history = useMetricsHistory(window_, ["gtt", "gpu", "disk", "power", "cpu", "network"], "auto");
  const exportMut = useMetricsExport();

  function toggleSeries(key: string) {
    setVisibleSeries((prev) => {
      const next = new Set(prev);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      return next;
    });
  }

  const activeSeries = TREND_SERIES.filter((s) => visibleSeries.has(s.key));

  return (
    <>
      <div className="eyebrow">{rangeLabel(window_)} trends · {activeSeries.map((s) => s.label).join(" / ") || "none shown"}</div>
      <div className="card">
        <div style={{ display: "flex", gap: 8, marginBottom: 8, flexWrap: "wrap" }}>
          {TREND_SERIES.map((s) => {
            const on = visibleSeries.has(s.key);
            return (
              <button
                key={s.key}
                className="chip"
                style={{ cursor: "pointer", opacity: on ? 1 : 0.45, borderColor: on ? s.color : undefined, color: on ? s.color : undefined }}
                onClick={() => toggleSeries(s.key)}
                title={on ? `Hide ${s.label}` : `Show ${s.label}`}
              >
                {s.label}
              </button>
            );
          })}
        </div>
        {history.isLoading ? (
          <div className="empty-note">Loading history…</div>
        ) : history.isError ? (
          <div className="empty-note">Metrics history unavailable.</div>
        ) : history.data && history.data.points.length > 0 ? (
          <TrendChart<MetricsHistoryPoint> points={history.data.points} series={activeSeries} ariaLabel="memory, GPU, CPU, storage, power, and network trend" />
        ) : (
          <div className="empty-note">No history recorded yet.</div>
        )}
        <div style={{ display: "flex", gap: 8, justifyContent: "flex-end", marginTop: 12, alignItems: "center", flexWrap: "wrap" }}>
          {exportMut.isError && <span className="error-note" style={{ padding: "4px 8px", fontSize: 11 }}>Export failed</span>}
          <button className="btn" disabled={exportMut.isPending} onClick={() => exportMut.mutate({ format: "csv", window_ })}>
            Export CSV
          </button>
          <button className="btn" disabled={exportMut.isPending} onClick={() => exportMut.mutate({ format: "json", window_ })}>
            Export JSON
          </button>
        </div>
      </div>
    </>
  );
}
