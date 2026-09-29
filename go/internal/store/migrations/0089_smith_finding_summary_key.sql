-- SPDX-License-Identifier: Apache-2.0
-- Schema v89 (multilanguage plan Phase 3, docs/adr/0016-localization.md,
-- 2026-09-29).
--
-- SummaryKey/Params let the FE render a translated version of a finding's
-- Summary (t("smith:"+summary_key, params)) instead of the always-English
-- prose — additive, same posture as 0075's confidence/confidence_note:
-- Summary itself is untouched and stays the ground truth for logs, audit,
-- and the CLI. No backfill needed: every pre-existing row was written by a
-- check that predates this phase, so summary_key stays '' (falls back to
-- Summary, exactly today's rendering) until a future sweep re-runs the
-- now-migrated check functions.
ALTER TABLE smith_findings ADD COLUMN summary_key TEXT NOT NULL DEFAULT '';
ALTER TABLE smith_findings ADD COLUMN params TEXT NOT NULL DEFAULT '{}';
