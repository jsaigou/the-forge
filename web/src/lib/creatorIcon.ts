// creatorIconSlug maps a model's `creator`/maker string to the manifest slug of
// that company's brand icon, so the maker's logo can render next to its name on
// model cards and loaded bays (operator feedback 2026-08-14: "When we display
// the company that made an AI, we should also show their logo"). Returns "" for
// creators with no vendored mark — callers should render no icon in that case
// (a letter-badge next to the name would be clutter, not identity).
import { ICON_MANIFEST } from "../assets/icons/manifest";

const CREATOR_ALIASES: Record<string, string> = {
  deepseek: "deepseek",
  alibaba: "alibaba",
  meta: "meta",
  google: "google",
  openai: "openai",
  microsoft: "microsoft",
  mistral: "mistral",
  anthropic: "anthropic",
  moonshot: "moonshot",
  zhipu: "zhipu",
  // Operator feedback 2026-08-14: company watermarks must be the CORPORATE
  // mark, not a product/model mark — tencent (was the Hunyuan swirl),
  // huggingscience (Carbon 8B's company; was the carbon model hexagon),
  // deepreinforce (Ornith's company; was the cockatiel mascot raster).
  tencent: "tencent",
  hunyuan: "hunyuan",
  huggingscience: "huggingscience",
  huggingfacebio: "huggingscience",
  huggingface: "huggingscience",
  deepreinforce: "deepreinforce",
  nvidia: "nvidia",
  xai: "grok",
  poolside: "poolside",
  fireworks: "fireworks",
  openrouter: "openrouter",
  qwen: "qwen",
  glm: "zhipu",
};

export function creatorIconSlug(creator: string | null | undefined): string {
  if (!creator) return "";
  const s = creator.trim().toLowerCase();
  // Try the full string, then the first word ("Moonshot AI" → "moonshot",
  // "HuggingFaceBio" → "huggingfacebio"), then a punctuation-stripped form.
  const candidates = [s, s.split(/[\s.]+/)[0], s.replace(/[^a-z0-9]/g, "")];
  for (const c of candidates) {
    const slug = CREATOR_ALIASES[c];
    if (slug && ICON_MANIFEST[slug]) return slug;
  }
  return "";
}
