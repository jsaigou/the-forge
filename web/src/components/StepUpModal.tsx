import { useRef, useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api";
import { useStepUp } from "../lib/queries";
import { Modal } from "./Modal";

// StepUpModal — a minimal, reusable step-up authentication modal (password +
// TOTP). This is the first FE consumer of the step-up surface; FE-AUTH (FE-6)
// may extend it with WebAuthn/passkey support later. The backend endpoint
// POST /api/v1/auth/step-up is already implemented (auth_handlers.go).
//
// On the shared Modal shell (ADR-0023 contract b), which owns the dialog
// semantics: initial focus targets the password/TOTP input via
// initialFocusRef — what the old autoFocus attributes did, kept explicit
// because autoFocus fired during commit, before the shell could capture
// the trigger to restore focus to — and Escape / backdrop dismiss unless a
// verification is in flight.
//
// Props:
//   open         — whether the modal is visible
//   requiredFactor — the factor the policy demands ("password" | "totp")
//   onSuccess    — called after a successful step-up (caller retries the action)
//   onClose      — called when the user dismisses the modal
interface StepUpModalProps {
  open: boolean;
  requiredFactor: "password" | "totp";
  onSuccess: () => void;
  onClose: () => void;
}

export function StepUpModal({ open, requiredFactor, onSuccess, onClose }: StepUpModalProps) {
  const { t } = useTranslation("common");
  const stepUp = useStepUp();
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [error, setError] = useState<string | null>(null);
  const busy = stepUp.isPending;

  if (!open) return null;

  function submit() {
    setError(null);
    const factor = requiredFactor === "totp" ? "totp" : "password";
    const req =
      factor === "password"
        ? { factor: "password" as const, password }
        : { factor: "totp" as const, code };
    stepUp.mutate(req, {
      onSuccess: () => {
        setPassword("");
        setCode("");
        onSuccess();
      },
      onError: (e) => setError(apiErrorMessage(e)),
    });
  }

  const showPassword = requiredFactor === "password";
  const showTOTP = requiredFactor === "totp";

  return (
    <Modal title={t("step_up.title")} onClose={() => { if (!busy) onClose(); }} initialFocusRef={inputRef}>
      <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>
        <Trans i18nKey="step_up.hint" ns="common" values={{ factor: requiredFactor }} components={{ b: <b /> }} />
      </div>
      {showPassword && (
        <label className="form-row" style={{ marginBottom: 10 }}>
          {t("step_up.password")}
          <input
            ref={inputRef}
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
          />
        </label>
      )}
      {showTOTP && (
        <label className="form-row" style={{ marginBottom: 10 }}>
          {t("step_up.totp_code")}
          <input
            ref={inputRef}
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="123456"
            autoComplete="one-time-code"
            maxLength={8}
          />
        </label>
      )}
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      <div className="form-actions">
        <button className="btn" disabled={busy} onClick={onClose}>{t("step_up.cancel")}</button>
        <button
          className="btn primary"
          disabled={busy || (showPassword && !password) || (showTOTP && !code)}
          onClick={submit}
        >
          {busy ? t("step_up.verifying") : t("step_up.verify")}
        </button>
      </div>
    </Modal>
  );
}
