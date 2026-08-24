-- SPDX-License-Identifier: Apache-2.0
-- Schema v55 (smith S3 — resolution loop, docs/v5-smith-experience.md §2.4).
-- Adds resolved_by_action_id to smith_investigations so the FE can render a
-- "resolved by" link pointing at the action that closed the investigation.
-- Additive ALTER TABLE — NULL for all existing rows (no data migration needed).
ALTER TABLE smith_investigations ADD COLUMN resolved_by_action_id INTEGER;
