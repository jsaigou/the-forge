import { useState } from "react";
import { AskSmith } from "../help/AskSmith";

// SmithChatTray — Console's collapsible clone of the "Ask the smith" chat
// card (operator request 2026-08-27, replacing the runbook-only
// SuggestionsTray this used to show). Reuses AskSmith verbatim — the same
// component the Help page's smith tab renders — so Console gets the full
// chat, quick questions, sweep controls, and suggestions list without a
// second implementation to keep in sync. Defaults collapsed: the full chat
// is heavier than a suggestions summary and shouldn't dominate the fold.
// The whole header row is the click target in both states — title included —
// not a separate small button, so the collapse/expand control is obvious and
// easy to hit (operator feedback 2026-08-27: the old expanded-state toggle
// was a disconnected `.tab`-styled pill floating above the chat, low-contrast
// and easy to miss).
function TrayHeader({ open, onClick }: { open: boolean; onClick: () => void }) {
  return (
    <button
      type="button"
      aria-expanded={open}
      data-tour-id="smith-tray"
      onClick={onClick}
      style={{
        display: "flex", alignItems: "center", gap: 8, width: "100%",
        background: "none", border: "none", color: "var(--text)",
        cursor: "pointer", padding: 0, fontSize: 12, fontWeight: 700,
      }}
    >
      <span style={{ letterSpacing: "0.02em" }}>SMITH</span>
      <span
        style={{
          marginLeft: "auto", color: "var(--text-mute)",
          display: "inline-block", transition: "transform 0.15s ease",
          transform: open ? "rotate(180deg)" : "none",
        }}
      >
        ▾
      </span>
    </button>
  );
}

export function SmithChatTray() {
  const [open, setOpen] = useState(false);

  if (!open) {
    return (
      <div className="card" style={{ marginBottom: 12 }}>
        <TrayHeader open={false} onClick={() => setOpen(true)} />
      </div>
    );
  }

  return (
    <div style={{ marginBottom: 12 }}>
      <TrayHeader open={true} onClick={() => setOpen(false)} />
      <div style={{ marginTop: 8 }}>
        <AskSmith />
      </div>
    </div>
  );
}
