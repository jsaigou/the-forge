-- SPDX-License-Identifier: Apache-2.0
-- The Forge Node Agent (forge-agent/) is removed 2026-09-14 as part of the
-- PairNode deprecation — it was the only paired node this table ever held,
-- and no v0.5 code ever read from it (the "Add node" pairing UI its
-- docstring describes was never built). Safe to drop outright: no reader,
-- no writer, anywhere in the live codebase.
DROP TABLE IF EXISTS nodes;
