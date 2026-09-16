-- SPDX-License-Identifier: Apache-2.0
-- Schema v88 (virtual models, 2026-09-15). A virtual model is a
-- wire-visible name a consumer can request when it doesn't care which real
-- config serves it, only that the config has some general characteristic —
-- "give me whatever's smart" or "give me whatever's fast" — resolved
-- dynamically at request time, never pinned to one config_id the way
-- model_aliases (0086) is.
--
-- Two resolution kinds, deliberately NOT the same axis:
--   capability_tier: points at a capability_tiers row (curated, ranked by
--     relative capability — ADR-0014's whole "curated, never derived"
--     rationale). Resolves to whichever tier member is already loaded and
--     ranks best; if none are loaded, the tier's own rank-1 member (a real
--     load is triggered, same as requesting that config directly).
--   throughput: NOT tied to any capability tier — capability_tier_id stays
--     NULL. Resolves against real measured decode_tps across every visible
--     config, which (unlike cross-benchmark capability scores) is a single
--     directly-comparable number on this host, safe to rank automatically.
--     A config that has never been throughput-profiled is never a
--     candidate — no guessing. Same already-loaded-first rule.
-- The CHECK below keeps the two shapes from ever being stored
-- inconsistently (a throughput row with a dangling tier reference, or a
-- capability_tier row with none at all).
--
-- ON DELETE CASCADE on capability_tier_id, same reasoning as model_aliases'
-- own FK: a capability_tier-kind virtual model with no target tier is
-- meaningless, not a valid degraded state — delete it with the tier
-- instead of leaving a dangling reference (perf_classes' own FK, by
-- contrast, is ON DELETE SET NULL because a config without a tier is a
-- perfectly normal state; a virtual model without one is not).
CREATE TABLE virtual_models (
    id                  INTEGER PRIMARY KEY,
    name                TEXT NOT NULL UNIQUE,
    kind                TEXT NOT NULL CHECK (kind IN ('capability_tier', 'throughput')),
    capability_tier_id  INTEGER REFERENCES capability_tiers(id) ON DELETE CASCADE,
    visibility          TEXT NOT NULL DEFAULT 'visible' CHECK (visibility IN ('visible', 'hidden')),
    notes               TEXT NOT NULL DEFAULT '',
    CHECK (
        (kind = 'capability_tier' AND capability_tier_id IS NOT NULL) OR
        (kind = 'throughput' AND capability_tier_id IS NULL)
    )
);
