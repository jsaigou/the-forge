import { useTranslation } from "react-i18next";
import { formatBytesPerSec, formatGB, formatPct } from "../../lib/format";
import { useMetrics } from "../../lib/queries";

// Widget "resource-gauges" (see lib/widgetRegistry.ts). RAM/GPU/CPU/TEMP/
// STORAGE/NETWORK, consuming Phase 4's collector fixes (real /proc/stat CPU
// utilization, /proc/net/dev throughput, per-mount storage, extra hwmon
// temp channels — see go/internal/collector). Live snapshot only: storage in
// particular has no historical series at all (a deliberate Phase 4 scope
// cut, see collector/disk.go) — that's why this widget reads useMetrics()
// rather than useMetricsHistory, unlike resource-trend below.
export function ResourceGaugesWidget() {
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
      <div className="eyebrow">{t("resource_gauges.title")}</div>
      <div className="stats" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))" }}>
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
        <div className="stat">
          <div className="k">{t("resource_gauges.network_k")}</div>
          <div className="v">{m ? formatBytesPerSec(m.net_rx_bytes_per_sec) : "…"} ↓</div>
          <div className="d">{m ? formatBytesPerSec(m.net_tx_bytes_per_sec) : ""} ↑</div>
        </div>
        {(m?.storage ?? []).map((mount) => (
          <div className="stat" key={mount.path}>
            <div className="k">{t("resource_gauges.storage_k", { name: mount.name })}</div>
            <div className="v">{formatPct(mount.pct)}</div>
            <div className="d">
              {formatGB(mount.used_bytes)} / {formatGB(mount.total_bytes)} GB · {mount.path}
            </div>
          </div>
        ))}
      </div>
    </>
  );
}
