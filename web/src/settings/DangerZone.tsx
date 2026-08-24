import { useEffect, useRef, useState, type ReactNode } from "react";
import { ApiError, apiErrorMessage } from "../lib/api";
import { ConfirmButton } from "../components/ConfirmButton";
import { StepUpModal } from "../components/StepUpModal";
import { useStepUpGate } from "../lib/useStepUpGate";
import type { PreflightCheck, PreflightResult } from "../lib/types";
import { SAVE_FLASH_MS, noteSaveSuccess } from "./useSettingsGroup";

// settings/DangerZone.tsx — Sprint 12 (was H) Phase 7. The reusable
// arm→(step-up)→check→apply state machine for boot-critical settings, per
// the sprint plan's "Danger Zone" section. Two real call sites use this:
// the standalone "danger" section (infra.server/paths/ports/tailscale, a
// real POST /system/preflight dry-run endpoint) and an embedded instance in
// Security for the two forward-auth keys (no separate preflight endpoint —
// the CIDR lockout check is inline in PUT /auth/config's own 422, so
// `onCheck` is omitted there and a failing Save itself produces the
// checklist).
//
// Deliberately NOT generic over the field-rendering: the caller supplies
// `renderFields`, using its own <Field> + fields.ts records — this
// component only owns the collapsed/armed/check/apply/relock mechanics and
// chrome (.danger-zone, ConfirmButton's arm/disarm idiom for Unlock,
// useStepUpGate, the checklist). See risk 1 in the plan: the 700ms
// SaveButton flash must survive a relock/section-nav/search-nav unmount —
// this component owns its own "saved" pause before collapsing, same
// rationale as useSettingsGroup's delayed close elsewhere in Settings.
//
// No "save anyway" override on a failed checklist — see the plan's Danger
// Zone section: that would reintroduce exactly the failure mode the gate
// exists to eliminate. The failure copy instead names the real escape
// hatch (`forge config set`).

const IDLE_RELOCK_MS = 5 * 60 * 1000;

type Phase = "collapsed" | "armed" | "preflighting" | "checked-ok" | "checked-failed" | "saving" | "saved";

// preflightFromError extracts the {checks} half of a 422
// {error:"preflight_failed", fields, checks} response — the same shape
// POST /system/preflight's 200 body and a failing PUT's 422 body both
// carry, so both the explicit-Check path and the Save-is-the-check path
// (no onCheck) render through the identical checklist UI.
function preflightFromError(e: unknown): PreflightResult | null {
  if (e instanceof ApiError && e.body && typeof e.body === "object") {
    const body = e.body as { error?: string; checks?: PreflightCheck[] };
    if (body.error === "preflight_failed" && Array.isArray(body.checks)) {
      return { ok: false, checks: body.checks };
    }
  }
  return null;
}

function Checklist({ checks }: { checks: PreflightCheck[] }) {
  return (
    <div style={{ marginTop: 10, marginBottom: 10 }}>
      {checks.map((c, i) => (
        <div className={`preflight-check ${c.level}`} key={`${c.field}-${i}`}>
          <span className="lvl">{c.level === "ok" ? "✓" : c.level === "warn" ? "!" : "✕"}</span>
          <span className="field">{c.field}</span>
          <span className="msg">{c.message}</span>
        </div>
      ))}
    </div>
  );
}

export function DangerZone<T>({
  title,
  blurb,
  data,
  isError,
  canAdmin,
  renderFields,
  onCheck,
  onSave,
  extraActions,
}: {
  title: string;
  blurb: ReactNode;
  data: T | undefined;
  isError?: boolean;
  canAdmin: boolean;
  renderFields: (active: T, setField: <K extends keyof T>(key: K, value: T[K]) => void, disabled: boolean) => ReactNode;
  /** Present only for groups with a real dry-run endpoint (system settings). Absent groups check on Save itself. */
  onCheck?: (draft: T) => Promise<PreflightResult>;
  onSave: (draft: T) => Promise<unknown>;
  extraActions?: ReactNode;
}) {
  const [phase, setPhase] = useState<Phase>("collapsed");
  const [draft, setDraft] = useState<T | null>(null);
  const [checks, setChecks] = useState<PreflightCheck[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const gate = useStepUpGate();
  const lastActivity = useRef(Date.now());

  const active = draft ?? data;
  const armed = phase !== "collapsed";

  function touch() {
    lastActivity.current = Date.now();
  }

  function collapse() {
    setPhase("collapsed");
    setDraft(null);
    setChecks(null);
    setError(null);
  }

  // 5-minute idle auto-relock — tighter than the step-up TTL, so this is
  // the binding guard, not the assurance token's own expiry.
  useEffect(() => {
    if (!armed) return;
    const id = setInterval(() => {
      if (phase !== "saving" && Date.now() - lastActivity.current > IDLE_RELOCK_MS) collapse();
    }, 10_000);
    return () => clearInterval(id);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [armed, phase]);

  function unlock() {
    if (!data) return;
    touch();
    setDraft(data);
    setPhase("armed");
  }

  function setField<K extends keyof T>(key: K, value: T[K]) {
    if (!active) return;
    touch();
    setDraft({ ...active, [key]: value });
    // Editing after a check invalidates it — never apply a stale checklist
    // against a draft it didn't actually see.
    if (phase === "checked-ok" || phase === "checked-failed") {
      setPhase("armed");
      setChecks(null);
    }
  }

  function runCheck() {
    if (!draft || !onCheck) return;
    touch();
    setError(null);
    const attempt = () => {
      setPhase("preflighting");
      onCheck(draft)
        .then((res) => {
          setChecks(res.checks);
          setPhase(res.ok ? "checked-ok" : "checked-failed");
        })
        .catch((e) => {
          setPhase("armed");
          if (!gate.handle(e, attempt)) setError(apiErrorMessage(e));
        });
    };
    attempt();
  }

  function save() {
    if (!draft) return;
    touch();
    setError(null);
    const attempt = () => {
      setPhase("saving");
      onSave(draft)
        .then(() => {
          noteSaveSuccess();
          setPhase("saved");
          setChecks(null);
          // Same 700ms-flash rationale as useSettingsGroup — give the
          // operator a moment to see "saved" before the form collapses out
          // from under them.
          setTimeout(collapse, SAVE_FLASH_MS);
        })
        .catch((e) => {
          // A fresh 422 here is the TOCTOU case (something changed between
          // Check and Apply) or, for groups with no onCheck, the ONLY check
          // that ever runs. Either way: draft is preserved, never save
          // partially, never offer to bypass.
          const preflight = preflightFromError(e);
          if (preflight) {
            setChecks(preflight.checks);
            setPhase("checked-failed");
            return;
          }
          setPhase(onCheck ? "checked-ok" : "armed");
          if (!gate.handle(e, attempt)) setError(apiErrorMessage(e));
        });
    };
    attempt();
  }

  if (isError) {
    return (
      <>
        <div className="eyebrow">{title}</div>
        <div className="card"><div className="empty-note">Admin role required to view this section.</div></div>
      </>
    );
  }
  if (!active) {
    return (
      <>
        <div className="eyebrow">{title}</div>
        <div className="card"><div className="empty-note">Loading…</div></div>
      </>
    );
  }

  // A function, not a `const` derived from `phase` comparisons — TS's
  // aliased-condition narrowing otherwise treats this boolean's later
  // negation as proof `phase` can't be "saving", which isn't true (the
  // button's own `disabled` expression checks both independently).
  function canApply(): boolean {
    return phase === "checked-ok" || (phase === "armed" && !onCheck);
  }

  return (
    <>
      <div className="eyebrow">{title}</div>
      <div className="danger-zone">
        <div className="dz-head">
          <span className="dz-title">{armed ? "Unlocked — editing enabled" : "Locked"}</span>
          {canAdmin && (
            armed ? (
              <button className="btn" onClick={collapse}>Lock</button>
            ) : (
              <ConfirmButton
                label="Unlock"
                confirmLabel="Confirm?"
                warning="This is boot-critical infrastructure config — a bad save can take the daemon down."
                onConfirm={unlock}
              />
            )
          )}
        </div>
        <div className="dz-blurb">{blurb}</div>
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <fieldset disabled={!armed || phase === "saving"}>
          {renderFields(active, setField, !armed || phase === "saving")}
        </fieldset>
        {checks && checks.length > 0 && <Checklist checks={checks} />}
        {phase === "checked-failed" && (
          <div className="error-note" style={{ marginBottom: 12 }}>
            Nothing was written. Fix the failing field(s) above and re-check, or use{" "}
            <code>forge config set</code> directly if you need to bypass validation entirely.
          </div>
        )}
        {armed && canAdmin && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            {onCheck && (
              <button className="btn" disabled={phase === "preflighting" || phase === "saving"} onClick={runCheck}>
                {phase === "preflighting" ? "Checking…" : "Check"}
              </button>
            )}
            <button className="btn primary" disabled={!canApply() || phase === "saving"} onClick={save}>
              {phase === "saving" ? "Applying…" : phase === "saved" ? "✓ Applied" : "Apply"}
            </button>
          </div>
        )}
        {extraActions}
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}
