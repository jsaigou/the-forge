import { useState } from "react";
import { Trans, useTranslation } from "react-i18next";
import { ICON_MANIFEST } from "../assets/icons/manifest";
import { Icon } from "./Icon";
import type { InheritedIcon } from "../lib/iconInheritance";

// IconPicker (Sprint I — icon inheritance hierarchy,
// docs/v5-prerelease-readiness.md). Today the only way to set an icon
// anywhere in the app is a bare file upload (ModelForm's raw <input
// type="file">) — there is no way to pick one of the ~20 already-vendored
// vendor marks. This is the one shared picker for genealogy/family/model/
// config icons: a grid of vendor marks (kind:"vendor" in the manifest —
// badge glyphs like "reasoning"/"vision" are deliberately excluded, they
// aren't selectable identity icons) plus the existing upload mechanism as
// an escape hatch, plus a Clear action that makes the inheritance chain
// legible instead of magic.
//
// Phase 3 (pre-release feedback sprint): adds a light/dark variant toggle.
// value/valueDark are this entity's own raw fields; the header previews
// both RESOLVED marks (falling back through `inherited` exactly like
// registry.resolveLogos), and a segmented control chooses which variant the
// grid/upload/Clear below act on. Dark unset (valueDark === "") reads as
// "same as light", not "missing" — see the header hint text.

const VENDOR_SLUGS = Object.entries(ICON_MANIFEST)
  .filter(([, entry]) => entry.kind === "vendor")
  .map(([slug]) => slug)
  .sort();

function prettySlug(slug: string): string {
  return slug.charAt(0).toUpperCase() + slug.slice(1);
}

export function IconPicker({
  value,
  valueDark = "",
  inherited,
  onSelect,
  onUpload,
  onClear,
  pending,
}: {
  // The raw logo/logo_dark fields on this entity ("" = unset, inherits).
  value: string;
  valueDark?: string;
  // What "" would resolve to via the chain, if anything — {logo, logoDark,
  // label} when the parent level is also unset, so the hint can still name
  // it. See lib/iconInheritance.ts.
  inherited?: InheritedIcon | null;
  onSelect: (slug: string, dark: boolean) => void;
  onUpload: (file: File, dark: boolean) => void;
  onClear?: (dark: boolean) => void;
  pending?: boolean;
}) {
  const { t } = useTranslation("common");
  const [open, setOpen] = useState(false);
  const [variant, setVariant] = useState<"light" | "dark">("light");
  const dark = variant === "dark";

  const effectiveLight = value || inherited?.logo || "";
  // Dark's own resolution: this entity's own dark, else its own light
  // (an entity that only ever set light still renders correctly in dark
  // theme), else inherited dark, else inherited light.
  const effectiveDark = valueDark || value || inherited?.logoDark || inherited?.logo || "";
  const activeValue = dark ? valueDark : value;

  return (
    <div>
      <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <span title={t("catalog_form.icon_picker.light_theme_title")}>
            {effectiveLight ? <Icon slug={effectiveLight} name={value || prettySlug(effectiveLight)} /> : <span className="icon" />}
          </span>
          <span title={t("catalog_form.icon_picker.dark_theme_title")}>
            {effectiveDark ? <Icon slug={effectiveDark} name={valueDark || prettySlug(effectiveDark)} /> : <span className="icon" />}
          </span>
        </div>
        <div style={{ fontSize: 11, color: "var(--text-mute)", flex: 1 }}>
          {value ? (
            <>{t("catalog_form.icon_picker.light_set_to")} <code>{value.startsWith("data:") ? t("catalog_form.icon_picker.uploaded_image") : value}</code></>
          ) : inherited ? (
            inherited.logo ? (
              <Trans
                i18nKey="catalog_form.icon_picker.light_inherits_from"
                ns="common"
                values={{ icon: inherited.logo.startsWith("data:") ? t("catalog_form.icon_picker.uploaded_image") : inherited.logo, label: inherited.label }}
                components={{ 1: <code /> }}
              />
            ) : (
              <>{t("catalog_form.icon_picker.light_inherits_none", { label: inherited.label })}</>
            )
          ) : (
            <>{t("catalog_form.icon_picker.light_not_set")}</>
          )}
          {" · "}
          {valueDark ? (
            <>{t("catalog_form.icon_picker.dark_set_to")} <code>{valueDark.startsWith("data:") ? t("catalog_form.icon_picker.uploaded_image") : valueDark}</code></>
          ) : (
            <>{t("catalog_form.icon_picker.dark_same_as_light")}</>
          )}
        </div>
        <div className="seg" style={{ display: "flex", border: "1px solid var(--border)", borderRadius: 8, overflow: "hidden" }}>
          <button
            type="button"
            className="btn"
            style={{ fontSize: 11, padding: "4px 8px", borderRadius: 0, border: "none", background: variant === "light" ? "var(--panel-2, var(--bg-2))" : "transparent" }}
            aria-pressed={variant === "light"}
            onClick={() => setVariant("light")}
          >
            {t("catalog_form.icon_picker.light_tab")}
          </button>
          <button
            type="button"
            className="btn"
            style={{ fontSize: 11, padding: "4px 8px", borderRadius: 0, border: "none", borderLeft: "1px solid var(--border)", background: variant === "dark" ? "var(--panel-2, var(--bg-2))" : "transparent" }}
            aria-pressed={variant === "dark"}
            onClick={() => setVariant("dark")}
          >
            {t("catalog_form.icon_picker.dark_tab")}
          </button>
        </div>
        <button
          type="button"
          className="btn"
          style={{ fontSize: 11, padding: "4px 8px" }}
          onClick={() => setOpen((o) => !o)}
          disabled={pending}
        >
          {open ? t("catalog_form.icon_picker.close") : t("catalog_form.icon_picker.change_icon")}
        </button>
      </div>
      {open && (
        <div style={{ marginTop: 8, border: "1px solid var(--border)", borderRadius: 8, padding: 10 }}>
          <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 8, textTransform: "uppercase", letterSpacing: ".06em" }}>
            {t("catalog_form.icon_picker.editing_mark", {
              variant: dark ? t("catalog_form.icon_picker.editing_mark_dark") : t("catalog_form.icon_picker.editing_mark_light"),
              suffix: !dark && !value && !valueDark ? "" : dark && !valueDark ? t("catalog_form.icon_picker.editing_mark_same_as_light_suffix") : "",
            })}
          </div>
          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fill, minmax(38px, 1fr))",
              gap: 6,
              maxHeight: 180,
              overflow: "auto",
            }}
          >
            {VENDOR_SLUGS.map((slug) => (
              <button
                key={slug}
                type="button"
                title={prettySlug(slug)}
                onClick={() => {
                  onSelect(slug, dark);
                  setOpen(false);
                }}
                style={{
                  border: activeValue === slug ? "2px solid var(--heat)" : "1px solid var(--border)",
                  borderRadius: 8,
                  background: "var(--panel)",
                  cursor: "pointer",
                  padding: 3,
                  display: "flex",
                  alignItems: "center",
                  justifyContent: "center",
                }}
              >
                <Icon slug={slug} name={prettySlug(slug)} sm />
              </button>
            ))}
          </div>
          <div
            style={{
              display: "flex",
              alignItems: "center",
              flexWrap: "wrap",
              gap: 10,
              marginTop: 10,
              borderTop: "1px solid var(--border)",
              paddingTop: 10,
            }}
          >
            {onClear && (
              <button
                type="button"
                className="btn"
                style={{ fontSize: 11, padding: "4px 8px" }}
                disabled={!activeValue}
                onClick={() => {
                  onClear(dark);
                  setOpen(false);
                }}
              >
                {dark
                  ? t("catalog_form.icon_picker.clear_dark")
                  : `${t("catalog_form.icon_picker.clear")}${inherited ? t("catalog_form.icon_picker.clear_inherit_suffix", { label: inherited.label }) : ""}`}
              </button>
            )}
            <label className="form-row" style={{ margin: 0, fontSize: 11, flex: 1 }}>
              {t("catalog_form.icon_picker.upload_label", { suffix: dark ? t("catalog_form.icon_picker.upload_dark_suffix") : "" })}
              <input
                type="file"
                accept="image/webp,image/png,image/jpeg,image/svg+xml,video/webm"
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  if (file) {
                    onUpload(file, dark);
                    setOpen(false);
                  }
                }}
              />
            </label>
          </div>
        </div>
      )}
    </div>
  );
}
