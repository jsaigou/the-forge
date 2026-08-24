import { useState } from "react";
import { ReservationModal } from "../components/ReservationModal";
import { SchedulerJobs } from "../components/SchedulerJobs";
import { SchedulerTunables } from "../components/SchedulerTunables";
import { dayLabel, formatClock, isSameDay } from "../lib/format";
import { useCancelReservation, useReservations, useSchedulerStatus, useStatus } from "../lib/queries";
import { useSession } from "../lib/session";

export function Scheduling() {
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
      <div className="eyebrow">Reservations · 7-day view</div>
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
                    onClick={() => canOperate && confirm(`Cancel reservation "${r.label}"?`) && cancel.mutate(r.label)}
                    title={canOperate ? "Click to cancel" : undefined}
                  >
                    <div className="rt">{formatClock(r.start)}–{formatClock(r.end)}</div>
                    <div className="rl">{r.label}</div>
                    <div className="rs">{r.scope === "bay" ? `${status.data?.slot_labels[r.bay ?? ""] ?? r.bay} · bay hold` : r.scope}</div>
                  </div>
                ))}
              </div>
            );
          })}
        </div>
      </div>
      {canOperate && status.data && (
        <button className="load-btn" style={{ margin: "14px 0 0", color: "var(--cool)" }} onClick={() => setShowModal(true)}>
          + New reservation
        </button>
      )}
      {showModal && status.data && <ReservationModal status={status.data} onClose={() => setShowModal(false)} />}

      {/* P3 track (forge/p3sched): cron-style forced loads — admin-defined
          jobs that fire sched.EnsureLoaded on a schedule. Sits directly
          below the reservations calendar, the other "reserve the box in
          advance" surface. */}
      <div className="eyebrow">Scheduled jobs</div>
      <SchedulerJobs />

      <div className="eyebrow">Smart queue · MCP</div>
      <div className="card">
        <h3><span className="tick" /> Live queue — one scheduler, many consumers</h3>
        {(schedulerStatus.data?.queue ?? []).length === 0 && <div className="empty-note">Queue is empty.</div>}
        {(schedulerStatus.data?.queue ?? []).map((t) => (
          <div className="qrow" key={t.ticket_id}>
            <span className="who">{t.requested_by}</span>
            <span className="want">{t.model}</span>
            <span className={`pos ${t.status === "loading" ? "run" : ""}`}>
              {t.status}{t.target_slot ? ` → ${status.data?.slot_labels[t.target_slot] ?? t.target_slot}` : ""}
            </span>
          </div>
        ))}
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 10, lineHeight: 1.5 }}>
          Idle bays auto-unload after the configured timeout when a queued request needs the memory. Reservations take
          precedence over interactive loads.
        </div>
      </div>

      <div className="eyebrow">Settings</div>
      <SchedulerTunables />
    </section>
  );
}
