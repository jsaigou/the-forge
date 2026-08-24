import { useState } from "react";
import { formatGB } from "../../lib/format";
import { depthLabel, STALE_EXPLANATION } from "../../lib/profileFormat";
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
          <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => onEdit(b)}>Edit</button>
          <ConfirmButton
            className="btn"
            style={{ fontSize: 11, padding: "4px 8px" }}
            pending={deletePending}
            onConfirm={() => onDelete(b)}
            warning={
              inherited
                ? `Delete "${b.metric}"? It's scoped to ${row.scope} ${row.ownerLabel}, so this removes it everywhere that scope is shown, not just here.`
                : `Delete "${b.metric}" from this config?`
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
        {config.is_default && <span className="chip">default</span>}
        {config.visibility === "hidden" && <span className="chip" style={{ color: "var(--text-mute)" }}>hidden</span>}
        {!profile ? (
          <span className="chip" style={{ color: "var(--text-mute)" }} title="This config didn't convert into a runnable mode (usually a missing build backend) — see the server log.">no mode</span>
        ) : profile.stale ? (
          <span className="chip" style={{ color: "var(--warn)" }} title={STALE_EXPLANATION}>stale</span>
        ) : profile.measured_at > 0 ? (
          <span className="chip" style={{ color: "var(--ok)" }}>profiled</span>
        ) : (
          <span className="chip" style={{ color: "var(--text-mute)" }}>unprofiled</span>
        )}
        <span style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
          {depths.length > 1 && (
            <button className="btn" style={{ fontSize: 11 }} onClick={() => setExpanded((v) => !v)}>
              {expanded ? "Show less" : "Show curve"}
            </button>
          )}
          {canAdmin && (
            <button className="btn" style={{ fontSize: 11 }} onClick={() => onAddBenchmark(config.id)}>
              + Benchmark
            </button>
          )}
          {canAdmin && profile && (
            <button
              className="btn"
              disabled={someoneElseRunning}
              title={someoneElseRunning ? `A profile run is already in progress (${profileController.activeMode}) — only one can run at a time.` : undefined}
              onClick={() => profileController.requestProfile(config.name)}
              style={{ fontSize: 11 }}
            >
              {running ? "Profiling…" : "Profile…"}
            </button>
          )}
        </span>
      </div>
      <div style={{ fontSize: 11, color: "var(--text-dim)", marginTop: 2 }}>
        {model ? `${model.name}${variant ? ` / ${variant.name}` : ""}` : `variant #${config.variant_id}`}
      </div>

      <div style={{ marginTop: 8, fontSize: 12 }}>
        {!profile ? null : (
          <div style={{ color: "var(--text-dim)" }}>
            {profile.safe_memory_bytes > 0 ? `${formatGB(profile.safe_memory_bytes, 1)} GB` : "—"}
            {" · typical "}
            {typical ? `${typical.pp2048_tps.toFixed(0)} pf / ${typical.tg128_tps.toFixed(1)} dec`
              : profile.prefill_tps > 0 || profile.decode_tps > 0 ? `${profile.prefill_tps.toFixed(0)} pf / ${profile.decode_tps.toFixed(1)} dec` : "—"}
            {worst && ` · worst ${worst.pp2048_tps.toFixed(0)} pf / ${worst.tg128_tps.toFixed(1)} dec`}
            {profile.measured_at > 0 && ` · measured ${new Date(profile.measured_at * 1000).toLocaleDateString()}`}
          </div>
        )}
        {profile && !profile.stale && profile.measured_at === 0 && (
          <div style={{ color: "var(--text-mute)" }}>Not profiled — no measured memory or throughput.</div>
        )}
      </div>

      {expanded && depths.length > 1 && (
        <div style={{ margin: "8px 0", padding: 10, background: "var(--panel-2, var(--panel))", borderRadius: 6 }}>
          <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10, textTransform: "uppercase", letterSpacing: ".05em" }}>
            <span style={{ width: 120 }}>Depth</span>
            <span style={{ width: 100 }}>Prefill T/s</span>
            <span style={{ width: 100 }}>Decode T/s</span>
          </div>
          {depths.map((d) => (
            <div className="qrow" key={d.depth_tokens} style={{ fontSize: 12 }}>
              <span style={{ width: 120 }}>{depthLabel(d.depth_tokens, profile?.n_ctx ?? 0)} ({d.depth_tokens} tok)</span>
              <span style={{ width: 100 }}>{d.pp2048_tps.toFixed(0)}</span>
              <span style={{ width: 100 }}>{d.tg128_tps.toFixed(1)}</span>
            </div>
          ))}
        </div>
      )}

      <div style={{ marginTop: 8 }}>
        {benchmarks.length === 0 ? (
          <div style={{ fontSize: 11, color: "var(--text-mute)" }}>No curated benchmarks — the model has none either.</div>
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
