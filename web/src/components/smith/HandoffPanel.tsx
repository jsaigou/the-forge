import { useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import { useSmithActionApprove, useSmithActionHandoff } from "../../lib/queries";
import { useStepUpGate } from "../../lib/useStepUpGate";
import type { SmithAction, SmithHandoffCandidate } from "../../lib/types";
import { StepUpModal } from "../StepUpModal";
import { RunbookCard } from "./RunbookCard";

// HandoffPanel — Wave 3 / P2+P3 (track W3-C, peaceful-plotting-reef.md +
// the P3 remote-probe seam). Renders only when action.self_evicting &&
// action.handoff is set (ActionCard gates on that, never rendering a bare
// Approve button alongside this).
//
// Handoff FSM (docs/v5-smith.md §4.5): not_required | required
// -[runbook]-> runbook_issued -[acknowledge]-> acknowledged
// | required -[remote]-> remote_swapped
// | * -[cancel]-> (action rejected)
// "remote" and "runbook" are ALTERNATIVE branches off "required", not two
// steps of one flow — probing happens at proposal-creation time
// (handoff.candidates is real, not a placeholder, by the time this panel
// ever renders), so both options are offered together at "required".
// THE GATE: approval succeeds iff handoff.state is "acknowledged" OR
// "remote_swapped" — issuing the runbook alone does NOT unblock (the
// backend still 409s on "runbook_issued"), and picking a candidate is a
// full alternative to acknowledging the runbook, not an extra step on top
// of it.
export function HandoffPanel({ action }: { action: SmithAction }) {
  const handoff = action.handoff!;
  const handoffMut = useSmithActionHandoff(action.id);
  const approve = useSmithActionApprove(action.id);
  const gate = useStepUpGate();
  const [handoffError, setHandoffError] = useState<string | null>(null);
  const [approveError, setApproveError] = useState<string | null>(null);

  function resolve(resolution: "runbook" | "acknowledge" | "remote" | "cancel") {
    setHandoffError(null);
    handoffMut.mutate(resolution, { onError: (e) => setHandoffError(apiErrorMessage(e)) });
  }

  function handleApprove() {
    setApproveError(null);
    approve.mutate(undefined, {
      onError: (e) => {
        if (gate.handle(e, handleApprove)) return;
        setApproveError(apiErrorMessage(e));
      },
    });
  }

  // The backend always picks the first healthy candidate (handoff.go's
  // resolveHandoffRemote) — the FE doesn't offer a per-candidate choice,
  // just visibility into what's healthy and one button for the one that
  // would actually be used.
  const remoteCandidate: SmithHandoffCandidate | undefined = handoff.candidates.find((c) => c.healthy);
  const resolved = handoff.state === "acknowledged" || handoff.state === "remote_swapped";

  return (
    <div
      className="card handoff-panel"
      style={{
        marginTop: 10, padding: 12, borderLeft: "3px solid var(--warn)",
        background: "color-mix(in srgb, var(--warn) 8%, var(--panel))",
      }}
    >
      <div style={{ fontSize: 12, fontWeight: 600, color: "var(--warn)", marginBottom: 4 }}>
        Handoff required — approving this evicts smith's own brain
      </div>
      <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 8 }}>{handoff.reason}</div>
      <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 10, display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
        <span>brain slot: <span style={{ fontFamily: "var(--mono)" }}>{handoff.brain_slot || "—"}</span></span>
        <span>brain model: <span style={{ fontFamily: "var(--mono)" }}>{handoff.brain_model || "—"}</span></span>
        <span className="chip">{handoff.state.replace(/_/g, " ")}</span>
      </div>

      {handoffError && <div className="error-note" style={{ marginBottom: 8 }}>{handoffError}</div>}

      {handoff.state === "required" && (
        <div style={{ marginBottom: 10 }}>
          <button className="btn primary" disabled={handoffMut.isPending} onClick={() => resolve("runbook")}>
            {handoffMut.isPending ? "Issuing…" : "Get handoff runbook"}
          </button>

          {handoff.candidates.length > 0 && (
            <div style={{ marginTop: 10 }}>
              <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 6 }}>
                Or hand off to a remote brain for this operation:
              </div>
              <div style={{ display: "flex", flexDirection: "column", gap: 4, marginBottom: 8 }}>
                {handoff.candidates.map((c) => (
                  <div key={c.offering_id} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 11 }}>
                    <span
                      style={{
                        display: "inline-block", width: 7, height: 7, borderRadius: "50%", flex: "0 0 auto",
                        background: c.healthy ? "var(--ok)" : "var(--text-mute)",
                      }}
                    />
                    <span style={{ fontFamily: "var(--mono)", color: "var(--text-dim)" }}>{c.model}</span>
                    <span style={{ color: "var(--text-mute)" }}>via {c.provider}</span>
                    {!c.healthy && <span style={{ color: "var(--text-mute)" }}>(unreachable)</span>}
                  </div>
                ))}
              </div>
              {remoteCandidate ? (
                <button className="btn" disabled={handoffMut.isPending} onClick={() => resolve("remote")}>
                  {handoffMut.isPending ? "Switching…" : `Switch brain to ${remoteCandidate.model}`}
                </button>
              ) : (
                <div style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
                  No candidate is currently reachable — use the runbook path instead.
                </div>
              )}
            </div>
          )}
        </div>
      )}

      {(handoff.state === "runbook_issued" || handoff.state === "acknowledged") && handoff.runbook.length > 0 && (
        <div style={{ marginBottom: 10 }}>
          <RunbookCard steps={handoff.runbook} storageKey={`${action.id}-handoff`} />
        </div>
      )}

      {handoff.state === "runbook_issued" && (
        <button className="btn primary" disabled={handoffMut.isPending} onClick={() => resolve("acknowledge")}>
          {handoffMut.isPending ? "…" : "Acknowledge — I've read this"}
        </button>
      )}

      {resolved && (
        <div style={{ marginTop: 4 }}>
          <div style={{ fontSize: 11.5, color: "var(--ok)", marginBottom: 8 }}>
            {handoff.state === "remote_swapped" ? "Brain swapped to a remote candidate" : "Handoff acknowledged"}
            {handoff.acknowledged_by ? ` by ${handoff.acknowledged_by}` : ""}
            {handoff.acknowledged_at ? ` at ${new Date(handoff.acknowledged_at * 1000).toLocaleString()}` : ""}. Approval can now proceed.
            {handoff.state === "remote_swapped" && " A swap-back proposal will appear once this operation finishes."}
          </div>
          {approveError && <div className="error-note" style={{ marginBottom: 8 }}>{approveError}</div>}
          <button className="btn primary" disabled={approve.isPending} onClick={handleApprove}>
            {approve.isPending ? "Approving…" : "Approve"}
          </button>
        </div>
      )}

      <div style={{ marginTop: 10 }}>
        <button className="btn" disabled={handoffMut.isPending} onClick={() => resolve("cancel")} style={{ fontSize: 11 }}>
          Cancel this action
        </button>
      </div>

      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </div>
  );
}
