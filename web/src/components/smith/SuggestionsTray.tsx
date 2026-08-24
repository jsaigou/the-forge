import { useState } from "react";
import { useSmithActions } from "../../lib/queries";
import { ActionCard } from "./ActionCard";

// SuggestionsTray — Console-top duplicate of Diagnostics' Suggestions group
// (operator feedback 2026-08-14). Suggestions are informational, not alarms,
// so the tray defaults to ONE collapsed header row (count + first title);
// clicking it expands the full ActionCard list in place. Renders nothing at
// all when smith has no pending runbook suggestions — the common case
// shouldn't occupy the fold (same contract as PendingActionsCard).
export function SuggestionsTray() {
  const actions = useSmithActions("pending");
  const [open, setOpen] = useState(false);
  if (actions.isLoading || actions.isError) return null;
  const suggestions = (actions.data?.actions ?? []).filter((a) => a.kind === "runbook");
  if (suggestions.length === 0) return null;
  return (
    <div className="card" style={{ marginBottom: 12 }}>
      <button
        type="button"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
        style={{
          display: "flex", alignItems: "center", gap: 8, width: "100%",
          background: "none", border: "none", color: "var(--text)",
          cursor: "pointer", padding: 0, fontSize: 12, fontWeight: 700,
        }}
      >
        <span className="chip">{suggestions.length}</span>
        <span>smith suggestions</span>
        {!open && (
          <span
            style={{
              fontSize: 11, color: "var(--text-mute)", fontWeight: 400,
              whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis",
            }}
          >
            {suggestions[0].title}
          </span>
        )}
        <span style={{ marginLeft: "auto", color: "var(--text-mute)" }}>{open ? "▴" : "▾"}</span>
      </button>
      {open && (
        <>
          <div style={{ fontSize: 11, color: "var(--text-mute)", margin: "8px 0" }}>
            Informational only — nothing here means anything is broken.
          </div>
          {suggestions.map((a) => (
            <ActionCard key={a.id} action={a} />
          ))}
        </>
      )}
    </div>
  );
}
