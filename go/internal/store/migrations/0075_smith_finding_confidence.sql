-- SPDX-License-Identifier: Apache-2.0
-- Schema v75 (Tier 1 Sprint 4, smith diagnosis upgrades, 2026-08-27).
--
-- Confidence-scored findings: smith_findings never carried any gradation
-- beyond severity — a check that read every probe it wanted and one that
-- fell back to a degraded/missing source looked identical in the API and
-- the UI. Confidence is derived from evidence completeness (which probes a
-- check actually managed to read), never guessed by a model — there is no
-- LLM in the deterministic check path at all. Additive, no backfill needed:
-- every pre-existing row was written by a check that either read
-- everything it needed or errored out entirely (SeverityCrit via runOne's
-- panic recovery) — "high confidence" describes that prior behavior
-- exactly, matching Finding.normalize()'s own default.
ALTER TABLE smith_findings ADD COLUMN confidence TEXT NOT NULL DEFAULT 'high';
ALTER TABLE smith_findings ADD COLUMN confidence_note TEXT NOT NULL DEFAULT '';
