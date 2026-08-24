import { ICON_MANIFEST, type IconEdge } from "../assets/icons/manifest";
import { Logo } from "./Logo";

// Icon (Sprint 0 §0.8) renders a vendored brand/badge SVG inline with a
// contrasting ring, falling back to the letter-badge (Logo) for any slug not
// in the manifest. It wraps Logo so every current caller can migrate
// `<Logo slug name sm/>` → `<Icon slug name sm/>` with no behavior change until
// the icon set is vendored.
//
// Ring rule: edge "dark" (near-black border) → white ring on dark themes;
// edge "light" (near-white border) → black ring on light themes; "none" → no
// ring. The concrete CSS lives with FE-0; this component only sets the
// `edge-<edge>` class so styling is a pure lookup — the runtime does no image
// analysis (edge is precomputed into the manifest at build time).
//
// Phase 3 (pre-release feedback sprint): a dark-theme variant, from either of
// two independent sources —
//
//   1. AUTOMATIC: the manifest entry for `slug` itself carries svgDark/
//      edgeDark (vendored only for the few slugs svgl also publishes a dark
//      file for — nvidia/qwen/openai as of this sprint; see manifest.ts's
//      header comment). Every caller passing that slug gets the pairing for
//      free — provider chips, badge glyphs, service icons — with zero
//      plumbing beyond the slug they already pass.
//   2. EXPLICIT: the optional `slugDark` prop, for catalog entities whose
//      operator uploaded/selected a SEPARATE mark for dark theme (genealogy/
//      family/model/config `logo_dark`) — a different slug or data: URL
//      entirely, not just a variant of the same manifest entry.
//
// slugDark (when it differs from slug) wins over the automatic case, since an
// explicit per-entity choice is more specific than a vendored default.
//
// THEME SELECTION IS CSS-ONLY — deliberately no useState/matchMedia
// subscription here. When a dark variant exists (either source), both marks
// render into the DOM as absolutely-positioned `.icon-v` children and
// theme.css shows exactly one via the same three-branch selector it already
// uses for `.icon.edge-*` ([data-theme="light"] override, @media
// prefers-color-scheme fallback for :root:not([data-theme])) — a theme flip
// repaints for free with no re-render, and the system-preference case works
// without JS ever reading matchMedia. Each variant's ring is a STATIC
// function of its own edge (not theme-conditional like the single-mark case
// below): a variant only ever renders in its own home theme (the dark mark
// in dark theme, the light mark in light theme), so there's no "same asset
// crossing both themes" case to flip a ring for.
//
// When there's no dark variant at all — by far the common case — a single
// mark renders directly under `.icon` exactly as before this sprint, so
// nothing here changes shape or cost for the common path.

function ringClass(edge: IconEdge): string {
  switch (edge) {
    case "dark":
      return "edge-dark";
    case "light":
      return "edge-light";
    default:
      return "";
  }
}

// variantMark renders one `.icon-v` child. An SVG mark gets
// dangerouslySetInnerHTML directly on the .icon-v span itself (so the
// pre-existing `.icon-v > svg` sizing rule in theme.css matches with no
// extra nesting level); a data: URL mark instead gets a plain <img> child,
// matched by the dedicated `.icon-v > img` sizing rule.
function variantMark(svg: string | undefined, dataURL: string | undefined, name: string, className: string) {
  if (dataURL) {
    return (
      <span className={className}>
        <img src={dataURL} alt={name} />
      </span>
    );
  }
  return <span className={className} dangerouslySetInnerHTML={{ __html: svg ?? "" }} />;
}

export function Icon({
  slug,
  slugDark,
  name,
  sm = false,
  xl = false,
}: {
  slug: string;
  // Explicit dark-theme override (Phase 3) — a catalog entity's separately
  // uploaded/selected logo_dark. "" or equal to slug = no override; the
  // manifest's own automatic pairing (if any) still applies. See this
  // file's header comment for the two independent dark-variant sources.
  slugDark?: string;
  name: string;
  sm?: boolean;
  xl?: boolean;
}) {
  // Sprint B: the config/model expanded detail views want a 128×128
  // logo (`.icon.xl`) — a third size alongside the existing 34px default
  // and 26px `sm`, following the same boolean-prop convention rather than
  // inventing a size-enum for one new case.
  const sizeClass = xl ? "xl" : sm ? "sm" : "";

  if (slug.startsWith("data:")) {
    // Uploaded icons land as data URLs (paths.icons_dir unset on ForgeHost —
    // Sprint A2, 2026-07-31). An explicit dark override can itself be a
    // second uploaded data: URL or a manifest slug.
    const dark = slugDark && slugDark !== slug ? slugDark : "";
    if (!dark) {
      return (
        <span className={`icon ${sizeClass}`.trim()} title={name}>
          <img src={slug} alt={name} />
        </span>
      );
    }
    const darkEntry = ICON_MANIFEST[dark];
    return (
      <span className={`icon ${sizeClass}`.trim()} title={name}>
        {variantMark(undefined, slug, name, "icon-v light")}
        {dark.startsWith("data:")
          ? variantMark(undefined, dark, name, "icon-v dark")
          : variantMark(darkEntry?.svg, undefined, name, `icon-v dark ${ringClass(darkEntry?.edge ?? "none")}`.trim())}
      </span>
    );
  }

  const entry = ICON_MANIFEST[slug];
  if (!entry) {
    // Unknown slug → letter-badge fallback (identical to the pre-§0.8 Logo).
    // An explicit slugDark can't rescue an unknown light slug — the light
    // mark is what anchors identity (name, ring baseline), so this stays a
    // clean fallback rather than a half-resolved icon.
    return <Logo slug={slug} name={name} sm={sm} xl={xl} />;
  }

  // Resolve the dark side: explicit override slug wins over the manifest's
  // own automatic pairing.
  const overrideSlug = slugDark && slugDark !== slug ? slugDark : "";
  let darkSVG: string | undefined;
  let darkDataURL: string | undefined;
  let darkEdge: IconEdge = entry.edge;
  if (overrideSlug) {
    if (overrideSlug.startsWith("data:")) {
      darkDataURL = overrideSlug;
      darkEdge = entry.edge; // no image analysis at runtime; reuse the light mark's edge
    } else {
      const overrideEntry = ICON_MANIFEST[overrideSlug];
      darkSVG = overrideEntry?.svg;
      darkEdge = overrideEntry?.edge ?? entry.edge;
    }
  } else if (entry.svgDark) {
    darkSVG = entry.svgDark;
    darkEdge = entry.edgeDark ?? entry.edge;
  }

  if (!darkSVG && !darkDataURL) {
    // No dark variant from either source — single mark, no light/dark DOM
    // split, identical to pre-Phase-3 behavior.
    return (
      <span
        className={`icon ${sizeClass} ${ringClass(entry.edge)}`.trim()}
        title={name}
        dangerouslySetInnerHTML={{ __html: entry.svg }}
      />
    );
  }

  return (
    <span className={`icon ${sizeClass}`.trim()} title={name}>
      {variantMark(entry.svg, undefined, name, `icon-v light ${ringClass(entry.edge)}`.trim())}
      {variantMark(darkSVG, darkDataURL, name, `icon-v dark ${ringClass(darkEdge)}`.trim())}
    </span>
  );
}
