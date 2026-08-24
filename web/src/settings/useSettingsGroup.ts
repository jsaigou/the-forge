import type { UseMutationResult } from "@tanstack/react-query";
import { useState } from "react";
import { apiErrorMessage } from "../lib/api";
import { useStepUpGate } from "../lib/useStepUpGate";

// settings/useSettingsGroup.ts — Sprint 12 (was H) Phase 6. The direct
// generalization of the near-identical active/draft/save body repeated at
// Settings.tsx:382-468/:474-604 (pre-Phase-5) and Scheduling.tsx:8-51
// (pre-Phase-0) — see settings/panels/Billing.tsx's Currency/CostSettingsPanel
// and components/SchedulerTunables.tsx for the two shapes this replaces.
// Every new Phase 6 panel (General/Routing/Monitoring/Scheduling) uses this
// instead of hand-rolling the same draft/save/gate dance a fourth+ time.
//
// Bakes in the 700ms delayed-close on success (Sprint K convention — gives
// SaveButton's ✓ flash time to paint before the form reverts to its
// read-only state) and step-up handling (useStepUpGate) for any group whose
// PUT is requireAssurance-gated.

export const SAVE_FLASH_MS = 700;

// Module-level last-success timestamp, shared across every panel, so
// SettingsSearch can defer a navigation that would unmount a panel
// mid-✓-flash (plan risk 1: search navigation is one of the three new
// unmount paths that must not fire within 700ms of a successful save).
let lastSaveSuccessAt = 0;
export function noteSaveSuccess() {
  lastSaveSuccessAt = Date.now();
}
export function msSinceLastSaveSuccess(): number {
  return Date.now() - lastSaveSuccessAt;
}

// V (the mutation's variables type) is inferred from whatever mutation hook
// is passed in — most Phase 6 endpoints take a Partial<T> patch body
// (V = Partial<T>), a couple take the full object (V = T, e.g. scheduler
// seed, which mirrors updateSchedulerConfig's existing full-replace
// contract). draft is always a full T (forms here always submit everything
// they displayed, never a hand-assembled partial), which is a valid V
// either way — hence the `as V` at the one call site below.
export function useSettingsGroup<T, V = T>(data: T | undefined, mutation: UseMutationResult<T, unknown, V>) {
  const [draft, setDraft] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const gate = useStepUpGate();
  const active = draft ?? data;
  const dirty = draft !== null;

  function setField<K extends keyof T>(key: K, value: T[K]) {
    if (!active) return;
    setDraft({ ...active, [key]: value });
  }

  function save(): Promise<void> {
    if (!draft) return Promise.resolve();
    setError(null);
    return new Promise<void>((resolve, reject) => {
      const attempt = () =>
        mutation.mutate(draft as V, {
          onSuccess: () => {
            noteSaveSuccess();
            setTimeout(() => setDraft(null), SAVE_FLASH_MS);
            resolve();
          },
          onError: (e) => {
            if (!gate.handle(e, attempt)) {
              setError(apiErrorMessage(e));
              reject(e);
            }
          },
        });
      attempt();
    });
  }

  function reset() {
    setDraft(null);
    setError(null);
  }

  return {
    active,
    draft,
    setField,
    dirty,
    save,
    reset,
    error,
    pending: mutation.isPending,
    isError: mutation.isError,
    gate,
  };
}
