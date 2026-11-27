import type { CatalogConfig } from "./types";

// lib/configPayload.ts (2026-09-14) — UpdateConfig/CreateConfig are
// full-replace: any field the JSON body omits is decoded server-side as its
// Go zero value and overwrites whatever was there (go/internal/httpapi/
// catalog_configs.go's configBody, go/internal/store/catalog.go's
// UpdateConfig). Both config editors used to build their write payload as
// `Partial<CatalogConfig>`, which type-checks even when a field with no
// form control is simply left out — and both did, independently: a config
// saved from CatalogPanel's ConfigForm silently wiped fingerprint/
// chat_template_caps_override/reasoning_effort_default (no control existed
// for any of the three); a config saved from ConfigEditView silently wiped
// capability_tier_id/capability_rank (fixed once already for chat_template_caps_
// override/reasoning_effort_default when they were added, per that file's
// own comment — but the fix carries the value through unchanged rather than
// exposing a control, so an operator still has no way to ever set it there).
//
// ConfigWritePayload omits id/chat_template_caps/chat_template_caps_probed_at
// — a write must never send any of the three. id travels in the URL only
// (server-side configBody has no id/ID field at all — DisallowUnknownFields
// rejects a body carrying one as "unknown field \"id\"", found live
// 2026-09-14 verifying this exact type's first use). The two probe fields
// are genuinely absent from configBody too — "deliberately absent... they
// are read-only, written only by the engine's post-load probe" per its own
// comment — so they 400 the same way (also found live the same session,
// immediately after the id fix). Every other CatalogConfig field stays
// required, matching CatalogConfig's own requiredness, so a form building
// its payload field-by-field from local state fails `tsc` the moment it
// omits a real writable field instead of compiling clean and wiping that
// field in production.
//
// These three are OMITTED here, not merely optional — but note that does
// NOT make `{...existing, oneField: x}` (existing: CatalogConfig) fail
// `tsc`: TypeScript's excess-property check only applies to a literal's own
// directly-written properties, not ones arriving through a spread, so a
// spread carrying id/chat_template_caps/chat_template_caps_probed_at
// type-checks fine and then 400s at runtime as "unknown field" (confirmed
// live 2026-09-14 — this is a real TS limitation, not a hypothetical one).
// Every spread-from-a-full-record call site (icon select/clear handlers,
// the Model Behavior matrix's patch()) must explicitly strip these three —
// `const { id, chat_template_caps, chat_template_caps_probed_at, ...rest }
// = existing;` — by hand; the type only protects the field-by-field
// construction the two main config editors already do. A config field
// added to CatalogConfig in the future is still required here
// automatically, with no separate list to remember to update.
export type ConfigWritePayload = Omit<CatalogConfig, "id" | "chat_template_caps" | "chat_template_caps_probed_at">;

// Read-only on the wire: a write must never carry these (see above). The server drops them if they arrive from a GET body, but the decode of
// a hand-built body still rejects them, so the one helper below is the only place that knows the list.
export const CONFIG_READ_ONLY_KEYS = ["id", "chat_template_caps", "chat_template_caps_probed_at"] as const satisfies readonly (keyof CatalogConfig)[];

// toConfigWritePayload turns a full Config record into a write body: the read-only fields are stripped and `changes` replace what the caller
// edits, so a save can never wipe a field the caller does not show. Use it at every site that starts from an existing record; a form that
// builds its body field by field keeps the field-by-field construction (the type above checks that one).
export function toConfigWritePayload(existing: CatalogConfig, changes: Partial<ConfigWritePayload> = {}): ConfigWritePayload {
  const { id: _id, chat_template_caps: _caps, chat_template_caps_probed_at: _probedAt, ...rest } = existing;
  return { ...rest, ...changes };
}

// Every key of CatalogConfig, at runtime. Typing it as Record<keyof CatalogConfig, true> makes `tsc` fail the moment a field is added to (or
// removed from) CatalogConfig without being listed here, and scripts/config-fields.test.mjs checks this list against the Go side's
// golden field list (go/internal/httpapi/catalog_configs_fields_test.go), so the two languages cannot drift apart silently.
export const CATALOG_CONFIG_KEYS: Record<keyof CatalogConfig, true> = {
  id: true,
  name: true,
  variant_id: true,
  weight_artifact_id: true,
  engine_id: true,
  build_id: true,
  mmproj_artifact_id: true,
  n_ctx: true,
  extra_args: true,
  status: true,
  visibility: true,
  is_default: true,
  fingerprint: true,
  logo: true,
  logo_dark: true,
  modalities: true,
  output_modalities: true,
  capability_tier_id: true,
  capability_rank: true,
  chat_template_caps: true,
  chat_template_caps_probed_at: true,
  chat_template_caps_override: true,
  reasoning_effort_default: true,
};
