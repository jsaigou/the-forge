-- SPDX-License-Identifier: Apache-2.0
-- ADR-0022 (specialist slot pool, draft): a slot belongs to a pool. 'llm' (every existing row) is the four LLM slots and behaves exactly as before;
-- 'specialist' is the pool for small, specialised, often always-on models and services. No CHECK on purpose: the values are validated in
-- code, so a later pool needs no table rewrite (see the SQLite CHECK-widening trap in the project notes). Additive: the previous binary keeps
-- running on this schema because it never selects the column.
ALTER TABLE slots ADD COLUMN pool TEXT NOT NULL DEFAULT 'llm';
