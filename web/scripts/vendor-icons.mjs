#!/usr/bin/env node
// Sprint 0 §0.8 — icon vendoring step.
//
// The embedded PWA runs under a strict CSP with no runtime external fetch, so
// every brand/badge icon must be vendored into the repo at build time. This
// script:
//
//   1. Computes the slug set = union of
//        - config/models.toml `logo` values (brand marks),
//        - the provider/model `logo` slugs the registry/providers table use,
//        - the canonical badge icon slugs (§0.7, vocabulary overhauled
//          Sprint J2: reasoning, coding, vision, hearing, uncensored,
//          long-context — fast/multimodal/moe/mtp/dense/abliterated retired).
//   2. Pulls each brand slug's SVG from pheralb/svgl (the svgl.app library;
//      the polish plan cited "lobehub/svgl" — pheralb/svgl is the canonical
//      MIT-licensed source behind svgl.app) into
//      web/src/assets/icons/<slug>.svg (committed).
//   3. Vendors inline SVGs for slugs svgl has no mark for (oss) and for
//      every badge slug (conceptual icons, authored here — not brand marks).
//   4. Precomputes each icon's `edge` by classifying its border/dominant fill
//      (near-black → "dark", near-white → "light", neither → "none") so the
//      runtime does no image analysis — <Icon> just picks a contrasting ring.
//   4b. (Phase 3) For the few svgl slugs with a real dark-theme file
//      (SVGL_SLUGS's doc comment), also vendors that file and classifies its
//      own edge — a failed/missing dark fetch degrades to light-only, never
//      fails the whole slug.
//   5. Regenerates web/src/assets/icons/manifest.ts mapping slug → {svg, edge}.
//
// Run:    npm run icons
// Output: committed web/src/assets/icons/*.svg + manifest.ts (this file is the
//         build artifact; the build inlines the SVG markup via the manifest).
//
// The manifest is a pure TS object literal (no ?raw imports) so tsc needs no
// extra module resolution; values are JSON.stringify'd to avoid escaping bugs.

import { mkdir, writeFile, rm, readFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const ICONS_DIR = resolve(__dirname, "../src/assets/icons");
const MANIFEST_PATH = resolve(ICONS_DIR, "manifest.ts");
const ATTRIBUTIONS_PATH = resolve(ICONS_DIR, "attributions.ts");

// pheralb/svgl raw SVG root (MIT). The polish-plan citation "lobehub/svgl" was a
// misremembering; pheralb/svgl is the repo behind svgl.app. Brand slugs map to
// their exact filenames in static/library/.
const SVGL_RAW = "https://raw.githubusercontent.com/pheralb/svgl/main/static/library";

// slug → svgl filename, OR { light, dark } when svgl also publishes a
// dark-theme file for that mark (Phase 3 icon-variant work). A plain string
// is shorthand for light-only — most svgl marks are either self-adapting
// (currentColor) or fully colored and don't need a second file. Ground-
// truthed against the live repo this session (curl -o /dev/null -w
// "%{http_code}" against every "<x>_dark.svg"/"<x>-dark.svg" candidate for
// our slug set): of 11 svgl slugs, exactly THREE have a real dark file —
// nvidia-icon-dark.svg, qwen_dark.svg (which alibaba rides too, same file),
// and openai_dark.svg. Every other candidate 404s; don't add a "*_dark.svg"
// guess without checking, svgl's dark-file naming isn't consistent (compare
// nvidia's "-dark" vs qwen's "_dark").
const SVGL_SLUGS = {
  google: "google.svg",
  // nvidia + alibaba moved to LOBEHUB_SLUGS (operator feedback 2026-08-14):
  // svgl's nvidia-icon-* files are tight-cropped (the eye fills the whole
  // viewBox — "too zoomed in and cropped"), and svgl has no Alibaba Group
  // mark at all (this slug used to ride qwen_light.svg, so the "Alibaba"
  // watermark rendered as a second Qwen star — "Qwen is missing an icon
  // for Alibaba"). lobehub's nvidia-color.svg carries real viewBox padding
  // and alibaba-color.svg is the distinct orange Group mark.
  qwen: { light: "qwen_light.svg", dark: "qwen_dark.svg" },
  openai: { light: "openai.svg", dark: "openai_dark.svg" },
  meta: "meta.svg",
  anthropic: "claude-ai-icon.svg",
  gemini: "gemini.svg",
  mistral: "mistral-ai_logo.svg",
  deepseek: "deepseek.svg",
  // moonshot: svgl's kimi-icon.svg is the correct Kimi K3 logo (blue rounded
  // square with "K" letter). Restored from pre-feedback-round-3 state — the
  // lobehub replacement was wrong (operator QA sprint 2026-08-14).
  moonshot: "kimi-icon.svg",
  microsoft: "microsoft.svg",
};

// lobehub/lobe-icons (MIT — the library behind lobehub.com/icons) raw SVG root.
// Second brand source, used for marks svgl has no entry for (the comfyui
// hand-composite first established this sourcing pattern, 2026-07-31).
const LOBEHUB_RAW = "https://raw.githubusercontent.com/lobehub/lobe-icons/master/packages/static-svg/icons";

// slug → lobe-icons filename. Ground-truthed 2026-07-31 (Sprint A3):
//   gemma    — replaces the 2026-07-30 hand-drawn sparkle (operator-specified:
//              https://lobehub.com/icons/gemma)
//   hunyuan  — Hy-MT2 is Tencent Hunyuan-MT (operator call over the corporate
//              Tencent mark)
//   zhipu    — glm-5.2's creator (zai-color.svg does NOT exist; zhipu does)
//   poolside — Laguna-S-2.1's creator
//   openrouter / fireworks — provider presets (Sprint E) ride these slugs
const LOBEHUB_SLUGS = {
  gemma: "gemma-color.svg",
  hunyuan: "hunyuan-color.svg",
  zhipu: "zhipu-color.svg",
  poolside: "poolside-color.svg",
  openrouter: "openrouter-color.svg",
  fireworks: "fireworks-color.svg",
  // Operator feedback 2026-08-14 — prep icons (not all in use yet): a color
  // Claude mark alongside svgl's mono `anthropic`; Gemini's own mark (svgl
  // gemini.svg, distinct from google.svg); Grok (xAI); Bedrock (AWS).
  claude: "claude-color.svg",
  grok: "grok.svg",
  bedrock: "bedrock-color.svg",
  // Operator feedback 2026-08-14 (company-mark round): corporate marks the
  // old sourcing was missing or faking.
  //   tencent  — Hy-MT2's watermark showed the Hunyuan *product* swirl; the
  //              operator wants the Tencent corporate mark (blue twin-arcs).
  //   alibaba  — the real orange Alibaba Group mark (svgl had none; the slug
  //              used to ride the Qwen star).
  //   nvidia   — padded green eye, replaces svgl's tight-cropped icon files.
  //   huggingscience — Carbon 8B's company (HuggingFace's science org); the
  //              HF emoji face is its brand mark. deepreinforce.ai is a
  //              parked domain (no asset to vendor) so that one is authored
  //              in INLINE_SVGS below.
  tencent: "tencent-color.svg",
  alibaba: "alibaba-color.svg",
  nvidia: "nvidia-color.svg",
  huggingscience: "huggingface-color.svg",
};

// Slugs whose vendored mark is a flat mono-black glyph (e.g. lobehub's grok).
// These get black fills/strokes coerced to currentColor so the mark self-adapts
// to the theme text color (the same trick normalizeSvg applies to the fill-less
// OpenAI knot) instead of rendering as invisible black-on-black in dark mode.
// Without this, classifyEdge returns "dark" and a contrasting ring is added —
// but a ring alone can't rescue a black glyph on a dark panel.
const CURRENTCOLOR_SLUGS = new Set(["grok"]);
function coerceCurrentColor(svg) {
  return svg.replace(/(fill|stroke)\s*=\s*"(#000000|#000|black)"/gi, '$1="currentColor"');
}

// Inline SVGs for slugs svgl has no mark for + every badge slug (§0.7
// canonical vocabulary). Badges are conceptual indicators rendered inline with
// the model name; they use currentColor so they adapt to the theme text color
// (edge "none" — no ring). Brand mark oss carries its own brand color.
//
// The Swallow bird silhouette that used to live here (Tohoku University
// model) was removed in the pre-release feedback sprint, Phase 3
// (2026-08-12) — it was a hand-authored stand-in invented because svgl has
// no real Swallow mark, not a sourced brand asset. Swallow models now
// inherit the operator's own uploaded genealogy-level mark (or the letter
// badge, like any other unsourced mark) instead of a fake logo. See
// migration 0040_icon_inheritance_takeover.sql.
const INLINE_SVGS = {
  // Open-source — a cycle/open ring in the brand teal.
  oss: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#12b0a8" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3a9 9 0 0 1 8.5 6"/><path d="M20.5 3.5v5h-5"/><path d="M12 21a9 9 0 0 1-8.5-6"/><path d="M3.5 20.5v-5h5"/></svg>`,
  // Carbon 8B (HuggingFaceBio genomic foundation model) — operator feedback
  // 2026-08-14 asked for a black-and-white SVG replacement for the prior
  // raster mark, with clean lines and sharp angles. Authored here as a hexagon
  // (the genomic-chemistry motif) crossed by a DNA-strand zigzag, stroked in
  // currentColor with miter joins so it self-adapts to both themes. Replaces
  // the cropped carbon.png raster (moved out of RASTER_SVGS this sprint).
  carbon: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="miter" stroke-linecap="square"><path d="M12 2.5 3.5 7v10L12 21.5 20.5 17V7z"/><path d="M8 9.5l4 2 4-2M8 14.5l4-2 4 2"/></svg>`,
  // Carbon 8B model mark (operator feedback 2026-08-14: "Carbon 8b needs a
  // new svg logo. Draw one and replace the old one" — the old one was the
  // family's uploaded cream-tile raster). A hexagon ring crossed by a DNA
  // double helix (two sinusoidal strands + three base-pair rungs) — the
  // genomic-chemistry motif, currentColor so it self-adapts to both themes.
  // Replaces the prior octagon+double-bonds mark after operator supplied a
  // noisy PNG-to-SVG conversion of the real brand glyph (2026-08-14); this
  // is the cleaned reconstruction with simple lines and curves.
  carbon8b: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="miter" stroke-linecap="square"><path d="M12 2.5 3.5 7v10L12 21.5 20.5 17V7z"/><path d="M7 6C7 7.5 17 7.5 17 9C17 10.5 7 10.5 7 12C7 13.5 17 13.5 17 15C17 16.5 7 16.5 7 18"/><path d="M17 6C17 7.5 7 7.5 7 9C7 10.5 17 10.5 17 12C17 13.5 7 13.5 7 15C7 16.5 17 16.5 17 18"/><path d="M7 9h10M7 12h10M7 15h10"/></svg>`,
  // DeepReinforce (Ornith's company) — operator feedback 2026-08-14:
  // "Ornith seems to be missing a company icon, try to find one." Tried:
  // svgl (404), lobehub (404), deepreinforce.ai (parked/for-sale domain —
  // no brand asset). Authored fallback: an open reinforcement loop (arc
  // with arrowhead) around a rising step line — policy-improvement motif,
  // currentColor, distinct from the oss cycle ring.
  deepreinforce: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M19.5 12a7.5 7.5 0 1 1-2.2-5.3"/><path d="M17.5 2.8v4h-4"/><path d="M8.5 14.5l2.3-2.3 1.9 1.9 3-3"/></svg>`,
  // (gemma was here as a hand-drawn sparkle 2026-07-30 → replaced by the real
  // lobehub mark in LOBEHUB_SLUGS, Sprint A3, operator-specified.)
  // ComfyUI — the real mark, replacing a hand-drawn node-graph glyph invented
  // 2026-07-31 when svgl (this file's primary source) turned out to have no
  // ComfyUI entry. Operator supplied the real source: lobehub/lobe-icons
  // (MIT — the library behind lobehub.com/icons), specifically the
  // yellow-on-blue "Avatar" variant. Composited by hand from that package's
  // published constants (packages/static-svg/icons/comfyui-color.svg for the
  // glyph path; src/ComfyUI/style.ts for AVATAR_BACKGROUND #162DD4 /
  // AVATAR_COLOR #F0FF41 / AVATAR_ICON_MULTIPLE 0.6) — a full-bleed
  // background rect (rounding comes from the .icon container's own
  // border-radius, not baked in here) plus the glyph scaled 0.6 and centered
  // (translate(4.8 4.8) — (24 - 24*0.6)/2 — matches lobehub's own Avatar
  // layout math).
  comfyui: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24"><rect width="24" height="24" fill="#162DD4"/><path fill="#F0FF41" transform="translate(4.8 4.8) scale(0.6)" d="M5.485 23.76c-.568 0-1.026-.207-1.325-.598-.307-.402-.387-.964-.22-1.54l.672-2.315a.605.605 0 00-.1-.536.622.622 0 00-.494-.243H2.085c-.568 0-1.026-.207-1.325-.598-.307-.403-.387-.964-.22-1.54l2.31-7.917.255-.87c.343-1.18 1.592-2.14 2.786-2.14h2.313c.276 0 .519-.18.595-.442l.764-2.633C9.906 1.208 11.155.249 12.35.249l4.945-.008h3.62c.568 0 1.027.206 1.325.597.307.402.387.964.22 1.54l-1.035 3.566c-.343 1.178-1.593 2.137-2.787 2.137l-4.956.01H11.37a.618.618 0 00-.594.441l-1.928 6.604a.605.605 0 00.1.537c.118.153.3.243.495.243l3.275-.006h3.61c.568 0 1.026.206 1.325.598.307.402.387.964.22 1.54l-1.036 3.565c-.342 1.179-1.592 2.138-2.786 2.138l-4.957.01h-3.61z"/></svg>`,

  // Carbon 8B (HuggingFaceBio genomic foundation model) — the real brand mark,
  // cropped from the official banner (operator-supplied source 2026-07-31:
  // huggingface.co/HuggingFaceBio/Carbon-8B figures/carbon-8b-banner.png, the
  // hex+DNA glyph top-left). Raster wrapped in SVG — the mark is a raster
  // asset, and the cream tile (#f4f3ec, sampled from the banner) keeps it
  // full-bleed like every other brand icon. The PNG lives at
  // scripts/assets/carbon.png so the mark can be re-cropped without editing
  // this file; wrapped at build time by main() (see RASTER_SVGS below).
  // ── Badge vocabulary (§0.7, overhauled Sprint J2) — conceptual icons,
  // currentColor, edge "none". fast/multimodal/moe/mtp/dense retired
  // (architecture trivia, suppressed entirely in registry.go's
  // deriveBadges — no glyph anywhere). `abliterated` slug retired in favor
  // of `uncensored` (same concept, new name + glyph — a cracked shield
  // rather than the old plain shield-with-slash). `hearing` (ear glyph) is
  // new, synthesized from a card's modalities (audio), not a key_feature
  // string. `reasoning` replaced (was a node/circuit graph that read as
  // architecture, not thinking — now a thought bubble with three dots).
  uncensored: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"><path d="M12 2.5 4 5.5v5.5c0 5 3.4 8.8 8 10.5 4.6-1.7 8-5.5 8-10.5V5.5L12 2.5Z"/><path d="M13.5 6 9.5 12.5h3.5L9 19"/></svg>`,
  reasoning: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round" stroke-linecap="round"><path d="M12 4c-4.4 0-8 2.9-8 6.5 0 2.1 1.2 4 3.1 5.2-.1.9-.5 1.9-1.3 2.8 1.5-.1 2.8-.6 3.8-1.4.8.2 1.6.3 2.4.3 4.4 0 8-2.9 8-6.5S16.4 4 12 4Z"/><circle cx="9" cy="10.5" r=".9" fill="currentColor" stroke="none"/><circle cx="12" cy="10.5" r=".9" fill="currentColor" stroke="none"/><circle cx="15" cy="10.5" r=".9" fill="currentColor" stroke="none"/></svg>`,
  coding: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M8 7l-5 5 5 5"/><path d="M16 7l5 5-5 5"/></svg>`,
  vision: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linejoin="round"><path d="M2 12s3.6-7 10-7 10 7 10 7-3.6 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/></svg>`,
  hearing: `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9.5a6.5 6.5 0 1 1 13 0c0 4.5-4 5.5-4 9a3 3 0 1 1-6 0"/><path d="M14.5 9.5a2.5 2.5 0 0 0-5 0v1a2 2 0 1 1 0 4"/></svg>`,
  "long-context": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><rect x="2.5" y="8" width="19" height="8" rx="1"/><path d="M7 8v3M11 8v4M15 8v3M19 8v4"/></svg>`,
};

// Raster brand marks (Sprint A3) — a PNG committed under scripts/assets/
// wrapped in an <svg><image> at build time. `background` adds a full-bleed
// rect (color sampled from the source asset) so flat tile marks classify
// their edge as usual; omit it for assets with meaningful transparency.
//   ornith — DeepReinforce's cockatiel mascot, the V4 asset rescued from
//            ForgeHost (/opt/forge/static/icons/ornith.png, never committed to
//            git), downscaled 1046²→128². No official vector mark exists
//            (svgl/lobehub/deepreinforce.com all checked 2026-07-31); the
//            raster is the real brand. Transparent corners around the dark
//            circle → no background rect; classifyEdge falls through to
//            "dark" (no fills), which is correct for a charcoal circle.
//   aiand  — the real AI& mark (Sprint E, 2026-08-04): a red/near-black
//            angled-stripe glyph, pulled from www.aiand.com's own
//            light-mode favicon (no vector mark on svgl or lobehub — both
//            checked live, 404). Cropped to its content bbox and padded to
//            128². Given a background tile (unlike ornith) because the
//            source glyph itself has large transparent margins — without a
//            tile the near-black stroke would nearly vanish against a dark
//            theme page background; #fbf9f9 is sampled from aiand.com's own
//            CSS (background-color: #fbf9f9), not invented.
//   (carbon moved to INLINE_SVGS as an authored B/W vector, 2026-08-14.)
const RASTER_SVGS = {
  ornith: { file: "ornith.png" },
  aiand: { file: "aiand.png", background: "#fbf9f9" },
};

// ── Attribution metadata (Sprint F) ─────────────────────────────────────────
//
// Every icon this script vendors gets one attribution record, emitted to
// web/src/assets/icons/attributions.ts alongside manifest.ts. Recording
// provenance here — the one place that already knows where every mark came
// from — means the Attributions page can't silently miss a newly-added icon
// the way a hand-maintained credits list would.

const SOURCE_META = {
  svgl: {
    license: "MIT",
    licenseUrl: "https://github.com/pheralb/svgl/blob/main/LICENSE",
    projectUrl: "https://svgl.app",
  },
  lobehub: {
    license: "MIT",
    licenseUrl: "https://github.com/lobehub/lobe-icons/blob/master/LICENSE",
    projectUrl: "https://lobehub.com/icons",
  },
};

// Per-slug overrides for marks whose real provenance isn't just "fetched
// from the SOURCE_META root" — raster brand marks (cropped from a vendor's
// own asset, not FOSS-licensed) and comfyui (coded inline in INLINE_SVGS but
// hand-composited from a real lobehub asset — see that entry's comment).
const ATTRIBUTION_OVERRIDES = {
  comfyui: { source: "lobehub", sourceFile: "comfyui-color.svg (hand-composited)", ...SOURCE_META.lobehub },
  carbon: {
    source: "inline",
    sourceFile: "authored in vendor-icons.mjs (B/W hex+strand, 2026-08-14)",
    license: "in-repo",
    licenseUrl: "",
    projectUrl: "https://huggingface.co/HuggingFaceBio/Carbon-8B",
  },
  carbon8b: {
    source: "inline",
    sourceFile: "authored in vendor-icons.mjs (hexagon+DNA-helix, 2026-08-14)",
    license: "in-repo",
    licenseUrl: "",
    projectUrl: "https://huggingface.co/HuggingFaceBio/Carbon-8B",
  },
  deepreinforce: {
    source: "inline",
    sourceFile: "authored in vendor-icons.mjs (reinforcement loop, 2026-08-14; deepreinforce.ai is a parked domain, svgl/lobehub 404)",
    license: "in-repo",
    licenseUrl: "",
    projectUrl: "https://huggingface.co/deepreinforce-ai/Ornith-1.0-35B",
  },
  ornith: {
    source: "raster",
    sourceFile: "scripts/assets/ornith.png",
    license: "brand mark (not FOSS-licensed)",
    licenseUrl: "",
    projectUrl: "https://huggingface.co/deepreinforce-ai/Ornith-1.0-35B",
  },
  aiand: {
    source: "raster",
    sourceFile: "scripts/assets/aiand.png",
    license: "brand mark (not FOSS-licensed)",
    licenseUrl: "",
    projectUrl: "https://www.aiand.com",
  },
};

function attrFor(slug, source, sourceFile) {
  const override = ATTRIBUTION_OVERRIDES[slug];
  if (override) return { slug, ...override };
  if (source === "svgl" || source === "lobehub") {
    return { slug, source, sourceFile, ...SOURCE_META[source] };
  }
  // Inline badge/brand marks with no external source: authored in this file.
  return { slug, source: "inline", sourceFile: "authored in vendor-icons.mjs", license: "in-repo", licenseUrl: "", projectUrl: "" };
}

// ── kind classification (Sprint I) ──────────────────────────────────────────
//
// Sprint I's IconPicker (genealogy/family/model/config icon selection) needs
// to offer only vendor brand marks, not the conceptual badge glyphs mixed
// into the same manifest. BADGE_SLUGS mirrors go/internal/registry/
// registry.go's canonicalBadge map exactly (§0.7's canonical vocabulary) —
// every other vendored slug is a selectable "vendor" mark.

const BADGE_SLUGS = new Set([
  "reasoning", "coding", "vision", "hearing", "uncensored", "long-context",
]);

function kindFor(slug) {
  return BADGE_SLUGS.has(slug) ? "badge" : "vendor";
}

// ── Edge classification ─────────────────────────────────────────────────────
//
// §0.8: edge ∈ {"dark","light","none"} is precomputed so the runtime does no
// image analysis. A near-black border → "dark" (white ring on dark themes); a
// near-white border → "light" (black ring on light themes); colored or
// self-adapting marks → "none". We approximate "border" by the icon's dominant
// fill color (true pixel-sampling would need an image rasterizer; the dominant
// fill is a faithful, dependency-free proxy for these flat brand marks).

function hexToRgb(hex) {
  let h = hex.replace("#", "");
  if (h.length === 3) h = h.split("").map((c) => c + c).join("");
  if (h.length !== 6) return null;
  const n = parseInt(h, 16);
  if (Number.isNaN(n)) return null;
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}

function parseColor(raw) {
  const v = raw.trim().toLowerCase();
  if (v === "none" || v === "transparent" || v === "currentcolor" || v === "") return null;
  if (v.startsWith("#")) return hexToRgb(v);
  const rgb = v.match(/rgba?\(\s*(\d+)[\s,]+(\d+)[\s,]+(\d+)/i);
  if (rgb) return [parseInt(rgb[1], 10), parseInt(rgb[2], 10), parseInt(rgb[3], 10)];
  return null; // named colors / url(#…) — not classified
}

function luminance([r, g, b]) {
  // Perceptual luminance (0–255).
  return 0.299 * r + 0.587 * g + 0.114 * b;
}

function classifyEdge(svg) {
  // currentColor marks self-adapt to the theme text color ⇒ no ring needed.
  if (svg.includes("currentColor")) return "none";
  const colorAttr = /(?:fill|stroke|stop-color)\s*=\s*"([^"]*)"/gi;
  const counts = new Map();
  let m;
  while ((m = colorAttr.exec(svg)) !== null) {
    const rgb = parseColor(m[1]);
    if (!rgb) continue;
    const key = rgb.join(",");
    counts.set(key, (counts.get(key) ?? 0) + 1);
  }
  if (counts.size === 0) {
    // No explicit fills ⇒ SVG default fill is black ⇒ near-black border.
    return "dark";
  }
  // Dominant color = most frequently referenced solid fill.
  let best = null;
  let bestN = -1;
  for (const [k, n] of counts) {
    if (n > bestN) {
      bestN = n;
      best = k;
    }
  }
  const lum = luminance(best.split(",").map(Number));
  if (lum < 60) return "dark";
  if (lum > 230) return "light";
  return "none";
}

// ── Fetch + vendor ──────────────────────────────────────────────────────────

async function fetchSvgl(filename) {
  const resp = await fetch(`${SVGL_RAW}/${filename}`, { redirect: "follow" });
  if (!resp.ok) throw new Error(`svgl ${filename}: HTTP ${resp.status}`);
  const text = await resp.text();
  const trimmed = text.trim();
  if (!trimmed.startsWith("<svg")) {
    throw new Error(`svgl ${filename}: response is not SVG`);
  }
  return trimmed;
}

// Normalize a vendored SVG so it inlines safely + renders at the icon size:
// strip xml prolog/comments and inner <title> (the <Icon> span sets its own
// title={name}, so an inner <title> would shadow it with a stale label like
// "Qwen"). For colorless marks (no fill/stroke/stop-color anywhere — e.g. the
// OpenAI knot, whose paths default to black), inject `fill="currentColor"` on
// the root so a black-on-transparent mark adapts to the theme text color and
// stays visible on both light + dark (a border ring alone can't rescue a black
// mark on a dark panel). svgl marks already carry a viewBox.
function normalizeSvg(svg) {
  const stripped = svg
    .replace(/<\?xml[^>]*\?>\s*/gi, "")
    .replace(/<!--[\s\S]*?-->\s*/g, "")
    .replace(/<title>[\s\S]*?<\/title>\s*/gi, "")
    .replace(/<script[\s\S]*?<\/script>\s*/gi, "")
    .replace(/<script\b[^>]*\/>\s*/gi, "")
    .replace(/\son\w+\s*=\s*"[^"]*"/gi, "")
    .replace(/\son\w+\s*=\s*'[^']*'/gi, "")
    .replace(/\son\w+\s*=\s*[^\s>]+/gi, "")
    .replace(/href\s*=\s*["']javascript:[^"']*["']/gi, "")
    .replace(/xlink:href\s*=\s*["']javascript:[^"']*["']/gi, "")
    .trim();
  if (!/(?:fill|stroke|stop-color)\s*=/i.test(stripped)) {
    return stripped.replace(/<svg(?=[\s>])/i, '<svg fill="currentColor"');
  }
  return stripped;
}

async function main() {
  await rm(ICONS_DIR, { recursive: true, force: true });
  await mkdir(ICONS_DIR, { recursive: true });

  const entries = [];
  const attributions = [];

  // 1. Brand marks from pheralb/svgl. Entries may be a plain filename
  // (light-only) or { light, dark } (Phase 3). A missing/failed dark file
  // is non-fatal and degrades to light-only, exactly like a failed slug
  // used to degrade to the letter-badge fallback — it must never take the
  // light file down with it.
  for (const [slug, spec] of Object.entries(SVGL_SLUGS)) {
    const { light, dark } = typeof spec === "string" ? { light: spec, dark: undefined } : spec;
    try {
      const raw = await fetchSvgl(light);
      const svg = normalizeSvg(raw);
      const edge = classifyEdge(svg);
      await writeFile(resolve(ICONS_DIR, `${slug}.svg`), svg, "utf8");

      let svgDark, edgeDark;
      if (dark) {
        try {
          const rawDark = await fetchSvgl(dark);
          svgDark = normalizeSvg(rawDark);
          edgeDark = classifyEdge(svgDark);
          await writeFile(resolve(ICONS_DIR, `${slug}-dark.svg`), svgDark, "utf8");
        } catch (e) {
          console.error(`[vendor-icons] FAILED svgl dark ${slug} (${dark}): ${e.message}`);
          console.error(`[vendor-icons]   ${slug} falls back to its light mark in dark theme.`);
        }
      }

      entries.push([slug, svg, edge, kindFor(slug), svgDark, edgeDark]);
      attributions.push(attrFor(slug, "svgl", light));
      console.log(`[vendor-icons] svgl  ${slug.padEnd(12)} ← ${light}${svgDark ? ` (+dark ← ${dark})` : ""}  edge=${edge}`);
    } catch (e) {
      console.error(`[vendor-icons] FAILED svgl ${slug} (${light}): ${e.message}`);
      console.error(`[vendor-icons]   <Icon> will fall back to the letter-badge for this slug.`);
    }
  }

  // 2. Brand marks from lobehub/lobe-icons (same failure semantics as svgl).
  for (const [slug, filename] of Object.entries(LOBEHUB_SLUGS)) {
    try {
      const resp = await fetch(`${LOBEHUB_RAW}/${filename}`, { redirect: "follow" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const raw = await resp.text();
      let svg = normalizeSvg(raw.trim());
      if (CURRENTCOLOR_SLUGS.has(slug)) svg = coerceCurrentColor(svg);
      const edge = classifyEdge(svg);
      await writeFile(resolve(ICONS_DIR, `${slug}.svg`), svg, "utf8");
      entries.push([slug, svg, edge, kindFor(slug), undefined, undefined]);
      attributions.push(attrFor(slug, "lobehub", filename));
      console.log(`[vendor-icons] lobehub ${slug.padEnd(12)} ← ${filename}  edge=${edge}`);
    } catch (e) {
      console.error(`[vendor-icons] FAILED lobehub ${slug} (${filename}): ${e.message}`);
      console.error(`[vendor-icons]   <Icon> will fall back to the letter-badge for this slug.`);
    }
  }

  // 3. Inline SVGs (badges + svgl-absent brands).
  for (const [slug, svgRaw] of Object.entries(INLINE_SVGS)) {
    const svg = normalizeSvg(svgRaw);
    const edge = classifyEdge(svg);
    await writeFile(resolve(ICONS_DIR, `${slug}.svg`), svg, "utf8");
    entries.push([slug, svg, edge, kindFor(slug), undefined, undefined]);
    attributions.push(attrFor(slug, "inline", slug));
    console.log(`[vendor-icons] inline ${slug.padEnd(12)}  edge=${edge}`);
  }

  // 4. Raster brand marks — wrap the committed PNG in <svg><image>.
  for (const [slug, { file, background }] of Object.entries(RASTER_SVGS)) {
    const png = await readFile(resolve(__dirname, "assets", file));
    const b64 = png.toString("base64");
    const bg = background ? `<rect width="24" height="24" fill="${background}"/>` : "";
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24">${bg}<image href="data:image/png;base64,${b64}" width="24" height="24" preserveAspectRatio="xMidYMid slice"/></svg>`;
    const edge = classifyEdge(svg);
    await writeFile(resolve(ICONS_DIR, `${slug}.svg`), svg, "utf8");
    entries.push([slug, svg, edge, kindFor(slug), undefined, undefined]);
    attributions.push(attrFor(slug, "raster", `assets/${file}`));
    console.log(`[vendor-icons] raster ${slug.padEnd(12)} ← assets/${file}  edge=${edge}`);
  }

  // 3. Regenerate manifest.ts. Values are JSON.stringify'd (valid double-quoted
  // JS string literals) so backticks/quotes/newlines in SVG markup can't break
  // the generated module. svgDark/edgeDark (Phase 3) are only emitted when a
  // dark file was actually vendored, so most entries stay light-only exactly
  // as before.
  const obj = entries
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([slug, svg, edge, kind, svgDark, edgeDark]) => {
      const darkFields = svgDark ? `, svgDark: ${JSON.stringify(svgDark)}, edgeDark: ${JSON.stringify(edgeDark)}` : "";
      return `  ${JSON.stringify(slug)}: { svg: ${JSON.stringify(svg)}, edge: ${JSON.stringify(edge)}, kind: ${JSON.stringify(kind)}${darkFields} },`;
    })
    .join("\n");

  const manifest = `// Sprint 0 §0.8 — icon manifest (GENERATED by web/scripts/vendor-icons.mjs; do not edit by hand).
//
// \`npm run icons\` vendors brand/badge SVGs from pheralb/svgl (the svgl.app
// library; MIT) and lobehub/lobe-icons (MIT), plus authored inline badge marks
// and raster brand marks wrapped in <svg><image>, into
// web/src/assets/icons/<slug>.svg (committed, CSP-safe, offline) and
// regenerates this file, mapping each slug to its inline SVG markup plus a
// precomputed \`edge\` classification (near-black border → "dark", near-white →
// "light", neither/self-adapting → "none"). Precomputing \`edge\` at build time
// means the runtime does no image analysis — the <Icon> component just picks a
// contrasting ring from it. The slug is the only wire contract (§0.8); no API
// field ever references a manifest entry directly.
//
// svgDark/edgeDark (Phase 3, pre-release feedback sprint) are present only for
// the handful of slugs svgl also publishes a dark-theme file for — see
// SVGL_SLUGS's doc comment for exactly which. Absent means "use svg/edge in
// both themes", not "broken" — most vendored marks are already self-adapting
// (currentColor) or fully colored and never needed a second file.

export type IconEdge = "dark" | "light" | "none";

// kind (Sprint I): "vendor" brand marks are selectable in IconPicker
// (genealogy/family/model/config icons); "badge" glyphs are the canonical
// capability indicators (registry.go's canonicalBadge map) and are never
// offered there.
export type IconKind = "vendor" | "badge";

export interface IconEntry {
  svg: string; // inline <svg>…</svg> markup (vendored, trusted at build time)
  edge: IconEdge; // precomputed border classification for ring contrast
  kind: IconKind;
  svgDark?: string; // dark-theme variant, when svgl publishes one
  edgeDark?: IconEdge;
}

// slug → entry. Regenerated by web/scripts/vendor-icons.mjs.
export const ICON_MANIFEST: Record<string, IconEntry> = {
${obj}
};
`;

  await writeFile(MANIFEST_PATH, manifest, "utf8");
  console.log(`[vendor-icons] wrote ${entries.length} icons → ${ICONS_DIR}`);
  console.log(`[vendor-icons] regenerated manifest.ts (${entries.length} entries)`);

  // Regenerate attributions.ts (Sprint F) — one record per vendored icon,
  // sourced from the same fetch/compose steps above so it can't drift from
  // what actually got vendored.
  const attrObj = attributions
    .sort((a, b) => a.slug.localeCompare(b.slug))
    .map((a) => `  ${JSON.stringify(a)},`)
    .join("\n");

  const attributionsOut = `// Sprint F — icon attribution manifest (GENERATED by web/scripts/vendor-icons.mjs; do not edit by hand).
//
// One record per icon in manifest.ts, recording exactly where it came from.
// Deliberately a separate file from manifest.ts: that file inlines every SVG
// (including two base64-encoded PNGs), and the Attributions page has no
// reason to pull that payload in just to list credits.

export type IconSource = "svgl" | "lobehub" | "inline" | "raster";

export interface IconAttribution {
  slug: string;
  source: IconSource;
  sourceFile: string; // upstream filename, or where the mark was authored/cropped from
  license: string;
  licenseUrl: string; // "" when license isn't a URL-linkable FOSS license (e.g. brand marks)
  projectUrl: string; // "" when there's no canonical project page (in-repo authored marks)
}

// Regenerated by web/scripts/vendor-icons.mjs.
export const ICON_ATTRIBUTIONS: IconAttribution[] = [
${attrObj}
];
`;

  await writeFile(ATTRIBUTIONS_PATH, attributionsOut, "utf8");
  console.log(`[vendor-icons] regenerated attributions.ts (${attributions.length} entries)`);
}

main().catch((e) => {
  console.error(`[vendor-icons] fatal: ${e}`);
  process.exit(1);
});
