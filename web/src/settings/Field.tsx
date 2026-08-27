import { ApplyBadge } from "../components/ApplyBadge";
import { InfoTip } from "../components/InfoTip";
import type { SettingRecord } from "./fields";

// settings/Field.tsx — Sprint 12 (was H) Phase 6. Renders one SettingRecord:
// label → InfoTip (rec.help) → ApplyBadge (rec.apply) → the right input.
// The registry owns presentation metadata only, never data plumbing — value/
// onChange/disabled come from the caller's own useSettingsGroup draft, same
// as every hand-written `<label className="form-row">` site elsewhere in
// Settings did before this.

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
          {rec.label}
          <InfoTip text={rec.help} />
          {badge}
        </span>
      </span>
    );
  }

  return (
    <label className="form-row" id={rec.id}>
      <span style={{ display: "flex", alignItems: "center", gap: 6 }}>
        {rec.label}
        {rec.unit && <span style={{ color: "var(--text-mute)" }}>({rec.unit})</span>}
        <InfoTip text={rec.help} />
        {badge}
      </span>
      {renderInput(rec, value, onChange, disabled)}
    </label>
  );
}

function renderInput(rec: SettingRecord, value: FieldValue, onChange: (v: FieldValue) => void, disabled?: boolean) {
  switch (rec.input) {
    case "select":
      return (
        <select value={String(value)} disabled={disabled} onChange={(e) => onChange(e.target.value)}>
          {rec.options?.map((o) => (
            <option key={o.value} value={o.value}>{o.label}</option>
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
          placeholder={rec.placeholder}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      );
  }
}
