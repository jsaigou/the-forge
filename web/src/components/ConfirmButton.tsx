import { useEffect, useRef, useState, type CSSProperties, type MouseEvent } from "react";

// ConfirmButton — Sprint K, amicro "Delete" (Shake Interaction), reworked
// as an inline arm-then-confirm control (operator's call, replacing every
// window.confirm() in the app): first click shakes the button and swaps
// its label to "Confirm?" (plus the same warning text the old confirm()
// dialog carried, rendered inline — dependent counts, force-delete-while-
// loaded, etc. must survive this migration, not get silently dropped);
// second click within `disarmMs` fades the button and fires `onConfirm`;
// no second click within that window auto-disarms back to the idle label.
//
// stopPropagation on the wrapping span (not just the button) matters at
// call sites where the whole row/card is itself a click target.
export function ConfirmButton({
  onConfirm,
  warning,
  pending = false,
  label = "Delete",
  confirmLabel = "Confirm?",
  pendingLabel = "…",
  className = "btn",
  style,
  disarmMs = 3000,
}: {
  onConfirm: () => void;
  warning?: string;
  pending?: boolean;
  label?: string;
  confirmLabel?: string;
  pendingLabel?: string;
  className?: string;
  style?: CSSProperties;
  disarmMs?: number;
}) {
  const [armed, setArmed] = useState(false);
  const [shaking, setShaking] = useState(false);
  const [fading, setFading] = useState(false);
  const disarmTimer = useRef<ReturnType<typeof setTimeout> | null>(null);
  const shakeTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    return () => {
      if (disarmTimer.current) clearTimeout(disarmTimer.current);
      if (shakeTimer.current) clearTimeout(shakeTimer.current);
    };
  }, []);

  function onClick(e: MouseEvent) {
    e.stopPropagation();
    if (!armed) {
      setArmed(true);
      setShaking(true);
      shakeTimer.current = setTimeout(() => setShaking(false), 400);
      disarmTimer.current = setTimeout(() => setArmed(false), disarmMs);
      return;
    }
    if (disarmTimer.current) clearTimeout(disarmTimer.current);
    setFading(true);
    setTimeout(onConfirm, 200);
  }

  return (
    // marginLeft/marginRight (the common "push to the row's right edge"
    // case) need to land on the wrapper — it's the actual flex child of
    // whatever row this sits in, not the button, which is only ever a flex
    // child of this tightly-fit wrapper. Applying them to the button too is
    // harmless (a no-op inside a content-sized wrapper).
    <span
      className="confirm-btn-wrap"
      style={{ marginLeft: style?.marginLeft, marginRight: style?.marginRight }}
      onClick={(e) => e.stopPropagation()}
    >
      <button
        type="button"
        className={`${className} confirm-btn ${armed ? "armed" : ""} ${shaking ? "shake" : ""} ${fading ? "fading" : ""}`.trim()}
        style={style}
        disabled={pending}
        onClick={onClick}
      >
        {pending ? pendingLabel : armed ? confirmLabel : label}
      </button>
      {armed && warning && <span className="confirm-warning">{warning}</span>}
    </span>
  );
}
