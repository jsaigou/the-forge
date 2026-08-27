import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { qk } from "./queries";
import type { HFDownloadDeletedEvent, HFDownloadDoneEvent, HFDownloadFailedEvent, HFDownloadProgressEvent, HFDownloadStateChangedEvent, ProfileDoneEvent, ProfileFailedEvent, ProfileProgressEvent, SmithActionUpdateEvent, SmithMessageDoneEvent, SmithProcedureStepEvent, SmithStatusEvent, SmithTierChangedEvent, SmithToolActivityEvent, SmithTokenEvent, Status } from "./types";

// Single app-wide EventSource against the existing SSE bus (foundry/app.py
// _push_event). status_update carries the full status payload already, so we
// write it straight into the Query cache instead of refetching. Every other
// event type just invalidates the queries it can affect — cheap, and avoids
// needing new backend slot:*/queue:*/usage:*/service:* event types for the
// MVP (see docs/rewrite-plan.md Phase 7 plan notes). slot:activity (Sprint
// K, 2026-08-05) is the one exception: it's a genuinely low-latency signal
// (a busy↔idle edge, pushed within ~2s of the collector noticing) that a
// 15s poll or 30s SSE heartbeat can't approximate, so it merges directly
// into the cached Status rather than triggering a refetch.
export function useLiveEvents(): void {
  const qc = useQueryClient();

  useEffect(() => {
    const source = new EventSource("/api/v1/events", { withCredentials: true });

    source.addEventListener("status_update", (ev) => {
      try {
        const status = JSON.parse((ev as MessageEvent).data) as Status;
        // Sprint K (2026-08-05): defense-in-depth against a real bug where
        // an infra-service start/stop published a bare {action, unit}
        // status_update instead of a full Status — this unconditional
        // setQueryData blanked slots/services/slot_labels for every
        // connected client until the next 15s poll. Fixed at the source
        // (runUnitOp now reuses probeAndPush), but this guard stays: any
        // status_update payload missing `slots` is not a full Status and
        // must never overwrite a good cached one.
        if (status.slots != null) {
          qc.setQueryData(qk.status, status);
        }
      } catch {
        // malformed payload — ignore, next poll will recover
      }
      // Phase 0 Fix D: do NOT invalidate qk.schedulerStatus here. The SSE
      // status_update payload already carries the status dict (above), and
      // schedulerStatus is covered by its own 10s poll plus the explicit
      // load_*/unload_*/switch_* invalidations below. Invalidating on every
      // status_update caused /api/v1/scheduler/status to refetch on top of
      // its 10s poll, which (before Fix A) ran uncached get_metrics() — i.e.
      // 4× rocm-smi + a systemctl sweep per SSE push.
    });

    // Sprint K (2026-08-05): merge, never replace — a status_update that
    // lands slightly before or after this event must not clobber it (or
    // vice versa). No-ops if status hasn't been fetched yet (nothing to
    // merge into; the next status_update/poll will carry the current value
    // anyway, since backend slot_activity is poll-computed fresh each time,
    // not itself edge-latched).
    source.addEventListener("slot:activity", (ev) => {
      try {
        const { slot, active } = JSON.parse((ev as MessageEvent).data) as { slot: string; active: boolean };
        qc.setQueryData<Status>(qk.status, (prev) =>
          prev ? { ...prev, slot_activity: { ...prev.slot_activity, [slot]: active } } : prev
        );
      } catch {
        // malformed payload — ignore, next poll will recover
      }
    });

    const invalidate = (keys: readonly (readonly unknown[])[]) => () => {
      for (const k of keys) qc.invalidateQueries({ queryKey: k as unknown[] });
    };

    source.addEventListener("switch_started", invalidate([qk.status, qk.schedulerStatus]));
    // Cost/savings sprint Phase 5: the Dashboard's usage window is now
    // user-selectable, so invalidating the literal "7d" key would miss any
    // other window the operator has selected. ["usage"] is a prefix match
    // (react-query invalidates by key-prefix) and also covers qk.usageEvents.
    source.addEventListener("switch_complete", invalidate([qk.status, qk.schedulerStatus, ["usage"]]));
    source.addEventListener("switch_failed", invalidate([qk.status, qk.schedulerStatus]));
    source.addEventListener("load_started", invalidate([qk.status, qk.schedulerStatus]));
    source.addEventListener("load_complete", invalidate([qk.status, qk.schedulerStatus]));
    source.addEventListener("load_failed", invalidate([qk.status, qk.schedulerStatus]));
    source.addEventListener("unload_complete", invalidate([qk.status, qk.schedulerStatus]));
    source.addEventListener("config_updated", invalidate([qk.reservations, qk.schedulerConfig, qk.status]));
    source.addEventListener("registry:refreshed", invalidate([qk.modelCards("7d"), qk.configCards("7d")]));
    // product/QA sprint, 2026-07-29 — a fresh notification landed; refetch
    // both the default (active-only) and include_dismissed views.
    source.addEventListener("notification:new", invalidate([qk.notifications(false), qk.notifications(true)]));

    // Wave 3 / P2 (track W3-C) — smith action model + handoff. Both are
    // Pattern B: the row shape is too varied (detail/handoff/result) to
    // hand-maintain a cache write for, so every change just invalidates and
    // lets the next render refetch. ["smith","actions"] is a prefix match
    // covering every useSmithActions(status, investigationId) variant.
    // pending_count (on action_update) can move the Console/Diagnostics
    // pending chip, so qk.smith.status is invalidated too.
    //
    // Sprint S3-Web: action_update also carries action_id, so we invalidate
    // the singular qk.smith.action(id) — a transcript ActionCard resolving
    // live state via GET /actions/{id} refetches without a manual poll.
    source.addEventListener("smith:action_update", (ev) => {
      try {
        const { action_id } = JSON.parse((ev as MessageEvent).data) as SmithActionUpdateEvent;
        qc.invalidateQueries({ queryKey: qk.smith.action(action_id) });
      } catch {
        // malformed payload — still invalidate the list + status below
      }
      invalidate([["smith", "actions"], qk.smith.status])();
    });
    source.addEventListener("smith:handoff_update", invalidate([["smith", "actions"], qk.smith.status]));

    // Autonomous-remediation Sprint 2/3 — the procedure engine's run state
    // (step completed, paused at a checkpoint, resumed, finished). Emitted
    // by the backend since Sprint 2; this is the first FE listener for it.
    // Invalidates the run's own live query plus the singular action (its
    // status can move too — e.g. checkpoint pause holds "executing"), plus
    // (Sprint 4) the run-history list and this run's scorecard, since both
    // are derived from the same row and go stale on every step/status move.
    source.addEventListener("smith:procedure_step", (ev) => {
      try {
        const { action_id } = JSON.parse((ev as MessageEvent).data) as SmithProcedureStepEvent;
        qc.invalidateQueries({ queryKey: qk.smith.procedureRun(action_id) });
        qc.invalidateQueries({ queryKey: qk.smith.action(action_id) });
        qc.invalidateQueries({ queryKey: qk.smith.procedureRuns });
        qc.invalidateQueries({ queryKey: qk.smith.procedureScorecard(action_id) });
      } catch {
        // malformed payload — nothing to recover here, the next poll/refetch
        // will still pick up the real state
      }
    });

    // Maintenance mode (autonomous-remediation plan, Sprint 1) — entered,
    // exited, or TTL-expired all publish the same event; the client just
    // refetches current state rather than trusting the push payload.
    source.addEventListener("maintenance:changed", invalidate([qk.maintenance]));

    // P3 — reasoning tier. smith:token is the one high-frequency event in
    // this app: batched server-side (~120ms) but still far too often for
    // Pattern B's invalidate-and-refetch, and there's no server-side
    // "current message" query to merge into the way slot:activity merges
    // into Status. Pattern A-variant instead: append into a client-only key
    // (qk.smith.streaming) nothing ever fetches from the server — the chat
    // component reads it directly, and smith:message_done clears it once
    // the persisted row (fetched via the conversation invalidation below)
    // is authoritative, so a dropped token batch is cosmetic, never lost
    // history.
    source.addEventListener("smith:token", (ev) => {
      try {
        const { message_id, delta } = JSON.parse((ev as MessageEvent).data) as SmithTokenEvent;
        qc.setQueryData<string>(qk.smith.streaming(message_id), (prev) => (prev ?? "") + delta);
      } catch {
        // malformed payload — ignore, smith:message_done still reconciles
      }
    });
    // P7 — one tool round's liveness signal, same Pattern-A-variant as
    // smith:token above (client-only append, no server query backs it).
    // "started" fires before the tool runs — the round gate withholds a
    // fenced-mode round's content from smith:token until it's known to be
    // prose, so without this a slow run_check would look like a hang.
    source.addEventListener("smith:tool_call", (ev) => {
      try {
        const evt = JSON.parse((ev as MessageEvent).data) as SmithToolActivityEvent;
        qc.setQueryData<SmithToolActivityEvent[]>(qk.smith.toolActivity(evt.message_id), (prev) => [...(prev ?? []), evt]);
      } catch {
        // malformed payload — cosmetic, the persisted tool_call message
        // (fetched via the conversation invalidation on message_done) is
        // authoritative regardless
      }
    });
    // S4 phase 2 — the backend already composed a human sentence about turn
    // progress (e.g. "loading brain model — first load typically takes
    // 20-90s"); this event previously reached the browser with no listener
    // at all. Same client-only cache-slot pattern as smith:token above.
    source.addEventListener("smith:status", (ev) => {
      try {
        const { message_id, status } = JSON.parse((ev as MessageEvent).data) as SmithStatusEvent;
        qc.setQueryData<string>(qk.smith.turnStatus(message_id), status);
      } catch {
        // malformed payload — cosmetic, the turn proceeds regardless
      }
    });
    source.addEventListener("smith:message_done", (ev) => {
      try {
        const { conversation_id, message_id } = JSON.parse((ev as MessageEvent).data) as SmithMessageDoneEvent;
        qc.removeQueries({ queryKey: qk.smith.streaming(message_id) });
        qc.removeQueries({ queryKey: qk.smith.toolActivity(message_id) });
        qc.removeQueries({ queryKey: qk.smith.turnStatus(message_id) });
        qc.invalidateQueries({ queryKey: qk.smith.conversation(conversation_id) });
      } catch {
        // malformed payload — the conversation just won't refresh until the
        // next explicit fetch; nothing to recover here
      }
      qc.invalidateQueries({ queryKey: qk.smith.conversations });
    });
    source.addEventListener("smith:tier_changed", (ev) => {
      try {
        const { conversation_id } = JSON.parse((ev as MessageEvent).data) as SmithTierChangedEvent;
        qc.invalidateQueries({ queryKey: qk.smith.conversation(conversation_id) });
      } catch {
        // malformed payload — ignore
      }
      qc.invalidateQueries({ queryKey: qk.smith.status });
    });

    // PROFILE track: write progress straight into the cache (Pattern A —
    // like status_update), so the progress view re-renders without an HTTP
    // round-trip. profile:done/failed invalidate the profile list + cards
    // so the measured values appear.
    source.addEventListener("profile:started", () => {
      qc.setQueryData(qk.profileProgress, { phase: "evicting", running: true });
      // Invalidate status so the Dashboard/Console refetch from the
      // freshly-probed snapshot (the backend triggers an early collector
      // poll at each phase transition).
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    });
    source.addEventListener("profile:progress", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as ProfileProgressEvent;
        qc.setQueryData(qk.profileProgress, { ...data, running: true });
      } catch { /* malformed — ignore */ }
      // Invalidate status so the Dashboard/Console reflect slot state
      // changes (evictions, loads) in real time during the run.
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    });
    source.addEventListener("profile:done", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as ProfileDoneEvent;
        // ProfileDoneEvent is {result: ProfileResult} — mode lives one level
        // down, not at the top. Pull it up so the "mode: xxx" line and the
        // topbar throbber tooltip don't blank out the instant this lands.
        qc.setQueryData(qk.profileProgress, { ...data, phase: "done", running: false, mode: data.result?.mode });
      } catch { /* malformed — ignore */ }
      qc.invalidateQueries({ queryKey: qk.profiles });
      qc.invalidateQueries({ queryKey: qk.modelCards("7d") });
      qc.invalidateQueries({ queryKey: qk.configCards("7d") });
      qc.invalidateQueries({ queryKey: qk.status });
      qc.invalidateQueries({ queryKey: qk.schedulerStatus });
    });
    source.addEventListener("profile:failed", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as ProfileFailedEvent;
        qc.setQueryData(qk.profileProgress, { ...data, phase: "failed", running: false });
      } catch { /* malformed — ignore */ }
      qc.invalidateQueries({ queryKey: qk.profiles });
      qc.invalidateQueries({ queryKey: qk.status });
    });

    // HF model acquisition (Contract 1 amendment, go/internal/hfdownload/
    // events.go). download:progress is high-frequency and decoration-only
    // (see useHFDownloadProgress's doc comment) — write straight into the
    // per-job client-only slot, no invalidation (that would defeat the
    // point of a smooth byte-level counter by forcing a network refetch on
    // every ~1s sample). The other three are real state transitions, so
    // they invalidate the poll instead of trying to merge — same
    // Pattern B this file uses everywhere else — so a job flipping to
    // done/failed/paused/etc. is reflected without waiting a full 3s.
    source.addEventListener("download:progress", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as HFDownloadProgressEvent;
        qc.setQueryData(qk.hfDownloadProgress(data.job_id), data);
      } catch { /* malformed — ignore */ }
    });
    source.addEventListener("download:state_changed", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as HFDownloadStateChangedEvent;
        qc.invalidateQueries({ queryKey: qk.hfDownload(data.job_id) });
      } catch { /* malformed — ignore */ }
      qc.invalidateQueries({ queryKey: qk.hfDownloads });
    });
    source.addEventListener("download:done", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as HFDownloadDoneEvent;
        qc.invalidateQueries({ queryKey: qk.hfDownload(data.job_id) });
      } catch { /* malformed — ignore */ }
      qc.invalidateQueries({ queryKey: qk.hfDownloads });
      // A newly-registered model needs to show up in the catalog surfaces.
      qc.invalidateQueries({ queryKey: ["catalog"] });
      qc.invalidateQueries({ queryKey: qk.modelCards("7d") });
      qc.invalidateQueries({ queryKey: qk.configCards("7d") });
    });
    source.addEventListener("download:failed", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as HFDownloadFailedEvent;
        qc.invalidateQueries({ queryKey: qk.hfDownload(data.job_id) });
      } catch { /* malformed — ignore */ }
      qc.invalidateQueries({ queryKey: qk.hfDownloads });
    });
    source.addEventListener("download:deleted", (ev) => {
      try {
        const data = JSON.parse((ev as MessageEvent).data) as HFDownloadDeletedEvent;
        qc.removeQueries({ queryKey: qk.hfDownload(data.job_id) });
      } catch { /* malformed — ignore */ }
      qc.invalidateQueries({ queryKey: qk.hfDownloads });
    });

    return () => source.close();
  }, [qc]);
}
