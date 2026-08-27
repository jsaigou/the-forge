import { useServiceModeToggle } from "../lib/queries";
import { useSession } from "../lib/session";
import { Icon } from "./Icon";
import { formatGB } from "../lib/format";
import type { InfraService } from "../lib/types";

// §0.5 / FE-1a: trimmed rows — name + active dot only. Unit/port noise is
// dropped (still on the wire, just not rendered). The A0/LLM-Proxy row
// additionally surfaces its Compressor passthrough state via a chip indicator
// (compressor_passthrough is non-null only on that row, per the frozen shape).
// A0/STT/Embedding/Aligner/TTS are all permanent daemons — no toggle
// buttons on service chips (start/stop control for ComfyUI lives in the
// provider/add-on column via the `addon` prop).
//
// Backend wiring landed: services_handlers.go now appends a real "Aligner"
// row whenever cfg.Ports["aligner"] is set (port 8086 — a pre-existing,
// permanently-running unit distinct from A3), and
// extraUnits() (cmd/forge/main.go) probes forge-aligner.service
// automatically since it walks every [ports] key. That still requires the
// live store's infra.ports to actually carry the "aligner" key (not true on
// every deployment/test env) — ALIGNER_STUB below stays as a fallback so the
// panel never invents a fake active/inactive value in that case (amber
// "unmonitored" dot, no toggle, tooltip explains why). It self-removes the
// instant a real "Aligner" row is present (see the `hasAligner` check).
const ALIGNER_NAME = "Aligner";
const ALIGNER_STUB: InfraService = {
  name: ALIGNER_NAME,
  unit: "forge-aligner",
  port: null,
  active: false,
  kind: "systemd",
  mode_key: null,
  detail: "not yet monitored by the backend — stub row, see FE-1 report",
  compressor_passthrough: null,
  logo: "qwen", // Qwen3-ForcedAligner-0.6B — icon names the model, same as the real backend-fed row
};

// Compressor proxy rows ("Compressor (local)", "Compressor (deepseek)", …) are
// collapsed into one chip rather than rendered as separate rows or sub-rows.
// Originally (2026-07-29) this was a multi-row tile with a full-width claim
// and a per-proxy stop/play button each — three-plus controls duplicating
// what Settings → Compressor already owns, and no single answer to "is
// Compressor OK?". Operator follow-up (2026-07-31): one chip, one indicator,
// one button, matching every other row in this strip. Per-proxy detail moves
// to a hover tooltip; per-proxy *control* stays in Settings → Compressor
// (Settings.tsx's per-proxy switches), unchanged.
const COMPRESSOR_PREFIX = "Compressor (";

// aggregateCompressorStatus reduces the per-proxy compressor_state values
// (server-computed in compressorServiceRows, services_handlers.go — the same
// branch logic, exposed as a stable enum instead of parsed from `detail`
// prose) into one indicator, in the operator's stated priority order:
// a failure always shows red even if other proxies are merely bypassed.
// Sprint 4 (resource bounding + monitoring): compressor_resource_health is factored in
// additively, never merged with compressor_state — a proxy that's bypassed
// (routing posture "off") can still be restart-looping or leaking (resource
// health "warn"), so a health problem is checked even in the all-bypassed
// case rather than being masked by the early "off" return.
function aggregateCompressorStatus(rows: InfraService[]): "on" | "warn" | "crit" | "off" {
  const states = rows.map((s) => s.compressor_state).filter((st) => !!st) as string[];
  if (states.length === 0) return "off";
  const healthWarn = rows.some(
    (s) => s.compressor_resource_health === "restarting" || s.compressor_resource_health === "memory_growth",
  );
  if (states.some((st) => st === "down")) return "crit";
  if (states.every((st) => st === "bypassed") && !healthWarn) return "off";
  if (states.some((st) => st === "bypassed" || st === "idle") || healthWarn) return "warn";
  return "on";
}

function CompressorGroupTile({ rows }: { rows: InfraService[] }) {
  const status = aggregateCompressorStatus(rows);
  const tooltip = rows
    .map((s) => {
      const proxy = s.name.slice(COMPRESSOR_PREFIX.length, -1);
      const bits = [`${proxy}: ${s.detail ?? "unknown"}`];
      if (s.compressor_resource_health && s.compressor_resource_health !== "unknown" && s.compressor_resource_health !== "ok") {
        bits.push(`health: ${s.compressor_resource_health.replace("_", " ")}`);
      }
      if (s.compressor_rss_bytes != null) bits.push(`${formatGB(s.compressor_rss_bytes, 1)} GB`);
      if (s.compressor_restarts != null) bits.push(`${s.compressor_restarts} restarts`);
      return bits.join(", ");
    })
    .join(" · ");

  // Operator feedback 2026-08-14: no play/stop button here (per-proxy
  // control stays in Settings → Compressor) and no status light — the chip's
  // border GLOW carries the state (same language as every other chip).
  return (
    <div className={`cchip cchip-${status}`} title={tooltip}>
      <div className="nm">Compressor</div>
    </div>
  );
}

// ServiceChip renders one service row. The chip's border glow communicates
// on/warn/crit/off. Always-on systemd chips have no control; true service
// modes (ComfyUI) keep their start/stop button — restored 2026-08-15 after
// the QA-undo commit stripped it along with the duplicate rows.
export function ServiceChip({ s, addon = false }: { s: InfraService; addon?: boolean }) {
  const { canOperate } = useSession();
  const serviceModeToggle = useServiceModeToggle();
  const isStub = s.detail === "not yet monitored by the backend — stub row, see FE-1 report";
  const toggleable = !isStub && s.kind === "service_mode";
  const busy = toggleable && serviceModeToggle.isPending;
  const isA0 = s.compressor_passthrough != null;
  const passthroughOn = s.compressor_passthrough === true;
  const glow = isStub ? "warn" : s.active ? "on" : "off";
  return (
    <div className={`cchip${addon ? " cchip-provider" : ""} cchip-${glow}`} key={s.name}>
      {s.logo && <Icon slug={s.logo} name={s.name} sm />}
      <div className="nm">{s.name}</div>
      {isA0 && passthroughOn && (
        <span className="chip remote" title="Compressor bypassed — requests route uncompressed">
          passthrough
        </span>
      )}
      {/* Operator feedback 2026-08-25: match ProviderCreditTile's layout —
          controls right-justified (flex spacer before them), plus an ↗ link
          opening the service UI in a new window. ComfyUI serves :3001; the
          host comes from the dashboard URL so it works over tailnet or
          localhost alike. */}
      {toggleable && s.port != null && (
        <a
          href={`http://${window.location.hostname}:${s.port}`}
          target="_blank"
          rel="noreferrer"
          title={`Open ${s.name} in a new window`}
          style={{ marginLeft: "auto", color: "var(--text-mute)", fontSize: 11, flex: "0 0 auto", textDecoration: "none" }}
          onClick={(e) => e.stopPropagation()}
        >
          ↗
        </a>
      )}
      {toggleable && canOperate && (
        <button
          className="icon-btn action"
          style={{ width: 20, height: 20, fontSize: 10, flex: "0 0 auto", ...(s.port == null ? { marginLeft: "auto" } : {}) }}
          title={s.active ? "Stop" : "Start"}
          disabled={busy}
          onClick={() => {
            if (s.mode_key) serviceModeToggle.mutate({ name: s.mode_key, action: s.active ? "stop" : "start" });
          }}
        >
          {/* Glyphs match ProviderCreditTile's enable/disable control (Console.tsx)
              — one visual language for every toggle in a cchip-provider tile. */}
          {s.active ? "⏸" : "▶"}
        </button>
      )}
    </div>
  );
}

// Console usability pass #2 (2026-07-29): the extraTiles slot is gone — the
// resource dials moved into their own chip to the right of this strip and
// the provider tiles moved to the Load Bays section (both in Console.tsx),
// so this component is back to rendering just the service rows.
// Operator feedback 2026-08-14: ComfyUI moved out of this strip into the
// add-on column (Console.tsx renders it via ServiceChip with addon=true).
export function ServicesBar({ services }: { services: InfraService[] | undefined }) {
  if (!services) {
    return <div className="services empty-note">Loading services…</div>;
  }

  const hasAligner = services.some((s) => s.name === ALIGNER_NAME);
  const withAligner = hasAligner ? services : [...services, ALIGNER_STUB];

  const compressorRows = withAligner.filter((s) => s.name.startsWith(COMPRESSOR_PREFIX));
  const rows = withAligner.filter((s) => !s.name.startsWith(COMPRESSOR_PREFIX) && s.name !== "ComfyUI");

  return (
    <div className="services">
      {compressorRows.length > 0 && <CompressorGroupTile rows={compressorRows} />}
      {rows.map((s) => <ServiceChip key={s.name} s={s} />)}
    </div>
  );
}
