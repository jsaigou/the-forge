-- Restore Carbon 8B logo to the 'carbon8b' mark.
-- Migration 0051 reverted to 'carbon' because the prior carbon8b glyph
-- (octagon+double-bonds) was unsatisfactory. The carbon8b mark has since
-- been redesigned (vendor-icons.mjs, 2026-08-14) as a clean hexagon crossed
-- by a DNA double helix — the genomic-chemistry motif — so it now supersedes
-- the generic carbon.svg family mark.
UPDATE models SET logo = 'carbon8b' WHERE name = 'Carbon 8B';
