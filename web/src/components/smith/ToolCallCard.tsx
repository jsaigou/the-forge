import type { SmithToolActivityEvent, SmithToolCallEvidence } from "../../lib/types";

// ToolCallCard — P7 (docs/v5-smith.md §9). Renders one persisted tool_call
// message's round of activity: a chip per call (name/ok-or-error/duration),
// expandable to the args + result summary. No fetches, no images — same
// CSP-clean posture as SourcesList.tsx, which this deliberately mirrors.
export function ToolCallCard({ evidence }: { evidence: SmithToolCallEvidence }) {
  if (evidence.calls.length === 0) return null;
  return (
    <details className="smith-sources smith-toolcall">
      <summary>
        Round {evidence.round}: {evidence.calls.map((c) => c.name).join(", ")}
      </summary>
      <div className="smith-sources-list">
        {evidence.calls.map((c, i) => (
          <div className="smith-source-row" key={`${c.name}-${i}`}>
            <span
              className="chip"
              style={{
                fontSize: 9,
                color: c.ok ? "var(--ok)" : "var(--crit)",
                borderColor: `color-mix(in srgb, ${c.ok ? "var(--ok)" : "var(--crit)"} 40%, var(--border))`,
              }}
            >
              {c.ok ? "ok" : "error"}
            </span>
            <code style={{ fontSize: 11 }}>{c.name}</code>
            <span style={{ color: "var(--text-mute)", fontSize: 10.5 }}>{c.duration_ms}ms</span>
            <span style={{ color: "var(--text-mute)", fontSize: 10.5, flex: 1 }}>{c.error ?? c.summary}</span>
          </div>
        ))}
      </div>
    </details>
  );
}

// ToolActivityList — the LIVE equivalent while a turn is still streaming
// (useSmithToolActivity), rendered above the pending assistant bubble. A
// tool round is otherwise completely silent (the round gate withholds its
// content from smith:token until it commits to prose), so this is what
// keeps a 60s run_check from reading as a hang.
export function ToolActivityList({ events }: { events: SmithToolActivityEvent[] }) {
  if (events.length === 0) return null;
  // Collapse to the latest status per (round, name) — "started" then
  // "done"/"error" for the same call both arrive, only the latest matters.
  const latest = new Map<string, SmithToolActivityEvent>();
  for (const e of events) latest.set(`${e.round}:${e.name}`, e);
  return (
    <div className="smith-tool-activity">
      {[...latest.values()].map((e) => (
        <span key={`${e.round}:${e.name}`} className="chip" style={{ fontSize: 10 }}>
          {e.status === "started" ? "⋯" : e.status === "error" ? "✕" : "✓"} {e.name}
        </span>
      ))}
    </div>
  );
}
