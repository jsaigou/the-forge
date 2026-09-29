import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ReservationModal } from "../components/ReservationModal";
import { SchedulerJobs } from "../components/SchedulerJobs";
import { SchedulerTunables } from "../components/SchedulerTunables";
import { dayLabel, formatClock, isSameDay } from "../lib/format";
import { useCancelReservation, useReservations, useSchedulerStatus, useStatus } from "../lib/queries";
import { useSession } from "../lib/session";

export function Scheduling() {
  const { t } = useTranslation("scheduling");
  const status = useStatus();
  const reservations = useReservations();
  const schedulerStatus = useSchedulerStatus();
  const cancel = useCancelReservation();
  const { canOperate } = useSession();
  const [showModal, setShowModal] = useState(false);

  const today = new Date();
  const days = Array.from({ length: 7 }, (_, i) => {
    const d = new Date(today);
    d.setDate(d.getDate() + i);
    return d;
  });

  return (
    <section className="page">
      <div className="eyebrow">{t("calendar.title")}</div>
      <div className="calwrap">
        <div className="cal">
          {days.map((day) => {
            const { dn, dd } = dayLabel(day);
            const dayReservations = (reservations.data?.reservations ?? []).filter((r) =>
              isSameDay(new Date(r.start), day),
            );
            return (
              <div className={`calday ${isSameDay(day, today) ? "today" : ""}`} key={day.toISOString()}>
                <div className="dh">
                  <span className="dn">{dn}</span>
                  <span className="dd">{dd}</span>
                </div>
                {dayReservations.map((r) => (
                  <div
                    className={`resblk ${r.scope === "whole_box" ? "box" : ""}`}
                    key={r.label}
                    onClick={() => canOperate && confirm(t("calendar.cancel_confirm", { label: r.label })) && cancel.mutate(r.label)}
                    title={canOperate ? t("calendar.cancel_title") : undefined}
                  >
                    <div className="rt">{formatClock(r.start)}–{formatClock(r.end)}</div>
                    <div className="rl">{r.label}</div>
                    <div className="rs">{r.scope === "bay" ? t("calendar.bay_hold_suffix", { slot: status.data?.slot_labels[r.bay ?? ""] ?? r.bay }) : r.scope}</div>
                  </div>
                ))}
              </div>
            );
          })}
        </div>
      </div>
      {canOperate && status.data && (
        <button className="load-btn" style={{ margin: "14px 0 0", color: "var(--cool)" }} onClick={() => setShowModal(true)}>
          {t("calendar.new_reservation")}
        </button>
      )}
      {showModal && status.data && <ReservationModal status={status.data} onClose={() => setShowModal(false)} />}

      {/* P3 track (forge/p3sched): cron-style forced loads — admin-defined
          jobs that fire sched.EnsureLoaded on a schedule. Sits directly
          below the reservations calendar, the other "reserve the box in
          advance" surface. */}
      <div className="eyebrow">{t("scheduled_jobs_title")}</div>
      <SchedulerJobs />

      <div className="eyebrow">{t("smart_queue.title")}</div>
      <div className="card">
        <h3><span className="tick" /> {t("smart_queue.heading")}</h3>
        {(schedulerStatus.data?.queue ?? []).length === 0 && <div className="empty-note">{t("smart_queue.empty")}</div>}
        {(schedulerStatus.data?.queue ?? []).map((ticket) => (
          <div className="qrow" key={ticket.ticket_id}>
            <span className="who">{ticket.requested_by}</span>
            <span className="want">{ticket.model}</span>
            <span className={`pos ${ticket.status === "loading" ? "run" : ""}`}>
              {ticket.status}{ticket.target_slot ? ` → ${status.data?.slot_labels[ticket.target_slot] ?? ticket.target_slot}` : ""}
            </span>
          </div>
        ))}
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 10, lineHeight: 1.5 }}>
          {t("smart_queue.note")}
        </div>
      </div>

      <div className="eyebrow">{t("settings_title")}</div>
      <SchedulerTunables />
    </section>
  );
}
