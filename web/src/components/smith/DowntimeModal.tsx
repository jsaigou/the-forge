import { useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import { useSmithActionProcedurePreview, useSmithActionProcedurize } from "../../lib/queries";
import { useStepUpGate } from "../../lib/useStepUpGate";
import { StepUpModal } from "../StepUpModal";

// DowntimeModal — "let smith fix it" (autonomous-remediation Sprint 3,
// docs/v5-smith.md §13). Confirms converting a pending atomic action into
// its equivalent procedure before it happens: the procedure runner's
// checkpoint/rollback/maintenance machinery means approving it can hold a
// maintenance window or pause mid-run in ways the plain atomic action never
// did, so the operator sees that disclosure — sourced live from
// ProcedurePreview.Impact, never a hardcoded copy of it — before committing.
// Modal shell mirrors StepUpModal.tsx's open/onClose pattern; the step-up
// gate is owned locally here (procedurize carries the same
// action.smith.execute step-up as approve).

interface DowntimeModalProps {
  actionId: number;
  onClose: () => void;
  // extraWarning — an action-kind-specific disclosure this modal has no
  // other way to know about (e.g. delete_files' file-level irreversibility
  // notice). Approve now routes every procedurizable action through this
  // modal instead of a bare one-click button, so a kind that used to carry
  // its own separate warning (ConfirmButton's `warning` prop) needs it
  // preserved here instead of silently dropped.
  extraWarning?: string;
}

export function DowntimeModal({ actionId, onClose, extraWarning }: DowntimeModalProps) {
  const preview = useSmithActionProcedurePreview(actionId);
  const procedurize = useSmithActionProcedurize(actionId);
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const busy = procedurize.isPending;

  function confirm() {
    setError(null);
    procedurize.mutate(undefined, {
      onSuccess: onClose,
      onError: (e) => {
        if (gate.handle(e, confirm)) return;
        setError(apiErrorMessage(e));
      },
    });
  }

  const p = preview.data;

  return (
    <div className="modal-backdrop" onClick={(e) => { e.stopPropagation(); if (!busy) onClose(); }}>
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>Approve</h3>

        {extraWarning && (
          <div className="error-note" style={{ marginBottom: 14 }}>{extraWarning}</div>
        )}

        {preview.isLoading && (
          <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>Loading…</div>
        )}
        {preview.isError && (
          <div className="error-note" style={{ marginBottom: 14 }}>{apiErrorMessage(preview.error)}</div>
        )}

        {p && (
          <>
            <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>
              This will run <b>{p.title}</b> through smith's procedure engine — approve, post-verify,
              and (if it hits trouble) checkpoint/rollback, instead of the bare one-shot action.
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: 6, fontSize: 12.5, marginBottom: 14 }}>
              <div>
                <span style={{ color: "var(--text-mute)" }}>Estimated duration: </span>
                {p.est_duration_sec < 60
                  ? `${p.est_duration_sec}s`
                  : `${Math.round(p.est_duration_sec / 60)}m`}
              </div>
              {p.needs_maintenance && (
                <div style={{ color: "var(--warn)" }}>
                  A maintenance window will be held for the whole run — no other load/unload/switch/restart can happen until it finishes.
                </div>
              )}
              {p.daemon_restart && (
                <div style={{ color: "var(--warn)" }}>
                  This procedure restarts the daemon itself partway through — the run resumes automatically afterward.
                </div>
              )}
              {p.affected_slots && p.affected_slots.length > 0 && (
                <div>
                  <span style={{ color: "var(--text-mute)" }}>Affected slots: </span>
                  {p.affected_slots.join(", ")}
                </div>
              )}
              {p.affected_services && p.affected_services.length > 0 && (
                <div>
                  <span style={{ color: "var(--text-mute)" }}>Affected services: </span>
                  {p.affected_services.join(", ")}
                </div>
              )}
            </div>
          </>
        )}

        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

        <div className="form-actions">
          <button className="btn" disabled={busy} onClick={onClose}>Cancel</button>
          <button className="btn primary" disabled={busy || !p} onClick={confirm}>
            {busy ? "…" : "Run through smith"}
          </button>
        </div>

        <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
      </div>
    </div>
  );
}
