-- SPDX-License-Identifier: Apache-2.0
-- Schema v25 (Sprint A4, pre-release readiness, 2026-07-31). Data fix, not a
-- schema change: 10 models had logo='' and rendered as letter badges. Every
-- slug below is now in the Icon manifest (web/scripts/vendor-icons.mjs):
-- deepseek/swallow were already vendored (pure data fix); hunyuan/zhipu/
-- poolside are lobehub marks; carbon/ornith are raster wraps (scripts/assets/).
-- Empty creators are filled where they drive the card subtitle (DeepSeek,
-- Zhipu); Carbon 8B's creator stays empty (operator's own model).
-- Inherently a no-op on any DB without matching rows, so no WHERE EXISTS
-- guard is needed.
UPDATE models SET logo = 'deepseek' WHERE name IN ('deepseek-v4-flash','deepseek-v4-pro') AND (logo IS NULL OR logo = '');
UPDATE models SET logo = 'swallow'  WHERE name IN ('Qwen3 Swallow 8B RL','Swallow 32B') AND (logo IS NULL OR logo = '');
UPDATE models SET logo = 'zhipu'    WHERE name = 'glm-5.2'        AND (logo IS NULL OR logo = '');
UPDATE models SET logo = 'hunyuan'  WHERE name = 'Hy-MT2 30B'     AND (logo IS NULL OR logo = '');
UPDATE models SET logo = 'poolside' WHERE name = 'Laguna-S-2.1'   AND (logo IS NULL OR logo = '');
UPDATE models SET logo = 'ornith'   WHERE name = 'Ornith 1.0 35B' AND (logo IS NULL OR logo = '');
UPDATE models SET logo = 'carbon'   WHERE name = 'Carbon 8B'      AND (logo IS NULL OR logo = '');
UPDATE models SET creator = 'DeepSeek' WHERE name IN ('deepseek-v4-flash','deepseek-v4-pro') AND (creator IS NULL OR creator = '');
UPDATE models SET creator = 'Zhipu'    WHERE name = 'glm-5.2' AND (creator IS NULL OR creator = '');
