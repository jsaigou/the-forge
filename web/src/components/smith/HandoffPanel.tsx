import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../../lib/api";
import { appLocale } from "../../lib/format";
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
  const { t } = useTranslation("common");
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
        {t("handoff_panel.title")}
      </div>
      <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 8 }}>{handoff.reason}</div>
      <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 10, display: "flex", gap: 10, flexWrap: "wrap", alignItems: "center" }}>
        <span>{t("handoff_panel.brain_slot_label")} <span style={{ fontFamily: "var(--mono)" }}>{handoff.brain_slot || "—"}</span></span>
        <span>{t("handoff_panel.brain_model_label")} <span style={{ fontFamily: "var(--mono)" }}>{handoff.brain_model || "—"}</span></span>
        <span className="chip">{t(`handoff_panel.state_chip.${handoff.state}`, { defaultValue: handoff.state.replace(/_/g, " ") })}</span>
      </div>

      {handoffError && <div className="error-note" style={{ marginBottom: 8 }}>{handoffError}</div>}

      {handoff.state === "required" && (
        <div style={{ marginBottom: 10 }}>
          <button className="btn primary" disabled={handoffMut.isPending} onClick={() => resolve("runbook")}>
            {handoffMut.isPending ? t("handoff_panel.issuing") : t("handoff_panel.get_runbook_button")}
          </button>

          {handoff.candidates.length > 0 && (
            <div style={{ marginTop: 10 }}>
              <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 6 }}>
                {t("handoff_panel.remote_prompt")}
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
                    <span style={{ color: "var(--text-mute)" }}>{t("handoff_panel.via_prefix")}{c.provider}</span>
                    {!c.healthy && <span style={{ color: "var(--text-mute)" }}>{t("handoff_panel.unreachable")}</span>}
                  </div>
                ))}
              </div>
              {remoteCandidate ? (
                <button className="btn" disabled={handoffMut.isPending} onClick={() => resolve("remote")}>
                  {handoffMut.isPending ? t("handoff_panel.switching") : t("handoff_panel.switch_brain_button", { model: remoteCandidate.model })}
                </button>
              ) : (
                <div style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
                  {t("handoff_panel.no_candidate_reachable")}
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
          {handoffMut.isPending ? t("handoff_panel.acknowledging") : t("handoff_panel.acknowledge_button")}
        </button>
      )}

      {resolved && (
        <div style={{ marginTop: 4 }}>
          <div style={{ fontSize: 11.5, color: "var(--ok)", marginBottom: 8 }}>
            {handoff.state === "remote_swapped" ? t("handoff_panel.resolved_remote") : t("handoff_panel.resolved_ack")}
            {handoff.acknowledged_by ? t("handoff_panel.by_prefix") + handoff.acknowledged_by : ""}
            {handoff.acknowledged_at ? t("handoff_panel.at_prefix") + new Date(handoff.acknowledged_at * 1000).toLocaleString(appLocale()) : ""}{t("handoff_panel.approval_can_proceed")}
            {handoff.state === "remote_swapped" && t("handoff_panel.swap_back_notice")}
          </div>
          {approveError && <div className="error-note" style={{ marginBottom: 8 }}>{approveError}</div>}
          <button className="btn primary" disabled={approve.isPending} onClick={handleApprove}>
            {approve.isPending ? t("handoff_panel.approving") : t("action_card.approve")}
          </button>
        </div>
      )}

      <div style={{ marginTop: 10 }}>
        <button className="btn" disabled={handoffMut.isPending} onClick={() => resolve("cancel")} style={{ fontSize: 11 }}>
          {t("handoff_panel.cancel_button")}
        </button>
      </div>

      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </div>
  );
}
