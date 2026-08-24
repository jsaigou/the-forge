// settings/panels/Monitoring.tsx — Sprint 12 (was H) Phase 6. Two cards:
// "Monitor" (infra.monitor, all apply="live" — ReloadConfig, the collector
// re-reads *config.Config every poll cycle) and "Metrics" (metrics/settings
// — retention_days is live, sample_interval_s is restart, on the SAME
// endpoint; see MetricsSettings's doc comment in types.ts for why they're
// badged differently despite being adjacent fields).
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { useMetricsSettings, useMonitorSettings, useUpdateMetricsSettings, useUpdateMonitorSettings } from "../../lib/queries";
import { Field } from "../Field";
import { MONITORING_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";

const F = Object.fromEntries(MONITORING_FIELDS.map((f) => [f.id, f]));

function MonitorCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useMonitorSettings();
  const update = useUpdateMonitorSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">Monitor</div>
        <div className="card"><div className="empty-note">Operator role required to view monitor settings.</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">Monitor</div>
        <div className="card"><div className="empty-note">Loading monitor settings…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">Monitor</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["monitoring.poll_interval_s"]} value={g.active.poll_interval_s} disabled={!canAdmin}
            onChange={(v) => g.setField("poll_interval_s", Number(v))} />
          <Field rec={F["monitoring.hang_tps_thousandth"]} value={g.active.hang_tps_thousandth} disabled={!canAdmin}
            onChange={(v) => g.setField("hang_tps_thousandth", Number(v))} />
          <Field rec={F["monitoring.hang_sustain_s"]} value={g.active.hang_sustain_s} disabled={!canAdmin}
            onChange={(v) => g.setField("hang_sustain_s", Number(v))} />
          <Field rec={F["monitoring.switch_cooldown_s"]} value={g.active.switch_cooldown_s} disabled={!canAdmin}
            onChange={(v) => g.setField("switch_cooldown_s", Number(v))} />
          <Field rec={F["monitoring.gtt_warn_pct"]} value={g.active.gtt_warn_pct} disabled={!canAdmin}
            onChange={(v) => g.setField("gtt_warn_pct", Number(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function MetricsCard({ canAdmin }: { canAdmin: boolean }) {
  const cfg = useMetricsSettings();
  const update = useUpdateMetricsSettings();
  const g = useSettingsGroup(cfg.data, update);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">Metrics</div>
        <div className="card"><div className="empty-note">Operator role required to view metrics settings.</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">Metrics</div>
        <div className="card"><div className="empty-note">Loading metrics settings…</div></div>
      </>
    );
  }

  return (
    <>
      <div className="eyebrow">Metrics</div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        <div className="form-grid">
          <Field rec={F["monitoring.retention_days"]} value={g.active.retention_days} disabled={!canAdmin}
            onChange={(v) => g.setField("retention_days", Number(v))} />
          <Field rec={F["monitoring.sample_interval_s"]} value={g.active.sample_interval_s} disabled={!canAdmin}
            onChange={(v) => g.setField("sample_interval_s", Number(v))} />
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>Reset</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

export function Monitoring({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <MonitorCard canAdmin={canAdmin} />
      <MetricsCard canAdmin={canAdmin} />
    </>
  );
}
