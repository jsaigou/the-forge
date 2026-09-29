import { useTranslation } from "react-i18next";
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
  title,
}: {
  context: SmithChatContext[];
  title?: string;
}) {
  const { t } = useTranslation("common");
  const askSmith = useAskSmithAffordance();
  const resolvedTitle = title ?? t("ask_smith.default_title");
  return (
    <button
      className="tab"
      title={resolvedTitle}
      aria-label={resolvedTitle}
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
      {t("ask_smith.label")}
    </button>
  );
}
