// settings/panels/Scheduling.tsx — Sprint 12 (was H) Phase 6. Two cards:
// the live scheduler.config editor — a straight mirror, not a rebuild, of
// the Scheduling page's own <SchedulerTunables> (shared qk.schedulerConfig
// query key keeps both in sync with no extra plumbing) — and a second,
// net-new card for infra.scheduler, the boot seed sched.Config falls back
// to when scheduler.config has never been set (apply="seed": writing it
// changes nothing on the running daemon).
//
// Exported as SchedulingSettings, not Scheduling, to avoid any confusion
// with pages/Scheduling.tsx (the page this section mirrors) even though the
// different directories mean no real import collision.
import { SchedulerTunables } from "../../components/SchedulerTunables";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { useSchedulerSeed, useUpdateSchedulerSeed } from "../../lib/queries";
import { Field } from "../Field";
import { SCHEDULING_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";

const F = Object.fromEntries(SCHEDULING_FIELDS.map((f) => [f.id, f]));

function SeedCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useSchedulerSeed();
  const update = useUpdateSchedulerSeed();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">Boot seed</div>
        <div className="card"><div className="empty-note">Operator role required to view the scheduler boot seed.</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">Boot seed</div>
        <div className="card"><div className="empty-note">Loading…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">Boot seed</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          These are the defaults the scheduler falls back to only if <b>scheduler.config</b> (the tunables
          above) has never been set on a fresh boot. Changing this has no effect on the running daemon.
        </div>
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["scheduling.seed.idle_unload_s"]} value={g.active.idle_unload_s} disabled={!canAdmin}
            onChange={(v) => g.setField("idle_unload_s", Number(v))} />
          <Field rec={F["scheduling.seed.small_job_token_threshold"]} value={g.active.small_job_token_threshold} disabled={!canAdmin}
            onChange={(v) => g.setField("small_job_token_threshold", Number(v))} />
          <Field rec={F["scheduling.seed.priority_jump_cap"]} value={g.active.priority_jump_cap} disabled={!canAdmin}
            onChange={(v) => g.setField("priority_jump_cap", Number(v))} />
          <Field rec={F["scheduling.seed.reservation_soon_min"]} value={g.active.reservation_soon_min} disabled={!canAdmin}
            onChange={(v) => g.setField("reservation_soon_min", Number(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

export function SchedulingSettings({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <div className="eyebrow" id="scheduling-live">Live tunables</div>
      <SchedulerTunables />
      <SeedCard canAdmin={canAdmin} />
    </>
  );
}
