import { findProfileForConfig } from "./profileJoin";
import type { CatalogBenchmark, CatalogConfig, CatalogModel, CatalogOffering, CatalogVariant, ProfileListItem } from "./types";

// benchmarkGrouping — Phase 8 (pre-release feedback sprint). Regroups the
// flat, all-subject-types benchmark list by CONFIG — the unit an operator
// actually thinks in — mirroring the Go registry's benchesForConfig union
// (model ∪ variant ∪ config, most-specific first) so the FE view can never
// show something the backend's card-building code wouldn't also show.
//
// Three kinds of benchmark row this can't place under a config:
//   - a model with zero configs (remote-only) — grouped separately so its
//     model-scoped benchmarks (and offerings, for context) aren't lost.
//   - subject_type="offering" — deliberately never reaches any card (see
//     registry.go's loadSnapshot comment); surfaced honestly as legacy.
//   - an orphan — a subject_id pointing at a deleted parent, or a
//     variant/model-scoped row whose parent has no configs AND (for a
//     model) no offerings either. subjectLabel() already degrades these to
//     a bare "#42" elsewhere in the app.
//
// The `placed` accounting below is the structural guard against a benchmark
// silently vanishing from the regroup — there is no FE test runner in this
// repo, so this has to hold by construction, not by inspection.

export type BenchmarkScope = "config" | "variant" | "model";

export interface ScopedBenchmark {
  benchmark: CatalogBenchmark;
  scope: BenchmarkScope;
  // Human name of the entity this row is actually scoped to — always shown
  // alongside an inherited row so an operator never mistakes a model-wide
  // score for something specific to the config they're looking at.
  ownerLabel: string;
}

export interface ConfigGroup {
  config: CatalogConfig;
  model: CatalogModel | undefined;
  variant: CatalogVariant | undefined;
  profile: ProfileListItem | undefined;
  // Own rows first, then variant-inherited, then model-inherited — never
  // interleaved, so "own" is always what an operator sees first.
  benchmarks: ScopedBenchmark[];
}

export interface ModelGroup {
  model: CatalogModel;
  offerings: CatalogOffering[];
  benchmarks: ScopedBenchmark[];
}

export interface GroupedBenchmarks {
  groups: ConfigGroup[];
  remoteOnly: ModelGroup[];
  legacyOffering: CatalogBenchmark[];
  orphans: CatalogBenchmark[];
}

function scoped(b: CatalogBenchmark, scope: BenchmarkScope, ownerLabel: string): ScopedBenchmark {
  return { benchmark: b, scope, ownerLabel };
}

function pushInto(map: Map<number, CatalogBenchmark[]>, id: number, b: CatalogBenchmark) {
  const list = map.get(id);
  if (list) list.push(b);
  else map.set(id, [b]);
}

export function groupBenchmarks({
  configs,
  variants,
  models,
  offerings,
  benchmarks,
  profiles,
}: {
  configs: CatalogConfig[];
  variants: CatalogVariant[];
  models: CatalogModel[];
  offerings: CatalogOffering[];
  benchmarks: CatalogBenchmark[];
  profiles: ProfileListItem[];
}): GroupedBenchmarks {
  const modelById = new Map(models.map((m) => [m.id, m]));
  const variantById = new Map(variants.map((v) => [v.id, v]));

  const byConfigId = new Map<number, CatalogBenchmark[]>();
  const byVariantId = new Map<number, CatalogBenchmark[]>();
  const byModelId = new Map<number, CatalogBenchmark[]>();
  const legacyOffering: CatalogBenchmark[] = [];
  const orphans: CatalogBenchmark[] = [];

  // Every benchmark id starts unplaced; a row is marked placed only once it
  // actually renders somewhere below. Anything left over at the end is a
  // real orphan, not a classification bug.
  const placed = new Set<number>();

  for (const b of benchmarks) {
    switch (b.subject_type) {
      case "config":
        pushInto(byConfigId, b.subject_id, b);
        break;
      case "variant":
        pushInto(byVariantId, b.subject_id, b);
        break;
      case "model":
        pushInto(byModelId, b.subject_id, b);
        break;
      case "offering":
        legacyOffering.push(b);
        placed.add(b.id);
        break;
      default:
        orphans.push(b);
        placed.add(b.id);
    }
  }

  const groups: ConfigGroup[] = configs.map((c) => {
    const variant = variantById.get(c.variant_id);
    const model = variant ? modelById.get(variant.model_id) : undefined;

    const own = (byConfigId.get(c.id) ?? []).map((b) => {
      placed.add(b.id);
      return scoped(b, "config", c.name);
    });
    const fromVariant = variant
      ? (byVariantId.get(variant.id) ?? []).map((b) => {
          placed.add(b.id);
          return scoped(b, "variant", variant.name);
        })
      : [];
    const fromModel = model
      ? (byModelId.get(model.id) ?? []).map((b) => {
          placed.add(b.id);
          return scoped(b, "model", model.name);
        })
      : [];

    return {
      config: c,
      model,
      variant,
      profile: findProfileForConfig(profiles, c),
      benchmarks: [...own, ...fromVariant, ...fromModel],
    };
  });

  const configuredModelIds = new Set(
    configs
      .map((c) => variantById.get(c.variant_id)?.model_id)
      .filter((id): id is number => id != null),
  );

  const remoteOnly: ModelGroup[] = [];
  for (const m of models) {
    if (configuredModelIds.has(m.id)) continue;
    const modelBenches = (byModelId.get(m.id) ?? []).map((b) => {
      placed.add(b.id);
      return scoped(b, "model", m.name);
    });
    const modelOfferings = offerings.filter((o) => o.model_id === m.id);
    if (modelBenches.length === 0 && modelOfferings.length === 0) continue;
    remoteOnly.push({ model: m, offerings: modelOfferings, benchmarks: modelBenches });
  }

  for (const b of benchmarks) {
    if (!placed.has(b.id)) orphans.push(b);
  }

  return { groups, remoteOnly, legacyOffering, orphans };
}
