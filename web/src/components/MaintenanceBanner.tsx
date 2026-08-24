import { useState } from "react";
import { apiErrorMessage } from "../lib/api";
import { useMaintenance, useMaintenanceEnter, useMaintenanceExit } from "../lib/queries";
import { useStepUpGate } from "../lib/useStepUpGate";
import { ConfirmButton } from "./ConfirmButton";
import { StepUpModal } from "./StepUpModal";

// MaintenanceBanner — the system-wide quiet-host indicator (autonomous-
// remediation plan, Sprint 1). Visible from every tab, same "small pip,
// click for detail" idiom App.tsx already uses for the restart-required and
// profile-progress indicators — maintenance mode affects every tab equally
// (no model can load anywhere while it's active), so it lives in the same
// top-bar slot rather than being buried in one page.
//
// Renders nothing at all when inactive and the panel is closed — the
// common case shouldn't occupy the top bar (same contract as
// SuggestionsTray/PendingActionsCard elsewhere in this app).
export function MaintenanceBanner() {
  const maint = useMaintenance();
  const [open, setOpen] = useState(false);
  if (maint.isLoading || maint.isError) return null;

  const active = maint.data?.active ?? false;
  if (!active && !open) {
    return (
      <button
        type="button"
        className="icon-btn"
        title="Enter maintenance mode"
        onClick={() => setOpen(true)}
        style={{ display: "flex", alignItems: "center", justifyContent: "center", padding: 0, marginRight: 6 }}
      >
        <span style={{ width: 8, height: 8, borderRadius: "50%", background: "var(--text-mute)", opacity: 0.4 }} />
      </button>
    );
  }

  return (
    <>
      <button
        type="button"
        className="icon-btn"
        title={active ? `Maintenance mode active — ${maint.data?.reason ?? "no reason given"}. Click for detail.` : "Enter maintenance mode"}
        onClick={() => setOpen((o) => !o)}
        style={{ display: "flex", alignItems: "center", justifyContent: "center", padding: 0, marginRight: 6 }}
      >
        <span
          style={{
            width: 8, height: 8, borderRadius: "50%",
            background: active ? "var(--warn)" : "var(--text-mute)",
            boxShadow: active ? "0 0 7px var(--warn)" : "none",
          }}
        />
      </button>
      {open && (
        <MaintenancePanel onClose={() => setOpen(false)} />
      )}
    </>
  );
}

// MaintenancePanel — a small fixed popover under the pip. Deliberately not
// a modal: entering/exiting maintenance is exactly as consequential as
// approving a smith action (same step-up resource), never more, so it
// doesn't need to block the rest of the screen.
function MaintenancePanel({ onClose }: { onClose: () => void }) {
  const maint = useMaintenance();
  const enter = useMaintenanceEnter();
  const exit = useMaintenanceExit();
  const gate = useStepUpGate();
  const [reason, setReason] = useState("");
  const [minutes, setMinutes] = useState(60);
  const [error, setError] = useState<string | null>(null);

  const st = maint.data;
  const active = st?.active ?? false;

  function handleEnter() {
    setError(null);
    const body = { reason, duration_minutes: minutes };
    enter.mutate(body, {
      onError: (e) => {
        if (gate.handle(e, () => enter.mutate(body))) return;
        setError(apiErrorMessage(e));
      },
    });
  }

  function handleExit() {
    setError(null);
    exit.mutate(undefined, {
      onError: (e) => {
        if (gate.handle(e, () => exit.mutate())) return;
        setError(apiErrorMessage(e));
      },
    });
  }

  return (
    <div
      className="card"
      style={{
        position: "absolute", top: 44, right: 16, zIndex: 50, width: 320,
        boxShadow: "0 8px 24px rgba(0,0,0,0.3)",
      }}
    >
      <div style={{ display: "flex", alignItems: "center", marginBottom: 8 }}>
        <strong style={{ fontSize: 12 }}>Maintenance mode</strong>
        <button type="button" className="icon-btn" style={{ marginLeft: "auto" }} onClick={onClose}>×</button>
      </div>

      {error && <div className="error-note" style={{ marginBottom: 8 }}>{error}</div>}

      {active ? (
        <>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 4 }}>
            Active — no model may load, unload, switch, or restart until this window ends.
          </div>
          <div style={{ fontSize: 12, marginBottom: 4 }}>{st?.reason || "(no reason given)"}</div>
          {st?.entered_by && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 4 }}>Entered by {st.entered_by}</div>
          )}
          {st?.expires_at && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 10 }}>
              Auto-exits {new Date(st.expires_at * 1000).toLocaleString()}
            </div>
          )}
          <ConfirmButton
            label="End maintenance"
            confirmLabel="End it now"
            onConfirm={handleExit}
            pending={exit.isPending}
            warning="Loads/unloads/restarts become possible again immediately."
          />
        </>
      ) : (
        <>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
            While active, nothing may load, unload, switch, or restart anywhere on this host —
            use this before a manual repair that needs a quiet system.
          </div>
          <label className="form-row" style={{ marginBottom: 6 }}>
            Reason
            <input value={reason} onChange={(e) => setReason(e.target.value)} placeholder="e.g. rebuilding the vulkan binary" />
          </label>
          <label className="form-row" style={{ marginBottom: 10 }}>
            Duration (minutes)
            <input type="number" min={1} value={minutes} onChange={(e) => setMinutes(Number(e.target.value))} />
          </label>
          <button className="btn primary" disabled={!reason || enter.isPending} onClick={handleEnter}>
            {enter.isPending ? "…" : "Enter maintenance mode"}
          </button>
        </>
      )}

      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </div>
  );
}
