import { useEffect, useState } from "react";
import type { SmithRunbookStep } from "../../lib/types";
import { CopyButton } from "../CopyButton";

// RunbookCard — a runbook never executes (docs/v5-smith.md §4.6); this is
// purely a copy-and-track surface for the operator. P7 polish: a Copy all
// button for the whole recipe, per-step done checkboxes (persisted in
// localStorage when storageKey is given — e.g. the action ID — so progress
// survives a collapse/reload of the page), why/verify_command rendered
// alongside title/command/verify.
//
// Sprint S3-Web (§2.4.3): an optional whyCantRun line at the top states in
// one sentence WHY smith can't run this itself ("needs a reboot — that's
// yours"). The runbook kind is the one action kind that NEVER executes
// (ApproveAction short-circuits it straight to done_unverified server-side),
// so this line replaces the old generic "informational only" notice with a
// specific, honest reason.

function doneKey(storageKey: string): string {
  return `smith-runbook-done:${storageKey}`;
}

function loadDone(storageKey?: string): Set<number> {
  if (!storageKey) return new Set();
  try {
    const raw = localStorage.getItem(doneKey(storageKey));
    return raw ? new Set(JSON.parse(raw) as number[]) : new Set();
  } catch {
    return new Set();
  }
}

function saveDone(storageKey: string, done: Set<number>) {
  try {
    localStorage.setItem(doneKey(storageKey), JSON.stringify([...done]));
  } catch {
    // localStorage unavailable (private browsing, quota) — progress just
    // won't survive a reload; the checkboxes still work for this session
  }
}

// runbookAsShellScript renders the whole recipe as a commented shell
// snippet for "Copy all" — steps with no command (a purely informational
// step) become a comment-only line so the numbering still lines up.
function runbookAsShellScript(steps: SmithRunbookStep[]): string {
  return steps
    .map((s, i) => {
      const lines = [`# ${i + 1}. ${s.title}`];
      if (s.why) lines.push(`# why: ${s.why}`);
      lines.push(s.command || "# (no command — manual/informational step)");
      return lines.join("\n");
    })
    .join("\n\n");
}

export function RunbookCard({
  steps,
  storageKey,
  whyCantRun,
}: {
  steps: SmithRunbookStep[];
  storageKey?: string;
  whyCantRun?: string;
}) {
  const [done, setDone] = useState<Set<number>>(() => loadDone(storageKey));

  useEffect(() => {
    setDone(loadDone(storageKey));
  }, [storageKey]);

  function toggle(i: number) {
    const next = new Set(done);
    if (next.has(i)) next.delete(i);
    else next.add(i);
    setDone(next);
    if (storageKey) saveDone(storageKey, next);
  }

  if (steps.length === 0) return <div className="empty-note">No runbook steps.</div>;

  return (
    <div className="runbook">
      {whyCantRun && (
        <div className="runbook-why-cant-run" style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 6 }}>
          why smith can't run this: {whyCantRun}
        </div>
      )}
      <div className="runbook-header">
        <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
          {done.size} of {steps.length} done
        </span>
        <CopyButton text={runbookAsShellScript(steps)} title="Copy all steps as a shell script" label="Copy all" />
      </div>
      <ol className="runbook-list">
        {steps.map((step, i) => (
          <li key={`${step.title ?? ""}-${i}`} className={`runbook-step ${done.has(i) ? "done" : ""}`}>
            <label className="runbook-step-check">
              <input type="checkbox" checked={done.has(i)} onChange={() => toggle(i)} />
            </label>
            <div className="runbook-step-body">
              <div className="runbook-step-title">{step.title}</div>
              {step.why && <div className="runbook-step-why">{step.why}</div>}
              {step.command && (
                <div className="runbook-step-cmd">
                  <code>{step.command}</code>
                  <CopyButton text={step.command} title="Copy command" sm />
                </div>
              )}
              {step.verify && (
                <div className="runbook-step-verify">
                  verify: {step.verify}
                  {step.verify_command && (
                    <div className="runbook-step-cmd">
                      <code>{step.verify_command}</code>
                      <CopyButton text={step.verify_command} title="Copy verify command" sm />
                    </div>
                  )}
                </div>
              )}
            </div>
          </li>
        ))}
      </ol>
    </div>
  );
}
