// lib/offeringPreference.ts — Phase 7. The "which offering does a0 actually
// present for this model" rule, shared by CatalogPanel's Offerings section
// and Settings → Routing's model→provider map so the "preferred" badge
// can't drift between the two UIs. Mirrors
// router.GroupOfferingsByModel/SelectOfferingChain (go/internal/router/select.go)
// exactly: among enabled offerings of enabled providers, the lowest
// priority value wins; ties break by provider name then offering id.
import type { CatalogOffering } from "./types";

/** preferredOfferingIds returns the set of offering ids that are each their
 * model group's primary among 2+ routable offerings. Single-provider
 * models have nothing to prefer between and get no badge. */
export function preferredOfferingIds(offerings: CatalogOffering[], providerEnabled: (provider: string) => boolean): Set<number> {
  const routable = offerings.filter((o) => o.enabled && providerEnabled(o.provider));
  const countByModel = new Map<number, number>();
  for (const o of routable) countByModel.set(o.model_id, (countByModel.get(o.model_id) ?? 0) + 1);
  const sorted = [...routable].sort((a, b) => a.priority - b.priority || a.provider.localeCompare(b.provider) || a.id - b.id);
  const primaryByModel = new Map<number, number>();
  for (const o of sorted) {
    if (!primaryByModel.has(o.model_id)) primaryByModel.set(o.model_id, o.id);
  }
  const preferred = new Set<number>();
  for (const [modelId, offeringId] of primaryByModel) {
    if ((countByModel.get(modelId) ?? 0) > 1) preferred.add(offeringId);
  }
  return preferred;
}

/** groupOfferingsByModel groups offerings by model_id, each group sorted
 * (priority, provider name, id) — the same order preferredOfferingIds uses
 * internally, exposed for UIs that render one row per model. */
export function groupOfferingsByModel(offerings: CatalogOffering[]): Map<number, CatalogOffering[]> {
  const groups = new Map<number, CatalogOffering[]>();
  for (const o of offerings) {
    const g = groups.get(o.model_id);
    if (g) g.push(o);
    else groups.set(o.model_id, [o]);
  }
  for (const g of groups.values()) {
    g.sort((a, b) => a.priority - b.priority || a.provider.localeCompare(b.provider) || a.id - b.id);
  }
  return groups;
}
