-- SPDX-License-Identifier: Apache-2.0
-- Schema v64 (two-layer knowledge architecture, S0 follow-up).
--
-- 0063's equality-guarded deletes compared the settings column against TEXT
-- literals, but the Go settings writer stores JSON as BLOB ([]byte through
-- the sqlite driver). Rows still holding their original SQL-seeded value
-- (TEXT) matched and cleared; rows rewritten at least once through the
-- settings API carry byte-identical content as BLOB and never match a bare
-- TEXT comparison (SQLite compares across storage classes by type order,
-- not content). This migration repeats the two smith.web.* deletes with a
-- CAST so the comparison sees content regardless of storage class.
--
-- Idempotent everywhere: no-op where 0063 already cleared the row (fresh
-- installs), effective exactly where an API rewrite changed the storage
-- class under identical content. Customized base URLs still survive — the
-- guards remain exact-value.

DELETE FROM settings WHERE key = 'smith.web.searxng'
    AND CAST(value AS TEXT) = '{"base_url":"https://searxng.example.ts.net","enabled":true,"api_key":""}';

DELETE FROM settings WHERE key = 'smith.web.firecrawl'
    AND CAST(value AS TEXT) = '{"base_url":"https://firecrawl.example.ts.net","enabled":true,"api_key":""}';
