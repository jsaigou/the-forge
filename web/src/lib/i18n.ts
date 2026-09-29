// i18n bootstrap. English namespaces are bundled eagerly (Vite inlines them
// into the importing chunk — no extra network request for English users).
// Japanese namespaces are loaded lazily, one chunk per namespace, only once
// the operator actually switches to 日本語 — see docs/adr/0016-localization.md.
import i18next from "i18next";
import { initReactI18next } from "react-i18next";

export const SUPPORTED_LANGS = ["en", "ja"] as const;
export type Lang = (typeof SUPPORTED_LANGS)[number];

export const LANG_KEY = "forge.console.lang";

// Every translation namespace, one per app area. Phase 0 ships empty {}
// catalogs for the ones Phase 1+ will populate — see the plan for the
// per-area extraction order.
export const NAMESPACES = [
  "common",
  "nav",
  "console",
  "dashboard",
  "models",
  "scheduling",
  "settings",
  "help",
  "onboarding",
  "smith",
  "errors",
  "flags",
  "fields",
] as const;

// eslint-disable-next-line @typescript-eslint/no-explicit-any -- i18next's own Resource type needs values typed this loosely.
type Catalog = Record<string, any>;

const enModules = import.meta.glob<{ default: Catalog }>("../locales/en/*.json", { eager: true });
const jaModules = import.meta.glob<{ default: Catalog }>("../locales/ja/*.json");

function nsFromPath(path: string): string {
  const file = path.split("/").pop() ?? "";
  return file.replace(/\.json$/, "");
}

function buildEnResources(): Record<string, Catalog> {
  const out: Record<string, Catalog> = {};
  for (const [path, mod] of Object.entries(enModules)) {
    out[nsFromPath(path)] = mod.default;
  }
  return out;
}

const loadedJaNamespaces = new Map<string, Catalog>();

async function loadJaNamespaces(): Promise<Map<string, Catalog>> {
  await Promise.all(
    Object.entries(jaModules)
      .filter(([path]) => !loadedJaNamespaces.has(nsFromPath(path)))
      .map(async ([path, loader]) => {
        const ns = nsFromPath(path);
        const mod = await loader();
        loadedJaNamespaces.set(ns, mod.default);
      }),
  );
  return loadedJaNamespaces;
}

function detectInitialLang(): Lang {
  const saved = localStorage.getItem(LANG_KEY);
  if (saved === "en" || saved === "ja") return saved;
  return navigator.language?.toLowerCase().startsWith("ja") ? "ja" : "en";
}

let initPromise: Promise<Lang> | null = null;

// Idempotent — safe to call more than once (StrictMode double-invoke, HMR).
export function initI18n(): Promise<Lang> {
  if (initPromise) return initPromise;
  initPromise = (async () => {
    const lang = detectInitialLang();
    // ja resources must be part of the `resources` config passed to init(),
    // not added afterward via addResourceBundle — that method isn't usable
    // until the instance has finished initializing (a cold load with "ja"
    // already saved threw "addResourceBundle is not a function" and blanked
    // the whole app before this fix — caught live in a fresh, unauthenticated
    // tab, not the tab this session had already been iterating in).
    const resources: Record<string, Catalog> = { en: buildEnResources() };
    if (lang === "ja") resources.ja = Object.fromEntries(await loadJaNamespaces());
    await i18next.use(initReactI18next).init({
      lng: lang,
      fallbackLng: "en",
      supportedLngs: SUPPORTED_LANGS,
      ns: NAMESPACES,
      defaultNS: "common",
      resources,
      interpolation: { escapeValue: false },
      returnNull: false,
    });
    document.documentElement.lang = lang;
    return lang;
  })();
  return initPromise;
}

// Loads the ja bundle (if not already resident) before flipping the active
// language, so components re-render with translated text immediately — no
// loading flash, no reload. Safe post-init only (see initI18n's comment) —
// this is always called after the app has already rendered once.
export async function setLang(lang: Lang): Promise<void> {
  if (lang === "ja") {
    const namespaces = await loadJaNamespaces();
    for (const [ns, catalog] of namespaces) {
      i18next.addResourceBundle("ja", ns, catalog, true, true);
    }
  }
  await i18next.changeLanguage(lang);
  document.documentElement.lang = lang;
  localStorage.setItem(LANG_KEY, lang);
}

export function currentLang(): Lang {
  return i18next.language === "ja" ? "ja" : "en";
}
