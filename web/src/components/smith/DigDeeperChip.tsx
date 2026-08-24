// DigDeeperChip — S2-Web (docs/v5-smith-experience.md §2.2). The universal
// "dig deeper" escalation affordance on every fast answer. The backend
// (answers.go:73 `digDeeperChip`) appends
//   \n\n_Dig deeper — ask a follow-up and smith will think it through._
// as markdown to every fast answer's text. stripDigDeeper splits that known
// suffix off the answer body so MessageBubble can render the Markdown without
// it, and this component renders the chip button below the answer instead —
// making escalation always one click (§2.2) without ever being a concept.
//
// The chip is a known constant (not user-authored), so matching it exactly
// avoids stripping legitimate italic text. If the suffix isn't present
// (e.g. a reasoning-tier answer never gets the chip), hasDigDeeper is false
// and no chip renders.

const DIG_DEEPER_RE = /\n*_Dig deeper \u2014 ask a follow-up and smith will think it through\._\s*$/;

// stripDigDeeper splits a fast answer's text into the clean answer body and
// whether the dig-deeper chip suffix is present.
export function stripDigDeeper(text: string): { answer: string; hasDigDeeper: boolean } {
  const m = text.match(DIG_DEEPER_RE);
  if (!m || m.index == null) return { answer: text, hasDigDeeper: false };
  return { answer: text.slice(0, m.index).replace(/\n+$/, ""), hasDigDeeper: true };
}

export function DigDeeperChip({ onClick }: { onClick: () => void }) {
  return (
    <button className="smith-dig-deeper" type="button" onClick={onClick}>
      Dig deeper — ask a follow-up
    </button>
  );
}
