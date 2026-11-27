-- SPDX-License-Identifier: Apache-2.0
-- ADR-0020 F8: "retired" was encoded as a "(retired) " name prefix plus an empty binary_path, which the HF registrar
-- and smith string-matched. It is now a real column. The prefix and the empty path are still written when a build
-- is retired (for one release, so older readers keep working); this adds the column and backfills it from the
-- prefix so nothing changes for existing rows. Idempotent: a re-run only re-sets rows that are already retired.
ALTER TABLE builds ADD COLUMN retired INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0, 1));

UPDATE builds SET retired = 1 WHERE name LIKE '(retired) %';
