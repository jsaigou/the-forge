import { useState } from "react";
import { apiErrorMessage } from "../lib/api";
import {
  useConfigCards,
  useCreateSchedulerJob,
  useDeleteSchedulerJob,
  useRunSchedulerJobNow,
  useSchedulerJobs,
  useUpdateSchedulerJob,
} from "../lib/queries";
import { useSession } from "../lib/session";
import type { SchedulerJob } from "../lib/types";

// SchedulerJobs — P3 track (forge/p3sched): the operator surface for
// cron-style forced model loads. Rendered on the Scheduling page below the
// reservations calendar; fires go through sched.EnsureLoaded with
// requested_by="cron:<name>", so they contend with a0/MCP/dashboard loads
// through the same queue. Mutations are admin (create/update/delete/toggle);
// run-now is operator, like the reservation routes it mirrors.
const SLOT_OPTIONS = ["a1", "a2", "a3", "a4"];

const CRON_HELP =
  '5 fields: min hour day-of-month month day-of-week — e.g. "0 3 * * *" = daily 03:00, "*/30 * * * *" = every half hour, "0 9-17 * * 1-5" = hourly 09:00–17:00 weekdays.';

function fmtWhen(iso: string | null) {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

export function SchedulerJobs() {
  const { canOperate, canAdmin } = useSession();
  const jobs = useSchedulerJobs();
  const configCards = useConfigCards("7d");
  const create = useCreateSchedulerJob();
  const update = useUpdateSchedulerJob();
  const del = useDeleteSchedulerJob();
  const runNow = useRunSchedulerJobNow();

  const [name, setName] = useState("");
  const [cron, setCron] = useState("");
  const [configName, setConfigName] = useState("");
  const [slot, setSlot] = useState("");
  const [error, setError] = useState<string | null>(null);

  if (jobs.isError) {
    return (
      <div className="card">
        <div className="empty-note">Operator role required to view scheduled jobs.</div>
      </div>
    );
  }

  const configs = (configCards.data?.cards ?? []).filter((c) => c.visibility === "visible");

  function submit() {
    setError(null);
    if (!name || !cron || !configName) {
      setError("Name, cron expression, and config are required.");
      return;
    }
    create.mutate(
      { name, cron, config_name: configName, slot: slot || null },
      {
        onSuccess: () => {
          setName("");
          setCron("");
          setConfigName("");
          setSlot("");
        },
        onError: (e) => setError(apiErrorMessage(e)),
      },
    );
  }

  function toggle(j: SchedulerJob) {
    update.mutate({
      id: j.id,
      job: { name: j.name, cron: j.cron, config_name: j.config_name, slot: j.slot, enabled: !j.enabled },
    });
  }

  return (
    <div className="card">
      <h3><span className="tick" />Scheduled jobs — forced loads at fixed times</h3>
      {(jobs.data?.jobs ?? []).length === 0 && (
        <div className="empty-note">No scheduled jobs defined.</div>
      )}
      {(jobs.data?.jobs ?? []).map((j) => (
        <div className="qrow" key={j.id}>
          <span className="who">{j.name}</span>
          <span className="want" title={CRON_HELP}>{j.cron} → {j.config_name}{j.slot ? ` @ ${j.slot}` : ""}</span>
          <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
            last {fmtWhen(j.last_run_at)} · next {fmtWhen(j.next_run_at)}
          </span>
          <span className="pos">
            {canAdmin && (
              <button
                className="btn"
                disabled={update.isPending}
                onClick={() => toggle(j)}
                title="Toggle enabled"
              >
                {j.enabled ? "Enabled" : "Disabled"}
              </button>
            )}
            {canOperate && (
              <button
                className="btn"
                style={{ color: "var(--cool)" }}
                disabled={runNow.isPending}
                onClick={() => confirm(`Run "${j.name}" now?`) && runNow.mutate(j.id)}
              >
                Run now
              </button>
            )}
            {canAdmin && (
              <button
                className="btn"
                style={{ color: "var(--crit)" }}
                disabled={del.isPending}
                onClick={() => confirm(`Delete job "${j.name}"?`) && del.mutate(j.id)}
              >
                Delete
              </button>
            )}
          </span>
        </div>
      ))}

      {canAdmin && (
        <>
          <div className="eyebrow" style={{ marginTop: 14 }}>New job</div>
          {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
          <div className="form-grid">
            <label className="form-row">
              Name
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder="nightly-batch" />
            </label>
            <label className="form-row" title={CRON_HELP}>
              Cron
              <input value={cron} onChange={(e) => setCron(e.target.value)} placeholder="0 3 * * *" />
            </label>
            <label className="form-row">
              Config
              <select value={configName} onChange={(e) => setConfigName(e.target.value)}>
                <option value="">select…</option>
                {configs.map((c) => (
                  <option key={c.id} value={c.name}>{c.model_name} · {c.name}</option>
                ))}
              </select>
            </label>
            <label className="form-row">
              Slot
              <select value={slot} onChange={(e) => setSlot(e.target.value)}>
                <option value="">any (scheduler chooses)</option>
                {SLOT_OPTIONS.map((s) => (
                  <option key={s} value={s}>{s}</option>
                ))}
              </select>
            </label>
          </div>
          <div style={{ fontSize: 11, color: "var(--text-mute)", margin: "6px 0 10", lineHeight: 1.5 }}>{CRON_HELP}</div>
          <div className="form-actions">
            <button className="btn primary" disabled={create.isPending} onClick={submit}>
              {create.isPending ? "Creating…" : "+ Add job"}
            </button>
          </div>
        </>
      )}
    </div>
  );
}
