import { ConfirmButton } from "../ConfirmButton";
import { apiErrorMessage } from "../../lib/api";
import { formatBytesPerSec, formatDurationShort, formatGB } from "../../lib/format";
import {
  useHFDownloadApprove,
  useHFDownloadCancel,
  useHFDownloadDelete,
  useHFDownloadPause,
  useHFDownloadProgress,
  useHFDownloadResume,
  useHFDownloads,
} from "../../lib/queries";
import type { HFDownload, HFDownloadState } from "../../lib/types";

// HFDownloadTray — the live job list for the HF model-acquisition engine
// (go/internal/hfdownload). Poll (useHFDownloads, every 3s) is the source
// of truth; SSE (useHFDownloadProgress) only smooths the byte counter
// between polls — same split as ProfileRunCard/useProfileRunTracker.
//
// Progress bar reuses Console.tsx's ResourceBar track/fill idiom
// (.rb-track/.rb-fill) rather than inventing a new one.
export function HFDownloadTray() {
  const downloads = useHFDownloads();
  const jobs = downloads.data?.downloads ?? [];

  if (downloads.isLoading) return null;
  if (jobs.length === 0) return null;

  return (
    <>
      <div className="eyebrow">Downloads</div>
      <div className="card" style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        {jobs.map((job) => (
          <DownloadRow key={job.id} job={job} />
        ))}
      </div>
    </>
  );
}

function stateBadgeClass(state: HFDownloadState): string {
  switch (state) {
    case "done":
      return "badge yes";
    case "failed":
    case "cancelled":
      return "badge no";
    case "paused":
    case "pending_approval":
      return "badge evict";
    default:
      return "badge";
  }
}

function DownloadRow({ job }: { job: HFDownload }) {
  const progress = useHFDownloadProgress(job.id);
  const approve = useHFDownloadApprove();
  const pause = useHFDownloadPause();
  const resume = useHFDownloadResume();
  const cancel = useHFDownloadCancel();
  const del = useHFDownloadDelete();

  // Live SSE sample wins when it's for THIS job's current running pass;
  // otherwise fall back to the polled row (the source of truth on state
  // transitions and reconnect — see this file's header comment).
  const bytesDone = progress && progress.job_id === job.id ? progress.bytes_done : job.bytes_done;
  const bytesTotal = job.bytes_total || progress?.bytes_total || 0;
  const pct = bytesTotal > 0 ? Math.max(0, Math.min(100, (bytesDone / bytesTotal) * 100)) : 0;
  const isActive = job.state === "running" || job.state === "verifying" || job.state === "registering";
  const isTerminal = job.state === "done" || job.state === "failed" || job.state === "cancelled";
  const busy = approve.isPending || pause.isPending || resume.isPending || cancel.isPending || del.isPending;
  const err = approve.error ?? pause.error ?? resume.error ?? cancel.error ?? del.error;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 11.5 }}>
        <span style={{ fontFamily: "var(--mono)", flex: "1 1 auto", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
          {job.repo}
        </span>
        <span className={stateBadgeClass(job.state)}>{job.state}</span>
      </div>

      {job.config_name && (
        <div style={{ fontSize: 10.5, color: "var(--text-mute)" }}>→ repointing <span style={{ fontFamily: "var(--mono)" }}>{job.config_name}</span></div>
      )}

      {(isActive || job.state === "paused") && (
        <div className="resbar" style={{ margin: 0 }}>
          <div className="rb-row">
            <span className="rb-k" style={{ minWidth: 70, fontSize: 10.5, color: "var(--text-mute)" }}>
              {formatGB(bytesDone, 1)} / {formatGB(bytesTotal, 1)} GB
              {isActive && progress && progress.job_id === job.id && (
                <> · {formatBytesPerSec(progress.bytes_per_sec)} · ETA {formatDurationShort(progress.eta_s)}</>
              )}
            </span>
            <div className="rb-track">
              <div className="rb-fill" style={{ width: `${pct}%`, background: job.state === "paused" ? "var(--text-mute)" : "#FF006E" }} />
            </div>
          </div>
        </div>
      )}

      {job.error && <div className="error-note" style={{ fontSize: 10.5 }}>{job.error}</div>}
      {err && <div className="error-note" style={{ fontSize: 10.5 }}>{apiErrorMessage(err)}</div>}

      <div style={{ display: "flex", gap: 6, justifyContent: "flex-end" }}>
        {job.state === "pending_approval" && (
          <button type="button" className="btn primary" disabled={busy} onClick={() => approve.mutate(job.id)}>
            Approve
          </button>
        )}
        {isActive && (
          <button type="button" className="btn" disabled={busy} onClick={() => pause.mutate(job.id)}>
            Pause
          </button>
        )}
        {job.state === "paused" && (
          <button type="button" className="btn primary" disabled={busy} onClick={() => resume.mutate(job.id)}>
            Resume
          </button>
        )}
        {!isTerminal && (
          <ConfirmButton
            label="Cancel"
            confirmLabel="Cancel it?"
            warning={isActive || job.state === "paused" ? "Deletes the partially downloaded file." : undefined}
            pending={cancel.isPending}
            onConfirm={() => cancel.mutate(job.id)}
          />
        )}
        {isTerminal && (
          <button type="button" className="btn" disabled={busy} onClick={() => del.mutate(job.id)}>
            Dismiss
          </button>
        )}
      </div>
    </div>
  );
}
