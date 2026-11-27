// The sortable table: one row per config (and per remote-only model, in the offer colour). Clicking a name opens its depth curve, recorded
// benchmarks and actions; the box adds it to the comparison.
import { useTranslation } from "react-i18next";
import { formatCell, formatContext, NUMERIC_COLUMNS, type BenchRow, type NumericColumn } from "../../lib/benchmarkRows";
import { appLocale } from "../../lib/format";
import { depthLabel } from "../../lib/profileFormat";
import { runtimeLabel } from "../../lib/runtime";
import { safeHref } from "../../lib/safeHref";
import type { CatalogBenchmark } from "../../lib/types";
import { ConfirmButton } from "../ConfirmButton";
import type { ProfileRunController } from "./ProfileRunCard";

export type SortKey = "name" | "engine" | "ctx" | "state" | NumericColumn;
export interface Sort { key: SortKey; dir: 1 | -1 }

export const MAX_COMPARED = 4;

const COLUMNS: SortKey[] = ["name", "engine", "ctx", ...NUMERIC_COLUMNS, "state"];
const TEXT_COLUMNS = new Set<SortKey>(["name", "engine", "state"]);

function sortValue(row: BenchRow, key: SortKey): string | number | null {
  switch (key) {
    case "name": return row.name;
    case "engine": return row.remote ? row.providers.join(", ") : row.engine;
    case "ctx": return row.ctx;
    case "state": return row.state;
    default: return row.cells[key].value;
  }
}

// Rows without a value go last whichever way the column is sorted.
export function sortRows(rows: BenchRow[], sort: Sort): BenchRow[] {
  return [...rows].sort((a, b) => {
    const x = sortValue(a, sort.key);
    const y = sortValue(b, sort.key);
    if (x === null && y === null) return a.name.localeCompare(b.name);
    if (x === null) return 1;
    if (y === null) return -1;
    const order = typeof x === "string" ? x.localeCompare(String(y)) : x - (y as number);
    return order * sort.dir || a.name.localeCompare(b.name);
  });
}

// First click: names A→Z, memory smallest first, everything else largest first.
export function nextSort(current: Sort, key: SortKey): Sort {
  if (current.key === key) return { key, dir: current.dir === 1 ? -1 : 1 };
  return { key, dir: TEXT_COLUMNS.has(key) || key === "mem" ? 1 : -1 };
}

interface Actions {
  canAdmin: boolean;
  profileController: ProfileRunController;
  deletePending: boolean;
  onEdit: (b: CatalogBenchmark) => void;
  onAdd: (subject: { type: string; id: number }) => void;
  onDelete: (b: CatalogBenchmark) => void;
}

export function BenchmarkTable({ rows, sort, onSort, selected, onSelect, open, onOpen, ...actions }: Actions & {
  rows: BenchRow[];
  sort: Sort;
  onSort: (key: SortKey) => void;
  selected: string[];
  onSelect: (key: string) => void;
  open: string | null;
  onOpen: (key: string | null) => void;
}) {
  const { t } = useTranslation("settings");
  const locale = appLocale();
  // Bar length is relative to the largest value currently shown in that column.
  const max = Object.fromEntries(NUMERIC_COLUMNS.map((c) => [c, Math.max(0, ...rows.map((r) => r.cells[c].value ?? 0))])) as Record<NumericColumn, number>;

  return (
    <div className="bench-table" role="table" aria-label={t("benchmarks.table.aria")}>
      <div className="bench-grid bench-head" role="row">
        <span role="columnheader">{t("benchmarks.table.compare_col")}</span>
        {COLUMNS.map((c) => (
          <span role="columnheader" key={c} aria-sort={sort.key === c ? (sort.dir === 1 ? "ascending" : "descending") : "none"} className={TEXT_COLUMNS.has(c) ? "" : "num"}>
            <button type="button" className={sort.key === c ? "on" : ""} onClick={() => onSort(c)}>
              {t(`benchmarks.table.col.${c}`)}{sort.key === c ? (sort.dir === 1 ? " ↑" : " ↓") : ""}
            </button>
          </span>
        ))}
      </div>
      {rows.map((r) => {
        const picked = selected.includes(r.key);
        const isOpen = open === r.key;
        const full = !picked && selected.length >= MAX_COMPARED;
        return (
          <div key={r.key} className={`bench-item${r.remote ? " offer" : ""}${picked ? " picked" : ""}`}>
            <div className="bench-grid" role="row">
              <span role="cell">
                <input type="checkbox" checked={picked} disabled={full} onChange={() => onSelect(r.key)}
                  title={full ? t("benchmarks.table.compare_full", { max: MAX_COMPARED }) : undefined}
                  aria-label={t("benchmarks.table.compare_aria", { name: r.name })} />
              </span>
              <span role="cell" className="bench-name">
                <button type="button" aria-expanded={isOpen} onClick={() => onOpen(isOpen ? null : r.key)}>
                  <b>{r.name}</b>
                  <small>{r.sub}{r.hidden ? ` · ${t("benchmarks.table.hidden")}` : ""}</small>
                </button>
              </span>
              <span role="cell" className="bench-dim" title={r.remote ? r.providers.join(", ") : r.backend}>
                {r.remote ? r.providers.join(", ") || "–" : `${runtimeLabel(r.engine)}${r.backend === "vulkan" ? " · VK" : ""}`}
              </span>
              <span role="cell" className="num">{formatContext(r.ctx)}</span>
              {NUMERIC_COLUMNS.map((c) => {
                const cell = r.cells[c];
                const pct = cell.value !== null && max[c] > 0 ? Math.round((cell.value / max[c]) * 100) : 0;
                return (
                  <span role="cell" key={c} className={`num bench-bar${cell.value === null && !cell.text ? " none" : ""}`} style={{ ["--pct" as string]: `${pct}%` }}
                    title={cell.value === null && !cell.text ? undefined : t(cell.recorded ? "benchmarks.table.recorded" : "benchmarks.table.measured_here")}>
                    {formatCell(c, cell, locale)}{cell.recorded && (cell.value !== null || cell.text) ? "°" : ""}
                  </span>
                );
              })}
              <span role="cell">
                <span className={`bench-state ${r.state}`} title={stateNote(r, t)}>{t(`benchmarks.table.state.${r.state}`)}</span>
              </span>
            </div>
            {isOpen && <RowDetail row={r} {...actions} />}
          </div>
        );
      })}
    </div>
  );
}

function stateNote(r: BenchRow, t: (key: string, opts?: Record<string, unknown>) => string): string {
  if (r.state === "outdated") return t(`benchmarks.group.outdated_${r.profile?.reason || "launch"}_title`);
  if (r.state === "unsupported") return t("benchmarks.group.unsupported_title", { engine: r.profile?.reason });
  if (r.state === "unknown") return t("benchmarks.group.no_mode_title");
  if (r.state === "remote") return t("benchmarks.table.remote_note");
  return t(`benchmarks.group.${r.state}_title`);
}

function RowDetail({ row, canAdmin, profileController, deletePending, onEdit, onAdd, onDelete }: Actions & { row: BenchRow }) {
  const { t } = useTranslation("settings");
  const depths = row.profile?.depth_benchmarks ?? [];
  const running = profileController.activeMode === row.name;
  const otherRunning = profileController.busy && !running;
  const canProfile = canAdmin && !row.remote && (row.state === "current" || row.state === "outdated" || row.state === "never");
  const measured = row.profile && row.profile.measured_at > 0 ? row.profile.measured_at : 0;

  return (
    <div className="bench-detail">
      {!row.remote && (
        <div className="bench-depth">
          <strong>{t("benchmarks.table.depth_title")}</strong>
          {depths.length === 0 ? (
            <span className="bench-dim">{t("benchmarks.table.no_depth")}</span>
          ) : (
            <table>
              <thead><tr><th>{t("benchmarks.group.depth_col")}</th><th>{t("benchmarks.group.prefill_col")}</th><th>{t("benchmarks.group.decode_col")}</th></tr></thead>
              <tbody>
                {depths.map((d) => (
                  <tr key={d.depth_tokens}>
                    <td>{depthLabel(d.depth_tokens, row.profile?.n_ctx ?? 0, t)} ({d.depth_tokens})</td>
                    <td>{d.pp2048_tps.toFixed(0)}</td>
                    <td>{d.tg128_tps.toFixed(1)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {measured > 0 && <span className="bench-dim">{t("benchmarks.group.measured", { date: new Date(measured * 1000).toLocaleDateString(appLocale()) })}</span>}
        </div>
      )}
      <div className="bench-records">
        <span className="bench-dim">{stateNote(row, t)}</span>
        {row.benchmarks.length === 0 && <span className="bench-dim">{t("benchmarks.page.no_benchmarks_recorded")}</span>}
        {row.benchmarks.map(({ benchmark: b, scope, ownerLabel }) => (
          <div key={b.id} className="bench-record">
            <span className="chip" title={ownerLabel}>{t(`benchmarks.table.scope.${scope}`)}</span>
            <b>{b.metric}</b>
            <span className="mono">{b.value}</span>
            <span className="bench-dim">
              {safeHref(b.source_url) ? <a href={safeHref(b.source_url)} target="_blank" rel="noopener noreferrer">{b.source_date || t("benchmarks.table.source")}</a> : b.source_date}
              {b.notes ? ` ${b.notes}` : ""}
            </span>
            {canAdmin && (
              <span className="bench-record-actions">
                <button type="button" className="btn cat-chip-btn" onClick={() => onEdit(b)}>{t("benchmarks.page.edit")}</button>
                <ConfirmButton className="btn cat-chip-btn" pending={deletePending} onConfirm={() => onDelete(b)}
                  warning={scope === "config" || row.remote
                    ? t("benchmarks.group.delete_confirm_own", { metric: b.metric })
                    : t("benchmarks.group.delete_confirm_inherited", { metric: b.metric, scope: t(`benchmarks.table.scope.${scope}`), owner: ownerLabel })} />
              </span>
            )}
          </div>
        ))}
        {canAdmin && (
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            <button type="button" className="btn" onClick={() => onAdd(row.addTo)}>{t("benchmarks.group.add_benchmark")}</button>
            {canProfile && (
              <button type="button" className="btn primary" disabled={otherRunning}
                title={otherRunning ? t("benchmarks.group.someone_else_running", { mode: profileController.activeMode }) : undefined}
                onClick={() => profileController.requestProfile(row.name)}>
                {running ? t("benchmarks.group.profiling_ellipsis") : t("benchmarks.group.profile_ellipsis")}
              </button>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
