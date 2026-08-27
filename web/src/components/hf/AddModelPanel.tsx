import { useEffect, useMemo, useState } from "react";
import { Icon } from "../Icon";
import { apiErrorMessage } from "../../lib/api";
import { creatorIconSlug } from "../../lib/creatorIcon";
import { formatGB } from "../../lib/format";
import {
  useCatalogArtifacts,
  useCatalogConfigs,
  useCatalogModels,
  useCatalogVariants,
  useHFDownloadStart,
  useHFPreflightMutation,
  useHFSearchMutation,
  useHFTreeMutation,
} from "../../lib/queries";
import type { HFPreflightReport, HFQuantCandidate, HFSearchResult } from "../../lib/types";
import { HFDownloadTray } from "./HFDownloadTray";

// AddModelPanel — Models page "Add model" sub-tab: search -> pick a repo
// (expands IN PLACE, directly under the row you clicked — operator
// feedback 2026-08-26: a separate section rendered below the whole
// results list read as broken, since with more than a couple of results
// the file picker landed off-screen with no visual link back to what you
// clicked) -> ranked candidates -> pre-flight -> Download.

// isOfficialRepo/abliteratedFrom/baseModelOf all derive from fields the
// search response ALREADY carries (tags, author) — no extra per-result
// network call, so this scales to a full results page for free.

// isOfficialRepo: HF's search API has no "official" flag, but every GGUF
// repo tags itself `base_model:quantized:<org>/<name>` — when that org
// matches the repo's own author, the org quantized its own model rather
// than a third party (bartowski/mradermacher/unsloth/...) doing it.
function isOfficialRepo(result: HFSearchResult): boolean {
  const author = result.author.toLowerCase();
  if (!author) return false;
  return result.tags.some((t) => {
    const m = /^base_model:quantized:([^/]+)\//i.exec(t);
    return m ? m[1].toLowerCase() === author : false;
  });
}

// abliteratedFrom: "abliterated" (refusal-direction ablation) isn't a
// tag HF standardizes, but every repo carrying one names it in the repo
// id and/or tags — checked case-insensitively across both.
function abliteratedFrom(result: HFSearchResult): boolean {
  const hay = (result.id + " " + result.tags.join(" ")).toLowerCase();
  return hay.includes("abliterat");
}

// baseModelOf: the `base_model:<org>/<name>` tag (NOT the quantized:
// variant) names what this GGUF was actually built from — the real
// answer to "is this vanilla or a finetune," since a base_model tag
// pointing at e.g. an -Instruct model with no abliteration signal is a
// plain instruction-tuned release, not a modified one.
function baseModelOf(result: HFSearchResult): string | null {
  for (const t of result.tags) {
    const m = /^base_model:(?!quantized:)(.+)$/i.exec(t);
    if (m) return m[1];
  }
  return null;
}

export function AddModelPanel() {
  const [query, setQuery] = useState("");
  const [expandedRepo, setExpandedRepo] = useState<string | null>(null);

  const search = useHFSearchMutation();
  const results = search.data?.results ?? [];

  function runSearch() {
    const q = query.trim();
    if (!q) return;
    setExpandedRepo(null);
    search.mutate({ query: q });
  }

  return (
    <>
      <HFDownloadTray />

      <div className="eyebrow">Add a model from HuggingFace</div>
      <div className="card">
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}>
          <label className="form-row" style={{ flex: "1 1 320px" }}>
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && runSearch()}
              placeholder="Search HuggingFace, e.g. Qwen2.5-Coder or an org/repo"
            />
          </label>
          <button type="button" className="btn primary" disabled={!query.trim() || search.isPending} onClick={runSearch}>
            {search.isPending ? "Searching…" : "Search"}
          </button>
        </div>

        {search.isError && <div className="error-note" style={{ marginTop: 10 }}>{apiErrorMessage(search.error)}</div>}

        {search.isSuccess && (
          <div style={{ marginTop: 12, display: "flex", flexDirection: "column", gap: 6 }}>
            {results.length === 0 ? (
              <div className="empty-note">No repos found.</div>
            ) : (
              <>
                {!results.some(isOfficialRepo) && !results.some((r) => r.no_gguf) && (
                  <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 2 }}>
                    None of these are from the original publisher — every result below is a
                    community quantization (the publisher hasn't released a GGUF themselves).
                  </div>
                )}
                {results.map((r) => (
                  <div key={r.id}>
                    <SearchResultRow
                      result={r}
                      expanded={expandedRepo === r.id}
                      onToggle={() => setExpandedRepo(expandedRepo === r.id ? null : r.id)}
                    />
                    {expandedRepo === r.id && <RepoFiles repo={r.id} />}
                  </div>
                ))}
              </>
            )}
          </div>
        )}
      </div>
    </>
  );
}

function SearchResultRow({
  result,
  expanded,
  onToggle,
}: {
  result: HFSearchResult;
  expanded: boolean;
  onToggle: () => void;
}) {
  const slug = creatorIconSlug(result.author);
  const official = isOfficialRepo(result);
  const abliterated = abliteratedFrom(result);
  const baseModel = baseModelOf(result);

  // no_gguf: the true publisher, injected even though they have no
  // compatible file — not a toggle target (there's nothing to expand),
  // shown with its own badge and a link out to HuggingFace instead of
  // the Select flow every real result gets.
  if (result.no_gguf) {
    return (
      <div
        className="btn"
        style={{
          display: "flex", alignItems: "center", gap: 10, textAlign: "left",
          padding: "8px 10px", width: "100%", opacity: 0.85, cursor: "default",
        }}
      >
        {slug && <Icon slug={slug} name={result.author} sm />}
        <div style={{ flex: "1 1 auto", minWidth: 0 }}>
          <div style={{ display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
            <span style={{ fontFamily: "var(--mono)", fontSize: 12 }}>{result.id}</span>
            <span className="badge evict" title="This is the original publisher, but they haven't released a GGUF — this engine can't download or run it">
              No GGUF · not compatible
            </span>
          </div>
          <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginTop: 2 }}>
            The original publisher — download only, from HuggingFace directly (not through this flow)
          </div>
        </div>
        <a
          href={`https://huggingface.co/${result.id}`}
          target="_blank"
          rel="noreferrer"
          className="btn"
          onClick={(e) => e.stopPropagation()}
        >
          View on HuggingFace ↗
        </a>
      </div>
    );
  }

  return (
    <button
      type="button"
      className="btn"
      style={{
        display: "flex", alignItems: "center", gap: 10, textAlign: "left",
        justifyContent: "flex-start", padding: "8px 10px", width: "100%",
        border: expanded ? "1px solid var(--border-strong)" : undefined,
      }}
      onClick={onToggle}
    >
      {slug && <Icon slug={slug} name={result.author} sm />}
      <div style={{ flex: "1 1 auto", minWidth: 0 }}>
        <div style={{ display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
          <span style={{ fontFamily: "var(--mono)", fontSize: 12, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
            {result.id}
          </span>
          {official && <span className="badge yes" title="The same org that trained this model quantized it">Official</span>}
          {abliterated && <span className="badge evict" title="Refusal-direction ablation detected in the repo name/tags">Abliterated</span>}
          {result.pipeline_tag && <span className="chip">{result.pipeline_tag}</span>}
        </div>
        <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginTop: 2 }}>
          {result.downloads.toLocaleString()} downloads
          {baseModel && <> · based on <span style={{ fontFamily: "var(--mono)" }}>{baseModel}</span></>}
          {result.gated && " · gated (needs a token)"}
        </div>
      </div>
    </button>
  );
}

// useRepointOptions joins Configs/Variants/Models/Artifacts into a flat,
// sorted "pick an existing config to repoint" option list — the same
// name/artifact join CatalogPanel's ConfigForm already does inline for its
// own Variant/weight-artifact selects.
function useRepointOptions() {
  const configs = useCatalogConfigs();
  const variants = useCatalogVariants();
  const models = useCatalogModels();
  const artifacts = useCatalogArtifacts();
  return useMemo(() => {
    const variantById = new Map((variants.data ?? []).map((v) => [v.id, v]));
    const modelById = new Map((models.data ?? []).map((m) => [m.id, m]));
    const artifactById = new Map((artifacts.data ?? []).map((a) => [a.id, a]));
    return (configs.data ?? [])
      .map((c) => {
        const variant = variantById.get(c.variant_id);
        const model = variant ? modelById.get(variant.model_id) : undefined;
        const artifact = artifactById.get(c.weight_artifact_id);
        const currentFile = artifact?.file_path.split("/").pop();
        const modelLabel = model && variant ? `${model.name} / ${variant.name}` : c.name;
        return { name: c.name, label: `${modelLabel} — ${c.name}${currentFile ? ` (currently ${currentFile})` : ""}` };
      })
      .sort((a, b) => a.label.localeCompare(b.label));
  }, [configs.data, variants.data, models.data, artifacts.data]);
}

function RepoFiles({ repo }: { repo: string }) {
  const tree = useHFTreeMutation();
  const preflight = useHFPreflightMutation();
  const start = useHFDownloadStart();
  const [selected, setSelected] = useState<HFQuantCandidate | null>(null);
  const [report, setReport] = useState<HFPreflightReport | null>(null);
  const [repointTarget, setRepointTarget] = useState(""); // "" = register a new model
  const repointOptions = useRepointOptions();

  const runTree = tree.mutate;
  // Fetch the ranked candidate list once per repo.
  useEffect(() => {
    runTree({ repo });
  }, [repo, runTree]);

  function pick(c: HFQuantCandidate) {
    setSelected(c);
    setReport(null);
    preflight.mutate(
      { repo, files: [{ filename: c.filename, size_bytes: c.size_bytes }] },
      { onSuccess: (r) => setReport(r) },
    );
  }

  function download() {
    if (!selected) return;
    start.mutate({
      repo,
      files: [{ filename: selected.filename, size_bytes: selected.size_bytes }],
      config_name: repointTarget || undefined,
    });
  }

  const candidates = tree.data?.candidates ?? [];

  return (
    <div style={{ marginLeft: 16, borderLeft: "2px solid var(--border)", paddingLeft: 14, marginTop: 6, marginBottom: 10 }}>
      {tree.isPending && <div className="empty-note">Loading file tree…</div>}
      {tree.isError && <div className="error-note">{apiErrorMessage(tree.error)}</div>}
      {tree.isSuccess && candidates.length === 0 && <div className="empty-note">No GGUF files found in this repo.</div>}

      {candidates.length > 0 && (
        <table style={{ width: "100%", fontSize: 11.5, borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ color: "var(--text-mute)", textAlign: "left" }}>
              <th style={{ padding: "3px 6px", fontWeight: 500 }}>file</th>
              <th style={{ padding: "3px 6px", fontWeight: 500 }}>quant</th>
              <th style={{ padding: "3px 6px", fontWeight: 500, textAlign: "right" }}>size</th>
              <th style={{ padding: "3px 6px", fontWeight: 500 }}>fits</th>
              <th style={{ padding: "3px 6px" }} />
            </tr>
          </thead>
          <tbody>
            {candidates.map((c) => (
              <tr
                key={c.filename}
                style={
                  selected?.filename === c.filename
                    ? { background: "color-mix(in srgb, var(--text) 8%, transparent)" }
                    : c.recommended
                      ? { background: "color-mix(in srgb, var(--ok) 10%, transparent)" }
                      : undefined
                }
              >
                <td style={{ padding: "3px 6px", fontFamily: "var(--mono)", wordBreak: "break-all" }}>
                  {c.recommended && <span title="Recommended">★ </span>}
                  {c.filename}
                </td>
                <td style={{ padding: "3px 6px", color: "var(--text-dim)" }}>{c.quant || "—"}</td>
                <td style={{ padding: "3px 6px", textAlign: "right", color: "var(--text-dim)" }}>{formatGB(c.size_bytes, 1)} GB</td>
                <td style={{ padding: "3px 6px", color: c.fits_budget ? "var(--ok)" : "var(--text-mute)" }}>
                  {c.fits_budget ? "yes" : "no"}
                </td>
                <td style={{ padding: "3px 6px", textAlign: "right" }}>
                  <button type="button" className="btn" onClick={() => pick(c)}>
                    Select
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {selected && (
        <div className="card" style={{ marginTop: 10 }}>
          <div style={{ fontSize: 11.5, fontFamily: "var(--mono)", marginBottom: 8 }}>{selected.filename}</div>
          {preflight.isPending && <div className="empty-note">Running pre-flight checks…</div>}
          {preflight.isError && <div className="error-note">{apiErrorMessage(preflight.error)}</div>}
          {report && (
            <div style={{ display: "flex", flexDirection: "column", gap: 4, marginBottom: 10 }}>
              {report.checks.map((c) => (
                <div key={c.id} style={{ fontSize: 11, color: c.severity === "block" ? "var(--crit)" : c.severity === "warn" ? "var(--warn)" : "var(--ok)" }}>
                  {c.severity === "block" ? "✕" : c.severity === "warn" ? "⚠" : "✓"} {c.summary}
                </div>
              ))}
              {report.requires_backend && (
                <span className={`chip ${report.requires_backend}`} style={{ alignSelf: "flex-start" }}>
                  requires {report.requires_backend}
                </span>
              )}
            </div>
          )}
          {!start.isSuccess && (
            <label className="form-row" style={{ marginBottom: 10 }}>
              Repoint an existing config (optional)
              <select value={repointTarget} onChange={(e) => setRepointTarget(e.target.value)} style={{ flex: 1 }}>
                <option value="">— register as a new model —</option>
                {repointOptions.map((o) => (
                  <option key={o.name} value={o.name}>{o.label}</option>
                ))}
              </select>
            </label>
          )}
          {repointTarget && !start.isSuccess && (
            <div className="empty-note" style={{ marginBottom: 10 }}>
              This overwrites <strong>{repointTarget}</strong>'s current weight file in place — its
              quantization label, context size, and any mmproj/sharded companion files are left
              unchanged. Only repoint to a genuinely compatible build of the same model.
            </div>
          )}
          {start.isError && <div className="error-note">{apiErrorMessage(start.error)}</div>}
          {start.isSuccess ? (
            <div className="empty-note">Download started — see the Downloads list above.</div>
          ) : (
            <button
              type="button"
              className="btn primary"
              disabled={!report || report.blocked || start.isPending}
              onClick={download}
            >
              {start.isPending ? "Starting…" : repointTarget ? `Download & repoint ${repointTarget}` : "Download"}
            </button>
          )}
        </div>
      )}
    </div>
  );
}
