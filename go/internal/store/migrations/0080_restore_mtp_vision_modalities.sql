-- SPDX-License-Identifier: Apache-2.0
-- Schema v80 (a0 model-discovery sprint, 2026-09-13, found live while
-- verifying 0079's deploy). `0028_modalities.sql` set
-- modalities = '["text","vision"]' on three named models: 'Gemma 4 26B A4B
-- (MTP)', 'Qwen3.6 35B (Aggressive)', and 'Qwen3.6 35B MTP'. A live audit
-- of the real ForgeHost catalog today (cross-referencing every visible config
-- with a real, non-missing mmproj against its model's stored modalities)
-- found only the middle one still carries it — 'Gemma 4 26B A4B (MTP)' and
-- 'Qwen3.6 35B MTP' are both back to modalities = '[]', silently regressed
-- by some later catalog edit between 0028 (2026-08-05) and now (root cause
-- not chased down — likely a model-editing/family-editing form save that
-- doesn't round-trip a column it doesn't render, the same class of bug
-- Sprint D found for capability scores). This is why gemma4-26b-a4b,
-- gemma4-26b-a4b-nothink, and qwen36-35b-a3b were still advertising
-- text-only on /v1/models even after 0079 shipped the resolution-rule
-- wiring — the code was correctly reading genuinely wrong data.
--
-- Guarded the same way as 0028/0079: a no-op wherever these exact names
-- don't exist or already carry a non-empty modalities value (never
-- overwrites an operator's own deliberate '[]' — if one exists, whoever
-- set it should be asked, not silently overridden a second time).
UPDATE models SET modalities = '["text","vision"]'
WHERE name IN ('Gemma 4 26B A4B (MTP)', 'Qwen3.6 35B MTP')
  AND modalities = '[]';
