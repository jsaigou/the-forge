-- SPDX-License-Identifier: Apache-2.0
-- ADR-0020 F10 / phase 5. configs.parallel was accepted, validated, stored and returned but never read: the slot
-- count is the --parallel flag in extra_args (registry.go has said so since the column went dead). Migration 0094
-- first copied every config's column value into extra_args where the flag was missing, so dropping it loses nothing
-- the launcher used.
ALTER TABLE configs DROP COLUMN parallel;
