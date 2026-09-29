import { useTranslation } from "react-i18next";
import { useSavedFlash } from "../lib/useSavedFlash";

// SaveButton — Sprint K: brief rotate-to-check on a successful save
// (amicro "Settings"), the success feedback this app has never had (see
// useSavedFlash's doc comment). Drop-in for the ~15 near-identical
// `<button className="..." disabled={pending} onClick={...}>Save</button>`
// sites across Settings.tsx/CatalogPanel.tsx/ConfigEditView.tsx/
// ModelEditView.tsx — same repeated contract everywhere (idle label /
// pending label / disabled), which is what makes a shared component the
// right call instead of copying the flash logic into each site.
//
// label/pendingLabel default to the translated "Save"/"Saving…" — a caller
// that overrides them with its own literal text is responsible for that
// text's own translation (i18n Phase 1 extracts each call site in turn).
export function SaveButton({
  pending,
  isError = false,
  disabled,
  onClick,
  className = "btn primary",
  label,
  pendingLabel,
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
  const { t } = useTranslation("common");
  const saved = useSavedFlash(pending, isError);
  return (
    <button
      type={type}
      className={`${className} save-btn ${saved ? "saved" : ""}`.trim()}
      disabled={disabled ?? pending}
      onClick={onClick}
    >
      {pending
        ? (pendingLabel ?? t("save_button.saving"))
        : saved
          ? <span className="check">✓ {t("save_button.saved")}</span>
          : (label ?? t("save_button.save"))}
    </button>
  );
}
