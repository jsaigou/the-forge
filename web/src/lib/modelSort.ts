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
export type ConfigSortMode = "alpha" | "use" | "new";

// starredIds (product/QA sprint, 2026-07-29): when supplied, starred
// configs sort before everything else, ahead of the selected mode's own
// ordering — "starred items will appear before other items", regardless
// of which sort mode is active.
export function sortConfigCards(cards: ConfigCard[], mode: ConfigSortMode, starredIds?: Set<number>): ConfigCard[] {
  const byName = (a: ConfigCard, b: ConfigCard) => a.name.toLowerCase().localeCompare(b.name.toLowerCase());
  return [...cards].sort((a, b) => {
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
