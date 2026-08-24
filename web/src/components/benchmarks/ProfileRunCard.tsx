import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError, apiErrorMessage } from "../../lib/api";
import { depthLabel } from "../../lib/profileFormat";
import { qk, useProfileActive, useProfileProgress, useProfileRun, useStatus, type ProfileProgressState } from "../../lib/queries";
import { ScanFrame } from "../ScanFrame";
import { StepUpModal } from "../StepUpModal";

// ProfileRunCard — Phase 8 (pre-release feedback sprint). Salvages
// ProfilingPanel.tsx's confirm modal, live SSE progress card, phaseLabel,
// and step-up wiring verbatim — this is the destructive part (evicts
// A1–A4), so none of it gets "tidied" during the move into the merged
// Benchmarks & Profiling section. Split into a controller hook +
// presentational card so per-config Profile buttons (ConfigBenchmarkGroup)
// can trigger a run without each owning their own confirm/progress state —
// only one run can be in flight globally (runner.IsRunning() server-side).
//
// useProfileRunTracker() — the authoritative poll that finalizes a run —
// stays mounted once in App.tsx, not here: tying it to this card's mount
// lifetime would silently stop the poll if the operator switched
// Settings sections mid-run (see queries.ts's doc comment on that hook).
export function useProfileRunController() {
  const run = useProfileRun();
  const qc = useQueryClient();
  const [confirmMode, setConfirmMode] = useState<string | null>(null);
  const [stepUpOpen, setStepUpOpen] = useState(false);
  const [stepUpFactor, setStepUpFactor] = useState<"password" | "totp">("password");
  const [error, setError] = useState<string | null>(null);

  // The mode currently being profiled, or null when idle/finished — set on
  // submit below, cleared by useProfileRunTracker() once its poll confirms
  // the run is over.
  const { data: active } = useProfileActive();
  const activeMode = active?.mode ?? null;
  const progress = useProfileProgress(); // SSE phase detail — decoration only

  const submitting = run.isPending;
  const polling = activeMode != null;
  const showProgress = polling || submitting || progress?.phase === "done" || progress?.phase === "failed";
  // Only disable while actually running — a past failure/done state should
  // stay visible but NOT block a retry.
  const busy = polling || submitting;

  function requestProfile(mode: string) {
    setError(null);
    setConfirmMode(mode);
  }

  function startProfile(mode: string) {
    setConfirmMode(null);
    setError(null);
    run.mutate({ mode }, {
      onSuccess: () => {
        qc.setQueryData(qk.profileActive, { mode, startedAt: Date.now() });
        qc.setQueryData(qk.profileProgress, { phase: "evicting", running: true, mode });
      },
      onError: (e) => {
        if (e instanceof ApiError && e.status === 403) {
          const body = e.body as { error?: string; required?: string } | null;
          if (body?.error === "step_up_required") {
            setStepUpFactor(body.required === "totp" ? "totp" : "password");
            setStepUpOpen(true);
            setConfirmMode(mode); // remember to retry after step-up
            return;
          }
        }
        setError(apiErrorMessage(e));
      },
    });
  }

  function retryAfterStepUp() {
    setStepUpOpen(false);
    if (confirmMode) startProfile(confirmMode);
  }

  return {
    confirmMode, setConfirmMode,
    stepUpOpen, setStepUpOpen, stepUpFactor,
    error, setError,
    activeMode, progress, submitting, polling, busy, showProgress,
    requestProfile, startProfile, retryAfterStepUp,
  };
}

export type ProfileRunController = ReturnType<typeof useProfileRunController>;

// Sprint K: the backend already sends per-phase stage detail (profile.go's
// publishProgress calls) — this used to be a flat Record<string,string>
// showing the same static text regardless of what the run had actually
// measured so far. Now a function reading ProfileProgressEvent's fields.
function phaseLabel(p: ProfileProgressState | null): string {
  if (!p) return "";
  switch (p.phase) {
    case "evicting":
      return p.already_loaded
        ? `${p.mode ?? "Target"} already loaded in ${p.target_slot ?? "a slot"} — reusing it, evicting the rest…`
        : "Evicting all slots (A1–A4)…";
    case "loading":
      return `Loading model alone in ${p.slot ?? "a slot"}…`;
    case "verifying":
      return "Verifying actual n_ctx…";
    case "filling":
      return p.depth_target != null
        ? `Filling context to ${p.depth_target.toLocaleString()} tokens${p.actual_n_ctx ? ` (${depthLabel(p.depth_target, p.actual_n_ctx)})` : ""}…`
        : "Filling context with heterogeneous data…";
    case "measuring":
      return "Measuring peak memory…";
    case "benchmarking":
      if (p.depth_tokens != null) {
        const depth = p.actual_n_ctx ? ` at ${depthLabel(p.depth_tokens, p.actual_n_ctx)}` : ` at ${p.depth_tokens.toLocaleString()} tokens`;
        const tps = p.pp2048_tps != null || p.tg128_tps != null
          ? ` — pp ${p.pp2048_tps?.toFixed(1) ?? "…"} t/s, tg ${p.tg128_tps?.toFixed(1) ?? "…"} t/s`
          : "";
        return `Benchmarking${depth}${tps}`;
      }
      return "Benchmarking prefill/decode T/s…";
    case "done":
      return "Done — profile recorded.";
    case "failed":
      return "Failed.";
    default:
      return "";
  }
}

export function ProfileRunCard({ controller }: { controller: ProfileRunController }) {
  const status = useStatus();
  const qc = useQueryClient();
  const {
    confirmMode, setConfirmMode, stepUpOpen, stepUpFactor,
    error, progress, submitting, polling, showProgress,
    startProfile, retryAfterStepUp,
  } = controller;

  const slots = status.data?.slots ?? {};
  const loadedSlots = Object.entries(slots).filter(([, mode]) => mode != null) as [string, string | null][];

  // Selective eviction (profile.go's findLoadedSlot): if the target mode is
  // already loaded in a slot, that slot is reused, not evicted.
  function evictTargets(mode: string): [string, string | null][] {
    return loadedSlots.filter(([, m]) => m !== mode);
  }

  return (
    // Always-rendered wrapper (even when nothing inside is visible) so the
    // Settings-search landmark for "Profiling" has a stable DOM id to
    // scroll to — the progress card itself only renders during/just after
    // a run.
    <div id="profiling-runs">
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {showProgress && (
        <div className="card" style={{
          marginBottom: 12, padding: 14,
          background: progress?.phase === "failed"
            ? "color-mix(in srgb, var(--crit) 8%, var(--panel))"
            : "color-mix(in srgb, var(--warn) 8%, var(--panel))",
          borderLeft: `3px solid ${progress?.phase === "failed" ? "var(--crit)" : "var(--warn)"}`,
        }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
            {polling && <ScanFrame title="Profile run in progress" />}
            <span style={{
              fontSize: 12, fontWeight: 600,
              color: progress?.phase === "failed" ? "var(--crit)" : "var(--warn)",
            }}>
              {submitting
                ? "Starting profile run…"
                : progress?.phase === "failed"
                  ? "Profile run FAILED"
                  : progress?.phase === "done"
                    ? "Profile complete"
                    : "Profile run in progress"}
            </span>
          </div>
          <div style={{ fontSize: 13, marginBottom: 4 }}>
            {submitting
              ? "Sending request to evict all slots…"
              : phaseLabel(progress) || progress?.phase}
          </div>
          {progress?.mode && (
            <div style={{ fontSize: 11, color: "var(--text-dim)", marginBottom: 2 }}>
              mode: <span style={{ fontFamily: "var(--mono)" }}>{progress.mode}</span>
            </div>
          )}
          {(progress?.actual_n_ctx != null) && (
            <div style={{ fontSize: 11, color: "var(--text-dim)" }}>
              actual n_ctx: {progress.actual_n_ctx}
              {progress.target_n_ctx != null && progress.actual_n_ctx < progress.target_n_ctx
                ? ` (requested ${progress.target_n_ctx} — silently reduced)`
                : ""}
            </div>
          )}
          {(progress?.peak_bytes != null || progress?.safe_bytes != null) && (
            <div style={{ fontSize: 11, color: "var(--text-dim)" }}>
              peak {progress.peak_bytes} bytes → safe {progress.safe_bytes} bytes
            </div>
          )}
          {(progress?.prefill_tps != null || progress?.decode_tps != null) && (
            <div style={{ fontSize: 11, color: "var(--text-dim)" }}>
              prefill {progress.prefill_tps?.toFixed(1)} t/s · decode {progress.decode_tps?.toFixed(1)} t/s
            </div>
          )}
          {progress?.phase === "failed" && progress?.error && (
            <div className="error-note" style={{ marginTop: 8, fontSize: 11 }}>
              {progress.error}
            </div>
          )}
          {(progress?.phase === "done" || progress?.phase === "failed") && !polling && (
            <div style={{ marginTop: 8 }}>
              <button
                className="btn"
                style={{ fontSize: 11 }}
                onClick={() => qc.setQueryData(qk.profileProgress, { phase: "idle", running: false })}
              >
                Dismiss
              </button>
            </div>
          )}
          {polling && loadedSlots.length > 0 && progress?.phase === "evicting" && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 6 }}>
              Evicting: {loadedSlots.map(([slot, mode]) => `${mode} (${slot})`).join(", ")}
            </div>
          )}
          {polling && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 6 }}>
              Slots: {Object.keys(slots).length === 0
                ? "loading…"
                : Object.entries(slots).map(([slot, mode]) =>
                  mode ? `${slot}=${mode}` : `${slot}=empty`
                ).join(", ")}
            </div>
          )}
        </div>
      )}

      {confirmMode && (() => {
        const targetAlreadyLoaded = loadedSlots.some(([, mode]) => mode === confirmMode);
        const toEvict = evictTargets(confirmMode);
        return (
        <div
          className="modal-backdrop"
          onClick={(e) => {
            // Sprint I fix — see DetailModal.tsx's identical comment.
            e.stopPropagation();
            if (!submitting) setConfirmMode(null);
          }}
        >
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <h3>Profile {confirmMode}?</h3>
            <div className="error-note" style={{ marginBottom: 14, background: "color-mix(in srgb, var(--warn) 12%, transparent)" }}>
              <b>Warning:</b> {targetAlreadyLoaded
                ? <>{confirmMode} is already loaded and will be reused, not evicted.</>
                : <>This loads <b>{confirmMode}</b> alone to measure it.</>}{" "}
              {toEvict.length > 0
                ? <>Will unload <b>{toEvict.length} of {Object.keys(slots).length} slots</b> to make room.</>
                : "No other slots need to be evicted."} The slots evicted will remain unloaded after
              profiling completes — you or the scheduler will need to reload them.
            </div>
            {toEvict.length > 0 && (
              <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 10, lineHeight: 1.55 }}>
                Will be evicted:
                <ul style={{ margin: "4px 0 0 20px", padding: 0 }}>
                  {toEvict.map(([slot, mode]) => (
                    <li key={slot}><span style={{ fontFamily: "var(--mono)" }}>{mode}</span> (slot {slot})</li>
                  ))}
                </ul>
              </div>
            )}
            <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 14, lineHeight: 1.55 }}>
              The run takes 1–3 minutes: evict → load target (if needed) → fill context → measure memory →
              benchmark T/s at 4 depths → record → unload. Progress streams live on this panel.
            </div>
            <div className="form-actions">
              <button className="btn" disabled={submitting} onClick={() => setConfirmMode(null)}>Cancel</button>
              <button
                className="btn primary"
                disabled={submitting}
                onClick={() => startProfile(confirmMode)}
              >
                {submitting ? "Starting…" : "Evict & Profile"}
              </button>
            </div>
          </div>
        </div>
        );
      })()}

      <StepUpModal
        open={stepUpOpen}
        requiredFactor={stepUpFactor}
        onSuccess={retryAfterStepUp}
        onClose={() => controller.setStepUpOpen(false)}
      />
    </div>
  );
}
