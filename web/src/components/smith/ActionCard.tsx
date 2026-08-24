import { useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import {
  useSmithActionApprove,
  useSmithActionRecheck,
  useSmithActionReject,
  useSmithProcedureCheckpointAbort,
  useSmithProcedureCheckpointApprove,
  useSmithProcedureRun,
} from "../../lib/queries";
import { useStepUpGate } from "../../lib/useStepUpGate";
import type {
  SmithAction,
  SmithActionRisk,
  SmithActionStatus,
  SmithDeleteFileEntry,
  SmithRunbookStep,
  SmithVerifyResult,
} from "../../lib/types";
import { ConfirmButton } from "../ConfirmButton";
import { StepUpModal } from "../StepUpModal";
import { DeleteFilesCard } from "./DeleteFilesCard";
import { DowntimeModal } from "./DowntimeModal";
import { HandoffPanel } from "./HandoffPanel";
import { RunbookCard } from "./RunbookCard";

// ActionCard — Wave 3 / P2 (track W3-C, peaceful-plotting-reef.md). The
// core visual unit for a smith action: kind/title, risk + status chips, an
// expandable raw-detail block (same expand/collapse interaction as
// Diagnostics.tsx's EvidenceRow — duplicated locally rather than imported
// since Diagnostics doesn't export it), and status-appropriate actions.
//
// The risk palette deliberately reuses the existing severity vars
// (ok/info/warn/crit) rather than inventing a parallel one — info/low/high
// map onto cool/ok/crit, the same 3-of-4 slice Diagnostics' sevChip already
// uses for info/ok/crit (this repo has a standing lesson against forcing a
// new semantic color into an existing palette without a real gap).

const RISK_COLOR: Record<SmithActionRisk, string> = {
  info: "var(--cool)",
  low: "var(--ok)",
  high: "var(--crit)",
};

function riskChip(risk: SmithActionRisk) {
  const c = RISK_COLOR[risk] ?? "var(--text-mute)";
  return (
    <span className="chip" style={{ color: c, borderColor: `color-mix(in srgb, ${c} 40%, var(--border))` }}>
      {risk} risk
    </span>
  );
}

// done_unverified is deliberately its own color (warn), never flattened
// into "done" (ok) — the whole point of the backend's post-verify design is
// that a claimed success without re-measurement is a distinct, lesser
// outcome, not a green checkmark.
const STATUS_COLOR: Record<SmithActionStatus, string> = {
  pending: "var(--warn)",
  approved: "var(--cool)",
  executing: "var(--cool)",
  done: "var(--ok)",
  done_unverified: "var(--warn)",
  failed: "var(--crit)",
  rejected: "var(--text-mute)",
  superseded: "var(--text-mute)",
};

function statusChip(status: SmithActionStatus) {
  const c = STATUS_COLOR[status] ?? "var(--text-mute)";
  return (
    <span className="chip" style={{ color: c, borderColor: `color-mix(in srgb, ${c} 40%, var(--border))` }}>
      {status.replace(/_/g, " ")}
    </span>
  );
}

const SEV_COLOR: Record<string, string> = {
  ok: "var(--ok)",
  info: "var(--cool)",
  warn: "var(--warn)",
  crit: "var(--crit)",
};

function sevChip(sev: string) {
  const c = SEV_COLOR[sev] ?? "var(--text-mute)";
  return (
    <span className="chip" style={{ color: c, borderColor: `color-mix(in srgb, ${c} 40%, var(--border))` }}>
      {sev}
    </span>
  );
}

function fmtTime(unix: number | null): string {
  if (!unix) return "";
  const d = new Date(unix * 1000);
  if (isNaN(d.getTime())) return "";
  return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
}

function DetailBlock({ detail }: { detail: Record<string, unknown> }) {
  const [expanded, setExpanded] = useState(false);
  if (!detail || Object.keys(detail).length === 0) return null;
  let text: string;
  try {
    text = JSON.stringify(detail, null, 2);
  } catch {
    text = String(detail);
  }
  return (
    <div style={{ marginTop: 4 }}>
      <button
        className="tab"
        style={{ fontSize: 10, padding: "2px 8px", cursor: "pointer" }}
        onClick={() => setExpanded(!expanded)}
      >
        {expanded ? "▼ detail" : "▶ detail"}
      </button>
      {expanded && (
        <pre
          style={{
            marginTop: 4, padding: 8, borderRadius: 6, maxHeight: 240, overflow: "auto",
            fontSize: 10.5, lineHeight: 1.45, fontFamily: "var(--mono)",
            background: "var(--bg)", color: "var(--text-dim)",
            border: "1px solid var(--border)",
          }}
        >
          {text}
        </pre>
      )}
    </div>
  );
}

function VerifyList({ verify }: { verify: SmithVerifyResult[] }) {
  if (verify.length === 0) return null;
  return (
    <div style={{ marginTop: 8, display: "flex", flexDirection: "column", gap: 4 }}>
      {verify.map((v, i) => (
        <div key={v.check_id ?? i} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11, flexWrap: "wrap" }}>
          {sevChip(v.severity)}
          <span style={{ fontFamily: "var(--mono)", color: "var(--text-dim)" }}>{v.check_id}</span>
          <span style={{ color: "var(--text-mute)" }}>{v.summary}</span>
        </div>
      ))}
    </div>
  );
}

// ProcedureRunPanel — the checkpoint UI for a KindProcedure action currently
// "executing" (autonomous-remediation Sprint 2/3): a live step list (via
// smith:procedure_step SSE → useSmithProcedureRun, no manual polling) plus,
// when the run is paused at a Step.Checkpoint gate, the approve/abort
// controls (§13). The action's own status stays "executing" for the whole
// run — only run.status moves.
function ProcedureRunPanel({ actionId }: { actionId: number }) {
  const { data: run } = useSmithProcedureRun(actionId);
  const approve = useSmithProcedureCheckpointApprove(actionId);
  const abort = useSmithProcedureCheckpointAbort(actionId);
  const [error, setError] = useState<string | null>(null);

  if (!run) return null;

  function handleApprove() {
    setError(null);
    approve.mutate(undefined, { onError: (e) => setError(apiErrorMessage(e)) });
  }
  function handleAbort() {
    setError(null);
    abort.mutate(undefined, { onError: (e) => setError(apiErrorMessage(e)) });
  }

  return (
    <div style={{ marginTop: 8 }}>
      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        {run.steps.map((s) => (
          <div key={s.index} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11, flexWrap: "wrap" }}>
            <span style={{ color: s.ok ? "var(--ok)" : "var(--crit)" }}>{s.ok ? "✓" : "✗"}</span>
            <span style={{ color: "var(--text-dim)" }}>{s.title}</span>
            {s.error && <span style={{ color: "var(--crit)" }}>{s.error}</span>}
          </div>
        ))}
        {run.status === "running" && (
          <div style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11, color: "var(--text-mute)" }}>
            <span className="action-busy-dot dot-busy" />
            step {run.current_step + 1}…
          </div>
        )}
      </div>

      {run.status === "awaiting_checkpoint" && (
        <div
          className="card"
          style={{ marginTop: 8, padding: 10, borderLeft: "3px solid var(--warn)", background: "color-mix(in srgb, var(--warn) 8%, var(--panel))" }}
        >
          <div style={{ fontSize: 12, fontWeight: 600, color: "var(--warn)" }}>Paused for review</div>
          {run.checkpoint_note && (
            <div style={{ fontSize: 11.5, color: "var(--text-dim)", marginTop: 4 }}>{run.checkpoint_note}</div>
          )}
          {error && <div className="error-note" style={{ marginTop: 6 }}>{error}</div>}
          <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
            <button className="btn primary" disabled={approve.isPending} onClick={handleApprove}>
              {approve.isPending ? "…" : "Continue"}
            </button>
            <ConfirmButton
              onConfirm={handleAbort}
              pending={abort.isPending}
              label="Abort"
              confirmLabel="Abort this run?"
              warning="The procedure stops here — whatever already ran stays as it is."
            />
          </div>
        </div>
      )}
    </div>
  );
}

export function ActionCard({ action }: { action: SmithAction }) {
  const approve = useSmithActionApprove(action.id);
  const reject = useSmithActionReject(action.id);
  const recheck = useSmithActionRecheck(action.id);
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [showDowntimeModal, setShowDowntimeModal] = useState(false);

  function handleApprove() {
    setError(null);
    approve.mutate(undefined, {
      onError: (e) => {
        if (gate.handle(e, handleApprove)) return;
        setError(apiErrorMessage(e));
      },
    });
  }

  function handleReject() {
    setError(null);
    reject.mutate(undefined, { onError: (e) => setError(apiErrorMessage(e)) });
  }

  function handleRecheck() {
    setError(null);
    recheck.mutate(undefined, { onError: (e) => setError(apiErrorMessage(e)) });
  }

  // A self-evicting pending action never gets a naive Approve button — it
  // would just 409 handoff_required. HandoffPanel owns the (gated) real
  // approve button for that case; it only unlocks once handoff.state is
  // "acknowledged".
  const showNaiveApprove = action.status === "pending" && !action.self_evicting;
  const showHandoff = action.status === "pending" && action.self_evicting && action.handoff != null;
  const resultBand = action.status === "done" || action.status === "done_unverified" || action.status === "failed";

  // "Let smith fix it" (Sprint 3): server-computed (Sprint 6) — see
  // Action.MarshalJSON (go/internal/smith/actions.go), which mirrors this
  // exact status/self_evicting/kind-mapped logic in one place instead of a
  // second hand-maintained copy here that could silently drift out of sync
  // with the backend's own procedureForActionKind map.
  const canProcedurize = action.procedurizable;

  // runbook is the one action kind that NEVER executes (ApproveAction
  // short-circuits it straight to done_unverified server-side, no dispatch
  // ever runs) — it's always purely a suggestion the operator can act on
  // by hand or ignore. Presenting it with the same Approve/Reject language
  // as a real destructive action (delete_files, restart_forge_unit, …)
  // reads as "something needs fixing" when nothing does — real operator
  // feedback, 2026-08-12. "Reject" for a runbook has no different real
  // effect than "Approve" (neither executes), so this only changes labels
  // and framing, not the underlying mutations.
  const isSuggestion = action.kind === "runbook";

  // Sprint S4 (§5.5): a "done — I ran it myself" runbook offers a one-click
  // re-check of the source check(s) it was meant to fix. Which checks exist
  // determines the affordance — an attached investigation re-runs its
  // warn/crit checks, else detail.check_id (stamped by the Go proposers); a
  // runbook with neither has nothing to re-verify.
  const doneRunbook = action.kind === "runbook" && action.status === "done_unverified";
  const recheckCheckId = typeof action.detail?.check_id === "string" ? action.detail.check_id : undefined;
  const canRecheck = doneRunbook && (action.investigation_id != null || recheckCheckId != null);
  const recheckLabel = action.investigation_id != null
    ? "re-check the checks"
    : recheckCheckId
      ? `re-check ${recheckCheckId}`
      : "no check to re-verify";

  return (
    <div className="card action-card" style={{ padding: "10px 14px", marginBottom: 8 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <span className="chip">{action.kind.replace(/_/g, " ")}</span>
        <span style={{ fontSize: 12.5, fontWeight: 600, color: "var(--text)" }}>{action.title}</span>
        {riskChip(action.risk)}
        {statusChip(action.status)}
        {action.self_evicting && (
          <span className="chip" style={{ color: "var(--warn)", borderColor: "color-mix(in srgb, var(--warn) 40%, var(--border))" }}>
            self-evicting
          </span>
        )}
        <span style={{ fontSize: 10, color: "var(--text-mute)", marginLeft: "auto" }}>{fmtTime(action.created_at)}</span>
      </div>

      {isSuggestion && (
        <div style={{ marginTop: 8, fontSize: 11, color: "var(--text-dim)" }}>
          nothing is broken — this is a suggestion, not a failing check.
        </div>
      )}

      <DetailBlock detail={action.detail} />

      {/* Plain runbook-kind actions store their steps in detail.steps, NOT
          in action.handoff (which stays null/not_required for these — they
          never touch the self-eviction FSM at all). See RunbookCard.tsx's
          header comment on the two distinct data sources. Sprint S3-Web
          passes whyCantRun (detail.why_cant_run when the Go side provides
          one, else a concise default) so the card states WHY smith can't
          run this itself (§2.4.3). */}
      {action.kind === "runbook" && action.detail && Array.isArray(action.detail.steps) && (
        <div style={{ marginTop: 8 }}>
          <RunbookCard
            steps={action.detail.steps as SmithRunbookStep[]}
            storageKey={String(action.id)}
            whyCantRun={
              typeof action.detail.why_cant_run === "string"
                ? action.detail.why_cant_run
                : "these steps need a human to run them"
            }
          />
        </div>
      )}

      {action.kind === "delete_files" && action.detail && Array.isArray(action.detail.files) && (
        <DeleteFilesCard
          files={action.detail.files as SmithDeleteFileEntry[]}
          totalBytes={typeof action.detail.total_bytes === "number" ? action.detail.total_bytes : 0}
          guidance={typeof action.detail.guidance === "string" ? action.detail.guidance : undefined}
        />
      )}

      {error && <div className="error-note" style={{ marginTop: 8 }}>{error}</div>}

      {action.status === "executing" && action.kind !== "procedure" && (
        <div style={{ display: "flex", alignItems: "center", gap: 6, marginTop: 8, fontSize: 11, color: "var(--text-dim)" }}>
          <span className="action-busy-dot dot-busy" />
          Executing… (updates live via SSE)
        </div>
      )}

      {showNaiveApprove && (
        <div style={{ display: "flex", gap: 8, marginTop: 10, alignItems: "center" }}>
          {action.kind === "delete_files" ? (
            // delete_files is real, irreversible file deletion — the plain
            // one-click Approve every other kind gets isn't enough here;
            // this reuses the same arm-then-confirm interaction Reject
            // already uses elsewhere on this card.
            <ConfirmButton
              onConfirm={handleApprove}
              pending={approve.isPending}
              label="Approve"
              confirmLabel="Delete these files?"
              className="btn primary"
              warning="This will permanently delete every file listed above. This cannot be undone."
            />
          ) : (
            <button className="btn primary" disabled={approve.isPending} onClick={handleApprove}>
              {approve.isPending ? "…" : isSuggestion ? "done — I ran it myself" : "Approve"}
            </button>
          )}
          <ConfirmButton
            onConfirm={handleReject}
            pending={reject.isPending}
            label={isSuggestion ? "Dismiss" : "Reject"}
            confirmLabel={isSuggestion ? "Dismiss?" : "Reject?"}
            warning={isSuggestion ? undefined : "This proposal will be marked rejected."}
          />
          {canProcedurize && (
            <button className="btn" onClick={() => setShowDowntimeModal(true)}>
              Let smith fix it
            </button>
          )}
        </div>
      )}

      {showHandoff && <HandoffPanel action={action} />}

      {action.kind === "procedure" && action.status === "executing" && <ProcedureRunPanel actionId={action.id} />}

      {showDowntimeModal && <DowntimeModal actionId={action.id} onClose={() => setShowDowntimeModal(false)} />}

      {resultBand && action.result && (
        <div
          className="card"
          style={{
            marginTop: 8, padding: 10,
            borderLeft: `3px solid ${action.status === "failed" ? "var(--crit)" : action.status === "done_unverified" ? "var(--warn)" : "var(--ok)"}`,
            background: action.status === "failed"
              ? "color-mix(in srgb, var(--crit) 8%, var(--panel))"
              : action.status === "done_unverified"
                ? "color-mix(in srgb, var(--warn) 8%, var(--panel))"
                : "color-mix(in srgb, var(--ok) 8%, var(--panel))",
          }}
        >
          <div style={{
            fontSize: 12, fontWeight: 600,
            color: action.status === "failed" ? "var(--crit)" : action.status === "done_unverified" ? "var(--warn)" : "var(--ok)",
          }}>
            {action.status === "failed" ? "Failed" : action.status === "done_unverified" ? "Done — unverified" : "Done"}
          </div>
          <div style={{ fontSize: 12, color: "var(--text-dim)", marginTop: 4 }}>{action.result.message}</div>
          {action.result.error && (
            <div style={{ fontSize: 11, color: "var(--crit)", marginTop: 4 }}>{action.result.error}</div>
          )}
          <VerifyList verify={action.result.verify ?? []} />
        </div>
      )}

      {doneRunbook && (
        <div style={{ marginTop: 10, display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
          {canRecheck ? (
            <button className="btn" disabled={recheck.isPending} onClick={handleRecheck}>
              {recheck.isPending ? "…" : recheckLabel}
            </button>
          ) : (
            <div style={{ fontSize: 11, color: "var(--text-mute)" }}>no check to re-verify — done</div>
          )}
        </div>
      )}

      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </div>
  );
}
