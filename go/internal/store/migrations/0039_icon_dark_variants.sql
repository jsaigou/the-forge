-- SPDX-License-Identifier: Apache-2.0
-- Schema v39 (pre-release feedback sprint, Phase 3 — Models + icon system,
-- 2026-08-12). Adds a dark-theme icon override at every level the icon chain
-- already has a light one (0027_icon_hierarchy.sql): genealogy -> family ->
-- model -> config. "" falls back to the level's own light logo
-- (registry.resolveLogos). Purely additive; every existing row keeps
-- rendering exactly as today in both themes until an operator sets a dark
-- variant, since a light-only entity now just serves the same mark for both.
ALTER TABLE genealogies ADD COLUMN logo_dark TEXT NOT NULL DEFAULT '';
ALTER TABLE families    ADD COLUMN logo_dark TEXT NOT NULL DEFAULT '';
ALTER TABLE models      ADD COLUMN logo_dark TEXT NOT NULL DEFAULT '';
ALTER TABLE configs     ADD COLUMN logo_dark TEXT NOT NULL DEFAULT '';
