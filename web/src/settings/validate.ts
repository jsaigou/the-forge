// settings/validate.ts — registry-driven field validation (Sprint 3, 3C).
// Checked on blur by Field.tsx so a bad value is explained where it is typed,
// instead of surfacing later as a server banner. Pure and erasable-TypeScript
// (type-only imports) so scripts/validate.test.mjs can load it with node's
// runner. Returns an i18n key under the "settings" namespace plus its
// interpolation params, never display text — Field.tsx translates it.
import { durationSeconds } from "../lib/duration.ts";
import type { SettingRecord } from "./fields.ts";

export interface FieldIssue {
  key: "validation.required" | "validation.min" | "validation.max" | "validation.pattern" | "validation.duration";
  params: Record<string, string | number>;
}

type Value = string | number | boolean;

/** `value` is the STORED value (before `scale` is applied). */
export function validateField(rec: Pick<SettingRecord, "input" | "required" | "min" | "max" | "scale" | "pattern" | "patternHint" | "unit">, value: Value): FieldIssue | null {
  if (rec.input === "toggle" || rec.input === "select") return null;

  if (rec.input === "number") {
    const scale = rec.scale ?? 1;
    const shown = Number(value) / scale;
    if (typeof value === "string" && value.trim() === "") return rec.required ? { key: "validation.required", params: {} } : null;
    if (!Number.isFinite(shown)) return { key: "validation.required", params: {} };
    if (rec.min !== undefined && shown < rec.min) return { key: "validation.min", params: { min: rec.min } };
    if (rec.max !== undefined && shown > rec.max) return { key: "validation.max", params: { max: rec.max } };
    return null;
  }

  const text = String(value).trim();
  if (text === "") return rec.required ? { key: "validation.required", params: {} } : null;

  if (rec.input === "duration") {
    const secs = durationSeconds(text);
    if (secs === null) return { key: "validation.duration", params: {} };
    if (rec.min !== undefined && secs < rec.min) return { key: "validation.min", params: { min: `${rec.min}s` } };
    if (rec.max !== undefined && secs > rec.max) return { key: "validation.max", params: { max: `${rec.max}s` } };
    return null;
  }

  if (rec.pattern && !new RegExp(rec.pattern).test(text)) {
    return { key: "validation.pattern", params: { hint: rec.patternHint ?? "" } };
  }
  return null;
}
