-- SPDX-License-Identifier: Apache-2.0
-- Radiance rollout (docs/radiance-phase-1b-plan.md step f2): the quantizations table is GET-only through the API (ADR-0020 F8), so the
-- name the first Radiance model needs arrives with a migration. 'FP8' is the E4M3 weight format of the Radiance container
-- (`qwen3.6-35b-a3b-fp8.rad`). INSERT OR IGNORE: a row an operator already added keeps its values. Additive only.
INSERT OR IGNORE INTO quantizations (name) VALUES ('FP8');
