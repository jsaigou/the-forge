-- SPDX-License-Identifier: Apache-2.0
-- Schema v40 (pre-release feedback sprint, Phase 3, 2026-08-12). Data fix,
-- not a schema change (0027_icon_hierarchy.sql already added the columns):
-- registry.resolveLogos gives the MODEL level precedence over its
-- genealogy/family, so an operator's uploaded genealogy marks (Nemotron,
-- Qwen, Swallow, Carbon, Laguna, Hunyuan, GPT-OSS, GLM — all present on the
-- live catalog as of this sprint) were being shadowed by every model's own
-- generic vendor-brand slug (nvidia/alibaba/deepseek/...) and never actually
-- rendered. This clears the model-level override ONLY where a parent level
-- already resolves to something, so no icon ever goes blank — a model whose
-- whole chain above it is empty keeps its own icon rather than falling to a
-- letter badge. Guarded / no-op-safe (WHERE EXISTS + logo-empty checks), so
-- it's inert against any DB that doesn't have these exact rows.

-- Step 1: promote genealogies with no logo of their own from their models'
-- existing slug, so a genealogy that never got an upload doesn't go blank
-- once step 2 clears its models' own logos below. Ornith and DeepSeek are
-- the two live genealogies with no uploaded mark as of this sprint.
UPDATE genealogies SET logo = 'ornith'
  WHERE name = 'Ornith' AND logo = '' AND logo_dark = ''
  AND EXISTS (
    SELECT 1 FROM models m JOIN families f ON f.id = m.family_id
    WHERE f.genealogy_id = genealogies.id AND m.logo = 'ornith'
  );
UPDATE genealogies SET logo = 'deepseek'
  WHERE name = 'DeepSeek' AND logo = '' AND logo_dark = ''
  AND EXISTS (
    SELECT 1 FROM models m JOIN families f ON f.id = m.family_id
    WHERE f.genealogy_id = genealogies.id AND m.logo = 'deepseek'
  );

-- Step 2: clear models.logo, but only where a parent (family or genealogy)
-- now actually resolves to a non-empty mark — self-guarding against ever
-- producing a blank icon.
UPDATE models SET logo = ''
  WHERE logo <> ''
  AND EXISTS (
    SELECT 1 FROM families f LEFT JOIN genealogies g ON g.id = f.genealogy_id
    WHERE f.id = models.family_id
    AND (f.logo <> '' OR f.logo_dark <> '' OR COALESCE(g.logo, '') <> '' OR COALESCE(g.logo_dark, '') <> '')
  );

-- Step 3: same guarded clear for configs.logo (zero rows carry an override
-- on the live catalog today; correct on any other DB shape too).
UPDATE configs SET logo = ''
  WHERE logo <> ''
  AND EXISTS (
    SELECT 1 FROM variants v JOIN models m ON m.id = v.model_id
    JOIN families f ON f.id = m.family_id LEFT JOIN genealogies g ON g.id = f.genealogy_id
    WHERE v.id = configs.variant_id
    AND (m.logo <> '' OR m.logo_dark <> '' OR f.logo <> '' OR f.logo_dark <> ''
         OR COALESCE(g.logo, '') <> '' OR COALESCE(g.logo_dark, '') <> '')
  );

-- Step 4: the hand-authored Swallow mark (a bird silhouette invented in
-- vendor-icons.mjs, not a real brand asset) is removed from the icon
-- manifest this same sprint — no row may reference the 'swallow' slug
-- afterward. Live data already has both Swallow models on logo='' (they
-- inherit the operator's uploaded Swallow genealogy mark instead), but this
-- covers a DB replayed from 0025_model_logos.sql, which set logo='swallow'
-- directly.
UPDATE models SET logo = '' WHERE logo = 'swallow';
UPDATE families SET logo = '' WHERE logo = 'swallow';
UPDATE genealogies SET logo = '' WHERE logo = 'swallow';
UPDATE configs SET logo = '' WHERE logo = 'swallow';
UPDATE models SET logo_dark = '' WHERE logo_dark = 'swallow';
UPDATE families SET logo_dark = '' WHERE logo_dark = 'swallow';
UPDATE genealogies SET logo_dark = '' WHERE logo_dark = 'swallow';
UPDATE configs SET logo_dark = '' WHERE logo_dark = 'swallow';
