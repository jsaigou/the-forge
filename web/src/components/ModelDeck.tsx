import { useState } from "react";
import { sortModelCardsByUsage } from "../lib/modelSort";
import type { ModelCard, Status } from "../lib/types";
import { ModelCardView } from "./ModelCardView";

// ModelDeck (Phase 3, pre-release feedback sprint) — a family's model cards
// render as an overlapping deck, fanned like held cards, instead of the
// stacked grid Models.tsx used to render inline. Tapping the deck spreads it
// into the same `.gallery.portrait` row the grid already used, in place.
//
// A family with exactly one model gets no deck affordance at all — a stack
// of one that demands a click to reveal the one card underneath is pure
// friction, and it's the common case (most families on the live catalog
// have exactly one model).
//
// Accessibility is the part that has to be right, since this trades it for
// the visual: collapsed, the WHOLE deck — including the fully-visible front
// card — is one native <button class="deck-face">. Every ModelCardView
// inside it renders with interactive={false} (no role/tabIndex/onClick, no
// nested CopyButton) so nothing behind the button is independently
// reachable by click or Tab, and so no interactive content ends up nested
// inside a real <button> (HTML forbids that content model outright).
// Expanded, cards are the untouched, fully-interactive grid.
const PEEK_CAP = 3;

export function ModelDeck({
  cards,
  status,
  displayCurrency,
}: {
  cards: ModelCard[];
  status: Status;
  displayCurrency: string;
}) {
  const sorted = sortModelCardsByUsage(cards);
  const [open, setOpen] = useState(false);

  if (sorted.length <= 1) {
    return (
      <div className="gallery portrait">
        {sorted.map((card) => (
          <ModelCardView key={card.id} card={card} status={status} displayCurrency={displayCurrency} />
        ))}
      </div>
    );
  }

  const familyLabel = sorted[0].family || "family";

  if (open) {
    return (
      <div>
        <button type="button" className="deck-collapse" onClick={() => setOpen(false)}>
          ▲ Collapse {familyLabel} ({sorted.length})
        </button>
        <div className="gallery portrait deck-expand-in">
          {sorted.map((card) => (
            <ModelCardView key={card.id} card={card} status={status} displayCurrency={displayCurrency} />
          ))}
        </div>
      </div>
    );
  }

  const peek = sorted.slice(0, PEEK_CAP);
  const extra = sorted.length - peek.length;

  return (
    <button
      type="button"
      className="deck-face"
      aria-expanded={false}
      aria-label={`${familyLabel}: ${sorted.length} models, starting with ${peek[0].name}. Expand to see all.`}
      onClick={() => setOpen(true)}
    >
      {peek.map((card, i) => (
        <span
          key={card.id}
          className={i === 0 ? "deck-slide deck-slide-front" : "deck-slide"}
          // i===0 sizes the deck via normal flow (position set in CSS); the
          // rest fan out behind it. Computed inline rather than via a CSS
          // custom property so the transform math stays plain TS, not
          // calc() — one less TS/CSSProperties cast to carry.
          style={i === 0 ? undefined : { transform: `translate(${i * 9}px, ${i * 7}px) rotate(${i * 2.5}deg)`, zIndex: PEEK_CAP - i }}
          inert={i > 0}
        >
          <ModelCardView card={card} status={status} displayCurrency={displayCurrency} interactive={false} />
          {i === peek.length - 1 && extra > 0 && <span className="deck-more chip">+{extra}</span>}
        </span>
      ))}
    </button>
  );
}
