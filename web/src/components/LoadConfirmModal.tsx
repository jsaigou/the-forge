import { createPortal } from "react-dom";
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
  const { state, slotKeys, emptySlot, evictTarget, setEvictTarget, loadError, busy, doLoad, closeConfirm } = loadState;

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
        {state === "evict-needed" && (
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
            disabled={busy || (state === "evict-needed" && !evictTarget)}
            onClick={doLoad}
          >
            {busy ? "Loading…" : state === "evict-needed" ? "Evict & Load" : "Load"}
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
