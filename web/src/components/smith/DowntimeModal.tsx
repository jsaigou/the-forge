import { useState } from "react";
import { Trans, useTranslation } from "react-i18next";
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
  const { t } = useTranslation("common");
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
        <h3>{t("downtime_modal.title")}</h3>

        {extraWarning && (
          <div className="error-note" style={{ marginBottom: 14 }}>{extraWarning}</div>
        )}

        {preview.isLoading && (
          <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>{t("downtime_modal.loading")}</div>
        )}
        {preview.isError && (
          <div className="error-note" style={{ marginBottom: 14 }}>{apiErrorMessage(preview.error)}</div>
        )}

        {p && (
          <>
            <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>
              <Trans i18nKey="downtime_modal.run_description" ns="common" values={{ title: p.title }} components={{ b: <b /> }} />
            </div>
            <div style={{ display: "flex", flexDirection: "column", gap: 6, fontSize: 12.5, marginBottom: 14 }}>
              <div>
                <span style={{ color: "var(--text-mute)" }}>{t("downtime_modal.estimated_duration_label")}</span>
                {p.est_duration_sec < 60
                  ? t("downtime_modal.duration_seconds", { sec: p.est_duration_sec })
                  : t("downtime_modal.duration_minutes", { min: Math.round(p.est_duration_sec / 60) })}
              </div>
              {p.needs_maintenance && (
                <div style={{ color: "var(--warn)" }}>
                  {t("downtime_modal.maintenance_window_warning")}
                </div>
              )}
              {p.daemon_restart && (
                <div style={{ color: "var(--warn)" }}>
                  {t("downtime_modal.daemon_restart_warning")}
                </div>
              )}
              {p.affected_slots && p.affected_slots.length > 0 && (
                <div>
                  <span style={{ color: "var(--text-mute)" }}>{t("downtime_modal.affected_slots_label")}</span>
                  {p.affected_slots.join(", ")}
                </div>
              )}
              {p.affected_services && p.affected_services.length > 0 && (
                <div>
                  <span style={{ color: "var(--text-mute)" }}>{t("downtime_modal.affected_services_label")}</span>
                  {p.affected_services.join(", ")}
                </div>
              )}
            </div>
          </>
        )}

        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

        <div className="form-actions">
          <button className="btn" disabled={busy} onClick={onClose}>{t("downtime_modal.cancel")}</button>
          <button className="btn primary" disabled={busy || !p} onClick={confirm}>
            {busy ? "…" : t("downtime_modal.run_through_smith")}
          </button>
        </div>

        <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
      </div>
    </div>
  );
}
