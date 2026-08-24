-- SPDX-License-Identifier: Apache-2.0
-- Schema v17 (product/QA sprint, 2026-07-29 — model taxonomy fix).
--
-- Two problems reported in QA: "the tencent model HY-MT2 is listed as a
-- translation family instead of belonging to tencent" and "Ornith is its
-- own family" (it wasn't — no family at all) and "GPT-OSS was made by
-- OpenAI" (creator was already right; family was missing). Investigating
-- the live catalog found the deeper issue: `families` mixed three
-- unrelated axes — model lineage (Gemma, Qwen, Swallow), vendor (Nvidia,
-- Poolside), and use-case (Genomics, Translation) — with no way to
-- express "Nemotron 2 and Nemotron 3 are different families, same
-- genealogy" (the operator's own framing). This migration adds a
-- genealogy level above family, retargets every existing family onto it,
-- and fixes the data bugs the investigation surfaced along the way.
--
-- Vendor attribution (models.creator) is a SEPARATE axis from family/
-- genealogy and is fixed independently below (Hy-MT2 → Tencent).
--
-- Everything here is guarded on the SPECIFIC named row it's fixing already
-- existing — this migration also runs against a fresh/empty database
-- (tests, new installs), where every guard is false and the whole thing is
-- a no-op. Without these guards, a fresh DB would pick up 8 orphan
-- families/genealogies for models that were never seeded there (caught by
-- TestCatalogFullRoundTrip's exact-list assertion going from 1 family to 9
-- on the first attempt at this file).

CREATE TABLE IF NOT EXISTS genealogies (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);
ALTER TABLE families ADD COLUMN genealogy_id INTEGER REFERENCES genealogies(id) ON DELETE SET NULL;

-- ── Genealogies — one per existing family this migration retargets, plus ──
-- one per family-less model that gets a new family below. Guarded on the
-- pre-existing row so a fresh/test DB (nothing seeded yet) creates none.
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Gemma' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Gemma');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Nemotron' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Nvidia');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Laguna' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Poolside');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Carbon' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Genomics');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Hunyuan' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Translation');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Qwen' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Qwen');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Swallow' WHERE EXISTS (SELECT 1 FROM families WHERE name = 'Swallow');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'GPT-OSS' WHERE EXISTS (SELECT 1 FROM models WHERE name = 'GPT-OSS 120B');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'Ornith' WHERE EXISTS (SELECT 1 FROM models WHERE name = 'Ornith 1.0 35B');
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'DeepSeek' WHERE EXISTS (SELECT 1 FROM models WHERE name IN ('deepseek-v4-pro', 'deepseek-v4-flash'));
INSERT OR IGNORE INTO genealogies (name)
  SELECT 'GLM' WHERE EXISTS (SELECT 1 FROM models WHERE name = 'glm-5.2');

-- ── Rename existing families onto their genealogy, generation in the name ──
-- (Gemma/Nvidia/Poolside/Genomics/Translation/Swallow/Qwen keep their
-- existing model membership; Qwen and Swallow are additionally split below
-- since each mixed two different generations/lineages under one name.)
-- These UPDATEs are inherently no-ops on a fresh DB (no row named 'Gemma'
-- etc. exists yet), same guarding as the genealogy inserts above.
UPDATE families SET name = 'Gemma 4',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Gemma')
  WHERE name = 'Gemma';
UPDATE families SET name = 'Nemotron 3',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Nemotron')
  WHERE name = 'Nvidia';
UPDATE families SET name = 'Laguna S 2',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Laguna')
  WHERE name = 'Poolside';
UPDATE families SET name = 'Carbon',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Carbon')
  WHERE name = 'Genomics';
-- "Translation" was a use-case label standing in for what is really a
-- Tencent Hunyuan-lineage model — this is the literal fix for the reported
-- "Hy-MT2 listed as translation family instead of belonging to Tencent".
UPDATE families SET name = 'Hy-MT2',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Hunyuan')
  WHERE name = 'Translation';
UPDATE models SET creator = 'Tencent' WHERE name = 'Hy-MT2 30B' AND creator = '';

-- "Qwen" mixed three generations (2.5 / 3 / 3.6) under one family — split.
-- The existing family becomes the 2.5 generation; 3 and 3.6 are new (each
-- guarded on the model that would move into it actually existing).
UPDATE families SET name = 'Qwen 2.5',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Qwen')
  WHERE name = 'Qwen';
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'Qwen 3', (SELECT id FROM genealogies WHERE name = 'Qwen')
  WHERE EXISTS (SELECT 1 FROM models WHERE name = 'Qwen3 Coder Next');
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'Qwen 3.6', (SELECT id FROM genealogies WHERE name = 'Qwen')
  WHERE EXISTS (SELECT 1 FROM models WHERE name IN ('Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP'));
UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'Qwen 3')
  WHERE name = 'Qwen3 Coder Next';
UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'Qwen 3.6')
  WHERE name IN ('Qwen3.6 35B (Aggressive)', 'Qwen3.6 35B MTP');

-- "Swallow" wasn't a duplicate family so much as two DIFFERENT models that
-- happened to share the identical display name "Swallow LLM" (found while
-- investigating: their configs are swallow-32b and qwen3-swallow-8b — two
-- real, live, distinct working configs, not one model listed twice). Split
-- the family and rename both models so the collision doesn't reappear on
-- the Models page. Both stay under genealogy Swallow (same project
-- lineage), even though the second is architecturally Qwen3-derived.
UPDATE families SET name = 'Swallow 32B',
    genealogy_id = (SELECT id FROM genealogies WHERE name = 'Swallow')
  WHERE name = 'Swallow';
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'Qwen3 Swallow 8B', (SELECT id FROM genealogies WHERE name = 'Swallow')
  WHERE EXISTS (SELECT 1 FROM models WHERE name = 'Swallow LLM' AND hf_repo LIKE '%Qwen3-Swallow%');
UPDATE models SET name = 'Swallow 32B'
  WHERE name = 'Swallow LLM' AND hf_repo NOT LIKE '%Qwen3-Swallow%';
UPDATE models SET name = 'Qwen3 Swallow 8B RL', family_id = (SELECT id FROM families WHERE name = 'Qwen3 Swallow 8B')
  WHERE name = 'Swallow LLM' AND hf_repo LIKE '%Qwen3-Swallow%';

-- ── New families for previously family-less models ──────────────────────────
-- Each guarded on the specific model existing, same reasoning as above.
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'GPT-OSS', (SELECT id FROM genealogies WHERE name = 'GPT-OSS')
  WHERE EXISTS (SELECT 1 FROM models WHERE name = 'GPT-OSS 120B');
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'Ornith 1.0', (SELECT id FROM genealogies WHERE name = 'Ornith')
  WHERE EXISTS (SELECT 1 FROM models WHERE name = 'Ornith 1.0 35B');
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'DeepSeek V4', (SELECT id FROM genealogies WHERE name = 'DeepSeek')
  WHERE EXISTS (SELECT 1 FROM models WHERE name IN ('deepseek-v4-pro', 'deepseek-v4-flash'));
INSERT OR IGNORE INTO families (name, genealogy_id)
  SELECT 'GLM 5.2', (SELECT id FROM genealogies WHERE name = 'GLM')
  WHERE EXISTS (SELECT 1 FROM models WHERE name = 'glm-5.2');

UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'GPT-OSS')
  WHERE name = 'GPT-OSS 120B';
UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'Ornith 1.0')
  WHERE name = 'Ornith 1.0 35B';
UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'DeepSeek V4')
  WHERE name IN ('deepseek-v4-pro', 'deepseek-v4-flash');
UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'GLM 5.2')
  WHERE name = 'glm-5.2';

-- ── Dedupe ───────────────────────────────────────────────────────────────────
-- 'gpt-oss-120b' (lowercase) is a stub duplicate of 'GPT-OSS 120B' with no
-- variant of its own — EXCEPT it is the model_id an aiand remote offering
-- points at. Re-point that offering onto the canonical row before deleting
-- the stub, so the live remote-hosting entry isn't cascade-deleted with it.
UPDATE offerings SET model_id = (SELECT id FROM models WHERE name = 'GPT-OSS 120B')
  WHERE model_id = (SELECT id FROM models WHERE name = 'gpt-oss-120b');
DELETE FROM models WHERE name = 'gpt-oss-120b';

-- 'kimi-k2.7-code' — dropped entirely per explicit operator decision
-- ("unreliable from the provider"). Two rows exist; one has a live,
-- enabled aiand offering. ON DELETE CASCADE on offerings.model_id and
-- variants.model_id (both enforced — this DB always runs with
-- PRAGMA foreign_keys=ON, see db.go) means deleting the model row is
-- sufficient — this is the one case where cascading the offering away IS
-- the intended outcome (drop it everywhere, not just locally).
DELETE FROM models WHERE name = 'kimi-k2.7-code';
