import { useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { formatGB, formatIdle } from "../lib/format";
import { useSpecialistSlotLog } from "../lib/queries";
import type { SpecialistBehavior, SpecialistSlot } from "../lib/types";

// BehaviorControl — the one setting every specialist has: Always on / On demand / Disabled (the same three words on the Specialists tab, the
// Models page and Voice & Speech). `onPick` is not called for the value that is already selected.
export function BehaviorControl({
  value,
  disabled,
  onPick,
  label,
}: {
  value: SpecialistBehavior;
  disabled?: boolean;
  onPick: (b: SpecialistBehavior) => void;
  label: string;
}) {
  const { t } = useTranslation("specialists");
  return (
    <div className="seg-ctl" role="group" aria-label={label}>
      {(["always_on", "on_demand", "disabled"] as const).map((b) => (
        <button key={b} aria-pressed={value === b} disabled={disabled} onClick={() => { if (value !== b) onPick(b); }}>
          {t(`behavior.${b}`)}
        </button>
      ))}
    </div>
  );
}

// SpecialistCard — one specialist slot (ADR-0022). Deliberately smaller and quieter than an LLM <Bay>: slot id, the behavior pill and a state dot
// on one line, the occupant, and one status line. Everything else opens on click: a plain sentence about the state, the Behavior control and the
// one thing you would do about a problem (Retry now, Start, Show log). There is no Stop and no Remove here: Disabled unloads and frees the slot.
// A slightly different (teal) tint keeps the two kinds of slot from reading alike.
export function SpecialistCard({
  slot,
  canAdmin,
  busy,
  onBehavior,
  onRetry,
  onUnload,
  onAdd,
}: {
  slot: SpecialistSlot;
  canAdmin: boolean;
  busy: boolean;
  onBehavior: (b: SpecialistBehavior) => void;
  onRetry: () => void;
  onUnload: () => void;
  onAdd: () => void;
}) {
  const { t } = useTranslation("specialists");
  const [open, setOpen] = useState(false);
  const [showLog, setShowLog] = useState(false);
  const log = useSpecialistSlotLog(open && showLog ? slot.slot : null);
  const empty = slot.state === "empty";
  const running = slot.state === "running";
  const failed = slot.state === "failed";
  const stopped = slot.state === "stopped";
  const starting = slot.state === "starting";
  const stopping = slot.state === "stopping";

  if (empty) {
    return (
      <div className="spec-card empty" data-slot={slot.slot}>
        <div className="spec-top">
          <span className="spec-id">{slot.label || slot.slot.toUpperCase()}</span>
          <span className="spec-dot idle" aria-label={t("state.empty")} />
        </div>
        <div className="spec-name dim">{t("state.free")}</div>
        {canAdmin && (
          <div className="spec-meta">
            <button className="btn-mini" onClick={onAdd}>{t("card.add")}</button>
          </div>
        )}
      </div>
    );
  }

  const behavior = (slot.policy || "on_demand") as SpecialistBehavior;
  const line = failed
    ? t("card.failed")
    : running
      ? `${formatGB(slot.memory_bytes, 1)} GB`
      : t(`state.${slot.state}`);

  function onKey(e: KeyboardEvent<HTMLDivElement>) {
    if (e.target === e.currentTarget && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      setOpen((v) => !v);
    }
  }

  // The plain sentence for the state, so nobody has to know what "attempt 4" means.
  const say = failed
    ? t("say.failed", { error: slot.error || t("say.unknown_error") })
    : stopping
      ? t("say.stopping")
      : stopped
      ? t("say.stopped")
      : starting
        ? t("say.starting")
        : behavior === "always_on"
          ? t("say.running_always_on")
          : t("say.running_on_demand");

  return (
    <div
      className={`spec-card ${failed ? "failed" : ""} ${open ? "open" : ""}`}
      data-slot={slot.slot}
      role="button"
      tabIndex={0}
      aria-expanded={open}
      onClick={(e) => { if (!(e.target as HTMLElement).closest(".spec-more")) setOpen((v) => !v); }}
      onKeyDown={onKey}
    >
      <div className="spec-top">
        <span className="spec-id">{slot.label || slot.slot.toUpperCase()}</span>
        <span className={`spec-pill ${behavior === "always_on" ? "on" : ""}`}>{t(`behavior.${behavior}`)}</span>
        <span className={`spec-dot ${failed ? "fail" : running ? "run" : starting || stopping ? "start" : "idle"}`} aria-label={t(`state.${slot.state}`)} />
      </div>
      <div className="spec-name" title={slot.occupant}>{slot.occupant}</div>
      <div className={`spec-meta ${failed ? "bad" : ""}`}>{line}</div>
      {open && (
        <div className="spec-more">
          <div className={`spec-say ${failed ? "bad" : ""}`}>
            {say}
            {failed && slot.attempts > 0 && <small>{t("say.retrying", { n: slot.attempts })}</small>}
          </div>
          <div className="spec-kv">
            <span>{t("card.serves")} <b>{slot.functions.map((f) => t(`function.${f}`)).join(", ") || "—"}</b></span>
            {running && <span>{t("card.memory")} <b>{formatGB(slot.memory_bytes, 1)} GB</b></span>}
            {running && behavior === "on_demand" && <span>{t("card.idle")} <b>{formatIdle(slot.idle_seconds)}</b></span>}
          </div>
          {canAdmin && !stopping && (
            <div className="spec-actions">
              <span className="spec-note">{t("behavior.label")}</span>
              <BehaviorControl value={behavior} disabled={busy} onPick={onBehavior} label={t("behavior.label")} />
              {(failed || stopped) && (
                <button className="btn-mini primary" disabled={busy} onClick={onRetry}>
                  {failed ? t("card.retry") : t("card.start")}
                </button>
              )}
              {(failed || starting) && (
                <button className="btn-mini" onClick={() => setShowLog((v) => !v)}>{showLog ? t("card.hide_log") : t("card.show_log")}</button>
              )}
              {running && behavior === "on_demand" && (
                <button className="btn-mini" disabled={busy} onClick={onUnload}>{t("card.unload_now")}</button>
              )}
            </div>
          )}
          {showLog && (
            <pre className="spec-log" aria-label={t("card.log")}>
              {log.isLoading ? t("card.log_loading") : log.data?.lines || t("card.log_empty")}
            </pre>
          )}
        </div>
      )}
    </div>
  );
}
