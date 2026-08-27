import { useEffect, useMemo, useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import {
  useSmithBlockedWork,
  useSmithFindings,
  useSmithFindingsPurge,
  useSmithInvestigation,
  useSmithInvestigationAnalyze,
  useSmithInvestigationChecks,
  useSmithInvestigationCreate,
  useSmithInvestigationResolve,
  useSmithInvestigations,
  useSmithKBRef,
  useSmithProcedureRunsList,
  useSmithProcedureScorecard,
  useSmithStatus,
} from "../../lib/queries";
import { useSession } from "../../lib/session";
import type {
  SmithBlockedItem,
  SmithFinding,
  SmithProcedureRunSummary,
  SmithProcedureStepOutcome,
  SmithStoredFinding,
} from "../../lib/types";
import { ActionsTray } from "../smith/ActionsTray";
import { AskSmithButton } from "../smith/AskSmithButton";
import { Markdown } from "../smith/Markdown";
import { MissedPatterns } from "../smith/SelfContextChip";

// ── Helpers ─────────────────────────────────────────────────────────────────

type Severity = "ok" | "info" | "warn" | "crit";

const SEV_ORDER: Severity[] = ["crit", "warn", "info"];

const SEV_COLOR: Record<string, string> = {
  ok: "var(--ok)",
  info: "var(--cool)",
  warn: "var(--warn)",
  crit: "var(--crit)",
};

function sevChip(sev: string) {
  const c = SEV_COLOR[sev] ?? "var(--text-mute)";
  return (
    <span
      className="chip"
      style={{ color: c, borderColor: `color-mix(in srgb, ${c} 40%, var(--border))` }}
    >
      {sev}
    </span>
  );
}

// confChip renders a finding's confidence (Tier 1 Sprint 4): only when
// degraded from the default "high", matching the advisory-tone/hazard-badge
// precedent elsewhere in this codebase: a normal, fully-measured finding
// shouldn't carry a badge every row just to say "nothing was missing".
function confChip(confidence: string | undefined, note: string | undefined) {
  if (!confidence || confidence === "high") return null;
  const c = confidence === "medium" ? "var(--warn)" : "var(--crit)";
  return (
    <span
      className="chip"
      style={{ color: c, borderColor: `color-mix(in srgb, ${c} 40%, var(--border))` }}
      title={note || `${confidence} confidence: some evidence was unavailable`}
    >
      {confidence} confidence
    </span>
  );
}

function statusChip(status: string) {
  const c =
    status === "open" ? "var(--warn)" :
    status === "resolved" ? "var(--ok)" :
    "var(--text-mute)";
  return (
    <span
      className="chip"
      style={{ color: c, borderColor: `color-mix(in srgb, ${c} 40%, var(--border))` }}
    >
      {status}
    </span>
  );
}

function fmtTime(unixOrISO: string | number): string {
  const d = typeof unixOrISO === "number" ? new Date(unixOrISO * 1000) : new Date(unixOrISO);
  if (isNaN(d.getTime())) return String(unixOrISO);
  return d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", hour12: false });
}

// fmtDuration renders a whole-second duration compactly (e.g. "45s", "3m 12s").
function fmtDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  const m = Math.floor(seconds / 60);
  const s = seconds % 60;
  return s > 0 ? `${m}m ${s}s` : `${m}m`;
}

// toUnixSeconds normalizes a finding's created_at (an ISO string on stored
// rows) or an already-unix number into unix seconds for the ChatContext.at
// field. Falls back to "now" on a missing/unparseable value so the seed
// message still reads sensibly.
function toUnixSeconds(v: string | number | undefined): number {
  if (v == null) return Math.floor(Date.now() / 1000);
  const ms = typeof v === "number" ? v * 1000 : Date.parse(v);
  if (isNaN(ms)) return Math.floor(Date.now() / 1000);
  return Math.floor(ms / 1000);
}

// parseEvidence prettifies a finding's evidence for display. StoredFinding
// carries evidence as raw JSON text (string); Finding carries it as a JSON
// object (map[string]any on the wire). Both are rendered as pretty-printed
// JSON.
function renderEvidence(evidence: string | Record<string, unknown>): string {
  if (typeof evidence === "string") {
    try {
      return JSON.stringify(JSON.parse(evidence), null, 2);
    } catch {
      return evidence;
    }
  }
  try {
    return JSON.stringify(evidence, null, 2);
  } catch {
    return String(evidence);
  }
}

// ── Evidence row (expandable) ────────────────────────────────────────────────

function EvidenceRow({ evidence }: { evidence: string | Record<string, unknown> }) {
  const [expanded, setExpanded] = useState(false);
  const text = useMemo(() => renderEvidence(evidence), [evidence]);
  return (
    <div style={{ marginTop: 4 }}>
      <button
        className="tab"
        style={{ fontSize: 10, padding: "2px 8px", cursor: "pointer" }}
        onClick={() => setExpanded(!expanded)}
      >
        {expanded ? "▼ evidence" : "▶ evidence"}
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

// ── KB ref chip (expandable, P4, docs/v5-smith.md §4.7) ────────────────────
// Lazily resolves one Finding.kb_refs entry to its chunk on first expand
// (useSmithKBRef's `enabled: !!ref` gate); a finding with no expanded
// chips never fetches anything.

function KBRefChip({ refId }: { refId: string }) {
  const [expanded, setExpanded] = useState(false);
  const chunk = useSmithKBRef(expanded ? refId : null);
  return (
    <div>
      <button
        className="chip"
        style={{ cursor: "pointer", fontSize: 10 }}
        onClick={() => setExpanded(!expanded)}
        title="Knowledge base reference"
      >
        {expanded ? "▼" : "▶"} 📖 {refId}
      </button>
      {expanded && (
        <div
          style={{
            marginTop: 4, padding: 8, borderRadius: 6, maxWidth: 560,
            fontSize: 11.5, lineHeight: 1.5,
            background: "var(--bg)", color: "var(--text-dim)",
            border: "1px solid var(--border)",
          }}
        >
          {chunk.isLoading ? (
            "Loading…"
          ) : chunk.isError || !chunk.data ? (
            <span className="error-note" style={{ fontSize: 11 }}>
              {chunk.error ? apiErrorMessage(chunk.error) : "Unable to load this reference."}
            </span>
          ) : (
            <>
              <div style={{ fontWeight: 600, color: "var(--text)", marginBottom: 4 }}>
                {chunk.data.chunk.title}
              </div>
              <Markdown text={chunk.data.chunk.body} />
            </>
          )}
        </div>
      )}
    </div>
  );
}

// ── Finding card (shared by findings view and investigation detail) ─────────
// Accepts either a SmithFinding (from sweep responses; evidence is a JSON
// object, no sweep_kind/created_at) or a SmithStoredFinding (from the
// findings list / investigation detail; evidence is raw JSON text, has
// sweep_kind + created_at). The `stored` flag gates the extra metadata.

type FindingLike = SmithFinding | SmithStoredFinding;

function hasSweepKind(f: FindingLike): f is SmithStoredFinding {
  return "sweep_kind" in f;
}

function FindingCard({ f, stored }: { f: FindingLike; stored?: boolean }) {
  const sweepKind = stored && hasSweepKind(f) ? f.sweep_kind : undefined;
  const createdAt = stored && hasSweepKind(f) ? f.created_at : undefined;
  const repeatCount = stored && hasSweepKind(f) ? f.repeat_count : 0;
  // Sprint S3-Web (§2.3, R5): the "Ask smith" affordance lands on warn/crit
  // rows only; ok/info rows aren't problems to ask about.
  const askable = f.severity === "warn" || f.severity === "crit";
  return (
    <div className="card" style={{ padding: "10px 14px", marginBottom: 6 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        {sevChip(f.severity)}
        {confChip(f.confidence, f.confidence_note)}
        {repeatCount > 1 && <span className="chip" style={{ fontSize: 10 }}>×{repeatCount}</span>}
        <span style={{ fontSize: 12, fontWeight: 600, color: "var(--text)" }}>{f.check_id}</span>
        {sweepKind && <span className="chip">{sweepKind}</span>}
        {createdAt && (
          <span style={{ fontSize: 10, color: "var(--text-mute)", marginLeft: "auto" }}>
            {fmtTime(createdAt)}
          </span>
        )}
        {askable && (
          <AskSmithButton
            context={[{ code: f.check_id, message: f.summary, source: f.check_id, at: toUnixSeconds(createdAt) }]}
            title={`Ask smith about ${f.check_id}`}
          />
        )}
      </div>
      <div style={{ fontSize: 12, color: "var(--text-dim)", marginTop: 4 }}>{f.summary}</div>
      <EvidenceRow evidence={f.evidence as string | Record<string, unknown>} />
      {f.kb_refs.length > 0 && (
        <div style={{ marginTop: 4, display: "flex", gap: 4, flexWrap: "wrap" }}>
          {f.kb_refs.map((ref) => (
            <KBRefChip key={ref} refId={ref} />
          ))}
        </div>
      )}
    </div>
  );
}

// ── Manual purge controls ───────────────────────────────────────────────────
// Operator-only: delete standalone findings by age (or all). Investigation-
// attached findings are never purged (server-side guard). Each action
// confirms before mutating; there is no undo.

function FindingsPurgeButtons() {
  const { canOperate } = useSession();
  const purge = useSmithFindingsPurge();
  if (!canOperate) return null;
  const buttons: { label: string; maxAge: "all" | "72h" | "168h"; confirm: string }[] = [
    { label: "Older than 72h", maxAge: "72h", confirm: "Delete all standalone findings older than 72 hours?" },
    { label: "Older than a week", maxAge: "168h", confirm: "Delete all standalone findings older than a week?" },
    { label: "Delete all", maxAge: "all", confirm: "Delete ALL standalone findings? Findings attached to investigations are kept." },
  ];
  return (
    <div style={{ display: "flex", gap: 6, flexWrap: "wrap", alignItems: "center", marginTop: 8 }}>
      {buttons.map((b) => (
        <button
          key={b.maxAge}
          className="tab"
          style={{ fontSize: 10, padding: "2px 8px" }}
          disabled={purge.isPending}
          onClick={() => {
            if (window.confirm(b.confirm)) purge.mutate(b.maxAge);
          }}
        >
          {purge.isPending ? "Purging…" : b.label}
        </button>
      ))}
      {purge.isError && <span className="error-note" style={{ fontSize: 10 }}>{apiErrorMessage(purge.error)}</span>}
    </div>
  );
}

// ── Sweep result banner (also used by AskSmith.tsx's SweepControls) ────────

export function SweepResult({ count, worst }: { count: number; worst: string }) {
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", marginTop: 6 }}>
      <span style={{ fontSize: 12, color: "var(--text-dim)" }}>{count} findings</span>
      <span style={{ fontSize: 12, color: "var(--text-dim)" }}>worst:</span>
      {sevChip(worst)}
    </div>
  );
}

// Sweep controls (Quick sweep / Deep sweep / Custom picker) moved to
// AskSmith.tsx, 2026-08-27, merged into the chat card alongside the
// quick-question suggestion chips instead of its own Diagnostics card.

// ── Investigations: list + detail + manual open ────────────────────────────

function InvestigationRow({
  inv,
  active,
  onClick,
}: {
  inv: { id: number; trigger: string; status: string; opened_at: number; summary: string };
  active: boolean;
  onClick: () => void;
}) {
  return (
    <div
      className="card"
      style={{
        padding: "8px 12px", marginBottom: 4, cursor: "pointer",
        borderLeft: active ? "3px solid var(--cool)" : undefined,
      }}
      onClick={onClick}
    >
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        {statusChip(inv.status)}
        <span className="chip">{inv.trigger}</span>
        <span style={{ fontSize: 10, color: "var(--text-mute)", marginLeft: "auto" }}>
          {fmtTime(inv.opened_at)}
        </span>
        {/* Sprint S3-Web (§2.3, R5): "Ask smith" affordance on investigation
            rows. stopPropagation so clicking it doesn't open the detail view. */}
        <span onClick={(e) => e.stopPropagation()}>
          <AskSmithButton
            context={[{ code: inv.trigger, message: inv.summary || `Investigation #${inv.id}`, source: "investigation", at: inv.opened_at }]}
            title={`Ask smith about investigation #${inv.id}`}
          />
        </span>
      </div>
      {inv.summary && (
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginTop: 2 }}>{inv.summary}</div>
      )}
    </div>
  );
}

function InvestigationDetail({
  id,
  onBack,
  onSubChange,
}: {
  id: number;
  onBack: () => void;
  onSubChange?: (sub: string, opts?: { replace?: boolean }) => void;
}) {
  const detail = useSmithInvestigation(id);
  const checksRun = useSmithInvestigationChecks(id);
  const resolve = useSmithInvestigationResolve(id);
  const analyze = useSmithInvestigationAnalyze();
  const [runResult, setRunResult] = useState<{ count: number; worst: string } | null>(null);

  function runChecks(scope?: string, checkIds?: string[]) {
    setRunResult(null);
    checksRun.mutate(
      { scope, checkIds },
      {
        onSuccess: (r) => setRunResult({ count: r.count, worst: r.worst }),
        onError: () => setRunResult(null),
      },
    );
  }

  if (detail.isLoading) return <div className="empty-note">Loading investigation…</div>;
  if (detail.isError || !detail.data) return <div className="empty-note">Investigation not found.</div>;

  const inv = detail.data;

  return (
    <div className="card">
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap", marginBottom: 8 }}>
        <button className="tab" onClick={onBack} style={{ cursor: "pointer" }}>← back</button>
        {statusChip(inv.status)}
        <span className="chip">{inv.trigger}</span>
        <span style={{ fontSize: 10, color: "var(--text-mute)", marginLeft: "auto" }}>
          opened {fmtTime(inv.opened_at)}
          {inv.closed_at ? ` · closed ${fmtTime(inv.closed_at)}` : ""}
        </span>
      </div>

      {inv.summary && (
        <div style={{ fontSize: 12.5, color: "var(--text-dim)", marginBottom: 10 }}>{inv.summary}</div>
      )}

      {/* Run more checks */}
      {inv.status === "open" && (
        <div style={{ marginBottom: 10, paddingTop: 8, borderTop: "1px solid var(--border)" }}>
          <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 4 }}>Run more checks</div>
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
            <button
              className="tab"
              onClick={() => runChecks("quick")}
              disabled={checksRun.isPending}
            >
              Quick
            </button>
            <button
              className="tab"
              onClick={() => runChecks("deep")}
              disabled={checksRun.isPending}
            >
              Deep
            </button>
          </div>
          {checksRun.isError && (
            <div className="error-note" style={{ marginTop: 4 }}>
              {apiErrorMessage(checksRun.error)}
            </div>
          )}
          {runResult && <SweepResult count={runResult.count} worst={runResult.worst} />}
        </div>
      )}

      {/* Findings trail */}
      <div style={{ marginBottom: 10 }}>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 4 }}>
          Findings trail ({inv.findings.length})
        </div>
        {inv.findings.length === 0 ? (
          <div className="empty-note">No findings attached to this investigation.</div>
        ) : (
          inv.findings.map((f) => <FindingCard key={f.id} f={f} stored />)
        )}
      </div>

      {/* Actions (Wave 3 / P2, track W3-C): proposals raised against this
          investigation (auto-proposed from its findings, or manually
          created), with approve/reject/handoff. Additive alongside the
          findings trail above, not a replacement for it. */}
      <div style={{ marginBottom: 10 }}>
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 4 }}>Actions</div>
        <ActionsTray investigationId={inv.id} />
      </div>

      {/* Tier 2 commentary (P3): reuses the investigation's linked
          conversation (created on first use, smith_investigations.
           conversation_id) and jumps to Ask the smith with it open, mirroring
           the existing #help/smith/inv/{id} deep-link convention. */}
      <div style={{ marginBottom: 10, paddingTop: 8, borderTop: "1px solid var(--border)" }}>
        <button
          className="tab"
          onClick={() =>
            analyze.mutate(inv.id, {
              onSuccess: (r) => onSubChange?.(`smith/conv/${r.conversation_id}`),
            })
          }
          disabled={analyze.isPending}
        >
          {analyze.isPending ? "Asking the smith…" : "Ask the smith to analyze"}
        </button>
        {analyze.isError && (
          <div className="error-note" style={{ marginTop: 4 }}>
            {apiErrorMessage(analyze.error)}
          </div>
        )}
      </div>

      {/* Resolve / Dismiss */}
      {inv.status === "open" && (
        <div style={{ display: "flex", gap: 6, paddingTop: 8, borderTop: "1px solid var(--border)" }}>
          <button
            className="tab active"
            onClick={() => resolve.mutate("resolved")}
            disabled={resolve.isPending}
          >
            Resolve
          </button>
          <button
            className="tab"
            onClick={() => resolve.mutate("dismissed")}
            disabled={resolve.isPending}
          >
            Dismiss
          </button>
        </div>
      )}
      {resolve.isError && (
        <div className="error-note" style={{ marginTop: 4 }}>
          {apiErrorMessage(resolve.error)}
        </div>
      )}
    </div>
  );
}

// ── Procedure runs (autonomous-remediation Sprint 4, the supervision &
// evaluation harness, docs/v5-smith.md §13) ──────────────────────────────
// A browsable history of every "let smith fix it" procedure run: the run
// timeline (steps, checkpoints, verify results) plus a per-run scorecard
// (unattended completion, checkpoints reached, post-verify, downtime
// estimate vs. actual). This is the evidence the Sprint 6 build-refresh
// capstone runs get judged against, so it exists ahead of that, not after.

function procRunStatusColor(status: string): string {
  switch (status) {
    case "completed": return "var(--ok)";
    case "failed": return "var(--crit)";
    case "precondition_failed": return "var(--reserved)";
    case "aborted": return "var(--text-mute)";
    case "awaiting_checkpoint": return "var(--warn)";
    default: return "var(--cool)"; // running
  }
}

function ProcedureStepRow({ step }: { step: SmithProcedureStepOutcome }) {
  return (
    <div style={{ display: "flex", alignItems: "flex-start", gap: 8, padding: "4px 0", borderBottom: "1px solid var(--border)" }}>
      <span style={{ color: step.ok ? "var(--ok)" : "var(--crit)", fontSize: 12, flex: "0 0 auto", width: 14 }}>
        {step.ok ? "✓" : "✗"}
      </span>
      <div style={{ flex: "1 1 auto", minWidth: 0 }}>
        <div style={{ fontSize: 12, color: "var(--text)" }}>{step.title}</div>
        {step.error && <div style={{ fontSize: 11, color: "var(--crit)", marginTop: 2 }}>{step.error}</div>}
        {step.verify && step.verify.length > 0 && (
          <div style={{ display: "flex", gap: 4, marginTop: 4, flexWrap: "wrap" }}>
            {step.verify.map((v) => (
              <span
                key={v.check_id}
                className="chip"
                style={{ fontSize: 9, color: SEV_COLOR[v.severity] ?? "var(--text-mute)" }}
              >
                {v.check_id}: {v.severity}
              </span>
            ))}
          </div>
        )}
      </div>
      <span style={{ fontSize: 10, color: "var(--text-mute)", flex: "0 0 auto" }}>
        {(step.duration_ms / 1000).toFixed(1)}s
      </span>
    </div>
  );
}

// ScorecardStrip fetches lazily (enabled only while its run is expanded),
// same posture as KBRefChip above, since most runs in the history list will
// never be opened.
function ScorecardStrip({ actionId }: { actionId: number }) {
  const sc = useSmithProcedureScorecard(actionId);
  if (sc.isLoading) return <div className="empty-note">Loading scorecard…</div>;
  if (sc.isError || !sc.data) return null;
  const d = sc.data;
  if (d.precondition_failed) {
    // Not "0 checkpoints needed a human" / "post-verify not confirmed clean":
    // both would misleadingly imply an attempt happened. This run never
    // executed a single step, so neither framing applies.
    return (
      <div style={{ fontSize: 11, color: "var(--reserved)", padding: "8px 0 0", marginTop: 6, borderTop: "1px solid var(--border)" }}>
        precondition not met: not applicable to this host, nothing was attempted
      </div>
    );
  }
  return (
    <div style={{ display: "flex", gap: 14, flexWrap: "wrap", fontSize: 11, color: "var(--text-dim)", padding: "8px 0 0", marginTop: 6, borderTop: "1px solid var(--border)" }}>
      <span style={{ color: d.unattended_completion ? "var(--ok)" : "var(--text-dim)" }}>
        {d.unattended_completion
          ? "✓ unattended"
          : `${d.checkpoints_reached} checkpoint${d.checkpoints_reached === 1 ? "" : "s"} needed a human`}
      </span>
      <span style={{ color: d.post_verify_passed ? "var(--ok)" : "var(--text-mute)" }}>
        {d.post_verify_passed ? "✓ post-verify clean" : "post-verify not confirmed clean"}
      </span>
      {d.needs_maintenance && (
        <span>
          downtime: {d.actual_duration_seconds != null ? fmtDuration(d.actual_duration_seconds) : "—"} actual
          {d.est_duration_seconds ? ` vs. ${fmtDuration(d.est_duration_seconds)} estimated` : ""}
        </span>
      )}
    </div>
  );
}

function ProcedureRunRow({
  run,
  expanded,
  onClick,
}: {
  run: SmithProcedureRunSummary;
  expanded: boolean;
  onClick: () => void;
}) {
  const duration = run.finished_at != null ? run.finished_at - run.started_at : null;
  const color = procRunStatusColor(run.status);
  return (
    <div className="card" style={{ padding: "10px 14px", marginBottom: 6, cursor: "pointer" }} onClick={onClick}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <span className="chip" style={{ color, borderColor: `color-mix(in srgb, ${color} 40%, var(--border))` }}>
          {run.status}
        </span>
        <span style={{ fontSize: 12, fontWeight: 600, color: "var(--text)" }}>{run.procedure_id}</span>
        <span style={{ fontSize: 11, color: "var(--text-mute)" }}>{run.action_title}</span>
        <span style={{ fontSize: 10, color: "var(--text-mute)", marginLeft: "auto" }}>
          {fmtTime(run.started_at)}
          {duration != null ? ` · ${fmtDuration(duration)}` : ""}
        </span>
      </div>
      {expanded && (
        <div style={{ marginTop: 8, paddingTop: 8, borderTop: "1px solid var(--border)" }} onClick={(e) => e.stopPropagation()}>
          {run.steps.map((step) => (
            <ProcedureStepRow key={step.index} step={step} />
          ))}
          {run.checkpoint_note && (
            <div className="empty-note" style={{ marginTop: 6 }}>{run.checkpoint_note}</div>
          )}
          <ScorecardStrip actionId={run.action_id} />
        </div>
      )}
    </div>
  );
}

function ProcedureRunsSection() {
  const runs = useSmithProcedureRunsList(50);
  const [collapsed, setCollapsed] = useState(true);
  const [expandedId, setExpandedId] = useState<number | null>(null);

  return (
    <div className="card" style={{ marginTop: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <h3 style={{ margin: 0 }}>Procedure runs</h3>
        <button
          className="tab"
          style={{ marginLeft: "auto", cursor: "pointer" }}
          onClick={() => setCollapsed(!collapsed)}
        >
          {collapsed ? `Show${runs.data ? ` (${runs.data.length})` : ""}` : "Hide"}
        </button>
      </div>
      {!collapsed && (
        <div style={{ marginTop: 8 }}>
          {runs.isLoading ? (
            <div className="empty-note">Loading…</div>
          ) : runs.isError ? (
            <div className="error-note">{apiErrorMessage(runs.error)}</div>
          ) : !runs.data || runs.data.length === 0 ? (
            <div className="empty-note">No procedure runs yet.</div>
          ) : (
            runs.data.map((run) => (
              <ProcedureRunRow
                key={run.action_id}
                run={run}
                expanded={expandedId === run.action_id}
                onClick={() => setExpandedId(expandedId === run.action_id ? null : run.action_id)}
              />
            ))
          )}
        </div>
      )}
    </div>
  );
}

// ── Externally blocked work (P4, docs/v5-smith.md §4.7) ────────────────────
// docs/investigations.md, parsed. A section on Diagnostics, not its own tab
// (operator call): the tracker is read maybe once a session, not a live
// diagnostic surface like findings/investigations above it.

function BlockedItemRow({ item }: { item: SmithBlockedItem }) {
  const [expanded, setExpanded] = useState(false);
  return (
    <div className="card" style={{ padding: "10px 14px", marginBottom: 6 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        {statusChip(item.status)}
        <span style={{ fontSize: 12, fontWeight: 600, color: "var(--text)" }}>
          #{item.number} {item.title}
        </span>
        {item.last_checked && (
          <span style={{ fontSize: 10, color: "var(--text-mute)", marginLeft: "auto" }}>
            checked {item.last_checked}
          </span>
        )}
      </div>
      {item.blocked_on && (
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginTop: 4 }}>
          <span style={{ color: "var(--text-mute)" }}>Blocked on: </span>
          <Markdown text={item.blocked_on} />
        </div>
      )}
      <button
        className="tab"
        style={{ fontSize: 10, padding: "2px 8px", marginTop: 6, cursor: "pointer" }}
        onClick={() => setExpanded(!expanded)}
      >
        {expanded ? "▼ details" : "▶ details"}
      </button>
      {expanded && (
        <div style={{ marginTop: 6, paddingTop: 6, borderTop: "1px solid var(--border)" }}>
          {item.status_text && (
            <div style={{ fontSize: 11.5, color: "var(--text-dim)", marginBottom: 8 }}>
              <Markdown text={item.status_text} />
            </div>
          )}
          {item.where_to_check && (
            <div style={{ marginBottom: 8 }}>
              <div style={{ fontSize: 10, color: "var(--text-mute)", textTransform: "uppercase", letterSpacing: "0.05em", marginBottom: 2 }}>
                Where to check
              </div>
              <div style={{ fontSize: 11.5, color: "var(--text-dim)" }}>
                <Markdown text={item.where_to_check} />
              </div>
            </div>
          )}
          {item.when_unblocked && (
            <div>
              <div style={{ fontSize: 10, color: "var(--text-mute)", textTransform: "uppercase", letterSpacing: "0.05em", marginBottom: 2 }}>
                When unblocked
              </div>
              <div style={{ fontSize: 11.5, color: "var(--text-dim)" }}>
                <Markdown text={item.when_unblocked} />
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function BlockedWorkSection() {
  const blocked = useSmithBlockedWork();
  const [collapsed, setCollapsed] = useState(true);

  return (
    <div className="card" style={{ marginTop: 12 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <h3 style={{ margin: 0 }}>Externally blocked work</h3>
        <button
          className="tab"
          style={{ marginLeft: "auto", cursor: "pointer" }}
          onClick={() => setCollapsed(!collapsed)}
        >
          {collapsed ? `Show${blocked.data ? ` (${blocked.data.count})` : ""}` : "Hide"}
        </button>
      </div>
      {!collapsed && (
        <div style={{ marginTop: 8 }}>
          {blocked.isLoading ? (
            <div className="empty-note">Loading…</div>
          ) : blocked.isError ? (
            <div className="error-note">{apiErrorMessage(blocked.error)}</div>
          ) : !blocked.data || blocked.data.items.length === 0 ? (
            <div className="empty-note">Nothing tracked.</div>
          ) : (
            blocked.data.items.map((it) => <BlockedItemRow key={it.number} item={it} />)
          )}
        </div>
      )}
    </div>
  );
}

// ── Main Diagnostics component ──────────────────────────────────────────────

export function Diagnostics({
  sub,
  onSubChange,
}: {
  sub?: string;
  onSubChange?: (sub: string, opts?: { replace?: boolean }) => void;
}) {
  // Parse deep-link: #help/smith/inv/<id>
  const deepLinkedId = useMemo(() => {
    if (!sub) return null;
    const m = sub.match(/inv\/(\d+)/);
    return m ? parseInt(m[1], 10) : null;
  }, [sub]);

  const [selectedInvId, setSelectedInvId] = useState<number | null>(deepLinkedId);
  // S7-followup smith UX sprint: default to crit-only; warn/info are almost
  // never what the operator is looking for at a glance; the "all" tab is
  // still one click away.
  const [severityFilter, setSeverityFilter] = useState<string>("crit");
  const [manualSummary, setManualSummary] = useState("");
  const [showManual, setShowManual] = useState(false);

  // Sync selectedInvId when the deep-link hash changes
  useEffect(() => {
    if (deepLinkedId !== null) setSelectedInvId(deepLinkedId);
  }, [deepLinkedId]);

  function selectInv(id: number) {
    setSelectedInvId(id);
    onSubChange?.(`smith/inv/${id}`, { replace: true });
  }

  function backToList() {
    setSelectedInvId(null);
    onSubChange?.("smith", { replace: true });
  }

  // ── Data hooks ──
  const findings = useSmithFindings(severityFilter || undefined);
  const investigations = useSmithInvestigations();
  const createInvestigation = useSmithInvestigationCreate();
  const status = useSmithStatus();

  // ── Grouped findings (by severity, then all) ──
  const groupedFindings = useMemo(() => {
    const list = (findings.data?.findings ?? []).filter((f) => f.severity !== "ok");
    const m = new Map<string, SmithStoredFinding[]>();
    for (const f of list) {
      const sev = f.severity || "ok";
      if (!m.has(sev)) m.set(sev, []);
      m.get(sev)!.push(f);
    }
    return m;
  }, [findings.data]);

  return (
    <>
      {/* ── Findings ── */}
      <div className="card" style={{ marginBottom: 12 }}>
        <h3>Findings</h3>
        {status.data?.missed_patterns && status.data.missed_patterns.length > 0 && (
          <MissedPatterns patterns={status.data.missed_patterns} />
        )}
        {/* Severity filter */}
        <div style={{ display: "flex", gap: 4, marginBottom: 8, flexWrap: "wrap" }}>
          <button
            className={`tab ${!severityFilter ? "active" : ""}`}
            style={{ fontSize: 10, padding: "2px 8px" }}
            onClick={() => setSeverityFilter("")}
          >
            all
          </button>
          {SEV_ORDER.map((s) => (
            <button
              key={s}
              className={`tab ${severityFilter === s ? "active" : ""}`}
              style={{ fontSize: 10, padding: "2px 8px", ...(severityFilter === s ? { color: SEV_COLOR[s] } : {}) }}
              onClick={() => setSeverityFilter(s)}
            >
              {s}
            </button>
          ))}
        </div>

        <FindingsPurgeButtons />

        {findings.isLoading ? (
          <div className="empty-note">Loading findings…</div>
        ) : findings.isError ? (
          <div className="error-note">{apiErrorMessage(findings.error)}</div>
        ) : groupedFindings.size === 0 ? (
          <div className="empty-note">No findings recorded.</div>
        ) : (
          <>
            {findings.data && (
              <div style={{ fontSize: 10, color: "var(--text-mute)", marginBottom: 6 }}>
                {findings.data.count} total{severityFilter ? ` (filtered: ${severityFilter})` : ""}
              </div>
            )}
            {SEV_ORDER.filter((s) => groupedFindings.has(s)).map((sev) => (
              <div key={sev} style={{ marginBottom: 8 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 6, marginBottom: 4 }}>
                  {sevChip(sev)}
                  <span style={{ fontSize: 10, color: "var(--text-mute)" }}>
                    ({groupedFindings.get(sev)!.length})
                  </span>
                </div>
                {groupedFindings.get(sev)!.map((f) => (
                  <FindingCard key={f.id} f={f} stored />
                ))}
              </div>
            ))}
          </>
        )}
      </div>

      {/* ── Investigations ── */}
      <div className="card">
        <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
          <h3 style={{ margin: 0 }}>Investigations</h3>
          <button
            className="tab"
            style={{ marginLeft: "auto", cursor: "pointer" }}
            onClick={() => setShowManual(!showManual)}
          >
            {showManual ? "Cancel" : "+ Open investigation"}
          </button>
        </div>

        {/* Manual open form */}
        {showManual && (
          <div style={{ marginBottom: 8, paddingBottom: 8, borderBottom: "1px solid var(--border)" }}>
            <input
              type="text"
              placeholder="Summary (optional)"
              value={manualSummary}
              onChange={(e) => setManualSummary(e.target.value)}
              style={{
                width: "100%", padding: "6px 8px", fontSize: 12, marginBottom: 6,
                background: "var(--bg)", color: "var(--text)",
                border: "1px solid var(--border)", borderRadius: 6, fontFamily: "var(--ui)",
              }}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  createInvestigation.mutate(manualSummary, {
                    onSuccess: (inv) => {
                      setManualSummary("");
                      setShowManual(false);
                      selectInv(inv.id);
                    },
                  });
                }
              }}
            />
            <button
              className="tab active"
              onClick={() =>
                createInvestigation.mutate(manualSummary, {
                  onSuccess: (inv) => {
                    setManualSummary("");
                    setShowManual(false);
                    selectInv(inv.id);
                  },
                })
              }
              disabled={createInvestigation.isPending}
            >
              Create
            </button>
            {createInvestigation.isError && (
              <div className="error-note" style={{ marginTop: 4 }}>
                {apiErrorMessage(createInvestigation.error)}
              </div>
            )}
          </div>
        )}

        {/* Detail or list */}
        {selectedInvId !== null ? (
          <InvestigationDetail id={selectedInvId} onBack={backToList} onSubChange={onSubChange} />
        ) : investigations.isLoading ? (
          <div className="empty-note">Loading investigations…</div>
        ) : investigations.isError ? (
          <div className="error-note">{apiErrorMessage(investigations.error)}</div>
        ) : !investigations.data || investigations.data.investigations.length === 0 ? (
          <div className="empty-note">No investigations.</div>
        ) : (
          <>
            <div style={{ fontSize: 10, color: "var(--text-mute)", marginBottom: 6 }}>
              {investigations.data.count} total
            </div>
            {investigations.data.investigations.map((inv) => (
              <InvestigationRow
                key={inv.id}
                inv={inv}
                active={false}
                onClick={() => selectInv(inv.id)}
              />
            ))}
          </>
        )}
      </div>

      {/* ── Procedure runs (Sprint 4 supervision/evaluation harness) ── */}
      <ProcedureRunsSection />

      {/* ── Externally blocked work (P4) ── */}
      <BlockedWorkSection />
    </>
  );
}
