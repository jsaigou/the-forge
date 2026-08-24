import type { CatalogFamily, CatalogGenealogy, CatalogModel } from "./types";

// Sprint I follow-up (2026-08-05): IconPicker's `inherited` hint originally
// checked only the immediate parent level (a model's form checked its
// family, a config's form checked its model) — one hop short of
// registry.resolveLogos's real chain. A family/model with no own logo but a
// genealogy/family above it that DOES have one rendered the misleading "which
// has none either" dead-end text, even though the icon was correctly
// inheriting from further up. These walk the full chain so the hint always
// names the entity the icon will actually come from.
//
// Phase 3: carries the dark-theme pairing too. Mirrors
// registry.resolveLogos's level-first rule — a level with EITHER field set
// is where the chain stops, and dark falls back to that same level's light
// when only light was set there. Independently resolving light/dark per
// field would let a hint claim inheritance from two different levels at
// once, which resolveLogos never actually does.

export interface InheritedIcon {
  logo: string;
  logoDark: string;
  label: string;
}

// familyInheritedIcon is what a model would inherit if its own logo/logo_dark
// were unset — the family's own pair, else its genealogy's, else "" (naming
// whichever level was last checked).
export function familyInheritedIcon(
  familyId: number,
  families: CatalogFamily[],
  genealogies: CatalogGenealogy[],
): InheritedIcon | null {
  const fam = families.find((f) => f.id === familyId);
  if (!fam) return null;
  if (fam.logo || fam.logo_dark) return { logo: fam.logo, logoDark: fam.logo_dark || fam.logo, label: fam.name };
  const gen = fam.genealogy_id ? genealogies.find((g) => g.id === fam.genealogy_id) : undefined;
  if (gen) return { logo: gen.logo, logoDark: gen.logo_dark || gen.logo, label: gen.name };
  return { logo: "", logoDark: "", label: fam.name };
}

// modelInheritedIcon is what a config would inherit if its own logo/logo_dark
// were unset — the model's own pair, else the same family/genealogy walk
// above.
export function modelInheritedIcon(
  modelId: number,
  models: CatalogModel[],
  families: CatalogFamily[],
  genealogies: CatalogGenealogy[],
): InheritedIcon | null {
  const mdl = models.find((m) => m.id === modelId);
  if (!mdl) return null;
  if (mdl.logo || mdl.logo_dark) return { logo: mdl.logo, logoDark: mdl.logo_dark || mdl.logo, label: mdl.name };
  const famInherited = familyInheritedIcon(mdl.family_id, families, genealogies);
  if (famInherited) return famInherited;
  return { logo: "", logoDark: "", label: mdl.name };
}
