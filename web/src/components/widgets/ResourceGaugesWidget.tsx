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
  const metrics = useMetrics();
  const m = metrics.data;

  // Any of the extra temp channels can be genuinely absent on this hardware
  // (e.g. no junction/hotspot sensor) — null there is a real reading, not a
  // probe failure, so it's simply omitted from the detail line rather than
  // shown as "—".
  const tempDetails = [
    m?.cpu_package_temp_celsius != null ? `CPU ${Math.round(m.cpu_package_temp_celsius)}°C` : null,
    m?.gpu_junction_temp_celsius != null ? `junction ${Math.round(m.gpu_junction_temp_celsius)}°C` : null,
    m?.nvme_temp_celsius != null ? `NVMe ${Math.round(m.nvme_temp_celsius)}°C` : null,
  ].filter(Boolean);

  return (
    <>
      <div className="eyebrow">Resources · live</div>
      <div className="stats" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))" }}>
        <div className="stat">
          <div className="k">RAM</div>
          <div className="v">{m ? formatPct(m.memory.pct) : "…"}</div>
          <div className="d">{m ? `${formatGB(m.memory.used_bytes)} / ${formatGB(m.memory.total_bytes)} GB` : ""}</div>
        </div>
        <div className="stat">
          <div className="k">GPU</div>
          <div className="v">{m?.gpu_use_pct != null ? formatPct(m.gpu_use_pct) : "—"}</div>
          <div className="d">utilization</div>
        </div>
        <div className="stat">
          <div className="k">CPU</div>
          <div className="v">{m ? formatPct(m.cpu.pct) : "…"}</div>
          <div className="d">{m ? `load1 ${m.cpu.load1.toFixed(2)}` : ""}</div>
        </div>
        <div className="stat">
          <div className="k">Temp</div>
          <div className="v">{m?.temp_celsius != null ? `${Math.round(m.temp_celsius)}°C` : "—"}</div>
          <div className="d">{tempDetails.length > 0 ? tempDetails.join(" · ") : "GPU edge"}</div>
        </div>
        <div className="stat">
          <div className="k">Network</div>
          <div className="v">{m ? formatBytesPerSec(m.net_rx_bytes_per_sec) : "…"} ↓</div>
          <div className="d">{m ? formatBytesPerSec(m.net_tx_bytes_per_sec) : ""} ↑</div>
        </div>
        {(m?.storage ?? []).map((mount) => (
          <div className="stat" key={mount.path}>
            <div className="k">Storage · {mount.name}</div>
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
