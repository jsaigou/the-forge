import { createPortal } from "react-dom";
import { formatDurationShort, formatGB } from "../lib/format";
import type { UseLoadConfigResult } from "../lib/useLoadConfig";
import type { ConfigCard, Status } from "../lib/types";

// The confirm-before-load/evict dialog, extracted alongside useLoadConfig
// (Sprint B dedup) — previously duplicated verbatim in ConfigCardView and
// ModelCardView's ConfigRow.
export function LoadConfirmModal({
  card,
  status,
  loadState,
}: {
  card: ConfigCard;
  status: Status;
  loadState: UseLoadConfigResult;
}) {
  const {
    state, slotKeys, emptySlot, evictTarget, setEvictTarget, loadError,
    shortfall, evictChoice, toggleEvictChoice, busy, doLoad, closeConfirm,
  } = loadState;

  // createPortal to document.body — same reasoning as DetailModal's portal:
  // the hero card (ModelHeroView) renders ConfigRows inside a perspective
  // scene, which becomes the containing block for position:fixed and would
  // trap (and clip) this backdrop inside the card.
  return createPortal(
    <div
      className="modal-backdrop"
      onClick={(e) => {
        // Sprint I fix — see DetailModal.tsx's identical comment. This
        // modal is nested inside ConfigCardView's own clickable .mcard, so
        // an un-stopped click reopened the card's detail view instantly.
        e.stopPropagation();
        closeConfirm();
      }}
    >
      <div className="modal" onClick={(e) => e.stopPropagation()}>
        <h3>Load config</h3>
        <div style={{ fontSize: 13, color: "var(--text-dim)", marginBottom: 14 }}>
          Load <b>{card.name}</b>
          {state === "evict-needed"
            ? " by evicting an occupied slot"
            : emptySlot
              ? ` onto ${status.slot_labels[emptySlot] ?? emptySlot}`
              : ""}
          .
        </div>
        {shortfall && (
          <div style={{ marginBottom: 14 }}>
            <div className="error-note" style={{ marginBottom: 10 }}>
              Not enough free memory
              {shortfall.needBytes != null && shortfall.freeBytes != null
                ? ` (needs ${formatGB(shortfall.needBytes)} GB, ${formatGB(shortfall.freeBytes)} GB free)`
                : ""}
              . Unload to make room? Longest-idle first; the suggested ones are pre-selected.
            </div>
            {shortfall.candidates.map((c) => (
              <label key={c.slot} className="form-row" style={{ flexDirection: "row", gap: 8, alignItems: "center" }}>
                <input
                  type="checkbox"
                  checked={evictChoice.includes(c.slot)}
                  disabled={busy}
                  onChange={() => toggleEvictChoice(c.slot)}
                />
                <span>
                  {status.slot_labels[c.slot] ?? c.slot} · {c.mode}
                  <span style={{ color: "var(--text-dim)" }}>
                    {" · "}
                    {c.idle_seconds == null ? "idle time unknown" : `idle ${formatDurationShort(c.idle_seconds)}`}
                    {c.footprint_bytes > 0 ? ` · ${formatGB(c.footprint_bytes)} GB` : ""}
                  </span>
                </span>
              </label>
            ))}
          </div>
        )}
        {state === "evict-needed" && !shortfall && (
          <label className="form-row" style={{ marginBottom: 14 }}>
            Slot to evict
            <select value={evictTarget} onChange={(e) => setEvictTarget(e.target.value)}>
              <option value="">select…</option>
              {slotKeys
                .filter((s) => status.slots[s])
                .map((s) => (
                  <option key={s} value={s}>
                    {status.slot_labels[s]} · {status.slots[s]}
                  </option>
                ))}
            </select>
          </label>
        )}
        {loadError && (
          <div className="error-note" style={{ marginBottom: 12 }}>
            {loadError}
          </div>
        )}
        <div className="form-actions">
          <button className="btn" disabled={busy} onClick={closeConfirm}>
            Cancel
          </button>
          <button
            className="btn primary"
            disabled={busy || (state === "evict-needed" && !shortfall && !evictTarget) || (!!shortfall && evictChoice.length === 0)}
            onClick={doLoad}
          >
            {busy ? "Loading…" : shortfall ? "Unload & Load" : state === "evict-needed" ? "Evict & Load" : "Load"}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
