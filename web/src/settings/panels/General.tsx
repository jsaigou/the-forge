// settings/panels/General.tsx — read-only daemon strip and the restart-
// required banner (Status.restart_required — the fuller, always-visible
// counterpart to App.tsx's topbar pip).
import { useSystemSettings, useStatus } from "../../lib/queries";

function RestartBanner() {
  const status = useStatus();
  const info = status.data?.restart_required;
  if (!info) return null;
  const since = new Date(info.since);
  return (
    <div className="restart-banner">
      <b>Restart required</b> — {info.keys.length} restart-mode setting{info.keys.length === 1 ? "" : "s"} changed
      since the daemon last booted (by <b>{info.by}</b>, {since.toLocaleString()}): {info.keys.join(", ")}.
      Apply it from Settings → Danger Zone once that section ships (Phase 7), or manually via{" "}
      <code>sudo systemctl restart forge-daemon</code>.
    </div>
  );
}

function DaemonStrip({ canAdmin }: { canAdmin: boolean }) {
  const status = useStatus();
  // Admin-gated on the backend (GET /api/v1/system/settings) — skip the
  // request entirely for non-admins rather than firing a request that's
  // guaranteed to 403 (see useSystemSettings's doc comment).
  const system = useSystemSettings(canAdmin);

  return (
    <>
      <div className="eyebrow">Daemon</div>
      <div className="card">
        <RestartBanner />
        <div className="form-grid">
          <div className="form-row">Version<input value={status.data?.version ?? "…"} disabled readOnly /></div>
          <div className="form-row">Hostname<input value={status.data?.hostname ?? "…"} disabled readOnly /></div>
          {canAdmin && system.data && (
            <>
              <div className="form-row">Dashboard listen<input value={system.data.listen} disabled readOnly /></div>
              <div className="form-row">a0 router listen<input value={system.data.router_listen} disabled readOnly /></div>
              <div className="form-row">MCP listen<input value={system.data.mcp_listen} disabled readOnly /></div>
            </>
          )}
        </div>
        {canAdmin && (
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 4 }}>
            Read-only here for transparency — the same fields are editable in the Danger Zone,
            behind arm+step-up+preflight.
          </div>
        )}
      </div>
    </>
  );
}

export function General({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <DaemonStrip canAdmin={canAdmin} />
    </>
  );
}
