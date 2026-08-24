import { useSavedFlash } from "../lib/useSavedFlash";

// SaveButton — Sprint K: brief rotate-to-check on a successful save
// (amicro "Settings"), the success feedback this app has never had (see
// useSavedFlash's doc comment). Drop-in for the ~15 near-identical
// `<button className="..." disabled={pending} onClick={...}>Save</button>`
// sites across Settings.tsx/CatalogPanel.tsx/ConfigEditView.tsx/
// ModelEditView.tsx — same repeated contract everywhere (idle label /
// pending label / disabled), which is what makes a shared component the
// right call instead of copying the flash logic into each site.
export function SaveButton({
  pending,
  isError = false,
  disabled,
  onClick,
  className = "btn primary",
  label = "Save",
  pendingLabel = "Saving…",
  type = "button",
}: {
  pending: boolean;
  isError?: boolean;
  disabled?: boolean;
  onClick?: () => void;
  className?: string;
  label?: string;
  pendingLabel?: string;
  type?: "button" | "submit";
}) {
  const saved = useSavedFlash(pending, isError);
  return (
    <button
      type={type}
      className={`${className} save-btn ${saved ? "saved" : ""}`.trim()}
      disabled={disabled ?? pending}
      onClick={onClick}
    >
      {pending ? pendingLabel : saved ? <span className="check">✓ Saved</span> : label}
    </button>
  );
}
