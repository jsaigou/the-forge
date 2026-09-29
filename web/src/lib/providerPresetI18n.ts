import type { ProviderPreset } from "./providerPresets";

// Translation lookup for lib/providerPresets.ts's dataResidencyGroup/note,
// kept out of providerPresets.ts itself so PROVIDER_PRESETS stays pure
// English data (id/label/URLs/currency/country codes are identifiers or
// brand names and never translated). Mirrors lib/flagI18n.ts's house
// pattern for the sibling llamaFlags.ts i18n work.
type Translator = (key: string, opts?: Record<string, unknown>) => string;

// dataResidencyGroup shows up both on a live provider row (Provider.data_residency_group,
// a plain string copied from the preset at creation time) and on a preset
// itself — callers only ever have the string value, not necessarily the
// preset object, so this takes the string directly.
export function translatedDataResidencyGroup(t: Translator, group: string): string {
  if (!group) return group;
  return t(`common:provider_presets.data_residency_groups.${group}`, { defaultValue: group });
}

export function translatedProviderNote(t: Translator, preset: Pick<ProviderPreset, "id" | "note">): string | undefined {
  if (!preset.note) return undefined;
  return t(`common:provider_presets.notes.${preset.id}`, { defaultValue: preset.note });
}
