import type { ConfigCard, ModelCard } from "./types";

// §0.7: order the gallery by usage (most-used first). Primary sort key is
// derived.reliability.loads_ok (desc). Returns a new array; does not mutate
// the input. Ties keep the registry's original (curated) order via stable sort.
// Lives in lib (not ModelCardView) so the component file stays component-only
// for react-refresh.
export function sortModelCardsByUsage(cards: ModelCard[]): ModelCard[] {
  return [...cards].sort((a, b) => {
    const aOk = a.derived.reliability?.loads_ok ?? 0;
    const bOk = b.derived.reliability?.loads_ok ?? 0;
    return bOk - aOk;
  });
}

// Config gallery migration (ADR 0006, grilling Q3/Q4c): the console gallery
// sort toggle for ConfigCard. Primary key is the selected mode; ties (and,
// per Q4c, sibling configs of the same model regardless of tie) put the
// is_default config first, then fall back to alpha for stability.
// "favorites" (design/QA 2026-11-26) shows ONLY starred configs, A-Z; it needs starredIds, and with none supplied it is empty.
export type ConfigSortMode = "favorites" | "alpha" | "use" | "new";

/** The view the gallery opens on: the favorites when at least one config is starred, otherwise every config, so a new install shows what it has. */
export function defaultConfigSort(cards: ConfigCard[], starredIds: Set<number>): ConfigSortMode {
  return cards.some((c) => starredIds.has(c.id)) ? "favorites" : "alpha";
}

// starredIds (product/QA sprint, 2026-07-29): when supplied, starred
// configs sort before everything else, ahead of the selected mode's own
// ordering — "starred items will appear before other items", regardless
// of which sort mode is active.
export function sortConfigCards(cards: ConfigCard[], mode: ConfigSortMode, starredIds?: Set<number>): ConfigCard[] {
  const byName = (a: ConfigCard, b: ConfigCard) => a.name.toLowerCase().localeCompare(b.name.toLowerCase());
  const pool = mode === "favorites" ? cards.filter((c) => starredIds?.has(c.id)) : cards;
  return [...pool].sort((a, b) => {
    if (starredIds) {
      const aStar = starredIds.has(a.id);
      const bStar = starredIds.has(b.id);
      if (aStar !== bStar) return aStar ? -1 : 1;
    }
    let primary: number;
    switch (mode) {
      case "use":
        primary = (b.derived.reliability?.loads_ok ?? 0) - (a.derived.reliability?.loads_ok ?? 0);
        break;
      case "new":
        primary = b.created_at - a.created_at;
        break;
      case "favorites":
      case "alpha":
      default:
        primary = byName(a, b);
        break;
    }
    if (primary !== 0) return primary;
    if (a.model_id === b.model_id && a.is_default !== b.is_default) {
      return a.is_default ? -1 : 1;
    }
    return byName(a, b);
  });
}
