import { useEffect, useRef, useState } from "react";

// useSavedFlash — Sprint K. There is no toast system in this app; a
// successful save today is signalled only by the form collapsing (or, for
// the Currency/Cost panels, the whole Save row vanishing). This hook backs
// SaveButton.tsx's brief "✓ Saved" state: true for ~1.4s right after a
// mutation's `pending` flag drops from true to false with no error,
// then false again. Never fires on the initial mount (pending starts
// false, so there is no true→false edge to observe) or on a failed save.
export function useSavedFlash(pending: boolean, isError: boolean): boolean {
  const prevPending = useRef(pending);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    const wasPending = prevPending.current;
    prevPending.current = pending;
    if (wasPending && !pending && !isError) {
      setSaved(true);
      const t = setTimeout(() => setSaved(false), 1400);
      return () => clearTimeout(t);
    }
  }, [pending, isError]);

  return saved;
}
