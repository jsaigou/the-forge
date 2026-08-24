import { useState } from "react";
import { apiErrorMessage } from "../lib/api";
import { useStepUp } from "../lib/queries";

// StepUpModal — a minimal, reusable step-up authentication modal (password +
// TOTP). This is the first FE consumer of the step-up surface; FE-AUTH (FE-6)
// may extend it with WebAuthn/passkey support later. The backend endpoint
// POST /api/v1/auth/step-up is already implemented (auth_handlers.go).
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
  const stepUp = useStepUp();
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
    <div
      className="modal-backdrop"
      onClick={(e) => {
        // Sprint I fix — see DetailModal.tsx's identical comment.
        e.stopPropagation();
        if (!busy) onClose();
      }}
    >
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>Re-authenticate</h3>
        <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>
          This action requires <b>{requiredFactor}</b> assurance. Please re-authenticate to continue.
        </div>
        {showPassword && (
          <label className="form-row" style={{ marginBottom: 10 }}>
            Password
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
              autoFocus
            />
          </label>
        )}
        {showTOTP && (
          <label className="form-row" style={{ marginBottom: 10 }}>
            TOTP code
            <input
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="123456"
              autoComplete="one-time-code"
              autoFocus
              maxLength={8}
            />
          </label>
        )}
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-actions">
          <button className="btn" disabled={busy} onClick={onClose}>Cancel</button>
          <button
            className="btn primary"
            disabled={busy || (showPassword && !password) || (showTOTP && !code)}
            onClick={submit}
          >
            {busy ? "Verifying…" : "Verify"}
          </button>
        </div>
      </div>
    </div>
  );
}
