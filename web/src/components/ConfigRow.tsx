import { useLoadConfig } from "../lib/useLoadConfig";
import { useSession } from "../lib/session";
import type { ConfigCard, Status } from "../lib/types";
import { LoadConfirmModal } from "./LoadConfirmModal";

// ConfigRow — one compact, individually-loadable row for a config belonging
// to a model. Extracted from ModelCardView (Sprint B: the config list moved
// out of the model card face into ModelDetailView) — deliberately not a
// nested <ConfigCardView> (that would repeat the model's own icon/
// description/capabilities once per config, which is the whole reason
// "Models are the parent unit" — the parent view already shows that
// context once; see registry.Card.ID's doc comment on why model_id is a
// string that cross-references ConfigCard.model_id).
export function ConfigRow({ card, status }: { card: ConfigCard; status: Status }) {
  const { canOperate } = useSession();
  const loadState = useLoadConfig(card, status);
  const { state, activeSlot, busy, openConfirm, confirming } = loadState;

  return (
    <div className="qrow" style={{ alignItems: "center" }}>
      <span className="want" style={{ flex: "1 1 auto", minWidth: 0, fontFamily: "var(--mono)", fontSize: 12 }}>
        {card.name}
        {card.is_default && <span className="chip" style={{ marginLeft: 6, fontSize: 9.5 }}>default</span>}
      </span>
      <span style={{ width: 60, fontFamily: "var(--mono)", fontSize: 11, color: "var(--text-mute)" }}>
        {Math.round(card.n_ctx / 1024)}k
      </span>
      {!canOperate ? null : state === "loaded" ? (
        <button className="go loaded" style={{ fontSize: 11, padding: "5px 10px" }} disabled>
          Loaded{activeSlot ? ` · ${status.slot_labels[activeSlot]}` : ""}
        </button>
      ) : state === "loading" ? (
        <button className="go loaded loading-glare" style={{ fontSize: 11, padding: "5px 10px" }} disabled>
          Loading…
        </button>
      ) : (
        <button
          className="go"
          style={{ fontSize: 11, padding: "5px 10px" }}
          disabled={busy}
          onClick={openConfirm}
        >
          {state === "evict-needed" ? "Evict & load" : "Load"}
        </button>
      )}
      {confirming && <LoadConfirmModal card={card} status={status} loadState={loadState} />}
    </div>
  );
}
