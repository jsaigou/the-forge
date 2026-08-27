import { useEffect, useState } from "react";
import { formatGB, formatIdle } from "../lib/format";
import { useUnloadSlot } from "../lib/queries";
import { creatorIconSlug } from "../lib/creatorIcon";
import { useSession } from "../lib/session";
import type { ConfigCard, Reservation, SchedulerStatus, Status } from "../lib/types";
import { BadgeIcon } from "./BadgeIcon";
import { CopyButton } from "./CopyButton";
import { Icon } from "./Icon";
import { SquareSnake } from "./SquareSnake";

// Sprint K: unload staged progress. The prior investigation found no
// removable delay behind a slow unload — it's genuine systemd + GTT-drain
// teardown time (see docs/pitfalls.md) — so the fix here is honest,
// elapsed-aware feedback rather than a claimed speedup. Unlike profiling,
// the backend has no real phase events for unload; this is a client-side
// elapsed-time heuristic against known teardown behavior, not a fabricated
// progress bar.
function unloadStageLabel(elapsedS: number): string {
  if (elapsedS < 10) return "Stopping service…";
  if (elapsedS < 30) return "Waiting for GTT memory to drain…";
  return "Still draining — large models can take several minutes…";
}

// Ticks once a second so the elapsed-time label above actually counts up
// live, rather than being frozen at whatever value the last 15s status
// poll happened to catch.
function useNowSeconds(active: boolean): number {
  const [now, setNow] = useState(() => Date.now() / 1000);
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => setNow(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

interface BayProps {
  slotKey: string;
  status: Status;
  schedulerStatus: SchedulerStatus | undefined;
  // Epoch-ms of the last successful scheduler-status fetch (react-query
  // dataUpdatedAt). Used to interpolate the idle counter between polls so
  // seconds advance smoothly instead of jumping ~10s at a time.
  schedulerUpdatedAt?: number;
  configCards: ConfigCard[] | undefined;
  reservations: Reservation[] | undefined;
  onLoadClick: () => void;
}

function activeOrNextReservation(reservations: Reservation[] | undefined, slotKey: string): { r: Reservation; active: boolean } | null {
  if (!reservations) return null;
  const now = Date.now();
  const forBay = reservations.filter((r) => r.scope === "bay" && r.bay === slotKey);
  const active = forBay.find((r) => new Date(r.start).getTime() <= now && now < new Date(r.end).getTime());
  if (active) return { r: active, active: true };
  const upcoming = forBay
    .filter((r) => new Date(r.start).getTime() > now)
    .sort((a, b) => new Date(a.start).getTime() - new Date(b.start).getTime())[0];
  return upcoming ? { r: upcoming, active: false } : null;
}

export function Bay({ slotKey, status, schedulerStatus, schedulerUpdatedAt, configCards, reservations, onLoadClick }: BayProps) {
  const { canOperate } = useSession();
  const unload = useUnloadSlot();

  const slotLabel = status.slot_labels[slotKey] ?? slotKey.toUpperCase();
  const mode = status.slots[slotKey] ?? null;
  const loading = status.slot_loading[slotKey];
  const unloading = status.slot_unloading[slotKey];
  const modeInfo = mode ? status.modes_available[mode] : undefined;
  const card = mode ? configCards?.find((c) => c.name === mode) : undefined;
  const idleS = schedulerStatus?.idle_seconds[slotKey];
  const res = activeOrNextReservation(reservations, slotKey);
  const nowS = useNowSeconds((unloading?.in_progress ?? false) || idleS != null);

  if (loading?.in_progress) {
    return (
      <div className="bay">
        <span className="statusdot dot-busy" />
        <span className="slotid">{slotLabel}</span>
        <div style={{ margin: "auto 0", textAlign: "center" }}>
          <div style={{ fontSize: 13 }}>Loading {loading.mode}…</div>
        </div>
      </div>
    );
  }

  if (unloading?.in_progress) {
    const elapsedS = unloading.started_at != null ? Math.max(0, nowS - unloading.started_at) : 0;
    return (
      <div className="bay">
        <span className="statusdot dot-busy" />
        <span className="slotid">{slotLabel}</span>
        <div style={{ margin: "auto 0", textAlign: "center" }}>
          <div style={{ fontSize: 13 }}>Unloading {unloading.mode}…</div>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 4 }}>
            {unloadStageLabel(elapsedS)} ({Math.round(elapsedS)}s)
          </div>
        </div>
      </div>
    );
  }

  if (!mode) {
    if (res) {
      return (
        <div className="bay reserved empty">
          <span className="statusdot dot-res" />
          <span className="slotid">{slotLabel}</span>
          <div style={{ margin: "auto 0" }}>
            <span className="chip rocm" style={{ marginBottom: 8, display: "inline-block" }}>
              {res.active ? "reserved · active" : "reserved"}
            </span>
            <div style={{ fontSize: 12.5, color: "var(--text-dim)" }}>{res.r.label}</div>
            <div style={{ fontFamily: "var(--mono)", fontSize: 11, color: "var(--cool)", marginTop: 4 }}>
              {new Date(res.r.start).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })}
              {" – "}
              {new Date(res.r.end).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
            </div>
          </div>
        </div>
      );
    }
    return (
      <div className="bay empty">
        <span className="statusdot dot-empty" />
        <span className="slotid">{slotLabel}</span>
        <div style={{ margin: "auto 0" }}>
          <div style={{ fontSize: 13 }}>Bay free</div>
          {canOperate && (
            <button className="load-btn" data-tour-id="bay-load" onClick={onLoadClick}>
              + Load model
            </button>
          )}
        </div>
      </div>
    );
  }

  // Sprint K: active-LLM indicator. status.slot_activity is the poll/
  // reconnect source of truth (statusResponse.slot_activity), kept fresh
  // between polls by the low-latency slot:activity SSE merge (lib/sse.ts).
  const active = status.slot_activity[slotKey] ?? false;
  // Sprint S-B/S-C: who is consuming this slot right now (e.g.
  // "Examplehost (OpenCode)") — empty when idle or unknown. Backend populates
  // from a0's live request attribution; SMITH is rendered bold-caps.
  const consumer = status.slot_consumers?.[slotKey] ?? "";

  // Sprint K: profiling warning stripe. The backend's `profiling` field
  // only carries {running, mode} — it doesn't say which slots a run will
  // actually evict (that's a live decision inside profile.go, not
  // pre-computed). ProfilingPanel's own warning text already tells the
  // operator every loaded slot is at risk during a run ("other running
  // models will be evicted... may interrupt in-flight requests on evicted
  // slots"), so the honest, simple signal here is every currently-loaded
  // bay while a run is in progress — not a guess at which specific slot.
  const profilingHold = status.profiling?.running ?? false;

  // Idle counter interpolation: idle_seconds is a polled value (10s cadence),
  // so rendering it directly makes the readout jump several seconds at a
  // time. Add the wall-clock elapsed since the fetch (nowS − fetch time) so
  // it ticks up once a second, and reset to 0 while actively generating — a
  // busy bay isn't idle. Re-syncs for free on every poll (idleS jumps
  // forward ~10s, elapsed resets to ~0, the sum stays continuous).
  const idleDisplayS = active
    ? 0
    : idleS != null && schedulerUpdatedAt
      ? idleS + Math.max(0, nowS - schedulerUpdatedAt / 1000)
      : idleS;

  const makerSlug = card?.creator ? creatorIconSlug(card.creator) : "";

  return (
    <div className={`bay ${profilingHold ? "profiling-hold" : ""}`.trim()}>
      {/* Operator feedback 2026-08-25 (slots pass): the green corner light
          didn't earn its keep — the eject button takes its corner (absolute
          top-right). Slot name renders 50% larger with the model + maker
          icons inline at matching height; config name sits below the name
          row; busy/consumer line sits below that (consumer left, activity
          right). */}
      {canOperate && (
        <button
          className="icon-btn action bay-eject"
          style={{ position: "absolute", top: 10, right: 10, width: 24, height: 24, fontSize: 11 }}
          title="Unload"
          disabled={unload.isPending}
          onClick={() => unload.mutate(slotKey)}
        >
          ⏏
        </button>
      )}
      <div className="slot-head" style={{ display: "flex", alignItems: "center", gap: 7, paddingRight: 30 }}>
        <span className="slotid">{slotLabel}</span>
        {card ? <Icon slug={card.logo} slugDark={card.logo_dark} name={card.model_name} sm /> : <span className="logo sm">{mode.charAt(0).toUpperCase()}</span>}
        {makerSlug && <Icon slug={makerSlug} name={card?.creator ?? ""} sm />}
      </div>
      {/* The bay's headline is the loaded CONFIG name (status.slots value —
          what you load/unload), not the underlying model_name.
          Mobile F8: single-line ellipsis instead of wrapping long names. */}
      <div className="model" style={{ display: "flex", alignItems: "center", gap: 4, marginTop: 2 }}>
        <span className="mn" title={mode ?? undefined}>
          {mode}
        </span>
        <CopyButton text={mode ?? ""} title="Copy config name" sm />
      </div>
      <div className="state" style={{ color: "var(--ok)", display: "flex", alignItems: "center", gap: 8 }}>
        {active && consumer && consumer === "SMITH" ? (
          <span style={{ fontWeight: 700, letterSpacing: ".08em" }}>SMITH</span>
        ) : active && consumer ? (
          <span>{consumer}</span>
        ) : null}
        <span style={{ marginLeft: "auto", display: "inline-flex", alignItems: "center", gap: 6 }}>
          {active ? (
            <>
              <SquareSnake title="actively generating" />
              <span>active</span>
            </>
          ) : (
            <span>● loaded · idle {formatIdle(idleDisplayS)}</span>
          )}
          <span className={`chip ${modeInfo?.backend === "rocm" ? "rocm" : "vulkan"}`}>{modeInfo?.backend ?? "vulkan"}</span>
        </span>
      </div>
      <div className="readout">
        <div className="ro">
          <div className="v">{card?.derived.memory_req_bytes != null ? formatGB(card.derived.memory_req_bytes) : formatGB(undefined)} <small>GB</small></div>
          <div className="k">footprint</div>
        </div>
        <div className="ro">
          <div className="v">{modeInfo?.context ? `${Math.round(modeInfo.context / 1024)}k` : "—"}</div>
          <div className="k">context</div>
        </div>
        <div className="ro ro-badges">
          {/* product/QA sprint, 2026-07-29: render the actual badges (was
              just a count) — consistent with ConfigCardView, which already
              showed the real icons for the same card. "uses · 7d" cell
              removed (not useful in this context). Console usability pass,
              same day: this cell spans the readout's full width (ro-badges)
              — squeezed into a 1fr third-column, text-pill badges stacked
              one per line and tripled a loaded bay's height. */}
          <div className="v" style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
            {card?.badges && card.badges.length > 0
              ? card.badges.map((b) => <BadgeIcon key={b.id} badge={b} />)
              : "—"}
          </div>
          <div className="k">badges</div>
        </div>
      </div>
    </div>
  );
}
