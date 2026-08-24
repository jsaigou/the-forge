-- SPDX-License-Identifier: Apache-2.0
-- Schema v20 (Console refinement pass #3, 2026-07-30). Data fix, not a
-- schema change: Gemma models were seeded with logo='google' (correct at
-- the vendor level, wrong at the model level — Gemma has its own icon,
-- see web/scripts/vendor-icons.mjs INLINE_SVGS.gemma). Inherently a no-op
-- on any DB without matching rows, so no WHERE EXISTS guard is needed.
UPDATE models SET logo = 'gemma'
WHERE logo = 'google' AND lower(name) LIKE 'gemma%';
