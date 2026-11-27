import { useEffect, useId, useRef, useState } from "react";
import type { TFunction } from "i18next";
import { useTranslation } from "react-i18next";
import { ApplyBadge } from "../components/ApplyBadge";
import { InfoTip } from "../components/InfoTip";
import { DURATION_UNITS, formatDuration, parseDuration, type DurationUnit } from "../lib/duration.ts";
import type { SettingRecord } from "./fields";
import { validateField } from "./validate.ts";

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
  row,
}: {
  rec: SettingRecord;
  value: FieldValue;
  onChange: (v: FieldValue) => void;
  disabled?: boolean;
  hideApplyBadge?: boolean;
  /** Render as one line of a settings table: group, label, input, what the value does now, and when it applies. */
  row?: { group: string; effect: string };
}) {
  const { t } = useTranslation("settings");
  const errId = useId();
  const [touched, setTouched] = useState(false);
  const label = fieldLabel(t, rec);
  const help = fieldHelp(t, rec);
  const badge = hideApplyBadge ? null : <ApplyBadge mode={rec.apply} />;
  if (rec.input === "toggle") {
    // ADR-0023 contract (a): a real input[type=checkbox] in a <label> — the
    // pre-Sprint-1 version was a span+onClick fake: mouse-only, no role, no
    // keyboard path (review §A1). The checkbox itself is styled as the
    // switch (see .toggle input[type=checkbox] in theme.css).
    return (
      <label
        id={rec.id}
        className={`toggle ${disabled ? "disabled" : ""}`}
      >
        <input
          type="checkbox"
          checked={Boolean(value)}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span className="field-label-line">
          {label}
          <InfoTip text={help} />
          {badge}
        </span>
      </label>
    );
  }

  // Registry-driven validation (review §A3): explain a bad value where it is
  // typed, once the field has been left. Re-validates live after that.
  const issue = touched ? validateField(rec, value) : null;
  const common = {
    disabled,
    invalid: !!issue,
    describedBy: issue ? errId : undefined,
    onBlur: () => setTouched(true),
    ariaLabel: row ? label : undefined,
  };

  if (row) {
    return (
      <div className="setrow" id={rec.id}>
        <span className="setrow-group">{row.group}</span>
        <span className="field-label-line">
          {label}
          <InfoTip text={help} />
        </span>
        <span className="setrow-value">
          {renderInput(rec, value, onChange, t, common)}
          {rec.unit && <span className="field-unit">{rec.unit}</span>}
        </span>
        <span className="setrow-effect">
          {issue ? <span id={errId} className="field-error" role="alert">{t(issue.key, issue.params)}</span> : row.effect}
        </span>
        <span>{badge}</span>
      </div>
    );
  }

  return (
    <label className="form-row" id={rec.id}>
      <span className="field-label-line">
        {label}
        {rec.unit && <span className="field-unit">({rec.unit})</span>}
        <InfoTip text={help} />
        {badge}
      </span>
      {renderInput(rec, value, onChange, t, common)}
      {issue && (
        <span id={errId} className="field-error" role="alert">
          {t(issue.key, issue.params)}
        </span>
      )}
    </label>
  );
}

interface InputCommon {
  disabled?: boolean;
  invalid: boolean;
  describedBy?: string;
  onBlur: () => void;
  ariaLabel?: string;
}

function renderInput(rec: SettingRecord, value: FieldValue, onChange: (v: FieldValue) => void, t: TFunction, c: InputCommon) {
  const aria = { "aria-invalid": c.invalid || undefined, "aria-describedby": c.describedBy, "aria-label": c.ariaLabel };
  switch (rec.input) {
    case "select":
      return (
        <select value={String(value)} disabled={c.disabled} onChange={(e) => onChange(e.target.value)}>
          {rec.options?.map((o) => (
            <option key={o.value} value={o.value}>{fieldOptionLabel(t, rec, o)}</option>
          ))}
        </select>
      );
    case "textarea":
      return (
        <textarea
          value={String(value)}
          disabled={c.disabled}
          onChange={(e) => onChange(e.target.value)}
          onBlur={c.onBlur}
          {...aria}
        />
      );
    case "number": {
      // `scale`: stored = displayed × scale (the hang threshold is stored in
      // thousandths of a token/sec and edited as tokens/sec).
      const scale = rec.scale ?? 1;
      const shown = typeof value === "number" ? Number((value / scale).toFixed(6)) : value;
      return (
        <input
          type="number"
          value={shown as number}
          min={rec.min}
          max={rec.max}
          step={rec.step ?? 1}
          disabled={c.disabled}
          onChange={(e) => {
            if (rec.optional && e.target.value === "") return onChange("");
            onChange(scale === 1 ? Number(e.target.value) : Math.round(Number(e.target.value) * scale));
          }}
          onBlur={c.onBlur}
          {...aria}
        />
      );
    }
    case "duration":
      return <DurationInput rec={rec} value={String(value)} onChange={onChange} t={t} c={c} />;
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
          disabled={c.disabled}
          onChange={(e) => onChange(e.target.value)}
          onBlur={c.onBlur}
          {...aria}
        />
      );
  }
}

// Go-duration fields ("60m", "6h") edited as a number plus a unit select —
// the stored value stays the Go string the backend parses. A stored value the
// widget cannot represent (sub-second units, a hand-typed oddity) falls back
// to the raw text so it stays visible and editable instead of being silently
// rewritten.
function DurationInput({ rec, value, onChange, t, c }: { rec: SettingRecord; value: string; onChange: (v: FieldValue) => void; t: TFunction; c: InputCommon }) {
  const parsed = parseDuration(value);
  const lastEmitted = useRef(value);
  const [unit, setUnit] = useState<DurationUnit>(parsed?.unit ?? "m");
  const [draft, setDraft] = useState<string>(parsed ? String(parsed.n) : "");

  // Resync when the stored value changes from outside (Reset, reload) — not
  // for the echo of our own emit, so typing "1." is not clobbered.
  useEffect(() => {
    if (value === lastEmitted.current) return;
    lastEmitted.current = value;
    const p = parseDuration(value);
    setUnit(p?.unit ?? "m");
    setDraft(p ? String(p.n) : "");
  }, [value]);

  if (value.trim() !== "" && !parsed) {
    return (
      <input
        type="text"
        value={value}
        disabled={c.disabled}
        onChange={(e) => onChange(e.target.value)}
        onBlur={c.onBlur}
        aria-invalid={c.invalid || undefined}
        aria-describedby={c.describedBy}
      />
    );
  }

  const emit = (rawN: string, u: DurationUnit) => {
    const next = rawN.trim() === "" ? "" : formatDuration(Number(rawN), u);
    lastEmitted.current = next;
    onChange(next);
  };

  return (
    <span className="duration-input">
      <input
        type="number"
        value={draft}
        min={0}
        step="any"
        disabled={c.disabled}
        aria-invalid={c.invalid || undefined}
        aria-describedby={c.describedBy}
        onChange={(e) => {
          setDraft(e.target.value);
          emit(e.target.value, unit);
        }}
        onBlur={c.onBlur}
      />
      <select
        value={unit}
        disabled={c.disabled}
        aria-label={t("duration.unit_label", { field: fieldLabel(t, rec) })}
        onChange={(e) => {
          const u = e.target.value as DurationUnit;
          setUnit(u);
          emit(draft, u);
        }}
        onBlur={c.onBlur}
      >
        {DURATION_UNITS.map((u) => (
          <option key={u} value={u}>{t(`duration.units.${u}`)}</option>
        ))}
      </select>
    </span>
  );
}
