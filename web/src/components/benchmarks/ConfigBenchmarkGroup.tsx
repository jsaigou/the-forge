import { useState } from "react";
import { useTranslation } from "react-i18next";
import { appLocale, formatGB } from "../../lib/format";
import { depthLabel, staleExplanation } from "../../lib/profileFormat";
import type { ConfigGroup, ScopedBenchmark } from "../../lib/benchmarkGrouping";
import type { CatalogBenchmark } from "../../lib/types";
import { Icon } from "../Icon";
import { ConfirmButton } from "../ConfirmButton";
import type { ProfileRunController } from "./ProfileRunCard";

// ConfigBenchmarkGroup — Phase 8 (pre-release feedback sprint). One config's
// section within the merged Benchmarks & Profiling view: identity header,
// measured profile strip (or an honest "not profiled" state), and the
// benchmark rows this config carries — its own plus what it inherits from
// its variant and model, most-specific first.
//
// Own vs. inherited needs three simultaneous visual cues, not one — a
// single treatment isn't enough to stop an operator reading a model-wide
// GPQA score as something specific to this config: a scope chip that NAMES
// its owner, a left border (the "quotation" idiom), and reduced opacity.

function ScopeChip({ scope, ownerLabel }: { scope: ScopedBenchmark["scope"]; ownerLabel: string }) {
  if (scope === "config") {
    return <span className="chip" style={{ color: "var(--cool)" }}>config</span>;
  }
  return <span className="chip" style={{ color: "var(--text-mute)" }}>{scope} · {ownerLabel}</span>;
}

function BenchmarkRow({
  row,
  canAdmin,
  onEdit,
  onDelete,
  deletePending,
}: {
  row: ScopedBenchmark;
  canAdmin: boolean;
  onEdit: (b: CatalogBenchmark) => void;
  onDelete: (b: CatalogBenchmark) => void;
  deletePending: boolean;
}) {
  const { t } = useTranslation("settings");
  const b = row.benchmark;
  const inherited = row.scope !== "config";
  return (
    <div
      className="qrow"
      style={{
        alignItems: "flex-start",
        opacity: inherited ? 0.72 : 1,
        borderLeft: inherited ? "2px solid var(--border)" : undefined,
        paddingLeft: inherited ? 10 : undefined,
        marginLeft: inherited ? 4 : undefined,
      }}
    >
      <span style={{ width: 100 }}><ScopeChip scope={row.scope} ownerLabel={row.ownerLabel} /></span>
      <span style={{ width: 150, fontSize: 12, fontWeight: 600 }}>{b.metric}</span>
      <span style={{ width: 70, fontFamily: "var(--mono)", fontSize: 12 }}>{b.value}</span>
      <span style={{ width: 130, fontSize: 11, color: "var(--text-dim)" }}>{b.notes || "—"}</span>
      <span style={{ fontSize: 10.5, color: "var(--text-mute)", flex: 1, fontFamily: "var(--mono)" }}>
        {b.source_url ? <a href={b.source_url} target="_blank" rel="noopener noreferrer" style={{ color: "var(--cool)" }}>{b.source_date || "link"}</a> : b.source_date || "—"}
      </span>
      {canAdmin && (
        <div className="actions" style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
          <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => onEdit(b)}>{t("benchmarks.page.edit")}</button>
          <ConfirmButton
            className="btn"
            style={{ fontSize: 11, padding: "4px 8px" }}
            pending={deletePending}
            onConfirm={() => onDelete(b)}
            warning={
              inherited
                ? t("benchmarks.group.delete_confirm_inherited", { metric: b.metric, scope: row.scope, owner: row.ownerLabel })
                : t("benchmarks.group.delete_confirm_own", { metric: b.metric })
            }
          />
        </div>
      )}
    </div>
  );
}

export function ConfigBenchmarkGroup({
  group,
  canAdmin,
  profileController,
  deletePending,
  onEditBenchmark,
  onAddBenchmark,
  onDeleteBenchmark,
}: {
  group: ConfigGroup;
  canAdmin: boolean;
  profileController: ProfileRunController;
  deletePending: boolean;
  onEditBenchmark: (b: CatalogBenchmark) => void;
  onAddBenchmark: (configId: number) => void;
  onDeleteBenchmark: (b: CatalogBenchmark) => void;
}) {
  const { t } = useTranslation("settings");
  const [expanded, setExpanded] = useState(false);
  const { config, model, variant, profile, benchmarks } = group;

  const depths = profile?.depth_benchmarks ?? [];
  const typical = depths[0];
  const worst = depths.length > 1 ? depths[depths.length - 1] : undefined;

  const running = profileController.activeMode === config.name;
  const someoneElseRunning = profileController.busy && !running;

  return (
    <div className="card" style={{ marginBottom: 10 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        {model?.logo && <Icon slug={model.logo} name={model.name} />}
        <span style={{ fontFamily: "var(--mono)", fontSize: 13, fontWeight: 600 }}>{config.name}</span>
        {config.is_default && <span className="chip">{t("benchmarks.group.default_chip")}</span>}
        {config.visibility === "hidden" && <span className="chip" style={{ color: "var(--text-mute)" }}>{t("benchmarks.group.hidden_chip")}</span>}
        {!profile ? (
          <span className="chip" style={{ color: "var(--text-mute)" }} title={t("benchmarks.group.no_mode_title")}>{t("benchmarks.group.no_mode_chip")}</span>
        ) : profile.stale ? (
          <span className="chip" style={{ color: "var(--warn)" }} title={staleExplanation(t)}>{t("benchmarks.group.stale_chip")}</span>
        ) : profile.measured_at > 0 ? (
          <span className="chip" style={{ color: "var(--ok)" }}>{t("benchmarks.group.profiled_chip")}</span>
        ) : (
          <span className="chip" style={{ color: "var(--text-mute)" }}>{t("benchmarks.group.unprofiled_chip")}</span>
        )}
        <span style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
          {depths.length > 1 && (
            <button className="btn" style={{ fontSize: 11 }} onClick={() => setExpanded((v) => !v)}>
              {expanded ? t("benchmarks.group.show_less") : t("benchmarks.group.show_curve")}
            </button>
          )}
          {canAdmin && (
            <button className="btn" style={{ fontSize: 11 }} onClick={() => onAddBenchmark(config.id)}>
              {t("benchmarks.group.add_benchmark")}
            </button>
          )}
          {canAdmin && profile && (
            <button
              className="btn"
              disabled={someoneElseRunning}
              title={someoneElseRunning ? t("benchmarks.group.someone_else_running", { mode: profileController.activeMode }) : undefined}
              onClick={() => profileController.requestProfile(config.name)}
              style={{ fontSize: 11 }}
            >
              {running ? t("benchmarks.group.profiling_ellipsis") : t("benchmarks.group.profile_ellipsis")}
            </button>
          )}
        </span>
      </div>
      <div style={{ fontSize: 11, color: "var(--text-dim)", marginTop: 2 }}>
        {model ? `${model.name}${variant ? ` / ${variant.name}` : ""}` : t("benchmarks.group.variant_fallback", { id: config.variant_id })}
      </div>

      <div style={{ marginTop: 8, fontSize: 12 }}>
        {!profile ? null : (
          <div style={{ color: "var(--text-dim)" }}>
            {profile.safe_memory_bytes > 0 ? `${formatGB(profile.safe_memory_bytes, 1)} GB` : "—"}
            {" · "}{t("benchmarks.group.typical")}{" "}
            {typical ? `${typical.pp2048_tps.toFixed(0)} pf / ${typical.tg128_tps.toFixed(1)} dec`
              : profile.prefill_tps > 0 || profile.decode_tps > 0 ? `${profile.prefill_tps.toFixed(0)} pf / ${profile.decode_tps.toFixed(1)} dec` : "—"}
            {worst && ` · ${t("benchmarks.group.worst")} ${worst.pp2048_tps.toFixed(0)} pf / ${worst.tg128_tps.toFixed(1)} dec`}
            {profile.measured_at > 0 && ` · ${t("benchmarks.group.measured", { date: new Date(profile.measured_at * 1000).toLocaleDateString(appLocale()) })}`}
          </div>
        )}
        {profile && !profile.stale && profile.measured_at === 0 && (
          <div style={{ color: "var(--text-mute)" }}>{t("benchmarks.group.not_profiled")}</div>
        )}
      </div>

      {expanded && depths.length > 1 && (
        <div style={{ margin: "8px 0", padding: 10, background: "var(--panel-2, var(--panel))", borderRadius: 6 }}>
          <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10, textTransform: "uppercase", letterSpacing: ".05em" }}>
            <span style={{ width: 120 }}>{t("benchmarks.group.depth_col")}</span>
            <span style={{ width: 100 }}>{t("benchmarks.group.prefill_col")}</span>
            <span style={{ width: 100 }}>{t("benchmarks.group.decode_col")}</span>
          </div>
          {depths.map((d) => (
            <div className="qrow" key={d.depth_tokens} style={{ fontSize: 12 }}>
              <span style={{ width: 120 }}>{depthLabel(d.depth_tokens, profile?.n_ctx ?? 0, t)} ({d.depth_tokens} tok)</span>
              <span style={{ width: 100 }}>{d.pp2048_tps.toFixed(0)}</span>
              <span style={{ width: 100 }}>{d.tg128_tps.toFixed(1)}</span>
            </div>
          ))}
        </div>
      )}

      <div style={{ marginTop: 8 }}>
        {benchmarks.length === 0 ? (
          <div style={{ fontSize: 11, color: "var(--text-mute)" }}>{t("benchmarks.group.no_curated")}</div>
        ) : (
          benchmarks.map((row) => (
            <BenchmarkRow
              key={row.benchmark.id}
              row={row}
              canAdmin={canAdmin}
              onEdit={onEditBenchmark}
              onDelete={onDeleteBenchmark}
              deletePending={deletePending}
            />
          ))
        )}
      </div>
    </div>
  );
}
