import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { ApplyBadge } from "../components/ApplyBadge";
import { InfoTip } from "../components/InfoTip";
import type { SettingRecord } from "./fields";

// settings/Field.tsx — Sprint 12 (was H) Phase 6. Renders one SettingRecord:
// label → InfoTip (rec.help) → ApplyBadge (rec.apply) → the right input.
// The registry owns presentation metadata only, never data plumbing — value/
// onChange/disabled come from the caller's own useSettingsGroup draft, same
// as every hand-written `<label className="form-row">` site elsewhere in
// Settings did before this.
//
// i18n (Phase 1 Step 7a): fields.ts's label/help/keywords/options[].label
// stay literal English on purpose — SettingsSearch.tsx full-text-matches
// against them directly. Translation is a parallel lookup keyed by each
// record's stable `id`, under the "settings" namespace's "fields" tree, with
// an i18next `defaultValue` fallback to the registry's own English string
// (same pattern as lib/chatTemplateCaps.ts) — a record with no translation
// yet just renders in English instead of breaking. Exported so panels that
// render a SettingRecord's text directly (a landmark row, or a custom
// pre-<Field> control like Voice.tsx's mode <select>) use the same lookup.
export function fieldLabel(t: TFunction, rec: SettingRecord): string {
  return t(`fields.${rec.id}.label`, { defaultValue: rec.label });
}
export function fieldHelp(t: TFunction, rec: SettingRecord): string {
  return t(`fields.${rec.id}.help`, { defaultValue: rec.help });
}
export function fieldOptionLabel(t: TFunction, rec: SettingRecord, opt: { value: string; label: string }): string {
  return t(`fields.${rec.id}.options.${opt.value}`, { defaultValue: opt.label });
}
export function fieldPlaceholder(t: TFunction, rec: SettingRecord): string | undefined {
  return rec.placeholder ? t(`fields.${rec.id}.placeholder`, { defaultValue: rec.placeholder }) : undefined;
}

type FieldValue = string | number | boolean;

export function Field({
  rec,
  value,
  onChange,
  disabled,
  hideApplyBadge,
}: {
  rec: SettingRecord;
  value: FieldValue;
  onChange: (v: FieldValue) => void;
  disabled?: boolean;
  hideApplyBadge?: boolean;
}) {
  const { t } = useTranslation("settings");
  const label = fieldLabel(t, rec);
  const help = fieldHelp(t, rec);
  const badge = hideApplyBadge ? null : <ApplyBadge mode={rec.apply} />;
  if (rec.input === "toggle") {
    return (
      <span
        id={rec.id}
        className={`toggle ${disabled ? "disabled" : ""}`}
        onClick={() => !disabled && onChange(!value)}
      >
        <span className={`sw ${value ? "on" : ""}`} />
        <span style={{ display: "flex", alignItems: "center", gap: 6 }}>
          {label}
          <InfoTip text={help} />
          {badge}
        </span>
      </span>
    );
  }

  return (
    <label className="form-row" id={rec.id}>
      <span style={{ display: "flex", alignItems: "center", gap: 6 }}>
        {label}
        {rec.unit && <span style={{ color: "var(--text-mute)" }}>({rec.unit})</span>}
        <InfoTip text={help} />
        {badge}
      </span>
      {renderInput(rec, value, onChange, t, disabled)}
    </label>
  );
}

function renderInput(rec: SettingRecord, value: FieldValue, onChange: (v: FieldValue) => void, t: TFunction, disabled?: boolean) {
  switch (rec.input) {
    case "select":
      return (
        <select value={String(value)} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
          {rec.options?.map((o) => (
            <option key={o.value} value={o.value}>{fieldOptionLabel(t, rec, o)}</option>
          ))}
        </select>
      );
    case "textarea":
      return (
        <textarea
          value={String(value)}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      );
    case "number":
      return (
        <input
          type="number"
          value={value as number}
          min={rec.min}
          max={rec.max}
          step={rec.step ?? 1}
          disabled={disabled}
          onChange={(e) => onChange(Number(e.target.value))}
        />
      );
    case "path":
    case "addr":
    case "cidr":
    case "text":
    default:
      return (
        <input
          type="text"
          value={String(value)}
          placeholder={fieldPlaceholder(t, rec)}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
}
