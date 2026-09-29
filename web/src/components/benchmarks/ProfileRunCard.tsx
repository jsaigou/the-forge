import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Trans, useTranslation } from "react-i18next";
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
function phaseLabel(p: ProfileProgressState | null, t: (key: string, opts?: Record<string, unknown>) => string): string {
  if (!p) return "";
  const tp = (key: string, opts?: Record<string, unknown>) => t(`benchmarks.profile_run.phase.${key}`, opts);
  switch (p.phase) {
    case "evicting":
      return p.already_loaded
        ? tp("evicting_reusing", { mode: p.mode ?? tp("target_fallback"), slot: p.target_slot ?? tp("slot_fallback") })
        : tp("evicting_all");
    case "loading":
      return tp("loading", { slot: p.slot ?? tp("slot_fallback") });
    case "verifying":
      return tp("verifying");
    case "filling":
      return p.depth_target != null
        ? tp("filling", { target: p.depth_target.toLocaleString(), suffix: p.actual_n_ctx ? tp("at_depth_paren", { depth: depthLabel(p.depth_target, p.actual_n_ctx, t) }) : "" })
        : tp("filling_heterogeneous");
    case "measuring":
      return tp("measuring");
    case "benchmarking":
      if (p.depth_tokens != null) {
        const depthSuffix = p.actual_n_ctx ? tp("at_depth", { depth: depthLabel(p.depth_tokens, p.actual_n_ctx, t) }) : tp("at_tokens", { tokens: p.depth_tokens.toLocaleString() });
        const tpsSuffix = p.pp2048_tps != null || p.tg128_tps != null
          ? tp("tps_suffix", { pp: p.pp2048_tps?.toFixed(1) ?? "…", tg: p.tg128_tps?.toFixed(1) ?? "…" })
          : "";
        return tp("benchmarking", { depthSuffix, tpsSuffix });
      }
      return tp("benchmarking_default");
    case "done":
      return tp("done");
    case "failed":
      return tp("failed");
    default:
      return "";
  }
}

export function ProfileRunCard({ controller }: { controller: ProfileRunController }) {
  const { t } = useTranslation("settings");
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
            {polling && <ScanFrame title={t("benchmarks.profile_run.in_progress_title")} />}
            <span style={{
              fontSize: 12, fontWeight: 600,
              color: progress?.phase === "failed" ? "var(--crit)" : "var(--warn)",
            }}>
              {submitting
                ? t("benchmarks.profile_run.starting")
                : progress?.phase === "failed"
                  ? t("benchmarks.profile_run.failed_banner")
                  : progress?.phase === "done"
                    ? t("benchmarks.profile_run.complete_banner")
                    : t("benchmarks.profile_run.in_progress_banner")}
            </span>
          </div>
          <div style={{ fontSize: 13, marginBottom: 4 }}>
            {submitting
              ? t("benchmarks.profile_run.sending_request")
              : phaseLabel(progress, t) || progress?.phase}
          </div>
          {progress?.mode && (
            <div style={{ fontSize: 11, color: "var(--text-dim)", marginBottom: 2 }}>
              {t("benchmarks.profile_run.mode_label")} <span style={{ fontFamily: "var(--mono)" }}>{progress.mode}</span>
            </div>
          )}
          {(progress?.actual_n_ctx != null) && (
            <div style={{ fontSize: 11, color: "var(--text-dim)" }}>
              {t("benchmarks.profile_run.actual_n_ctx_label", { value: progress.actual_n_ctx })}
              {progress.target_n_ctx != null && progress.actual_n_ctx < progress.target_n_ctx
                ? t("benchmarks.profile_run.silently_reduced", { target: progress.target_n_ctx })
                : ""}
            </div>
          )}
          {(progress?.peak_bytes != null || progress?.safe_bytes != null) && (
            <div style={{ fontSize: 11, color: "var(--text-dim)" }}>
              {t("benchmarks.profile_run.peak_safe", { peak: progress.peak_bytes, safe: progress.safe_bytes })}
            </div>
          )}
          {(progress?.prefill_tps != null || progress?.decode_tps != null) && (
            <div style={{ fontSize: 11, color: "var(--text-dim)" }}>
              {t("benchmarks.profile_run.prefill_decode", { prefill: progress.prefill_tps?.toFixed(1), decode: progress.decode_tps?.toFixed(1) })}
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
                {t("benchmarks.profile_run.dismiss")}
              </button>
            </div>
          )}
          {polling && loadedSlots.length > 0 && progress?.phase === "evicting" && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 6 }}>
              {t("benchmarks.profile_run.evicting_label", { list: loadedSlots.map(([slot, mode]) => `${mode} (${slot})`).join(", ") })}
            </div>
          )}
          {polling && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 6 }}>
              {t("benchmarks.profile_run.slots_label", {
                list: Object.keys(slots).length === 0
                  ? t("benchmarks.profile_run.slots_loading")
                  : Object.entries(slots).map(([slot, mode]) =>
                    mode ? `${slot}=${mode}` : `${slot}=${t("benchmarks.profile_run.slot_empty")}`
                  ).join(", "),
              })}
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
            <h3>{t("benchmarks.profile_run.confirm_heading", { mode: confirmMode })}</h3>
            <div className="error-note" style={{ marginBottom: 14, background: "color-mix(in srgb, var(--warn) 12%, transparent)" }}>
              <b>{t("benchmarks.profile_run.warning_prefix")}</b>{" "}
              {targetAlreadyLoaded
                ? t("benchmarks.profile_run.already_loaded_reused", { mode: confirmMode })
                : <Trans i18nKey="benchmarks.profile_run.loads_alone" ns="settings" values={{ mode: confirmMode }} components={{ b: <b /> }} />}{" "}
              {toEvict.length > 0
                ? <Trans i18nKey="benchmarks.profile_run.will_unload" ns="settings" values={{ count: toEvict.length, total: Object.keys(slots).length }} components={{ b: <b /> }} />
                : t("benchmarks.profile_run.no_other_slots")}{t("benchmarks.profile_run.slots_evicted_note")}
            </div>
            {toEvict.length > 0 && (
              <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 10, lineHeight: 1.55 }}>
                {t("benchmarks.profile_run.will_be_evicted")}
                <ul style={{ margin: "4px 0 0 20px", padding: 0 }}>
                  {toEvict.map(([slot, mode]) => (
                    <li key={slot}><span style={{ fontFamily: "var(--mono)" }}>{mode}</span>{t("benchmarks.profile_run.slot_suffix", { slot })}</li>
                  ))}
                </ul>
              </div>
            )}
            <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 14, lineHeight: 1.55 }}>
              {t("benchmarks.profile_run.run_duration_note")}
            </div>
            <div className="form-actions">
              <button className="btn" disabled={submitting} onClick={() => setConfirmMode(null)}>{t("benchmarks.profile_run.cancel")}</button>
              <button
                className="btn primary"
                disabled={submitting}
                onClick={() => startProfile(confirmMode)}
              >
                {submitting ? t("benchmarks.profile_run.starting_ellipsis") : t("benchmarks.profile_run.evict_and_profile")}
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
