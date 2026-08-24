import { useState } from "react";
import { apiErrorMessage } from "./api";

// useErrorState — Sprint 12 (was H) Phase 0. Lifted verbatim from
// CatalogPanel.tsx's original private copy (used by all 8 of its sub-tab
// sections) so other settings sections can share the same "one error string,
// set from any caught mutation error" idiom instead of re-declaring it.
export function useErrorState() {
  const [error, setError] = useState<string | null>(null);
  function showError(e: unknown) {
    setError(apiErrorMessage(e));
  }
  return { error, showError, clearError: () => setError(null) };
}
