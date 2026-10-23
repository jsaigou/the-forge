-- SPDX-License-Identifier: Apache-2.0
-- Data fix (2026-10-23). Registers the kintsugi branch rebased onto
-- upstream ggml-org/llama.cpp 7170cad4d (2026-10-22) as two additional,
-- not-yet-referenced builds beside the existing kintsugi ones, so a
-- config can be pointed at them (and pointed back) without touching
-- builds 9/10. No config is switched by this migration.
--
-- Derived from the existing kintsugi build rows (same engine_id) rather
-- than hard-coding an engine, and guarded so it is a no-op on any DB that
-- lacks those rows or already has these names. The rebased binaries need
-- `--load-mode none` where the old ones took `--no-mmap` (upstream
-- removed --mmap/--no-mmap/--mlock/--direct-io in ggml-org/llama.cpp
-- #28334); builds 9/10 already accept --load-mode.
INSERT INTO builds (engine_id, name, binary_path, backend, reason)
SELECT b.engine_id,
       'llama.cpp-kintsugi-rebase/build-vulkan-rebase',
       '/opt/forge/llama.cpp-kintsugi-rebase/build-vulkan-rebase/bin/llama-server',
       'vulkan',
       'kintsugi rebased onto upstream 7170cad4d (2026-10-22), branch rebase/upstream-20261022 - Vulkan; needs --load-mode none (no --no-mmap)'
FROM builds b
WHERE b.name = 'llama.cpp-kintsugi/build-vulkan-new'
  AND NOT EXISTS (SELECT 1 FROM builds WHERE name = 'llama.cpp-kintsugi-rebase/build-vulkan-rebase');

INSERT INTO builds (engine_id, name, binary_path, backend, reason)
SELECT b.engine_id,
       'llama.cpp-kintsugi-rebase/build-rocm-rebase',
       '/opt/forge/llama.cpp-kintsugi-rebase/build-rocm-rebase/bin/llama-server',
       'rocm',
       'kintsugi rebased onto upstream 7170cad4d (2026-10-22), branch rebase/upstream-20261022, rocm-therock-10.1 - verified on Flash-Next + MTP at 262k (cache reuse intact, ~2x decode); needs --load-mode none (no --no-mmap)'
FROM builds b
WHERE b.name = 'llama.cpp-kintsugi/build-rocm-new'
  AND NOT EXISTS (SELECT 1 FROM builds WHERE name = 'llama.cpp-kintsugi-rebase/build-rocm-rebase');
