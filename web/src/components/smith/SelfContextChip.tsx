import type { SmithMissedPattern, SmithStatus } from "../../lib/types";

// SmithIndicators / MissedPatterns — split from a single combined
// "SelfContextChip" card (docs/v5-smith-experience.md §2.1) at operator
// request: the brain/web-research chips now live inline in AskSmith's chat
// header (top right), and the missed-pattern ledger now lives in
// Diagnostics' Findings section, rather than both sharing one standalone
// status card above the chat. The raw brain.resolution code
// (local_slot:a3 / deterministic_only) is never shown — only a
// human-readable brain label, or nothing when no model is in use.

export function SmithIndicators({ status }: { status: SmithStatus }) {
  const br = status.brain;
  const hasModel = br.model && (br.resolution === "local_slot" || br.resolution === "remote");
  const brainLabel = hasModel
    ? br.resolution === "local_slot"
      ? `brain: ${br.model} on this box`
      : `brain: ${br.model} · remote`
    : null;

  if (!brainLabel && !status.web.enabled) return null;

  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
      {brainLabel && (
        <span
          className="chip"
          style={{ color: "var(--cool)", borderColor: "color-mix(in srgb, var(--cool) 40%, var(--border))" }}
        >
          {brainLabel}
        </span>
      )}
      {status.web.enabled && <span className="chip">web research on</span>}
    </div>
  );
}

function formatMissedAt(unixS: number): string {
  const d = new Date(unixS * 1000);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) +
    " " + d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

// S2-Web: the missed-pattern ledger (§3.7) surfaces as a collapsible
// "Questions smith had to think about" list. Operator-facing detail —
// promoting a candidate into the fast path is always a reviewed code
// change, never auto-learned, so this only surfaces candidates for
// follow-up.
export function MissedPatterns({ patterns }: { patterns: SmithMissedPattern[] }) {
  if (patterns.length === 0) return null;
  return (
    <details className="smith-missed-patterns">
      <summary>Questions smith had to think about ({patterns.length})</summary>
      <div className="smith-missed-list">
        {patterns.map((p, i) => (
          <div className="smith-missed-row" key={`${p.at}-${i}`}>
            <span className="smith-missed-text">{p.text}</span>
            {p.tools_used.length > 0 && (
              <span className="smith-missed-tools">{p.tools_used.join(", ")}</span>
            )}
            <span className="smith-missed-at">{formatMissedAt(p.at)}</span>
          </div>
        ))}
      </div>
    </details>
  );
}
