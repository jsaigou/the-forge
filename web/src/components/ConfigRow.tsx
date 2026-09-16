import { useCatalogModelAliases } from "../lib/queries";
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

  // Operator feedback (2026-09-14): this is a separate rendering path from
  // ConfigCardView (the Models page reaches configs through here, not
  // <ConfigCardView>) and was missing the alias line entirely — an operator
  // browsing a model's configs here had no way to know an alias existed.
  // Same plain-text treatment as ConfigCardView/Bay, no tooltip gating it.
  const { data: modelAliases } = useCatalogModelAliases();
  const aliasedAs = (modelAliases ?? []).filter((a) => a.config_id === card.id);

  return (
    <div className="qrow" style={{ flexDirection: "column", alignItems: "stretch", gap: 2 }}>
      <div style={{ display: "flex", alignItems: "center" }}>
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
      </div>
      {aliasedAs.length > 0 && (
        <div style={{ fontSize: 10.5, color: "var(--text)" }}>
          Alias: {aliasedAs.map((a) => a.name).join(", ")}
        </div>
      )}
      {confirming && <LoadConfirmModal card={card} status={status} loadState={loadState} />}
    </div>
  );
}
