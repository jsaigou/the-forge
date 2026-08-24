-- 0046_swallow_merge_and_logos.sql
-- Operator feedback 2026-08-14: catalog data fixes.
--
-- (1) Swallow: "Qwen3 Swallow 8B" and "Swallow 32B" are the SAME family. Today
--     they sit as two separate families under the Swallow genealogy (the split
--     was introduced by 0017_genealogies.sql:102-118 because two different
--     models happened to share the display name "Swallow LLM"). Merge both
--     models into one family named "Swallow" and drop the redundant row.
--
-- (2) GPT-OSS has no logo anywhere in the resolution chain — no migration ever
--     set one (0040 only promoted Ornith/DeepSeek genealogy logos), so the
--     family card renders a letter badge. Set the GPT-OSS genealogy mark to
--     'openai' (the OpenAI knot, which carries a dark variant) at both family
--     and genealogy level for robustness.
--
-- (3) Kimi: kimi-k2.7-code was deleted in 0017, so any Kimi/Moonshot family in
--     the live catalog was operator-added at runtime (its name isn't known
--     here). Best-effort: set 'moonshot' (the svgl Kimi mark) on any
--     Kimi/Moonshot-named family/genealogy still carrying an empty logo. If
--     the live family has a different name, the operator sets it via Settings
--     → Catalog → Taxonomy.
--
-- All statements are name-guarded and no-op on a fresh DB (the 0017/0040
-- pattern), so this is safe to ship to every install.

-- (1) Swallow merge. Rename "Swallow 32B" → "Swallow" (the canonical name;
-- guarded on the split still existing so an already-merged or empty DB is
-- untouched), repoint both Swallow models at it, then delete the now-empty
-- "Qwen3 Swallow 8B" family.
UPDATE families SET name = 'Swallow'
  WHERE name = 'Swallow 32B'
  AND EXISTS (SELECT 1 FROM families WHERE name = 'Qwen3 Swallow 8B');

UPDATE models SET family_id = (SELECT id FROM families WHERE name = 'Swallow')
  WHERE name IN ('Swallow 32B', 'Qwen3 Swallow 8B RL')
  AND EXISTS (SELECT 1 FROM families WHERE name = 'Swallow');

DELETE FROM families
  WHERE name = 'Qwen3 Swallow 8B'
  AND NOT EXISTS (
    SELECT 1 FROM models
    WHERE family_id = (SELECT id FROM families WHERE name = 'Qwen3 Swallow 8B')
  );

-- (2) GPT-OSS logo.
UPDATE genealogies SET logo = 'openai'
  WHERE name = 'GPT-OSS' AND (logo IS NULL OR logo = '');
UPDATE families SET logo = 'openai'
  WHERE name = 'GPT-OSS' AND (logo IS NULL OR logo = '');

-- (3) Kimi / Moonshot best-effort logo.
UPDATE genealogies SET logo = 'moonshot'
  WHERE (name LIKE 'Kimi%' OR name LIKE 'Moonshot%')
  AND (logo IS NULL OR logo = '');
UPDATE families SET logo = 'moonshot'
  WHERE (name LIKE 'Kimi%' OR name LIKE 'Moonshot%')
  AND (logo IS NULL OR logo = '');
