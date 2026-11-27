-- SPDX-License-Identifier: Apache-2.0
-- Data fix (2026-11-01). Retires the llama.cpp builds that came from the Kintsugi fork trees (ADR-0019): upstream
-- matches the carried patches, all nine ROCm configs moved to build 19 (plain upstream), and the trees under
-- /opt/forge/llama.cpp-kintsugi* were deleted the same day. Same convention as the earlier retirements: a
-- "(retired) " name prefix and an empty binary_path, which the HF registrar's build pick already skips.
--
-- Guarded two ways so it can never strand a config: a row is retired only if no config references it, and
-- already-retired rows are left alone (idempotent). Builds 12 (standard-vulkan, five Vulkan configs), 18
-- (experimental Clover) and 19 (plain upstream) are deliberately not listed.
UPDATE builds
SET name        = '(retired) ' || name,
    binary_path = '',
    reason      = reason || ' | retired 2026-11-01 (ADR-0019): Kintsugi patches obsolete, tree deleted'
WHERE name IN (
        'llama.cpp-kintsugi/build-vulkan-new',
        'llama.cpp-kintsugi/build-rocm-new',
        'llama.cpp-kintsugi-qwen4exp/build-rocm-qwen4exp',
        'llama.cpp-kintsugi-rebase/build-vulkan-rebase',
        'llama.cpp-kintsugi-rebase/build-rocm-rebase',
        'llama.cpp-kintsugi-rebase2/build-rocm-rebase2')
  AND NOT EXISTS (SELECT 1 FROM configs c WHERE c.build_id = builds.id);
