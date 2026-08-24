import { formatGB } from "../../lib/format";
import type { SmithDeleteFileEntry } from "../../lib/types";

// DeleteFilesCard — P6 FR7. Renders a delete_files action's file evidence
// table: docs/v5-smith.md §4.9's "the confirmation card lists every file
// with its evidence before the approve button arms". One row per file
// (path/folder/size), the total reclaim above the table so it reads before
// the operator scrolls the list — this is the last thing a human sees
// before an irreversible `rm`.
export function DeleteFilesCard({
  files,
  totalBytes,
  guidance,
}: {
  files: SmithDeleteFileEntry[];
  totalBytes: number;
  guidance?: string;
}) {
  if (files.length === 0) return null;
  return (
    <div className="card" style={{ marginTop: 8, padding: 10, borderLeft: "3px solid var(--crit)" }}>
      <div style={{ fontSize: 12, fontWeight: 600, color: "var(--crit)" }}>
        {files.length} file{files.length === 1 ? "" : "s"} — {formatGB(totalBytes, 1)} GB reclaimable
      </div>
      {guidance && (
        <div style={{
          marginTop: 6, padding: 8, borderRadius: 6, fontSize: 11, lineHeight: 1.5,
          color: "var(--text-dim)", background: "var(--bg)", border: "1px solid var(--border)",
        }}>
          <span style={{ fontWeight: 600, color: "var(--text)" }}>Want to keep one of these? </span>
          {guidance}
        </div>
      )}
      <div style={{ marginTop: 6, maxHeight: 220, overflow: "auto" }}>
        <table style={{ width: "100%", fontSize: 11, borderCollapse: "collapse" }}>
          <thead>
            <tr style={{ color: "var(--text-mute)", textAlign: "left" }}>
              <th style={{ padding: "2px 6px", fontWeight: 500 }}>path</th>
              <th style={{ padding: "2px 6px", fontWeight: 500 }}>folder</th>
              <th style={{ padding: "2px 6px", fontWeight: 500, textAlign: "right" }}>size</th>
            </tr>
          </thead>
          <tbody>
            {files.map((f) => (
              <tr key={f.path}>
                <td style={{ padding: "2px 6px", fontFamily: "var(--mono)", color: "var(--text-dim)", wordBreak: "break-all" }}>
                  {f.path}
                </td>
                <td style={{ padding: "2px 6px", color: "var(--text-mute)" }}>{f.folder_type}</td>
                <td style={{ padding: "2px 6px", textAlign: "right", color: "var(--text-dim)" }}>
                  {formatGB(f.size_bytes, 2)} GB
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
