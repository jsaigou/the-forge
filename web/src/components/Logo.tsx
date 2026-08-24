const KNOWN = new Set([
  "google", "nvidia", "alibaba", "oss", "moonshot", "deepseek",
  "anthropic", "meta", "mistral", "openai", "gemma",
]);

export function Logo({ slug, name, sm = false, xl = false }: { slug: string; name: string; sm?: boolean; xl?: boolean }) {
  const known = KNOWN.has(slug);
  const letter = (name || slug || "?").trim().charAt(0).toUpperCase();
  const sizeClass = xl ? "xl" : sm ? "sm" : "";
  return (
    <span className={`logo ${sizeClass} ${known ? slug : ""}`.trim()} title={name}>
      {letter}
    </span>
  );
}
