import { apiErrorMessage } from "../../lib/api";
import { useSmithActions } from "../../lib/queries";
import { ErrorBoundary } from "../ErrorBoundary";
import { ActionCard } from "./ActionCard";

// ActionsTray — Wave 3 / P2 (track W3-C). Shared list component over
// useSmithActions(status, investigationId). Two consumers:
//   - Console's pending-actions chip (compact=true — a single `.cchip` row
//     reusing the existing services-strip chip styling, status dot + count,
//     no card list; Console is already dense per this repo's design
//     history, so the full ActionCard list only ever renders on Diagnostics).
//   - Diagnostics' investigation detail (compact=false — the full card
//     list, filtered to that investigation via investigationId).
// Wrapped in its own ErrorBoundary per this repo's established pattern
// (every dashboard-ish surface got one after the ComfyUI black-screen
// incident — ProfilingPanel.tsx's use of ErrorBoundary is the one FE-side
// precedent for a component that mutates + polls, not just displays).
export function ActionsTray({
  status,
  investigationId,
  compact = false,
}: {
  status?: string;
  investigationId?: number;
  compact?: boolean;
}) {
  return (
    <ErrorBoundary resetKeys={[status, investigationId, compact]}>
      <ActionsTrayInner status={status} investigationId={investigationId} compact={compact} />
    </ErrorBoundary>
  );
}

function ActionsTrayInner({
  status,
  investigationId,
  compact,
}: {
  status?: string;
  investigationId?: number;
  compact: boolean;
}) {
  const actions = useSmithActions(status, investigationId);

  if (compact) {
    const pending = actions.data?.pending_count ?? 0;
    // A blocked handoff (self-evicting, awaiting acknowledge) is a stronger
    // signal than an ordinary pending approval — the operator can't just
    // click Approve, they need to read a runbook first — so it gets its
    // own dot color rather than blending into the generic "pending" warn.
    const anyBlocked = (actions.data?.actions ?? []).some(
      (a) => a.status === "pending" && a.self_evicting && a.handoff?.state !== "acknowledged",
    );
    const dotColor = pending === 0 ? "var(--text-mute)" : anyBlocked ? "var(--warn)" : "var(--cool)";
    return (
      <div className="cchip" title="smith actions awaiting approval">
        <span className="sd" style={{ background: dotColor }} />
        <span className="nm">smith actions</span>
        <span className="pu">{actions.isLoading ? "…" : `${pending} pending`}</span>
        <a href="#help/smith" style={{ marginLeft: "auto", fontSize: 10, color: "var(--text-mute)" }}>
          →
        </a>
      </div>
    );
  }

  if (actions.isLoading) return <div className="empty-note">Loading actions…</div>;
  if (actions.isError) return <div className="error-note">{apiErrorMessage(actions.error)}</div>;
  const list = actions.data?.actions ?? [];
  if (list.length === 0) return <div className="empty-note">No actions.</div>;
  return (
    <div>
      {list.map((a) => (
        <ActionCard key={a.id} action={a} />
      ))}
    </div>
  );
}
