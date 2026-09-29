import { useState } from "react";
import { useTranslation } from "react-i18next";
import { formatCurrency } from "../../lib/format";
import { useUsage } from "../../lib/queries";

// Widget "per-model-spend" (see lib/widgetRegistry.ts). Relocated verbatim
// from Dashboard's now-deleted Trends tab to the Cost tab — Phase 5
// (2026-08-12) — per-model spend belongs next to the rest of the cost
// picture, not on a diagnostics-flavored trends page. Row IDs stay uniquely
// keyed (`local:${model}` / `remote:${provider}:${model}`, was
// `${provider} (remote)`, which collided whenever a provider had more than
// one usage row).
export function PerModelSpendTableWidget({ window_ }: { window_: string }) {
  const { t } = useTranslation("dashboard");
  const usage = useUsage(window_);
  const [showAllModels, setShowAllModels] = useState(false);

  const currency = usage.data?.display_currency ?? "USD";
  const combinedRows = [
    ...(usage.data?.models ?? []).map((m) => ({
      id: `local:${m.model}`,
      name: m.model,
      uses: m.loads_ok,
      tokens: m.prompt_tokens + m.predicted_tokens,
      oom: m.load_failures,
      lockups: m.inference_hangs,
      spend: m.power_cost_display,
      remote: false,
    })),
    ...(usage.data?.external ?? []).map((e) => ({
      id: `remote:${e.provider}:${e.model}`,
      name: `${e.provider}${t("per_model_spend.remote_suffix")}`,
      uses: e.requests,
      tokens: e.prompt_tokens + e.completion_tokens,
      oom: null as number | null,
      lockups: null as number | null,
      spend: e.cost_display,
      remote: true,
    })),
  ];
  const visibleRows = showAllModels ? combinedRows : combinedRows.slice(0, 3);

  return (
    <div className="card">
      <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
        <span style={{ width: 150 }}>{t("per_model_spend.col_model")}</span>
        <span style={{ width: 60 }}>{t("per_model_spend.col_uses")}</span>
        <span style={{ width: 80 }}>{t("per_model_spend.col_tokens")}</span>
        <span style={{ width: 50 }}>{t("per_model_spend.col_oom")}</span>
        <span style={{ width: 60 }}>{t("per_model_spend.col_lockups")}</span>
        <span className="pos">{t("per_model_spend.col_spend")}</span>
      </div>
      {combinedRows.length === 0 && <div className="empty-note">{t("per_model_spend.no_usage")}</div>}
      {visibleRows.map((r) => (
        <div className="qrow" key={r.id}>
          <span className="want" style={{ width: 150, color: r.remote ? "var(--cool)" : undefined }}>{r.name}</span>
          <span style={{ width: 60, fontFamily: "var(--mono)" }}>{r.uses}</span>
          <span style={{ width: 80, fontFamily: "var(--mono)" }}>{r.tokens.toLocaleString()}</span>
          <span style={{ width: 50, fontFamily: "var(--mono)", color: r.oom ? "var(--warn)" : undefined }}>{r.oom ?? "—"}</span>
          <span style={{ width: 60, fontFamily: "var(--mono)", color: r.lockups ? "var(--crit)" : undefined }}>{r.lockups ?? "—"}</span>
          <span className="pos">{formatCurrency(r.spend, currency)}</span>
        </div>
      ))}
      {combinedRows.length > 3 && (
        <button className="btn" style={{ fontSize: 11, marginTop: 8 }} onClick={() => setShowAllModels((v) => !v)}>
          {showAllModels ? t("per_model_spend.show_top3") : t("per_model_spend.show_all", { count: combinedRows.length })}
        </button>
      )}
    </div>
  );
}
