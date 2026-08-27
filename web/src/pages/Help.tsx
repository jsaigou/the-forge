import { useEffect, useState } from "react";
import { ErrorBoundary } from "../components/ErrorBoundary";
import { AskSmith } from "../components/help/AskSmith";
import { Diagnostics } from "../components/help/Diagnostics";
import { Guide } from "../components/help/Guide";
import { reopenTour } from "../onboarding/useOnboarding";
import { useSession } from "../lib/session";

// Wave 2 nav reorg (docs/v5-smith-wave2.md §3 track W2-B): Help is now a
// sub-tab shell, not a standalone page. Two sub-tabs, Guide and Ask the
// smith, routed via the tab/sub hash router (#help/guide, #help/smith,
// plus #help/smith/conv/<id> deep links into a specific conversation). The
// Ask the smith tab merges the P3 chat UI (components/help/AskSmith.tsx)
// with diagnostics findings (components/help/Diagnostics.tsx) in a single
// scrollable page: chat on top, findings below. Legacy #help/ask and
// #help/diagnostics hashes redirect to #help/smith for backward compat.
//
// Local useState + onSubChange follows the Settings precedent: local state
// is the source of truth for which sub-tab is active, initialized from the
// `sub` prop and re-synced when the hash changes externally (back/forward,
// hash edit). onSubChange pushes the new sub into the hash so deep links
// work.

const HELP_TABS = [
  { key: "guide", label: "Guide" },
  { key: "smith", label: "Ask the smith" },
] as const;

type HelpTab = (typeof HELP_TABS)[number]["key"];

function parseSub(sub?: string): HelpTab {
  if (sub?.startsWith("smith") || sub?.startsWith("ask") || sub?.startsWith("diagnostics")) return "smith";
  return "guide";
}

export function Help({ sub, onSubChange }: { sub?: string; onSubChange?: (sub: string, opts?: { replace?: boolean }) => void }) {
  const [tab, setTab] = useState<HelpTab>(parseSub(sub));
  const { canOperate } = useSession();

  useEffect(() => {
    setTab(parseSub(sub));
  }, [sub]);

  useEffect(() => {
    if (!sub) return;
    if (sub.startsWith("diagnostics") || sub.startsWith("ask")) {
      const mapped = sub.replace(/^diagnostics/, "smith").replace(/^ask/, "smith");
      onSubChange?.(mapped, { replace: true });
    }
  }, [sub, onSubChange]);

  return (
    <section className="page">
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>Help</span>
        {/* Sprint 6: re-run entry for the first-run tour (useOnboarding owns
            the done-flag; this just re-opens it for this session). Gated on
            canOperate to match useOnboarding's own first-run gate — every
            tour target is an operator-only control, so a viewer replaying it
            would just hit dead spotlights. */}
        {canOperate && (
          <button
            className="btn sm"
            title="Replay the first-run guided tour"
            onClick={reopenTour}
          >
            Replay onboarding tour
          </button>
        )}
        <div className="tabs" style={{ marginLeft: "auto" }}>
          {HELP_TABS.map((t) => (
            <button
              key={t.key}
              className={`tab ${tab === t.key ? "active" : ""}`}
              onClick={() => {
                setTab(t.key);
                onSubChange?.(t.key);
              }}
            >
              {t.label}
            </button>
          ))}
        </div>
      </div>
      <ErrorBoundary key={tab} resetKeys={[tab]}>
        {tab === "guide" ? (
          <Guide />
        ) : (
          <>
            <AskSmith sub={sub} onSubChange={onSubChange} />
            <Diagnostics sub={sub} onSubChange={onSubChange} />
          </>
        )}
      </ErrorBoundary>
    </section>
  );
}
