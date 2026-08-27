// settings/panels/Danger.tsx — Sprint 12 (was H) Phase 7. The standalone
// "danger" section: infra.server/paths/tailscale (via <DangerZone>, all
// apply="restart") + infra.ports (a Record<string,number> map, its own
// small key/value editor — <Field> only renders scalars) + the daemon
// restart action with a health-poll overlay.
import { useQueryClient } from "@tanstack/react-query";
import { useRef, useState } from "react";
import { CopyButton } from "../../components/CopyButton";
import { apiErrorMessage, api } from "../../lib/api";
import { useSystemPreflight, useSystemRestart, useSystemSettings, useUpdateSystemSettings } from "../../lib/queries";
import { useStepUpGate } from "../../lib/useStepUpGate";
import { StepUpModal } from "../../components/StepUpModal";
import type { SystemSettings } from "../../lib/types";
import { Field } from "../Field";
import { DANGER_FIELDS } from "../fields";
import { DangerZone } from "../DangerZone";

const F = Object.fromEntries(DANGER_FIELDS.map((f) => [f.id, f]));

function PortsEditor({
  ports,
  disabled,
  onChange,
}: {
  ports: Record<string, number>;
  disabled: boolean;
  onChange: (ports: Record<string, number>) => void;
}) {
  const entries = Object.entries(ports);
  const [newKey, setNewKey] = useState("");
  const [newPort, setNewPort] = useState("");

  function setEntry(key: string, port: number) {
    onChange({ ...ports, [key]: port });
  }
  function removeEntry(key: string) {
    const next = { ...ports };
    delete next[key];
    onChange(next);
  }
  function addEntry() {
    if (!newKey || !newPort) return;
    onChange({ ...ports, [newKey]: Number(newPort) });
    setNewKey("");
    setNewPort("");
  }

  return (
    <div style={{ marginTop: 10 }}>
      <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
        <span style={{ width: 200 }}>Aux port name</span>
        <span style={{ width: 100 }}>Port</span>
      </div>
      {entries.map(([key, port]) => (
        <div className="qrow" key={key}>
          <span style={{ width: 200, fontFamily: "var(--mono)", fontSize: 12 }}>forge-{key}</span>
          <input
            type="number" min={1} max={65535} value={port} disabled={disabled}
            style={{ width: 90 }}
            onChange={(e) => setEntry(key, Number(e.target.value))}
          />
          {!disabled && (
            <button type="button" className="btn" style={{ marginLeft: 8 }} onClick={() => removeEntry(key)}>Remove</button>
          )}
        </div>
      ))}
      {!disabled && (
        <div className="qrow">
          <input placeholder="name" value={newKey} style={{ width: 200 }} onChange={(e) => setNewKey(e.target.value.toLowerCase())} />
          <input type="number" min={1} max={65535} placeholder="port" value={newPort} style={{ width: 90 }} onChange={(e) => setNewPort(e.target.value)} />
          <button type="button" className="btn" style={{ marginLeft: 8 }} onClick={addEntry} disabled={!newKey || !newPort}>Add</button>
        </div>
      )}
      {entries.length === 0 && <div className="empty-note">No auxiliary ports configured.</div>}
    </div>
  );
}

function SystemDangerZone({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useSystemSettings(canAdmin);
  const update = useUpdateSystemSettings();
  const preflight = useSystemPreflight();

  return (
    <DangerZone<SystemSettings>
      title="System"
      canAdmin={canAdmin}
      isError={cfg.isError}
      data={cfg.data}
      blurb={
        <>
          Boot-critical: listen addresses, paths, ports, and the Tailscale hostname. Every field here needs a
          daemon restart to take effect. Every check runs as the daemon's own user, not yours — a "permission
          denied" on a path you can <code>ls</code> yourself usually means the daemon's uid can't reach it, not a
          UI bug.
        </>
      }
      onCheck={(draft) =>
        preflight.mutateAsync({
          listen: draft.listen, router_listen: draft.router_listen, mcp_listen: draft.mcp_listen,
          db_path: draft.db_path, tts_unit: draft.tts_unit, models_dir: draft.models_dir,
          sysconfig_dir: draft.sysconfig_dir, state_dir: draft.state_dir, icons_dir: draft.icons_dir,
          vulkan_bin: draft.vulkan_bin, rocm_bin: draft.rocm_bin, ports: draft.ports, hostname: draft.hostname,
          cookie_secure: draft.cookie_secure,
        })
      }
      onSave={(draft) =>
        update.mutateAsync({
          listen: draft.listen, router_listen: draft.router_listen, mcp_listen: draft.mcp_listen,
          db_path: draft.db_path, tts_unit: draft.tts_unit, models_dir: draft.models_dir,
          sysconfig_dir: draft.sysconfig_dir, state_dir: draft.state_dir, icons_dir: draft.icons_dir,
          vulkan_bin: draft.vulkan_bin, rocm_bin: draft.rocm_bin, ports: draft.ports, hostname: draft.hostname,
          cookie_secure: draft.cookie_secure,
        })
      }
      renderFields={(active, setField, disabled) => (
        <>
          <div className="form-grid wide">
            <Field rec={F["danger.listen"]} value={active.listen} disabled={disabled} onChange={(v) => setField("listen", String(v))} />
            <Field rec={F["danger.router_listen"]} value={active.router_listen} disabled={disabled} onChange={(v) => setField("router_listen", String(v))} />
            <Field rec={F["danger.mcp_listen"]} value={active.mcp_listen} disabled={disabled} onChange={(v) => setField("mcp_listen", String(v))} />
            <Field rec={F["danger.db_path"]} value={active.db_path} disabled={disabled} onChange={(v) => setField("db_path", String(v))} />
            <Field rec={F["danger.tts_unit"]} value={active.tts_unit} disabled={disabled} onChange={(v) => setField("tts_unit", String(v))} />
            <Field rec={F["danger.models_dir"]} value={active.models_dir} disabled={disabled} onChange={(v) => setField("models_dir", String(v))} />
            <Field rec={F["danger.sysconfig_dir"]} value={active.sysconfig_dir} disabled={disabled} onChange={(v) => setField("sysconfig_dir", String(v))} />
            <Field rec={F["danger.state_dir"]} value={active.state_dir} disabled={disabled} onChange={(v) => setField("state_dir", String(v))} />
            <Field rec={F["danger.icons_dir"]} value={active.icons_dir} disabled={disabled} onChange={(v) => setField("icons_dir", String(v))} />
            <Field rec={F["danger.vulkan_bin"]} value={active.vulkan_bin} disabled={disabled} onChange={(v) => setField("vulkan_bin", String(v))} />
            <Field rec={F["danger.rocm_bin"]} value={active.rocm_bin} disabled={disabled} onChange={(v) => setField("rocm_bin", String(v))} />
            <Field rec={F["danger.hostname"]} value={active.hostname} disabled={disabled} onChange={(v) => setField("hostname", String(v))} />
            <Field rec={F["danger.cookie_secure"]} value={active.cookie_secure} disabled={disabled} onChange={(v) => setField("cookie_secure", Boolean(v))} />
          </div>
          <PortsEditor ports={active.ports} disabled={disabled} onChange={(p) => setField("ports", p)} />
        </>
      )}
    />
  );
}

function RestartOverlay({ onDone }: { onDone: (ok: boolean) => void }) {
  return (
    <div className="modal-backdrop">
      <div className="modal">
        <h3>Restarting forge-daemon…</h3>
        <div style={{ fontSize: 12.5, color: "var(--text-dim)", lineHeight: 1.6, marginBottom: 14 }}>
          Waiting for the daemon to come back. This does <b>not</b> unload any loaded model — A1–A4 are separate
          systemd units. It <b>does</b> drop every in-flight a0 request and SSE subscriber.
        </div>
        <div className="dot-busy" style={{ width: 8, height: 8, borderRadius: "50%", background: "var(--warn)", boxShadow: "0 0 7px var(--warn)", margin: "0 auto" }} />
        <div className="form-actions" style={{ marginTop: 16 }}>
          <button className="btn" onClick={() => onDone(false)}>Give up waiting</button>
        </div>
      </div>
    </div>
  );
}

function RestartAction({ canAdmin }: { canAdmin: boolean }) {
  const restart = useSystemRestart();
  const gate = useStepUpGate();
  const qc = useQueryClient();
  const [polling, setPolling] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Guards against a stale async pollHealth() resolving after the operator
  // has already dismissed the overlay ("give up waiting") — without this, a
  // late timeout or a late success could silently re-show the overlay or
  // fire a background invalidateQueries the operator no longer expects.
  // Bumped on every new restart AND on dismissal, so only the run that's
  // still current can act.
  const pollToken = useRef(0);

  async function pollHealth(token: number) {
    // Mirrors the server's own 750ms pre-restart delay (httpapi.go's
    // handleSystemRestart) — no point polling before the process has even
    // started shutting down.
    await new Promise((r) => setTimeout(r, 900));
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      if (pollToken.current !== token) return; // dismissed — stop acting
      if (await api.health()) {
        if (pollToken.current === token) {
          setPolling(false);
          qc.invalidateQueries();
        }
        return;
      }
      await new Promise((r) => setTimeout(r, 500));
    }
    if (pollToken.current === token) {
      setPolling(false);
      setError("Restart did not come back within 60s — check the daemon manually (systemctl status forge-daemon).");
    }
  }

  function doRestart() {
    setError(null);
    const attempt = () =>
      restart.mutate(undefined, {
        onSuccess: () => {
          const token = ++pollToken.current;
          setPolling(true);
          void pollHealth(token);
        },
        onError: (e) => {
          if (!gate.handle(e, attempt)) setError(apiErrorMessage(e));
        },
      });
    attempt();
  }

  function dismiss() {
    pollToken.current++; // invalidate any in-flight poll
    setPolling(false);
  }

  return (
    <>
      <div className="eyebrow" id="danger-restart">Restart</div>
      <div className="danger-zone">
        <div className="dz-head">
          <span className="dz-title">Restart forge-daemon</span>
        </div>
        <div className="dz-blurb">
          Applies every restart-mode change above immediately, without waiting for a natural restart. Does not
          unload models (A1–A4 are separate units); drops in-flight a0 requests and every SSE subscriber. The
          literal equivalent, if you'd rather run it yourself:
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 12 }}>
          <code style={{ fontSize: 12, background: "var(--bg-2)", padding: "6px 10px", borderRadius: 6 }}>
            sudo systemctl restart forge-daemon
          </code>
          <CopyButton text="sudo systemctl restart forge-daemon" />
        </div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        {canAdmin && (
          <div className="form-actions">
            <button className="btn primary" disabled={restart.isPending || polling} onClick={doRestart}>
              {restart.isPending ? "Requesting…" : "Restart daemon"}
            </button>
          </div>
        )}
      </div>
      {polling && <RestartOverlay onDone={dismiss} />}
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}

export function Danger({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <SystemDangerZone canAdmin={canAdmin} />
      <RestartAction canAdmin={canAdmin} />
    </>
  );
}
