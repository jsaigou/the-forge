// "Profile tonight" (docs: go/internal/profile/night.go): one overnight window, 23:00–06:00 on the server's clock, that measures every config
// without a current measurement, shortest first, then loads back what was running.
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../../lib/api";
import { appLocale } from "../../lib/format";
import { useProfileNight, useProfileNightArm, useProfileNightCancel } from "../../lib/queries";
import { useStepUpGate } from "../../lib/useStepUpGate";
import { Modal } from "../Modal";
import { StepUpModal } from "../StepUpModal";

export function ProfileNightCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const night = useProfileNight();
  const arm = useProfileNightArm();
  const cancel = useProfileNightCancel();
  const gate = useStepUpGate();
  const [confirming, setConfirming] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const d = night.data;
  if (night.isError) return <div className="error-note" style={{ marginBottom: 12 }}>{apiErrorMessage(night.error)}</div>;
  if (!d) return null;

  const when = (s: number) => new Date(s * 1000).toLocaleString(appLocale(), { weekday: "short", hour: "2-digit", minute: "2-digit", hour12: false });
  const minutes = (s: number) => Math.max(1, Math.round(s / 60));
  const total = d.queue.reduce((sum, q) => sum + q.estimate_seconds, 0);
  const available = d.window.ends_at - d.window.starts_at;
  const results = d.state.results ?? [];
  const count = (o: string) => results.filter((r) => r.outcome === o).length;
  const busy = arm.isPending || cancel.isPending;

  function run(action: typeof arm | typeof cancel) {
    setError(null);
    action.mutate(undefined, {
      onSuccess: () => setConfirming(false),
      onError: (e) => {
        if (gate.handle(e, () => run(action))) return;
        setConfirming(false);
        setError(apiErrorMessage(e));
      },
    });
  }

  return (
    <div className="card" id="profile-night" style={{ marginBottom: 12 }}>
      <div style={{ display: "flex", gap: 12, alignItems: "center", flexWrap: "wrap" }}>
        <div style={{ flex: 1, minWidth: 220, fontSize: 12.5 }}>
          {d.state.status === "idle" && (
            <span>{d.queue.length === 0 ? t("benchmarks.night.nothing") : t("benchmarks.night.idle", { count: d.queue.length, minutes: minutes(total), when: when(d.window.starts_at) })}</span>
          )}
          {d.state.status === "armed" && <span>{t("benchmarks.night.armed", { when: when(d.state.starts_at ?? 0), until: when(d.state.ends_at ?? 0), count: d.queue.length })}</span>}
          {d.state.status === "running" && (
            <span>
              {d.state.current ? t("benchmarks.night.running", { mode: d.state.current }) : t(`benchmarks.night.waiting.${d.state.waiting ?? "starting"}`)}
              {" · "}{t("benchmarks.night.progress", { done: results.length, left: d.queue.length, until: when(d.state.ends_at ?? 0) })}
            </span>
          )}
        </div>
        {canAdmin && d.state.status === "idle" && (
          <button type="button" className="btn primary" disabled={d.queue.length === 0 || busy} onClick={() => setConfirming(true)}>
            {t("benchmarks.night.button")}
          </button>
        )}
        {canAdmin && d.state.status !== "idle" && (
          <button type="button" className="btn" disabled={busy} onClick={() => run(cancel)}>
            {d.state.status === "armed" ? t("benchmarks.night.cancel") : t("benchmarks.night.stop")}
          </button>
        )}
      </div>

      {(results.length > 0 || (d.state.restore_failed?.length ?? 0) > 0) && (
        <details style={{ marginTop: 8, fontSize: 12 }}>
          <summary>
            {t("benchmarks.night.last", { profiled: count("profiled"), failed: count("failed"), left: count("no_time") + count("cancelled") })}
            {(d.state.restore_failed?.length ?? 0) > 0 && <b style={{ color: "var(--crit)" }}>{" · "}{t("benchmarks.night.restore_failed")}</b>}
          </summary>
          <ul style={{ margin: "6px 0 0 18px", padding: 0, lineHeight: 1.6 }}>
            {(d.state.restore_failed ?? []).map((r) => <li key={`restore-${r.mode}`} style={{ color: "var(--crit)" }}><span style={{ fontFamily: "var(--mono)" }}>{r.mode}</span> — {r.message}</li>)}
            {results.map((r) => (
              <li key={r.mode} style={{ overflowWrap: "anywhere" }}>
                <span style={{ fontFamily: "var(--mono)" }}>{r.mode}</span> — {t(`benchmarks.night.outcome.${r.outcome}`)}{r.message ? `: ${r.message}` : ""}
              </li>
            ))}
          </ul>
        </details>
      )}
      {error && <div className="error-note" style={{ marginTop: 8 }}>{error}</div>}

      {confirming && (
        <Modal title={t("benchmarks.night.confirm_title")} onClose={() => { if (!busy) setConfirming(false); }}>
          <div style={{ fontSize: 12.5, lineHeight: 1.6, marginBottom: 10 }}>
            {t("benchmarks.night.confirm_body", { from: when(d.window.starts_at), until: when(d.window.ends_at) })}
          </div>
          <ul style={{ margin: "0 0 10px 18px", padding: 0, fontSize: 12, maxHeight: 220, overflowY: "auto", lineHeight: 1.6 }}>
            {d.queue.map((q) => (
              <li key={q.mode}><span style={{ fontFamily: "var(--mono)" }}>{q.mode}</span> · {t("benchmarks.night.about_minutes", { minutes: minutes(q.estimate_seconds) })}</li>
            ))}
          </ul>
          {total > available && <div style={{ fontSize: 12, color: "var(--warn)", marginBottom: 10 }}>{t("benchmarks.night.wont_all_fit")}</div>}
          <div className="form-actions">
            <button type="button" className="btn" disabled={busy} onClick={() => setConfirming(false)}>{t("benchmarks.night.back")}</button>
            <button type="button" className="btn primary" disabled={busy} onClick={() => run(arm)}>{t("benchmarks.night.button")}</button>
          </div>
        </Modal>
      )}
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </div>
  );
}
