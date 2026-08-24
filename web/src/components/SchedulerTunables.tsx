import { useState } from "react";
import { apiErrorMessage } from "../lib/api";
import { useSchedulerConfig, useUpdateSchedulerConfig } from "../lib/queries";
import { useSession } from "../lib/session";
import { useStepUpGate } from "../lib/useStepUpGate";
import type { SchedulerConfig } from "../lib/types";
import { SaveButton } from "./SaveButton";
import { StepUpModal } from "./StepUpModal";

// SchedulerTunables — Sprint 12 (was H) Phase 0. Extracted verbatim (plus
// step-up + error-state handling that the original never had) from
// Scheduling.tsx's private SchedulerSettings, so both the Scheduling page
// and Settings' new "scheduling" section can render the same live editor.
// Both consumers use useSchedulerConfig/useUpdateSchedulerConfig, which
// share the qk.schedulerConfig query key — React Query keeps the two in
// sync with no extra plumbing.
//
// The step-up gate was added here, not just left for later, because Sprint
// 12 Phase 1 adds requireAssurance(page.settings) to PUT /scheduler/config
// (an existing gating anomaly — it was the only Settings-adjacent mutation
// missing one). Without this, that backend change would make Save silently
// 403 on the Scheduling page with no way to recover.
export function SchedulerTunables() {
  const { canAdmin } = useSession();
  const cfg = useSchedulerConfig();
  const update = useUpdateSchedulerConfig();
  const gate = useStepUpGate();
  const [draft, setDraft] = useState<SchedulerConfig | null>(null);
  const [error, setError] = useState<string | null>(null);
  const active = draft ?? cfg.data;

  function field(key: keyof SchedulerConfig, label: string) {
    return (
      <label className="form-row">
        {label}
        <input
          type="number"
          value={active![key]}
          disabled={!canAdmin}
          onChange={(e) => setDraft({ ...active!, [key]: Number(e.target.value) })}
        />
      </label>
    );
  }

  function save() {
    if (!draft) return;
    setError(null);
    const doSave = () =>
      update.mutate(draft, {
        // Sprint K precedent (see Settings.tsx's Currency/Cost panels):
        // delayed close so SaveButton's ✓ flash has time to paint.
        onSuccess: () => setTimeout(() => setDraft(null), 700),
        onError: (e) => { if (!gate.handle(e, save)) setError(apiErrorMessage(e)); },
      });
    doSave();
  }

  if (cfg.isError) {
    return <div className="card"><div className="empty-note">Operator role required to view scheduler settings.</div></div>;
  }
  if (!active) {
    return <div className="card"><div className="empty-note">Loading scheduler settings…</div></div>;
  }

  return (
    <>
      <div className="card">
        <h3><span className="tick" />Scheduler tunables</h3>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-grid">
          {field("idle_unload_s", "Idle unload (s)")}
          {field("small_job_token_threshold", "Small-job token threshold")}
          {field("priority_jump_cap", "Priority jump cap")}
          {field("reservation_soon_min", "Reservation-soon window (min)")}
        </div>
        {canAdmin && draft && (
          <div className="form-actions">
            <button className="btn" onClick={() => { setDraft(null); setError(null); }}>Reset</button>
            <SaveButton pending={update.isPending} isError={update.isError} onClick={save} />
          </div>
        )}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}
