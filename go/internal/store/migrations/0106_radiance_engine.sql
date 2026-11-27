-- SPDX-License-Identifier: Apache-2.0
-- Radiance Phase 1b step f2 (ADR-0026 section 5, plan f2). Additive only.
--
--   1. The engine row for the Radiance kind and its container format. The engines and formats tables are GET-only through the API
--      (ADR-0020 F8), so engine-level data needs a migration. INSERT OR IGNORE: a row an operator already added keeps its values.
--      Quantization names are NOT seeded here; they arrive with the model that needs them.
--   2. builds.launch_env: the host launch environment of an engine build (for Radiance RADIANCE_HOME, RADIANCE_KERNELS, ROCM_PATH,
--      LD_LIBRARY_PATH and similar) as a JSON object of strings, NULL when the build adds nothing. It is written only through the
--      guarded build-write API, which validates names, values and paths (store/build_writes.go, buildguard.CheckLaunchEnv). Nothing
--      about a host layout is a constant in Go; the launch builder copies this map into the slot_launches row's env_json.
INSERT OR IGNORE INTO engines (name, kind) VALUES ('Radiance', 'radiance');
INSERT OR IGNORE INTO formats (name) VALUES ('rad');
ALTER TABLE builds ADD COLUMN launch_env TEXT;
