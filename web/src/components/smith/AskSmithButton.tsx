import { useAskSmithAffordance } from "../../lib/queries";
import type { SmithChatContext } from "../../lib/types";
import { HammerIcon } from "../icons/HammerIcon";

// AskSmithButton — Sprint S3-Web (§2.3, R5). The one-click "Ask smith"
// affordance on every error-bearing row (Console alert chips, Diagnostics
// notification/finding/investigation rows). Clicking routes to #help/smith
// and seeds a context card via the additive `context` array on
// POST /api/v1/smith/chat — the server composes the seed message itself, so
// the row never string-formats evidence.
//
// Alignment is the call site's job (each row already has a right-aligned
// trailing element — a timestamp or deep-link — that carries the
// marginLeft:auto); this button just sits compactly next to it. The
// `context` prop is the already-shaped {code, message, source, at} item
// (source is the owning check id / notification code when known, so smith's
// classifier can resolve it — see classifyContextItems in intents.go).
export function AskSmithButton({
  context,
  title = "Ask smith about this",
}: {
  context: SmithChatContext[];
  title?: string;
}) {
  const askSmith = useAskSmithAffordance();
  return (
    <button
      className="tab"
      title={title}
      aria-label={title}
      style={{
        fontSize: 10,
        padding: "2px 7px",
        display: "inline-flex",
        alignItems: "center",
        gap: 4,
        cursor: "pointer",
      }}
      onClick={() => askSmith(context)}
    >
      <HammerIcon size={12} />
      ask smith
    </button>
  );
}
