import { useState } from "react";
import { useTranslation } from "react-i18next";
import { formatBytesPerSec, formatGB, formatPct, formatWatts } from "../../lib/format";
import { useMetricsExport, useMetricsHistory } from "../../lib/queries";
import { rangeLabel } from "../../lib/rangeLabels";
import type { MetricsHistoryPoint } from "../../lib/types";
import { TrendChart, type TrendSeriesDef } from "../charts/TrendChart";

export function ResourceTrendWidget({ window_ }: { window_: string }) {
  const { t } = useTranslation("dashboard");
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

  // Widget "resource-trend" (see lib/widgetRegistry.ts). Relocated verbatim
  // (plus the two new Phase 4 series, cpu/net) from Dashboard's now-deleted
  // Trends tab — Phase 5 (2026-08-12). Takes `window_` from the page's range
  // toggle; ADR-0012: rangeLabel is derived from window_ internally rather
  // than passed as a separate prop.
  const trendSeries: TrendSeriesDef<MetricsHistoryPoint>[] = [
    { key: "mem", label: t("resource_trend.series_mem"), get: (p) => p.gtt_used_bytes, color: "var(--cool)", format: (v) => `${formatGB(v)} GB` },
    { key: "gpu", label: t("resource_trend.series_gpu"), get: (p) => p.gpu_use_pct, color: "var(--heat)", format: (v) => formatPct(v) },
    { key: "cpu", label: t("resource_trend.series_cpu"), get: (p) => p.cpu_pct, color: "var(--m2)", format: (v) => formatPct(v) },
    { key: "disk", label: t("resource_trend.series_disk"), get: (p) => p.disk_used_bytes, color: "var(--heat-2)", format: (v) => `${formatGB(v)} GB` },
    { key: "power", label: t("resource_trend.series_power"), get: (p) => p.package_power_w, color: "var(--heat-deep)", format: formatWatts, dashed: true },
    { key: "net_rx", label: t("resource_trend.series_net_rx"), get: (p) => p.net_rx_bytes_per_sec, color: "var(--m1)", format: (v) => formatBytesPerSec(v), dashed: true },
    { key: "net_tx", label: t("resource_trend.series_net_tx"), get: (p) => p.net_tx_bytes_per_sec, color: "var(--m3)", format: (v) => formatBytesPerSec(v) },
  ];

  const activeSeries = trendSeries.filter((s) => visibleSeries.has(s.key));

  return (
    <>
      <div className="eyebrow">{t("resource_trend.header", { range: rangeLabel(window_, t), series: activeSeries.map((s) => s.label).join(" / ") || t("resource_trend.none_shown") })}</div>
      <div className="card">
        <div style={{ display: "flex", gap: 8, marginBottom: 8, flexWrap: "wrap" }}>
          {trendSeries.map((s) => {
            const on = visibleSeries.has(s.key);
            return (
              <button
                key={s.key}
                className="chip"
                style={{ cursor: "pointer", opacity: on ? 1 : 0.45, borderColor: on ? s.color : undefined, color: on ? s.color : undefined }}
                onClick={() => toggleSeries(s.key)}
                title={on ? t("resource_trend.hide", { series: s.label }) : t("resource_trend.show", { series: s.label })}
              >
                {s.label}
              </button>
            );
          })}
        </div>
        {history.isLoading ? (
          <div className="empty-note">{t("resource_trend.loading")}</div>
        ) : history.isError ? (
          <div className="empty-note">{t("resource_trend.error")}</div>
        ) : history.data && history.data.points.length > 0 ? (
          <TrendChart<MetricsHistoryPoint> points={history.data.points} series={activeSeries} ariaLabel={t("resource_trend.chart_aria")} />
        ) : (
          <div className="empty-note">{t("resource_trend.no_history")}</div>
        )}
        <div style={{ display: "flex", gap: 8, justifyContent: "flex-end", marginTop: 12, alignItems: "center", flexWrap: "wrap" }}>
          {exportMut.isError && <span className="error-note" style={{ padding: "4px 8px", fontSize: 11 }}>{t("resource_trend.export_failed")}</span>}
          <button className="btn" disabled={exportMut.isPending} onClick={() => exportMut.mutate({ format: "csv", window_ })}>
            {t("resource_trend.export_csv")}
          </button>
          <button className="btn" disabled={exportMut.isPending} onClick={() => exportMut.mutate({ format: "json", window_ })}>
            {t("resource_trend.export_json")}
          </button>
        </div>
      </div>
    </>
  );
}
