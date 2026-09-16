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
