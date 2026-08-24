import { useId, useState, type ReactNode } from "react";

// InfoTip (Sprint B) — the app's first positioned tooltip component. Every
// existing hover explanation in this codebase (BadgeIcon, ConfigCardView's
// capability bars, CompressorSavingsChips, ServicesBar…) rides the native
// `title` attribute, which has a real hover delay, no multi-line control,
// and no keyboard path at all. The load-options list's curated "why this
// exists" copy (lib/llamaFlags.ts) is multi-sentence and needs to be
// reachable by keyboard too, so it gets a real popover: shown on hover
// *and* focus, closed on blur/mouse-leave, wired to `aria-describedby` so
// assistive tech announces it the same way a native tooltip would.
export function InfoTip({ text, children, className }: { text: string; children?: ReactNode; className?: string }) {
  const [open, setOpen] = useState(false);
  const id = useId();

  return (
    <span
      className={`infotip ${className ?? ""}`.trim()}
      tabIndex={0}
      aria-describedby={open ? id : undefined}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
    >
      {children ?? (
        <span className="infotip-glyph" aria-hidden="true">
          ⓘ
        </span>
      )}
      {open && (
        <span role="tooltip" id={id} className="infotip-bubble">
          {text}
        </span>
      )}
    </span>
  );
}
