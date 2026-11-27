-- SPDX-License-Identifier: Apache-2.0
-- Data fix (2026-11-01). Registers three ROCm llama.cpp builds as additional,
-- not-yet-referenced rows beside the existing kintsugi ones, so configs can be
-- pointed at them (and back) without touching builds 9/10/16. No config is
-- switched by this migration.
--
--   * rebase2  - kintsugi's two carried patches (cross-turn cache reuse,
--                MTP/EOG checkpoint fix) on upstream a91eab67 (2026-10-31),
--                branch rebase/upstream-20261101.
--   * clover2  - rebase2 plus ggml-org/llama.cpp#31306 (Clover, draft-self)
--                and the server entropy-accept flags; EXPERIMENTAL, branch
--                feature/clover-draft-self-r2.
--   * plain    - unpatched upstream a91eab67, kept to measure whether the
--                carried patches are still needed.
--
-- Derived from the existing rebased ROCm row (same engine_id) rather than
-- hard-coding an engine, and guarded so each insert is a no-op on a DB that
-- lacks that row or already has the name. All three need `--load-mode none`.
INSERT INTO builds (engine_id, name, binary_path, backend, reason)
SELECT b.engine_id,
       'llama.cpp-kintsugi-rebase2/build-rocm-rebase2',
       '/opt/forge/llama.cpp-kintsugi-rebase2/build-rocm-rebase2/bin/llama-server',
       'rocm',
       'kintsugi (cache reuse + EOG checkpoint fix) rebased onto upstream a91eab67 (2026-10-31), branch rebase/upstream-20261101, rocm-therock-10.1; needs --load-mode none'
FROM builds b
WHERE b.name = 'llama.cpp-kintsugi-rebase/build-rocm-rebase'
  AND NOT EXISTS (SELECT 1 FROM builds WHERE name = 'llama.cpp-kintsugi-rebase2/build-rocm-rebase2');

INSERT INTO builds (engine_id, name, binary_path, backend, reason)
SELECT b.engine_id,
       'llama.cpp-kintsugi-clover2/build-rocm-clover2',
       '/opt/forge/llama.cpp-kintsugi-clover2/build-rocm-clover2/bin/llama-server',
       'rocm',
       'EXPERIMENTAL: rebase2 + ggml-org/llama.cpp#31306 (Clover, draft-self) + server entropy-accept flags, branch feature/clover-draft-self-r2; needs --load-mode none'
FROM builds b
WHERE b.name = 'llama.cpp-kintsugi-rebase/build-rocm-rebase'
  AND NOT EXISTS (SELECT 1 FROM builds WHERE name = 'llama.cpp-kintsugi-clover2/build-rocm-clover2');

INSERT INTO builds (engine_id, name, binary_path, backend, reason)
SELECT b.engine_id,
       'llama.cpp-upstream-plain/build-rocm-plain',
       '/opt/forge/llama.cpp-upstream-plain/build-rocm-plain/bin/llama-server',
       'rocm',
       'unpatched upstream a91eab67 (2026-10-31), A/B baseline for whether the carried kintsugi patches are still needed; needs --load-mode none'
FROM builds b
WHERE b.name = 'llama.cpp-kintsugi-rebase/build-rocm-rebase'
  AND NOT EXISTS (SELECT 1 FROM builds WHERE name = 'llama.cpp-upstream-plain/build-rocm-plain');
