// EvidenceBlock — S2-Web (docs/v5-smith-experience.md §2.2 FAST ANSWER). Renders
// the expandable evidence detail that follows a fast-path answer. The backend
// (reasoning.go:871) appends a separate smith_deterministic message with empty
// content + a JSON array of {label, value} evidence rows; this component
// renders that array as a collapsed <details> block under the answer bubble.
//
// Defensive: parseEvidence returns null on any shape mismatch, and
// MessageBubble renders nothing in that case — same posture as the
// action/runbook/tool_call evidence parsing in AskSmith.tsx.

export interface AnswerEvidenceRow {
  label: string;
  value: string;
}

// parseEvidence returns the evidence rows if `raw` is a JSON array of
// {label, value} objects (the FastAnswer.Evidence wire shape from
// answers.go), else null. Exported so MessageBubble can guard the
// evidence-message branch with the same shape check.
export function parseEvidence(raw: string | null): AnswerEvidenceRow[] | null {
  if (!raw) return null;
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!Array.isArray(parsed) || parsed.length === 0) return null;
  const rows: AnswerEvidenceRow[] = [];
  for (const item of parsed) {
    if (item && typeof item === "object" && "label" in item && "value" in item) {
      const obj = item as { label: unknown; value: unknown };
      if (typeof obj.label === "string" && typeof obj.value === "string") {
        rows.push({ label: obj.label, value: obj.value });
      }
    }
  }
  return rows.length > 0 ? rows : null;
}

export function EvidenceBlock({ evidence }: { evidence: AnswerEvidenceRow[] }) {
  return (
    <details className="smith-evidence">
      <summary>Evidence ({evidence.length})</summary>
      <div className="smith-evidence-rows">
        {evidence.map((row, i) => (
          <div className="smith-evidence-row" key={`${row.label}-${i}`}>
            <span className="smith-evidence-label">{row.label}</span>
            <span className="smith-evidence-value">{row.value}</span>
          </div>
        ))}
      </div>
    </details>
  );
}
