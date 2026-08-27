// settings/sections.tsx — Sprint 12 (was H). Lightweight navigation
// metadata for the Settings sidebar: key/label/group only, no component
// references. Settings.tsx (which already has every panel component in
// scope) maps this metadata to its own local component map — keeping this
// file free of any import on Settings.tsx or the panel components avoids a
// circular import (Settings.tsx → sections.tsx → Settings.tsx).
//
// Phase 5 update: Benchmarks and Profiling promoted out of the Catalog
// sub-tabs to their own sections; Currency + Cost & power merged into one
// "billing" section (two cards, see settings/panels/Billing.tsx).
//
// Phase 6 update: adds the net-new general/routing/monitoring/scheduling
// sections (the ~25 previously-CLI-only settings, see settings/fields.ts)
// and regroups to the plan's target 6-group shape — General/Money/Traffic/
// Workload/Catalog/Access.
//
// Phase 7 update (Sprint 12 was for a different Phase 7 — this is the
// pre-release feedback sprint's Phase 7, 2026-08-13): "danger" was already
// the 12th/final section (infra.server/paths/ports/tailscale + the daemon
// restart action, behind the Danger Zone's arm+step-up+preflight gate,
// settings/DangerZone.tsx). This update instead REMOVES "compressor" —
// Routing and Compressor are two halves of one request path, so Compressor's
// two cards now render from Routing.tsx directly (settings/panels/Routing.tsx)
// instead of owning a separate section. A bookmarked #settings/compressor link
// redirects to routing (see Settings.tsx's RETIRED_SLUGS).
//
// Phase 8 update (pre-release feedback sprint, same day): REMOVES
// "profiling" the same way — folded into "benchmarks", now labeled
// "Benchmarks & Profiling", since profiling produces exactly the kind of
// data (measured memory, prefill/decode T/s) that belongs next to the
// curated benchmarks it should be compared against. A bookmarked
// #settings/profiling link redirects to benchmarks.

export type SectionKey =
  | "general"
  | "providers"
  | "billing"
  | "routing"
  | "scheduling"
  | "monitoring"
  | "smith"
  | "voice"
  | "catalog"
  | "benchmarks"
  | "security"
  | "danger";

export interface SectionMeta {
  key: SectionKey;
  label: string;
  group: string;
  /** Bottom-of-nav, crit-tinted treatment. */
  danger?: boolean;
}

// Group order is the render order of the sidebar's group headers.
export const SETTINGS_SECTIONS: SectionMeta[] = [
  { key: "general", label: "General", group: "General" },
  { key: "providers", label: "Providers", group: "Money" },
  { key: "billing", label: "Billing", group: "Money" },
  { key: "routing", label: "Routing & Compressor", group: "Traffic" },
  { key: "scheduling", label: "Scheduling", group: "Workload" },
  { key: "monitoring", label: "Monitoring", group: "Workload" },
  { key: "smith", label: "smith", group: "Workload" },
  { key: "voice", label: "Voice & Speech", group: "Workload" },
  { key: "catalog", label: "Catalog", group: "Catalog" },
  { key: "benchmarks", label: "Benchmarks & Profiling", group: "Catalog" },
  { key: "security", label: "Security", group: "Access" },
  { key: "danger", label: "Danger Zone", group: "Danger", danger: true },
];

export const SETTINGS_GROUPS: string[] = [...new Set(SETTINGS_SECTIONS.map((s) => s.group))];

export const DEFAULT_SECTION: SectionKey = "general";

export function isSectionKey(v: unknown): v is SectionKey {
  return typeof v === "string" && SETTINGS_SECTIONS.some((s) => s.key === v);
}
