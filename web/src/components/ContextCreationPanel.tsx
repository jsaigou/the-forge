import { useState } from "react";
import { useTranslation } from "react-i18next";
import { formatPct, formatTokens } from "../lib/format";
import { useContextCreation } from "../lib/queries";
import type { ContextCreationResult, ContextCreationRow, ContextCreationWindow } from "../lib/types";
import { RangeToggle } from "./RangeToggle";

// WS-N2 (docs/compression-redesign/briefs/WS-N2.md): who is CREATING the
// context. Read-only view of the a0 creation ledger (CONTRACTS C2) — sizes and
// names only, never content. Context is append-only (P0): the point of the
// panel is to find what to create less of, and which clients edit history.
//
// Charts: one series per bar list (share of created characters), so there is
// nothing correlated to plot twice. Bars are plain divs on a shared 0-100%
// track; the number is always printed next to the bar.

const WINDOWS: ContextCreationWindow[] = ["24h", "7d", "30d"];
const TOP_N = 8;

const fmt = (n: number) => formatTokens(n);

function BarList({ rows, emptyLabel }: { rows: ContextCreationRow[]; emptyLabel: string }) {
  const { t } = useTranslation("dashboard");
  if (rows.length === 0) return <div className="empty-note">{emptyLabel}</div>;
  const shown = rows.slice(0, TOP_N);
  const rest = rows.length - shown.length;
  return (
    <div>
      {shown.map((r) => (
        <div key={r.key || "_"} style={{ marginBottom: 8 }}>
          <div style={{ display: "flex", gap: 8, alignItems: "baseline", fontSize: 12 }}>
            <span style={{ minWidth: 0, flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }} title={r.key}>
              {r.key || t("context_creation.unnamed")}
            </span>
            <span style={{ fontFamily: "var(--mono)", color: "var(--text-dim)", whiteSpace: "nowrap" }}>
              {formatPct(r.share * 100)} · {fmt(r.created_chars)}
            </span>
          </div>
          <div style={{ height: 6, borderRadius: 3, background: "var(--border)", marginTop: 3, overflow: "hidden" }}>
            <div
              style={{
                height: "100%",
                width: `${Math.max(0, Math.min(100, r.share * 100))}%`,
                minWidth: r.created_chars > 0 ? 2 : 0,
                background: "var(--cool)",
              }}
            />
          </div>
          {(r.big_results_8k > 0 || r.big_results_24k > 0) && (
            <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginTop: 2 }}>
              {r.max_result_chars > 0
                ? t("context_creation.big_inline", { n8: r.big_results_8k, n24: r.big_results_24k, max: fmt(r.max_result_chars) })
                : t("context_creation.big_inline_nomax", { n8: r.big_results_8k, n24: r.big_results_24k })}
            </div>
          )}
        </div>
      ))}
      {rest > 0 && <div style={{ fontSize: 11, color: "var(--text-mute)" }}>{t("context_creation.more_rows", { count: rest })}</div>}
    </div>
  );
}

function Section({ title, hint, res, loading, error }: {
  title: string;
  hint: string;
  res: ContextCreationResult | undefined;
  loading: boolean;
  error: boolean;
}) {
  const { t } = useTranslation("dashboard");
  return (
    <div className="card" style={{ minWidth: 0 }}>
      <div style={{ fontSize: 12, fontWeight: 600 }}>{title}</div>
      <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 10 }}>{hint}</div>
      {error ? (
        <div className="empty-note">{t("context_creation.load_error")}</div>
      ) : loading || !res ? (
        <div className="empty-note">{t("loading")}</div>
      ) : (
        <BarList rows={res.rows} emptyLabel={t("context_creation.no_data")} />
      )}
    </div>
  );
}

export function ContextCreationPanel() {
  const { t } = useTranslation("dashboard");
  const [win, setWin] = useState<ContextCreationWindow>("24h");
  const byTool = useContextCreation(win, "tool");
  const byConsumer = useContextCreation(win, "consumer");
  const byModel = useContextCreation(win, "model");

  const winOptions = WINDOWS.map((w) => ({ key: w, label: t(`context_creation.window_${w}`) }));
  // Global figures are identical across the three queries (request-level
  // rows); take them from whichever answered first.
  const g = byTool.data ?? byConsumer.data ?? byModel.data;
  const toolRows = byTool.data?.rows ?? [];
  const big8 = toolRows.reduce((a, r) => a + r.big_results_8k, 0);
  const big24 = toolRows.reduce((a, r) => a + r.big_results_24k, 0);
  const allFailed = byTool.isError && byConsumer.isError && byModel.isError;
  const noData = !!g && g.total_requests === 0 && g.total_created_chars === 0;
  const dropped = g?.ledger?.dropped ?? 0;
  const errors = (g?.ledger?.parse_failures ?? 0) + (g?.ledger?.panics ?? 0) + (g?.ledger?.flush_errors ?? 0);
  const winLabel = t(`context_creation.window_${win}`);
  const bustRows = g?.suspects_by_consumer ?? [];

  // overflow-wrap: the global :lang(ja) keep-all rule leaves long unspaced
  // Japanese runs unbreakable; let them wrap inside this panel's cards.
  return (
    <div style={{ overflowWrap: "anywhere" }}>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>{t("context_creation.title")}</span>
        <RangeToggle options={winOptions} value={win} onChange={setWin} />
      </div>
      <div style={{ fontSize: 11.5, color: "var(--text-dim)", margin: "0 0 10px" }}>{t("context_creation.intro")}</div>

      {allFailed ? (
        <div className="card">
          <div className="empty-note">{t("context_creation.unavailable")}</div>
        </div>
      ) : noData ? (
        <div className="card">
          <div className="empty-note">{t("context_creation.empty_window")}</div>
        </div>
      ) : (
        <>
          <div className="stats" style={{ gridTemplateColumns: "repeat(auto-fit, minmax(140px, 1fr))" }}>
            <div className="stat">
              <div className="k">{t("context_creation.stat_requests_k")}</div>
              <div className="v">{g ? g.total_requests.toLocaleString() : "…"}</div>
              <div className="d">{g ? t("context_creation.stat_requests_d", { chars: fmt(g.total_created_chars) }) : ""}</div>
            </div>
            <div className="stat">
              <div className="k">{t("context_creation.stat_schema_k")}</div>
              <div className="v">{g ? (g.schema_chars_p50 != null ? fmt(g.schema_chars_p50) : "—") : "…"}</div>
              <div className="d">{g && g.schema_chars_p50 == null ? t("context_creation.not_reported") : t("context_creation.stat_schema_d")}</div>
            </div>
            <div className="stat">
              <div className="k">{t("context_creation.stat_big_k")}</div>
              <div className="v">{byTool.data ? `${big8.toLocaleString()} / ${big24.toLocaleString()}` : "…"}</div>
              <div className="d">{t("context_creation.stat_big_d")}</div>
            </div>
            <div className="stat">
              <div className="k">{t("context_creation.stat_reuse_k")}</div>
              <div className="v">{g ? (g.reuse_p50 != null ? formatPct(g.reuse_p50 * 100) : "—") : "…"}</div>
              <div className="d">
                {g && g.reuse_p50 == null
                  ? t("context_creation.reuse_not_reported")
                  : t("context_creation.stat_reuse_d", { count: g?.reuse_samples ?? 0 })}
              </div>
            </div>
          </div>

          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 10, marginTop: 10 }}>
            <Section title={t("context_creation.by_tool")} hint={t("context_creation.by_tool_hint")} res={byTool.data} loading={byTool.isLoading} error={byTool.isError} />
            <Section title={t("context_creation.by_consumer")} hint={t("context_creation.by_consumer_hint")} res={byConsumer.data} loading={byConsumer.isLoading} error={byConsumer.isError} />
            <Section title={t("context_creation.by_model")} hint={t("context_creation.by_model_hint")} res={byModel.data} loading={byModel.isLoading} error={byModel.isError} />
          </div>

          <div className="card" style={{ marginTop: 10 }}>
            <div style={{ fontSize: 12, fontWeight: 600 }}>{t("context_creation.bust_title")}</div>
            <div style={{ fontSize: 11.5, color: "var(--text-dim)", margin: "4px 0 8px" }}>{t("context_creation.bust_explain")}</div>
            <div style={{ fontSize: 11.5, marginBottom: 8 }}>
              <span style={{ fontFamily: "var(--mono)", color: (g?.cache_bust_suspects ?? 0) > 0 ? "var(--heat)" : undefined }}>
                {g ? g.cache_bust_suspects.toLocaleString() : "…"}
              </span>{" "}
              <span style={{ color: "var(--text-mute)" }}>{t("context_creation.bust_window_total", { window: winLabel })}</span>
            </div>
            {bustRows.length === 0 ? (
              <div className="empty-note">{t("context_creation.bust_none", { window: winLabel })}</div>
            ) : (
              <div>
                {bustRows.map((r) => (
                  <div key={r.consumer} className="qrow" style={{ flexWrap: "wrap" }}>
                    <span className="want" style={{ minWidth: 0, flex: 1, overflow: "hidden", textOverflow: "ellipsis" }} title={r.consumer}>
                      {r.consumer || t("context_creation.unnamed")}
                    </span>
                    <span style={{ fontFamily: "var(--mono)", color: "var(--heat)" }}>{r.suspects.toLocaleString()}</span>
                    <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
                      {t("context_creation.bust_row", { requests: r.requests.toLocaleString(), window: winLabel })}
                    </span>
                  </div>
                ))}
              </div>
            )}
          </div>

          <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginTop: 8 }}>
            {t("context_creation.approx_note")}
            {dropped > 0 && <> {t("context_creation.partial_dropped", { count: dropped })}</>}
            {errors > 0 && <> {t("context_creation.partial_errors", { count: errors })}</>}
          </div>
        </>
      )}
    </div>
  );
}
