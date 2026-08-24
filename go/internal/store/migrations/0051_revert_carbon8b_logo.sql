-- Revert Carbon 8B logo to the original 'carbon' mark (hexagon+strand).
-- Migration 0049 changed it to 'carbon8b' (authored octagon) but the
-- original carbon.svg is the correct reference logo.
UPDATE models SET logo = 'carbon' WHERE name = 'Carbon 8B';
