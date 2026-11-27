-- SPDX-License-Identifier: Apache-2.0
-- The Radiance 2.x engine kind (docs/adr/0033-radiance-2x-yaml-launch.md). Additive only.
--
-- Radiance 2.x is the same engine with the same HTTP surface (health, /v1/models, /metrics), but it refuses command-line flags and reads one YAML
-- file, so its launch differs: the stored slot launch keeps the flag form and `forge slot-exec` writes the file when the slot starts. The engine
-- row carries the kind that selects that. The engines table is GET-only through the API (ADR-0020 F8), so the row needs a migration.
-- INSERT OR IGNORE: an engine row an operator already named this keeps its values. No build, config or artifact is touched.
INSERT OR IGNORE INTO engines (name, kind) VALUES ('Radiance 2.x', 'radiance2');
