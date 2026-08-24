import type { SmithMissedPattern, SmithStatus } from "../../lib/types";

// SelfContextChip — smith's identity line, extracted from the two
// near-identical copies that grew independently in AskSmith.tsx (P2
// placeholder) and Diagnostics.tsx (Wave 2 status strip). Both call sites
// now render this instead of hand-rolling the chips again.
//
// Per docs/v5-smith-experience.md §2.1: one human line + at most two chips.
// The checks-count / tools-mode / schedule chips moved to Diagnostics' sweep
// controls where they belong (operational detail, not identity). The raw
// brain.resolution code (local_slot:a3 / deterministic_only) is never shown —
// only a human-readable brain label, or nothing when no model is in use.
//
// S2-Web: the missed-pattern ledger (§3.7) surfaces as a collapsible
// "Questions smith had to think about" list below the identity line. This is
// operator-facing detail — promoting a candidate into the fast path is
// always a reviewed code change, never auto-learned, so the UI only surfaces
// candidates for follow-up.

function formatMissedAt(unixS: number): string {
  const d = new Date(unixS * 1000);
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric" }) +
    " " + d.toLocaleTimeString(undefined, { hour: "numeric", minute: "2-digit" });
}

function MissedPatterns({ patterns }: { patterns: SmithMissedPattern[] }) {
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

export function SelfContextChip({ status }: { status: SmithStatus }) {
  const br = status.brain;
  const hasModel = br.model && (br.resolution === "local_slot" || br.resolution === "remote");
  const brainLabel = hasModel
    ? br.resolution === "local_slot"
      ? `brain: ${br.model} on this box`
      : `brain: ${br.model} · remote`
    : null;

  const missed = status.missed_patterns;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <span style={{ fontWeight: 600 }}>smith</span>
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
      {missed && missed.length > 0 && <MissedPatterns patterns={missed} />}
    </div>
  );
}
