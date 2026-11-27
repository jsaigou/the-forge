// settings/panels/Monitoring.tsx — one table instead of two sparse cards (design feedback 2026-11-27): each setting is a line saying what its
// current value does and when a change applies. It still saves to two endpoints: "monitor" (infra.monitor, all apply="live" — ReloadConfig, the
// collector re-reads *config.Config every poll cycle) and "metrics" (metrics/settings — retention_days is live, sample_interval_s needs a
// restart, on the SAME endpoint; see MetricsSettings's doc comment in types.ts for why they're badged differently).
import { useTranslation } from "react-i18next";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { useMetricsSettings, useMonitorSettings, useUpdateMetricsSettings, useUpdateMonitorSettings } from "../../lib/queries";
import { Field } from "../Field";
import { MONITORING_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";

const F = Object.fromEntries(MONITORING_FIELDS.map((f) => [f.id, f]));

// Table order, by what the settings are for.
const MONITOR_ROWS = [
  ["gpu", "gtt_warn_pct"],
  ["hang", "hang_tps_thousandth"],
  ["hang", "hang_sustain_s"],
  ["checks", "poll_interval_s"],
  ["switching", "switch_cooldown_s"],
] as const;
const METRICS_ROWS = [
  ["history", "retention_days"],
  ["history", "sample_interval_s"],
] as const;

export function Monitoring({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const monitorData = useMonitorSettings();
  const updateMonitor = useUpdateMonitorSettings();
  const monitor = useSettingsGroup(monitorData.data, updateMonitor);
  const metricsData = useMetricsSettings();
  const updateMetrics = useUpdateMetricsSettings();
  const metrics = useSettingsGroup(metricsData.data, updateMetrics);

  const row = (group: string, key: string, value: number) => {
    const scale = F[`monitoring.${key}`].scale ?? 1;
    return { group: t(`monitoring.group.${group}`), effect: t(`monitoring.effect.${key}`, { value: Number((value / scale).toFixed(6)) }) };
  };
  const missing = (isError: boolean, resource: string) => (
    <div className="empty-note">{t(isError ? "shared.role_required" : "shared.loading", { resource: t(resource) })}</div>
  );

  // A rejected save is already shown by the group's own error; the other group still saves.
  const save = () => void Promise.allSettled([monitor.save(), metrics.save()]);
  const dirty = monitor.dirty || metrics.dirty;

  return (
    <>
      <div className="eyebrow">{t("monitoring.title")}</div>
      {monitor.error && <div className="error-note" style={{ marginBottom: 12 }}>{monitor.error}</div>}
      {metrics.error && <div className="error-note" style={{ marginBottom: 12 }}>{metrics.error}</div>}
      <div className="card setrows">
        <div className="setrow setrow-head" aria-hidden="true">
          <span>{t("monitoring.col.group")}</span>
          <span>{t("monitoring.col.setting")}</span>
          <span>{t("monitoring.col.value")}</span>
          <span>{t("monitoring.col.effect")}</span>
          <span>{t("monitoring.col.applies")}</span>
        </div>
        {monitor.active
          ? MONITOR_ROWS.map(([group, key]) => (
              <Field key={key} rec={F[`monitoring.${key}`]} value={monitor.active![key]} disabled={!canAdmin}
                row={row(group, key, monitor.active![key])} onChange={(v) => monitor.setField(key, Number(v))} />
            ))
          : missing(monitorData.isError, "monitoring.monitor_resource")}
        {metrics.active
          ? METRICS_ROWS.map(([group, key]) => (
              <Field key={key} rec={F[`monitoring.${key}`]} value={metrics.active![key]} disabled={!canAdmin}
                row={row(group, key, metrics.active![key])} onChange={(v) => metrics.setField(key, Number(v))} />
            ))
          : missing(metricsData.isError, "monitoring.metrics_resource")}
      </div>
      {canAdmin && dirty && (
        <div className="form-actions" style={{ marginTop: 12 }}>
          <button type="button" className="btn" onClick={() => { monitor.reset(); metrics.reset(); }}>{t("shared.reset")}</button>
          <SaveButton pending={monitor.pending || metrics.pending} isError={monitor.isError || metrics.isError} onClick={save} />
        </div>
      )}
      <StepUpModal open={monitor.gate.open} requiredFactor={monitor.gate.factor} onSuccess={monitor.gate.onSuccess} onClose={monitor.gate.onClose} />
      <StepUpModal open={metrics.gate.open} requiredFactor={metrics.gate.factor} onSuccess={metrics.gate.onSuccess} onClose={metrics.gate.onClose} />
    </>
  );
}
