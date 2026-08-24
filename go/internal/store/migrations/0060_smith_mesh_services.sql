-- SPDX-License-Identifier: Apache-2.0
-- Schema v60 — smith.mesh.services seam (open-source-readiness follow-up to
-- the Sprint 6 build_refresh eval, .sweep/build-refresh-eval-2026-08-20.md
-- §"Open-source readiness audit" finding 1).
--
-- The reachability family's mesh inventory used to be a hardcoded Go map in
-- internal/smith/answers.go (meshAddress) plus a second curated alias list
-- in intents.go — deployment topology compiled into the binary, wrong on
-- any other install.
--
-- TWO-LAYER KNOWLEDGE ARCHITECTURE (operator directive 2026-08-21): product
-- knowledge ships; live-environment data never ships. This migration
-- therefore creates the seam EMPTY — no deployment values in source. Each
-- install imports its own mesh inventory from an operator-maintained local
-- file via `foundryd smith import-local <file>` (internal/smith/
-- local_seed.go; docs/examples/smith-local-seed.example.json shows the
-- shape with synthetic values). An empty inventory is a valid state: the
-- reachability family then only knows the code-curated live probes
-- ("internet"/"tailnet").
--
-- Entry shape (imported, not seeded): {"name": canonical entity id,
-- "aliases": surface phrases the classifier matches, "address": host[:port]
-- reported by reachability answers}.

INSERT OR IGNORE INTO settings (key, value, updated_at) VALUES
    ('smith.mesh.services', '[]', strftime('%s', 'now'));
