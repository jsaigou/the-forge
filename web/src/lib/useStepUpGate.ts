import { useState } from "react";
import { ApiError } from "./api";

// useStepUpGate — Sprint 12 (was H) Phase 0. Lifted verbatim from
// SecurityPanel.tsx's original private copy (FE-6, docs/v5-sprint0-auth-design.md
// §6) so every settings section that mutates a `requireAssurance`-gated
// resource can share one implementation instead of hand-rolling its own.
//
// A mutation guarded by `requireAssurance(...)` on the backend returns 403
// `{error:"step_up_required", required:"password"|"totp"}` when the caller's
// session assurance is below the resource's configured minimum. `handle`
// recognizes that shape, opens a <StepUpModal> for the right factor, and
// remembers the caller's own retry closure; `onSuccess` (wired to the modal's
// onSuccess) re-runs it once step-up completes. Any other error is left for
// the caller's own error state.
export function useStepUpGate() {
  const [open, setOpen] = useState(false);
  const [factor, setFactor] = useState<"password" | "totp">("password");
  const [retry, setRetry] = useState<(() => void) | null>(null);

  function handle(e: unknown, retryFn: () => void): boolean {
    if (e instanceof ApiError && e.status === 403) {
      const body = e.body as { error?: string; required?: string } | null;
      if (body?.error === "step_up_required") {
        setFactor(body.required === "totp" ? "totp" : "password");
        setRetry(() => retryFn);
        setOpen(true);
        return true;
      }
    }
    return false;
  }

  function onSuccess() {
    setOpen(false);
    if (retry) retry();
  }

  return { open, factor, handle, onSuccess, onClose: () => setOpen(false) };
}
