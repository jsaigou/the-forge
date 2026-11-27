import { useTranslation } from "react-i18next";
import { formatBytesPerSec, formatGB, formatPct, formatWatts } from "../../lib/format";
import { useMetrics } from "../../lib/queries";
import type { Metrics } from "../../lib/types";

// Widget "resource-gauges" (see lib/widgetRegistry.ts). RAM/GPU/CPU/TEMP/
// STORAGE/NETWORK, consuming Phase 4's collector fixes (real /proc/stat CPU
// utilization, /proc/net/dev throughput, per-mount storage, extra hwmon
// temp channels — see go/internal/collector). Live snapshot only: storage in
// particular has no historical series at all (a deliberate Phase 4 scope
// cut, see collector/disk.go) — that's why this widget reads useMetrics()
// rather than useMetricsHistory, unlike resource-trend below.
//
// `compact` is the Resources tab's one-row form (design feedback 2026-11-27), which also shows package power.

// Mounts that are the same volume (same size and free space) are one tile: three identical figures told the operator nothing three times.
function volumes(storage: Metrics["storage"]) {
  const byVolume = new Map<string, Metrics["storage"]>();
  for (const m of storage ?? []) {
    const key = `${m.total_bytes}:${m.free_bytes}`;
    byVolume.set(key, [...(byVolume.get(key) ?? []), m]);
  }
  return [...byVolume.values()];
}

export function ResourceGaugesWidget({ compact = false }: { compact?: boolean }) {
  const { t } = useTranslation("dashboard");
  const metrics = useMetrics();
  const m = metrics.data;

  // Any of the extra temp channels can be genuinely absent on this hardware
  // (e.g. no junction/hotspot sensor) — null there is a real reading, not a
  // probe failure, so it's simply omitted from the detail line rather than
  // shown as "—".
  const tempDetails = [
    m?.cpu_package_temp_celsius != null ? t("resource_gauges.temp_cpu", { deg: Math.round(m.cpu_package_temp_celsius) }) : null,
    m?.gpu_junction_temp_celsius != null ? t("resource_gauges.temp_junction", { deg: Math.round(m.gpu_junction_temp_celsius) }) : null,
    m?.nvme_temp_celsius != null ? t("resource_gauges.temp_nvme", { deg: Math.round(m.nvme_temp_celsius) }) : null,
  ].filter(Boolean);

  return (
    <>
      {!compact && <div className="eyebrow">{t("resource_gauges.title")}</div>}
      <div className={compact ? "stats stats-compact" : "stats"} style={{ gridTemplateColumns: `repeat(auto-fit, minmax(${compact ? 140 : 150}px, 1fr))` }}>
        <div className="stat">
          <div className="k">{t("resource_gauges.ram_k")}</div>
          <div className="v">{m ? formatPct(m.memory.pct) : "…"}</div>
          <div className="d">{m ? `${formatGB(m.memory.used_bytes)} / ${formatGB(m.memory.total_bytes)} GB` : ""}</div>
        </div>
        <div className="stat">
          <div className="k">{t("resource_gauges.gpu_k")}</div>
          <div className="v">{m?.gpu_use_pct != null ? formatPct(m.gpu_use_pct) : "—"}</div>
          <div className="d">{t("resource_gauges.gpu_d")}</div>
        </div>
        <div className="stat">
          <div className="k">{t("resource_gauges.cpu_k")}</div>
          <div className="v">{m ? formatPct(m.cpu.pct) : "…"}</div>
          <div className="d">{m ? t("resource_gauges.cpu_d", { load: m.cpu.load1.toFixed(2) }) : ""}</div>
        </div>
        <div className="stat">
          <div className="k">{t("resource_gauges.temp_k")}</div>
          <div className="v">{m?.temp_celsius != null ? `${Math.round(m.temp_celsius)}°C` : "—"}</div>
          <div className="d">{tempDetails.length > 0 ? tempDetails.join(" · ") : t("resource_gauges.temp_fallback")}</div>
        </div>
        {compact && (
          <div className="stat">
            <div className="k">{t("resource_gauges.power_k")}</div>
            <div className="v">{formatWatts(m?.package_power_w)}</div>
            <div className="d">{t("resource_gauges.power_d")}</div>
          </div>
        )}
        <div className="stat">
          <div className="k">{t("resource_gauges.network_k")}</div>
          <div className="v">{m ? formatBytesPerSec(m.net_rx_bytes_per_sec) : "…"} ↓</div>
          <div className="d">{m ? formatBytesPerSec(m.net_tx_bytes_per_sec) : ""} ↑</div>
        </div>
        {volumes(m?.storage ?? []).map((mounts) => {
          const first = mounts[0];
          const names = mounts.map((x) => x.name).join(", ");
          return (
            <div className="stat" key={first.path} title={mounts.map((x) => x.path).join("\n")}>
              <div className="k">{t("resource_gauges.storage_k", { name: names })}</div>
              <div className="v">{formatPct(first.pct)}</div>
              <div className="d">{formatGB(first.used_bytes)} / {formatGB(first.total_bytes)} GB</div>
            </div>
          );
        })}
      </div>
    </>
  );
}
