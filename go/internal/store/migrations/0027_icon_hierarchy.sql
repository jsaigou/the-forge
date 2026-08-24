-- SPDX-License-Identifier: Apache-2.0
-- Schema v27 (Sprint I, pre-release readiness round 2, 2026-08-04). Adds a
-- logo column at each level above models.logo (which already exists,
-- 0008_model_catalog.sql) so registry.resolveLogo can implement a real
-- inheritance chain: config override -> model -> family -> genealogy ->
-- letter-badge fallback (docs/v5-prerelease-readiness.md, Sprint I). Purely
-- additive; every existing row keeps rendering exactly as today, since an
-- empty logo falls through to model.logo, the only source used until now.
ALTER TABLE genealogies ADD COLUMN logo TEXT NOT NULL DEFAULT '';
ALTER TABLE families    ADD COLUMN logo TEXT NOT NULL DEFAULT '';
ALTER TABLE configs     ADD COLUMN logo TEXT NOT NULL DEFAULT '';
