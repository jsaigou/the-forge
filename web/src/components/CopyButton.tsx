import { useState, type MouseEvent } from "react";

// CopyButton — Sprint K, amicro.vercel.app's "Copy Hash" morph: the icon
// swaps to a checkmark for ~1.2s on a successful copy, then reverts.
// Wraps navigator.clipboard.writeText; every prior copy button in the app
// (SecurityPanel.tsx) called that directly with zero success feedback.
//
// stopPropagation is load-bearing at nearly every call site (name headers
// sit inside clickable card/row wrappers), so it's baked in here rather
// than left to each caller to remember. `label` opts into a text `.btn`
// (SecurityPanel's "Copy [token]" style) instead of the default icon-only
// `.icon-btn` used next to name headers — same morph, different chrome.
export function CopyButton({
  text,
  title = "Copy",
  sm = false,
  label,
}: {
  text: string;
  title?: string;
  sm?: boolean;
  label?: string;
}) {
  const [copied, setCopied] = useState(false);

  function onClick(e: MouseEvent) {
    e.stopPropagation();
    navigator.clipboard.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1200);
    });
  }

  if (label) {
    return (
      <button type="button" className={`btn copy-btn labeled ${copied ? "copied" : ""}`.trim()} onClick={onClick}>
        <span className="copy-glyph" aria-hidden="true">{copied ? "✓" : "⧉"}</span> {copied ? "Copied!" : label}
      </button>
    );
  }

  return (
    <button
      type="button"
      className={`icon-btn copy-btn ${sm ? "sm" : ""} ${copied ? "copied" : ""}`.trim()}
      title={copied ? "Copied!" : title}
      onClick={onClick}
    >
      <span className="copy-glyph" aria-hidden="true">{copied ? "✓" : "⧉"}</span>
    </button>
  );
}
