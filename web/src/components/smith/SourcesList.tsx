import type { SmithMessageSource } from "../../lib/types";

// SourcesList — P5 (docs/v5-smith.md §4.8). Renders a chat message's
// web:true sources as a collapsible "Sources (N)" block under the assistant
// bubble. No fetches, no images — just links out, so this stays CSP-clean
// and never itself becomes a second surface that leaks a request to
// whatever host a search result named.
function hostOf(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export function SourcesList({ sources }: { sources: SmithMessageSource[] }) {
  if (sources.length === 0) return null;
  return (
    <details className="smith-sources">
      <summary>Sources ({sources.length})</summary>
      <div className="smith-sources-list">
        {sources.map((s, i) => (
          <div className="smith-source-row" key={s.url ?? s.title ?? i}>
            <span className="chip" style={{ fontSize: 9 }}>{s.provider}</span>
            <a href={s.url} target="_blank" rel="noopener noreferrer" title={s.url}>
              {s.title || s.url}
            </a>
            <span style={{ color: "var(--text-mute)", fontSize: 10.5 }}>{hostOf(s.url)}</span>
            {s.cached && (
              <span className="chip" style={{ fontSize: 9 }} title="Served from smith's web cache, not a fresh fetch">
                cached
              </span>
            )}
          </div>
        ))}
      </div>
    </details>
  );
}
