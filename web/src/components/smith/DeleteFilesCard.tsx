import { useState } from "react";
import { formatGB } from "../../lib/format";
import { useSmithSettings, useUpdateSmithSettings } from "../../lib/queries";
import type { SmithDeleteFileEntry } from "../../lib/types";

// DeleteFilesCard — P6 FR7. Renders a delete_files action's file evidence
// table: docs/v5-smith.md §4.9's "the confirmation card lists every file
// with its evidence before the approve button arms". One row per file
// (path/folder/size), the total reclaim above the table so it reads before
// the operator scrolls the list — this is the last thing a human sees
// before an irreversible `rm`.
//
// "Keep" per row (S7-followup smith UX sprint, 2026-08-26): appends the
// path to smith.comfyui.keep_files so it's never re-proposed by a future
// check — direct, no need to build a real ComfyUI workflow the way
// `guidance`'s advice requires. It only affects FUTURE sweeps, not this
// already-open bundled proposal (proposeComfyUIDelete deliberately never
// bundles per-file, so there's no partial-approve here) — the inline note
// says so, and points at reject+recheck as the way to exclude it from this
// specific proposal too.
export function DeleteFilesCard({
  files,
  totalBytes,
  guidance,
}: {
  files: SmithDeleteFileEntry[];
  totalBytes: number;
  guidance?: string;
}) {
  const settings = useSmithSettings();
  const updateSettings = useUpdateSmithSettings();
  const [justKept, setJustKept] = useState<Set<string>>(new Set());

  if (files.length === 0) return null;

  const keepFiles = settings.data?.comfyui_keep_files ?? [];

  function keepFile(path: string) {
    if (keepFiles.includes(path)) return;
    updateSettings.mutate(
      { comfyui_keep_files: [...keepFiles, path] },
      { onSuccess: () => setJustKept((prev) => new Set(prev).add(path)) },
    );
  }

  return (
    <div className="card" style={{ marginTop: 8, padding: 10, borderLeft: "3px solid var(--crit)" }}>
      <div style={{ fontSize: 12, fontWeight: 600, color: "var(--crit)" }}>
        {files.length} file{files.length === 1 ? "" : "s"} — {formatGB(totalBytes, 1)} GB reclaimable
      </div>
      <div style={{ fontSize: 11, color: "var(--text-dim)", marginTop: 2 }}>
        Deleting is irreversible — the risk is in approving, not in the files being listed here.
      </div>
      {guidance && (
        <div style={{
          marginTop: 6, padding: 8, borderRadius: 6, fontSize: 11, lineHeight: 1.5,
          color: "var(--text-dim)", background: "var(--bg)", border: "1px solid var(--border)",
        }}>
          <span style={{ fontWeight: 600, color: "var(--text)" }}>Want to keep one of these? </span>
          Click "Keep" on its row below (adds it to a standing exclusion list — this and any
          future check will skip it), or {guidance}
        </div>
      )}
      <div style={{ marginTop: 6, maxHeight: 220, overflow: "auto" }}>
        <table style={{ width: "100%", fontSize: 11, borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ color: "var(--text-mute)", textAlign: "left" }}>
              <th style={{ padding: "2px 6px", fontWeight: 500 }}>path</th>
              <th style={{ padding: "2px 6px", fontWeight: 500 }}>folder</th>
              <th style={{ padding: "2px 6px", fontWeight: 500, textAlign: "right" }}>size</th>
              <th style={{ padding: "2px 6px", fontWeight: 500 }} />
            </tr>
          </thead>
          <tbody>
            {files.map((f) => {
              const kept = keepFiles.includes(f.path) || justKept.has(f.path);
              return (
                <tr key={f.path}>
                  <td style={{ padding: "2px 6px", fontFamily: "var(--mono)", color: "var(--text-dim)", wordBreak: "break-all" }}>
                    {f.path}
                  </td>
                  <td style={{ padding: "2px 6px", color: "var(--text-mute)" }}>{f.folder_type}</td>
                  <td style={{ padding: "2px 6px", textAlign: "right", color: "var(--text-dim)" }}>
                    {formatGB(f.size_bytes, 2)} GB
                  </td>
                  <td style={{ padding: "2px 6px", textAlign: "right" }}>
                    {kept ? (
                      <span style={{ fontSize: 10, color: "var(--ok)" }}>kept — reject this proposal &amp; recheck to drop it now</span>
                    ) : (
                      <button
                        className="btn"
                        style={{ fontSize: 10, padding: "2px 6px" }}
                        disabled={updateSettings.isPending}
                        onClick={() => keepFile(f.path)}
                      >
                        Keep
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}
