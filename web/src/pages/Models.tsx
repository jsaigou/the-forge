import { useState } from "react";
import { RemoteOfferings } from "../components/RemoteOfferings";
import { UnifiedModelCarousel } from "../components/UnifiedModelCarousel";
import { useModelCards, useStatus } from "../lib/queries";

// Config gallery migration (ADR 0006, grilling Q2/Q6): the model gallery —
// family-grouped, full model detail, "understand all connections" — lives
// here as its own tab. The Console gallery is config-led (ConfigCardView,
// one card per Config); this page is model-led (ModelCardView, one card per
// Model).
//
// product/QA sprint, 2026-07-29: grouping is now two levels — genealogy (a
// vendor's own release lineage, e.g. Nemotron) nesting family (one
// generation within it, e.g. "Nemotron 3"). A model with no family is
// "Other" (always last); a family with no genealogy renders standalone
// under its own name at the top level (no genealogy heading to nest it
// under — e.g. "Swallow 32B" isn't part of a genealogy the way Nemotron's
// generations are).
//
// Unified carousel sprint (2026-08-14): the multiple per-family ModelDeck
// carousels were replaced with a single UnifiedModelCarousel that advances
// through family groups. Remote offerings moved under an "Offerings"
// sub-tab so the page has two clear modes: model cards and provider
// offerings.
export function Models() {
  const status = useStatus();
  const modelCards = useModelCards("7d");
  const [modelsTab, setModelsTab] = useState<"cards" | "offerings">("cards");

  const hasCards = (modelCards.data?.cards.length ?? 0) > 0;

  return (
    <section className="page">
      <div className="tabs" style={{ marginBottom: 16 }}>
        <button
          type="button"
          className={`tab${modelsTab === "cards" ? " active" : ""}`}
          onClick={() => setModelsTab("cards")}
        >
          Model Cards
        </button>
        <button
          type="button"
          className={`tab${modelsTab === "offerings" ? " active" : ""}`}
          onClick={() => setModelsTab("offerings")}
        >
          Offerings
        </button>
      </div>

      {modelsTab === "cards" ? (
        modelCards.data && status.data ? (
          hasCards ? (
            <UnifiedModelCarousel
              cards={modelCards.data.cards}
              status={status.data}
              displayCurrency={modelCards.data?.display_currency ?? "USD"}
            />
          ) : (
            <div className="empty-note">No models in the catalog yet.</div>
          )
        ) : (
          <div className="empty-note">Loading model registry…</div>
        )
      ) : (
        <RemoteOfferings />
      )}
    </section>
  );
}
