#!/usr/bin/env node
// Locale parity check — run before tsc in `npm run build`. Fails the build
// if any locale drifts from `en` (the source of truth): missing/extra keys,
// mismatched {{interpolation}} variables, or empty values. See
// docs/adr/0016-localization.md.
//
// Plural-aware: a key ending in _zero/_one/_two/_few/_many/_other is an
// i18next plural form. Each language only needs the suffixes ITS OWN plural
// rules require (from Intl.PluralRules) — English needs _one/_other, but
// Japanese has no singular/plural distinction and needs only _other. That's
// a real structural difference, not drift, so plural bases are checked
// against each language's own required categories rather than requiring
// every language to carry the exact same suffix set English does.
import { readFileSync, readdirSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const localesDir = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src", "locales");
const SOURCE_LANG = "en";
const PLURAL_SUFFIXES = ["zero", "one", "two", "few", "many", "other"];
const PLURAL_SUFFIX_RE = new RegExp(`_(${PLURAL_SUFFIXES.join("|")})$`);

function listLangs() {
  return readdirSync(localesDir, { withFileTypes: true })
    .filter((e) => e.isDirectory())
    .map((e) => e.name);
}

function loadCatalog(lang, ns) {
  const p = path.join(localesDir, lang, `${ns}.json`);
  return JSON.parse(readFileSync(p, "utf8"));
}

// Flattens {a: {b: "x"}} -> {"a.b": "x"}
function flatten(obj, prefix = "") {
  const out = {};
  for (const [k, v] of Object.entries(obj)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v !== null && typeof v === "object" && !Array.isArray(v)) {
      Object.assign(out, flatten(v, key));
    } else {
      out[key] = v;
    }
  }
  return out;
}

function interpolationVars(value) {
  if (typeof value !== "string") return new Set();
  return new Set([...value.matchAll(/\{\{\s*(\w+)\s*\}\}/g)].map((m) => m[1]));
}

function requiredPluralSuffixes(lang) {
  return new Set(new Intl.PluralRules(lang).resolvedOptions().pluralCategories);
}

// Splits a flat catalog into plain keys and plural bases:
// {"a.b_one": "x", "a.b_other": "y", "c": "z"} ->
//   plain: {c: "z"}, pluralBases: {"a.b": {one: "x", other: "y"}}
function splitPlurals(flat) {
  const plain = {};
  const pluralBases = {};
  for (const [key, value] of Object.entries(flat)) {
    const m = key.match(PLURAL_SUFFIX_RE);
    if (m) {
      const base = key.slice(0, -m[0].length);
      (pluralBases[base] ??= {})[m[1]] = value;
    } else {
      plain[key] = value;
    }
  }
  return { plain, pluralBases };
}

function main() {
  const langs = listLangs();
  const otherLangs = langs.filter((l) => l !== SOURCE_LANG);
  const namespaces = readdirSync(path.join(localesDir, SOURCE_LANG))
    .filter((f) => f.endsWith(".json"))
    .map((f) => f.replace(/\.json$/, ""));

  const errors = [];

  for (const ns of namespaces) {
    const sourceFlat = flatten(loadCatalog(SOURCE_LANG, ns));
    const source = splitPlurals(sourceFlat);
    const sourceRequired = requiredPluralSuffixes(SOURCE_LANG);

    for (const [key, value] of Object.entries(sourceFlat)) {
      if (typeof value === "string" && value.trim() === "") {
        errors.push(`${SOURCE_LANG}/${ns}.json: "${key}" is empty`);
      }
    }
    for (const [base, forms] of Object.entries(source.pluralBases)) {
      for (const cat of sourceRequired) {
        if (!(cat in forms)) {
          errors.push(`${SOURCE_LANG}/${ns}.json: plural "${base}" missing required category "_${cat}"`);
        }
      }
    }

    for (const lang of otherLangs) {
      let targetFlat;
      try {
        targetFlat = flatten(loadCatalog(lang, ns));
      } catch (e) {
        errors.push(`${lang}/${ns}.json: missing or invalid JSON (${e.message})`);
        continue;
      }
      const target = splitPlurals(targetFlat);
      const targetRequired = requiredPluralSuffixes(lang);

      // Plain (non-plural) keys must match exactly, with matching interpolation vars.
      for (const [key, value] of Object.entries(source.plain)) {
        if (!(key in target.plain)) {
          errors.push(`${lang}/${ns}.json: missing key "${key}" (present in ${SOURCE_LANG})`);
          continue;
        }
        const targetValue = target.plain[key];
        if (typeof targetValue === "string" && targetValue.trim() === "") {
          errors.push(`${lang}/${ns}.json: "${key}" is empty`);
        }
        const sourceVars = interpolationVars(value);
        const targetVars = interpolationVars(targetValue);
        if (sourceVars.size !== targetVars.size || [...sourceVars].some((v) => !targetVars.has(v))) {
          errors.push(
            `${lang}/${ns}.json: "${key}" interpolation vars {${[...targetVars]}} don't match ` +
              `${SOURCE_LANG} {${[...sourceVars]}}`,
          );
        }
      }
      for (const key of Object.keys(target.plain)) {
        if (!(key in source.plain)) {
          errors.push(`${lang}/${ns}.json: extra key "${key}" (not present in ${SOURCE_LANG})`);
        }
      }

      // Plural bases: target must carry exactly the categories ITS OWN
      // language requires (not English's), each with matching interpolation
      // vars against the source's own "other" form (every language has "other").
      for (const base of Object.keys(source.pluralBases)) {
        const targetForms = target.pluralBases[base];
        if (!targetForms) {
          errors.push(`${lang}/${ns}.json: missing plural "${base}" (present in ${SOURCE_LANG})`);
          continue;
        }
        const sourceVars = interpolationVars(source.pluralBases[base].other);
        for (const cat of targetRequired) {
          if (!(cat in targetForms)) {
            errors.push(`${lang}/${ns}.json: plural "${base}" missing required category "_${cat}" for locale "${lang}"`);
            continue;
          }
          const value = targetForms[cat];
          if (typeof value === "string" && value.trim() === "") {
            errors.push(`${lang}/${ns}.json: "${base}_${cat}" is empty`);
          }
          const targetVars = interpolationVars(value);
          if (sourceVars.size !== targetVars.size || [...sourceVars].some((v) => !targetVars.has(v))) {
            errors.push(
              `${lang}/${ns}.json: "${base}_${cat}" interpolation vars {${[...targetVars]}} don't match ` +
                `${SOURCE_LANG} {${[...sourceVars]}}`,
            );
          }
        }
        for (const cat of Object.keys(targetForms)) {
          if (!targetRequired.has(cat)) {
            errors.push(`${lang}/${ns}.json: plural "${base}_${cat}" is not a required category for locale "${lang}" (remove it)`);
          }
        }
      }
      for (const base of Object.keys(target.pluralBases)) {
        if (!(base in source.pluralBases)) {
          errors.push(`${lang}/${ns}.json: extra plural "${base}" (not present in ${SOURCE_LANG})`);
        }
      }
    }
  }

  if (errors.length > 0) {
    console.error(`Locale parity check failed (${errors.length} issue(s)):\n`);
    for (const e of errors) console.error(`  - ${e}`);
    process.exit(1);
  }

  console.log(`Locale parity check passed: ${namespaces.length} namespace(s) × ${otherLangs.length} locale(s).`);
}

main();
