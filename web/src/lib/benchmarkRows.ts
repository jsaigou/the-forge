// One flat row per local config and per remote-only model, for the sortable, comparable Benchmarks & Profiling table. A value measured by
// profiling wins over a recorded benchmark; among recorded ones the most specific scope wins (config, then variant, then model), newest first.
import type { BenchmarkScope, GroupedBenchmarks, ScopedBenchmark } from "./benchmarkGrouping";
import type { CatalogBuild, CatalogEngine, ProfileListItem, ProfileState } from "./types";

export const SCORE_METRICS = ["reasoning", "coding", "agentic_logic", "knowledge"] as const;
export type NumericColumn = "mem" | "pf" | "dc" | (typeof SCORE_METRICS)[number];
export const NUMERIC_COLUMNS: NumericColumn[] = ["mem", "pf", "dc", ...SCORE_METRICS];

export interface Cell {
  value: number | null;
  // A recorded benchmark that is not a number ("High") is shown as written.
  text: string | null;
  // true: from a recorded benchmark; false: measured here by profiling.
  recorded: boolean;
}

// "unknown": the config did not turn into a runnable mode, so there is no profile entry for it. "remote": served only by providers.
export type RowState = ProfileState | "unknown" | "remote";

export interface BenchRow {
  key: string;
  remote: boolean;
  configId: number | null;
  name: string;
  sub: string;
  engine: string; // engine kind; "" for remote rows
  backend: string;
  providers: string[];
  ctx: number | null;
  hidden: boolean;
  state: RowState;
  profile: ProfileListItem | undefined;
  cells: Record<NumericColumn, Cell>;
  benchmarks: ScopedBenchmark[];
  addTo: { type: string; id: number };
}

const SCOPE_RANK: Record<BenchmarkScope, number> = { config: 0, variant: 1, model: 2 };
const EMPTY: Cell = { value: null, text: null, recorded: false };

function recordedCell(list: ScopedBenchmark[], metric: string, convert: (n: number) => number): Cell {
  const best = list
    .filter((s) => s.benchmark.metric === metric)
    .sort(
      (a, b) =>
        SCOPE_RANK[a.scope] - SCOPE_RANK[b.scope] ||
        (b.benchmark.source_date || "").localeCompare(a.benchmark.source_date || "") ||
        b.benchmark.id - a.benchmark.id,
    )[0];
  if (!best) return EMPTY;
  const raw = best.benchmark.value.trim();
  const n = raw === "" ? NaN : Number(raw);
  return Number.isFinite(n) ? { value: convert(n), text: null, recorded: true } : { value: null, text: raw, recorded: true };
}

const toGB = (bytes: number) => bytes / (1024 * 1024 * 1024);
const asIs = (n: number) => n;
// Scores are stored as fractions; older rows already hold percentages.
const toPercent = (n: number) => (n <= 1 ? n * 100 : n);

function cellsFor(list: ScopedBenchmark[], profile: ProfileListItem | undefined): Record<NumericColumn, Cell> {
  const measured = profile && profile.measured_at > 0 ? profile : undefined;
  const pick = (value: number | undefined, convert: (n: number) => number, metric: string): Cell =>
    value && value > 0 ? { value: convert(value), text: null, recorded: false } : recordedCell(list, metric, convert);
  return {
    mem: pick(measured?.safe_memory_bytes, toGB, "safe_memory_bytes"),
    pf: pick(measured?.prefill_tps, asIs, "prefill_tps"),
    dc: pick(measured?.decode_tps, asIs, "decode_tps"),
    reasoning: recordedCell(list, "reasoning", toPercent),
    coding: recordedCell(list, "coding", toPercent),
    agentic_logic: recordedCell(list, "agentic_logic", toPercent),
    knowledge: recordedCell(list, "knowledge", toPercent),
  };
}

export function benchmarkRows(grouped: GroupedBenchmarks, engines: CatalogEngine[], builds: CatalogBuild[]): BenchRow[] {
  const kind = new Map(engines.map((e) => [e.id, e.kind]));
  const backend = new Map(builds.map((b) => [b.id, b.backend]));
  const local: BenchRow[] = grouped.groups.map((g) => ({
    key: `config-${g.config.id}`,
    remote: false,
    configId: g.config.id,
    name: g.config.name,
    sub: g.model ? `${g.model.name}${g.variant ? ` · ${g.variant.name}` : ""}` : `#${g.config.variant_id}`,
    engine: kind.get(g.config.engine_id) ?? "",
    backend: g.profile?.backend || backend.get(g.config.build_id ?? 0) || "",
    providers: [],
    ctx: g.config.n_ctx || null,
    hidden: g.config.visibility === "hidden",
    state: g.profile?.state ?? "unknown",
    profile: g.profile,
    cells: cellsFor(g.benchmarks, g.profile),
    benchmarks: g.benchmarks,
    addTo: { type: "config", id: g.config.id },
  }));
  const remote: BenchRow[] = grouped.remoteOnly.map((m) => ({
    key: `model-${m.model.id}`,
    remote: true,
    configId: null,
    name: m.model.name,
    sub: m.model.creator,
    engine: "",
    backend: "",
    providers: [...new Set(m.offerings.map((o) => o.provider))],
    ctx: Math.max(0, ...m.offerings.map((o) => o.context_length)) || null,
    hidden: m.model.visibility === "hidden",
    state: "remote",
    profile: undefined,
    cells: cellsFor(m.benchmarks, undefined),
    benchmarks: m.benchmarks,
    addTo: { type: "model", id: m.model.id },
  }));
  return [...local, ...remote];
}

export function formatCell(column: NumericColumn, cell: Cell, locale: string | undefined): string {
  if (cell.value === null) return cell.text ?? "–";
  if (column === "pf") return Math.round(cell.value).toLocaleString(locale);
  return cell.value.toLocaleString(locale, { minimumFractionDigits: 1, maximumFractionDigits: 1 });
}

export function formatContext(ctx: number | null): string {
  if (!ctx) return "–";
  return ctx >= 1024 * 1024 ? `${Math.round(ctx / (1024 * 1024))}M` : `${Math.round(ctx / 1024)}k`;
}
