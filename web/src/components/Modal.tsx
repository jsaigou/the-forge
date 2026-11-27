import { useEffect, useId, useRef, type ReactNode, type RefObject } from "react";
import { createPortal } from "react-dom";

// Modal — the one shared modal shell (ADR-0023 component contract b), the
// migration target for every .modal-backdrop surface in the app
// (ui-review-2026-11 §A1 item 2). Portals to document.body — DetailModal's
// containment reasoning: a transformed ancestor becomes the containing
// block for position:fixed and traps the backdrop, so the backdrop never
// lives in the opener's subtree. Contract semantics: role="dialog" +
// aria-modal, labelled by its heading (aria-labelledby), focus moved into
// the dialog on open, Tab trapped inside, Escape closes, focus restored to
// the trigger on close. Open dialogs form a stack and only the topmost one
// answers the keyboard, so a dialog mounted above another (the step-up
// gate over DowntimeModal) dismisses without also dismissing what is
// underneath it. Reuses theme.css's existing .modal-backdrop/.modal
// chrome — no new CSS. Consumers that need to guard dismissal (not while a
// request is pending) pass a guarded onClose; Escape, the backdrop click
// and focus restore all flow through it.
//
// DetailModal is too custom for the shell (its heading lives in whichever
// sub-view tops the view stack, and Escape pops one level instead of
// closing) but uses the same useDialogSemantics hook, so trap, restore
// and stacking behave identically everywhere.

const FOCUSABLE =
  "a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex='-1'])";

// Open dialogs, outermost-first. Holds the dialog DOM nodes, populated from
// refs inside the mount effect.
const openDialogs: HTMLElement[] = [];

// True when el is the topmost open dialog — for consumers with their own
// extra key handling (DetailModal's ArrowUp).
export function isTopmostDialog(el: HTMLElement | null): boolean {
  return el !== null && openDialogs[openDialogs.length - 1] === el;
}

// useDialogSemantics — the keyboard/focus half of the modal contract,
// exported for DetailModal's custom shell. The dialog element must be
// focusable (tabIndex={-1}). Initial focus lands on initialFocusRef's
// target if given, else the dialog itself — the container, not the first
// focusable, so opening never scroll-jumps inside .modal.wide's 85vh
// overflow container or arms a button under the Enter key; surfaces that
// want a specific control (StepUpModal's input) pass initialFocusRef.
export function useDialogSemantics({
  dialogRef,
  onClose,
  initialFocusRef,
}: {
  dialogRef: RefObject<HTMLElement | null>;
  onClose: () => void;
  initialFocusRef?: RefObject<HTMLElement | null>;
}) {
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;

  useEffect(() => {
    const dialog = dialogRef.current;
    if (!dialog) return;
    // The trigger to restore focus to, captured before anything moves focus
    // into the dialog. Restored only if it still exists — the trigger can
    // unmount with the view that closed over it.
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    openDialogs.push(dialog);
    (initialFocusRef?.current ?? dialog).focus();

    function onKey(e: KeyboardEvent) {
      if (openDialogs[openDialogs.length - 1] !== dialog) return; // only the topmost dialog answers
      if (e.key === "Escape") {
        e.preventDefault();
        e.stopPropagation();
        onCloseRef.current();
        return;
      }
      if (e.key !== "Tab") return;
      const items = dialog.querySelectorAll<HTMLElement>(FOCUSABLE);
      const active = document.activeElement;
      const inside = active instanceof Node && dialog.contains(active);
      if (items.length === 0) {
        e.preventDefault();
        dialog.focus();
        return;
      }
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey ? active === first || active === dialog || !inside : active === last || active === dialog || !inside) {
        e.preventDefault();
        (e.shiftKey ? last : first).focus();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => {
      const i = openDialogs.indexOf(dialog);
      if (i >= 0) openDialogs.splice(i, 1);
      document.removeEventListener("keydown", onKey);
      // The contains() guard covers the autoFocus edge: a child input that
      // grabbed focus during commit, before the trigger could be captured.
      if (opener && opener.isConnected && !dialog.contains(opener)) opener.focus();
    };
  }, [dialogRef, initialFocusRef, onCloseRef]);
}

interface ModalProps {
  /** Dismissal: Escape and the backdrop click both route through here; guard it if dismissal must be conditional. */
  onClose: () => void;
  /** Rendered as the id'd <h3> the dialog is labelled by (aria-labelledby). */
  title: ReactNode;
  children: ReactNode;
  /** Extra classes on the .modal element (e.g. "detail-modal"). */
  className?: string;
  /** Use the 720px .modal.wide chrome. */
  wide?: boolean;
  /** Element to focus on open instead of the dialog container (e.g. StepUpModal's input). */
  initialFocusRef?: RefObject<HTMLElement | null>;
  /** Whether a backdrop click dismisses. false keeps the backdrop inert (RestartOverlay: giving up on a restart is an explicit choice). */
  closeOnBackdrop?: boolean;
}

export function Modal({ onClose, title, children, className, wide, initialFocusRef, closeOnBackdrop = true }: ModalProps) {
  const titleId = useId();
  const dialogRef = useRef<HTMLDivElement | null>(null);
  useDialogSemantics({ dialogRef, onClose, initialFocusRef });

  return createPortal(
    <div
      className="modal-backdrop"
      onClick={(e) => {
        // Sprint I fix (see DetailModal.tsx): portal or not, React bubbles
        // this click into the opener's clickable ancestors.
        e.stopPropagation();
        if (closeOnBackdrop) onClose();
      }}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        tabIndex={-1}
        className={["modal", wide ? "wide" : "", className ?? ""].filter(Boolean).join(" ")}
        onClick={(e) => e.stopPropagation()}
      >
        <h3 id={titleId}>{title}</h3>
        {children}
      </div>
    </div>,
    document.body,
  );
}
