import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api";
import { appLocale } from "../lib/format";
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

function fmtWhen(iso: string | null) {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString(appLocale());
}

export function SchedulerJobs() {
  const { t } = useTranslation("scheduling");
  const cronHelp = t("jobs.cron_help");
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
        <div className="empty-note">{t("jobs.role_required")}</div>
      </div>
    );
  }

  const configs = (configCards.data?.cards ?? []).filter((c) => c.visibility === "visible");

  function submit() {
    setError(null);
    if (!name || !cron || !configName) {
      setError(t("jobs.validation_error"));
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
      <h3><span className="tick" />{t("jobs.title")}</h3>
      {(jobs.data?.jobs ?? []).length === 0 && (
        <div className="empty-note">{t("jobs.no_jobs")}</div>
      )}
      {(jobs.data?.jobs ?? []).map((j) => (
        <div className="qrow" key={j.id}>
          <span className="who">{j.name}</span>
          <span className="want" title={cronHelp}>{j.cron} → {j.config_name}{j.slot ? ` @ ${j.slot}` : ""}</span>
          <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
            {t("jobs.last_next", { last: fmtWhen(j.last_run_at), next: fmtWhen(j.next_run_at) })}
          </span>
          <span className="pos">
            {canAdmin && (
              <button
                className="btn"
                disabled={update.isPending}
                onClick={() => toggle(j)}
                title={t("jobs.toggle_title")}
              >
                {j.enabled ? t("jobs.enabled") : t("jobs.disabled")}
              </button>
            )}
            {canOperate && (
              <button
                className="btn"
                style={{ color: "var(--cool)" }}
                disabled={runNow.isPending}
                onClick={() => confirm(t("jobs.run_now_confirm", { name: j.name })) && runNow.mutate(j.id)}
              >
                {t("jobs.run_now")}
              </button>
            )}
            {canAdmin && (
              <button
                className="btn"
                style={{ color: "var(--crit)" }}
                disabled={del.isPending}
                onClick={() => confirm(t("jobs.delete_confirm", { name: j.name })) && del.mutate(j.id)}
              >
                {t("jobs.delete")}
              </button>
            )}
          </span>
        </div>
      ))}

      {canAdmin && (
        <>
          <div className="eyebrow" style={{ marginTop: 14 }}>{t("jobs.new_job_title")}</div>
          {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
          <div className="form-grid">
            <label className="form-row">
              {t("jobs.name")}
              <input value={name} onChange={(e) => setName(e.target.value)} placeholder={t("jobs.name_placeholder")} />
            </label>
            <label className="form-row" title={cronHelp}>
              {t("jobs.cron")}
              <input value={cron} onChange={(e) => setCron(e.target.value)} placeholder="0 3 * * *" />
            </label>
            <label className="form-row">
              {t("jobs.config")}
              <select value={configName} onChange={(e) => setConfigName(e.target.value)}>
                <option value="">{t("reservation_modal.select_ellipsis")}</option>
                {configs.map((c) => (
                  <option key={c.id} value={c.name}>{c.model_name} · {c.name}</option>
                ))}
              </select>
            </label>
            <label className="form-row">
              {t("jobs.slot")}
              <select value={slot} onChange={(e) => setSlot(e.target.value)}>
                <option value="">{t("jobs.slot_any")}</option>
                {SLOT_OPTIONS.map((s) => (
                  <option key={s} value={s}>{s}</option>
                ))}
              </select>
            </label>
          </div>
          <div style={{ fontSize: 11, color: "var(--text-mute)", margin: "6px 0 10", lineHeight: 1.5 }}>{cronHelp}</div>
          <div className="form-actions">
            <button className="btn primary" disabled={create.isPending} onClick={submit}>
              {create.isPending ? t("jobs.creating") : t("jobs.add_job")}
            </button>
          </div>
        </>
      )}
    </div>
  );
}
