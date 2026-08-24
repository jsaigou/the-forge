import { useCallback, useEffect, useState } from "react";
import { useSession } from "../lib/session";

// Guided first-run tour trigger (P4). The tour shows once per browser:
// localStorage `forge.onboarding.done` is the per-browser dismissal flag,
// so returning operators (and every viewer role below operator) never see
// it again unless they explicitly replay it from Help.
//
// Zero network cost by design: the gate reads only the already-resolved
// session context (App renders strictly inside SessionProvider, so
// useSession here is always post-load — "session loaded" is structural)
// plus localStorage. No queries are created, polled, or enabled by this
// hook; the tour itself is pure static content.

const STORAGE_KEY = "forge.onboarding.done";
const REOPEN_EVENT = "forge.onboarding.reopen";

// Re-entry point for Help's "Replay onboarding tour" button. Help has no
// prop channel back up to App (it only receives sub/onSubChange), so the
// replay rides a plain window Event instead of threading state through the
// tab router — dependency-free, and the listener lives in this hook's
// single instance mounted by App.
export function reopenTour(): void {
  window.dispatchEvent(new Event(REOPEN_EVENT));
}

export function useOnboarding(): { open: boolean; dismiss: () => void } {
  const [open, setOpen] = useState(false);
  const session = useSession();

  // Show when ALL of: flag absent AND session loaded AND admin/operator.
  useEffect(() => {
    if (session.canOperate && localStorage.getItem(STORAGE_KEY) === null) {
      setOpen(true);
    }
  }, [session.canOperate]);

  useEffect(() => {
    function onReopen() {
      setOpen(true);
    }
    window.addEventListener(REOPEN_EVENT, onReopen);
    return () => window.removeEventListener(REOPEN_EVENT, onReopen);
  }, []);

  const dismiss = useCallback(() => {
    try {
      localStorage.setItem(STORAGE_KEY, new Date().toISOString());
    } catch {
      // Private-mode / storage-disabled browsers: still close for this
      // session; the flag just won't persist and the tour may re-show on
      // next load, which beats crashing the shell over a preference.
    }
    setOpen(false);
  }, []);

  return { open, dismiss };
}
