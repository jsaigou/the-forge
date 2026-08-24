-- SPDX-License-Identifier: Apache-2.0
-- Schema v24 (Console service-chip polish, 2026-07-31). Data fix, not a
-- schema change: the ComfyUI service row was seeded with icon =
-- 'comfyui-mark.svg' (an svgl-style filename), but the Icon manifest
-- (web/src/assets/icons/manifest.ts) keys entries by bare slug — see
-- web/scripts/vendor-icons.mjs INLINE_SVGS.comfyui, added the same pass.
-- Same pattern as 0020_gemma_logo.sql: inherently a no-op on any DB without
-- a matching row, so no WHERE EXISTS guard is needed.
UPDATE services SET icon = 'comfyui' WHERE icon = 'comfyui-mark.svg';
