import { Trans, useTranslation } from "react-i18next";
import { Modal } from "./Modal";

// RestartOverlay — the "daemon is restarting, waiting for it to come back"
// overlay, shared by Settings → Routing's save-and-restart and Settings →
// Danger's restart action (ui-review-2026-11 §A2: the two panels carried two
// near-identical copies). Built on the Modal shell (ADR-0023 contract b).
// The backdrop stays inert — waiting out a restart must not be dismissable
// by a stray click; the Give up button and Escape are the only ways out,
// and both stop the health poll via the caller's onDone.
export function RestartOverlay({ onDone }: { onDone: () => void }) {
  const { t } = useTranslation("settings");
  return (
    <Modal title={t("danger.restart_overlay_title")} onClose={onDone} closeOnBackdrop={false}>
      <div style={{ fontSize: 12.5, color: "var(--text-dim)", lineHeight: 1.6, marginBottom: 14 }}>
        <Trans i18nKey="danger.restart_overlay_body" ns="settings" components={{ b0: <b />, b1: <b /> }} />
      </div>
      <div className="dot-busy" style={{ width: 8, height: 8, borderRadius: "50%", background: "var(--warn)", boxShadow: "0 0 7px var(--warn)", margin: "0 auto" }} />
      <div className="form-actions" style={{ marginTop: 16 }}>
        <button className="btn" onClick={onDone}>{t("danger.give_up")}</button>
      </div>
    </Modal>
  );
}
