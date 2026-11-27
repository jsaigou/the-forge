import { useState } from "react";
import { useTranslation } from "react-i18next";
import { BenchmarkTable, nextSort, sortRows, type Sort } from "../../components/benchmarks/BenchmarkTable";
import { ComparePanel } from "../../components/benchmarks/ComparePanel";
import { ProfileNightCard } from "../../components/benchmarks/ProfileNightCard";
import { ProfileRunCard, useProfileRunController } from "../../components/benchmarks/ProfileRunCard";
import { subjectLabel, BenchmarkForm } from "../../components/catalog/BenchmarkForm";
import { ConfirmButton } from "../../components/ConfirmButton";
import { Modal } from "../../components/Modal";
import { groupBenchmarks } from "../../lib/benchmarkGrouping";
import { benchmarkRows, type BenchRow } from "../../lib/benchmarkRows";
import { useErrorState } from "../../lib/useErrorState";
import {
  useCatalogBenchmarks,
  useCatalogBuilds,
  useCatalogConfigs,
  useCatalogEngines,
  useCatalogModels,
  useCatalogOfferings,
  useCatalogVariants,
  useCreateCatalogBenchmark,
  useDeleteCatalogBenchmark,
  useProfiles,
  useUpdateCatalogBenchmark,
} from "../../lib/queries";
import type { CatalogBenchmark } from "../../lib/types";

// Benchmarks & Profiling (redesigned from the approved 2026-11-27 mockup): one dense table of every config, and every remote-only model, that
// can be searched, filtered, sorted and compared; the overnight run and a single run's progress sit above it. Row model: lib/benchmarkRows.ts.
// Benchmark rows no row can carry (legacy offering-scoped ones, orphans) stay reachable in folded lists at the bottom.

type EditorState =
  | { mode: "new"; defaultSubject: { type: string; id: number } }
  | { mode: "edit"; benchmark: CatalogBenchmark }
  | null;

const FILTERS = {
  status: ["all", "need", "current", "unsupported"],
  where: ["all", "local", "remote"],
  backend: ["all", "rocm", "vulkan"],
  size: ["all", "small", "medium", "large"],
} as const;
type Filters = { [K in keyof typeof FILTERS]: (typeof FILTERS)[K][number] };

function matches(r: BenchRow, f: Filters, query: string, showHidden: boolean): boolean {
  if (!showHidden && r.hidden) return false;
  if (f.status === "need" && r.state !== "never" && r.state !== "outdated") return false;
  if ((f.status === "current" || f.status === "unsupported") && r.state !== f.status) return false;
  if (f.where !== "all" && r.remote !== (f.where === "remote")) return false;
  if (f.backend !== "all" && r.backend !== f.backend) return false;
  if (f.size !== "all") {
    const gb = r.cells.mem.value;
    if (gb === null) return false;
    if (f.size === "small" ? gb > 32 : f.size === "medium" ? gb <= 32 || gb > 64 : gb <= 64) return false;
  }
  return `${r.name} ${r.sub} ${r.engine} ${r.backend} ${r.providers.join(" ")}`.toLowerCase().includes(query);
}

export function Benchmarks({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const benchmarks = useCatalogBenchmarks();
  const configs = useCatalogConfigs();
  const variants = useCatalogVariants();
  const models = useCatalogModels();
  const offerings = useCatalogOfferings();
  const engines = useCatalogEngines();
  const builds = useCatalogBuilds();
  const profiles = useProfiles();

  const create = useCreateCatalogBenchmark();
  const update = useUpdateCatalogBenchmark();
  const remove = useDeleteCatalogBenchmark();
  const profileController = useProfileRunController();
  const { error, showError, clearError } = useErrorState();

  const [editor, setEditor] = useState<EditorState>(null);
  const [filters, setFilters] = useState<Filters>({ status: "all", where: "all", backend: "all", size: "all" });
  const [query, setQuery] = useState("");
  const [showHidden, setShowHidden] = useState(false);
  const [sort, setSort] = useState<Sort>({ key: "name", dir: 1 });
  const [selected, setSelected] = useState<string[]>([]);
  const [open, setOpen] = useState<string | null>(null);

  const grouped = groupBenchmarks({
    configs: configs.data ?? [],
    variants: variants.data ?? [],
    models: models.data ?? [],
    offerings: offerings.data ?? [],
    benchmarks: benchmarks.data ?? [],
    profiles: profiles.data?.profiles ?? [],
  });
  const rows = benchmarkRows(grouped, engines.data ?? [], builds.data ?? []);
  const shown = sortRows(rows.filter((r) => matches(r, filters, query.trim().toLowerCase(), showHidden)), sort);
  // The comparison keeps a picked row even when a filter hides it from the table.
  const compared = selected.map((k) => rows.find((r) => r.key === k)).filter((r): r is BenchRow => !!r);
  const inState = (state: string) => rows.filter((r) => r.state === state).length;

  function handleSubmit(draft: Partial<CatalogBenchmark>, id?: number) {
    clearError();
    if (id) {
      update.mutate({ id, b: draft }, { onSuccess: () => setEditor(null), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setEditor(null), onError: showError });
    }
  }

  function handleDelete(b: CatalogBenchmark) {
    remove.mutate(b.id, { onError: showError });
  }

  function closeEditor() {
    setEditor(null);
    clearError();
  }

  const anyError = benchmarks.isError || configs.isError;

  return (
    <div className="bench-page">
      <div className="eyebrow" id="benchmarks">{t("benchmarks.page.title")}</div>

      <ProfileRunCard controller={profileController} />
      <ProfileNightCard canAdmin={canAdmin} />

      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {anyError ? (
        <div className="empty-note">{t("benchmarks.page.catalog_unavailable")}</div>
      ) : (
        <>
          <div className="bench-toolbar">
            <input type="search" value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("benchmarks.table.search")} aria-label={t("benchmarks.table.search")} />
            {(Object.keys(FILTERS) as (keyof Filters)[]).map((group) => (
              <div className="bench-seg" key={group} role="group" aria-label={t(`benchmarks.table.filter.${group}.label`)}>
                {FILTERS[group].map((value) => (
                  <button type="button" key={value} className={`tab ${filters[group] === value ? "active" : ""}`} aria-pressed={filters[group] === value}
                    onClick={() => setFilters({ ...filters, [group]: value })}>
                    {t(`benchmarks.table.filter.${group}.${value}`)}
                  </button>
                ))}
              </div>
            ))}
            <label className="bench-check">
              <input type="checkbox" checked={showHidden} onChange={(e) => setShowHidden(e.target.checked)} />
              {t("benchmarks.table.show_hidden")}
            </label>
          </div>
          <div className="bench-summary">
            <span>{t("benchmarks.page.summary", { current: inState("current"), total: rows.filter((r) => !r.remote).length, never: inState("never"), outdated: inState("outdated"), curated: (benchmarks.data ?? []).length })}</span>
            <span>{t("benchmarks.table.shown", { shown: shown.length, total: rows.length })}</span>
          </div>

          {compared.length > 1 && <ComparePanel rows={compared} onClear={() => setSelected([])} />}

          {rows.length === 0 ? (
            <div className="empty-note">{t("benchmarks.page.no_configs")}</div>
          ) : shown.length === 0 ? (
            <div className="empty-note">{t("benchmarks.page.no_match_filter")}</div>
          ) : (
            <BenchmarkTable
              rows={shown}
              sort={sort}
              onSort={(key) => setSort(nextSort(sort, key))}
              selected={selected}
              onSelect={(key) => setSelected(selected.includes(key) ? selected.filter((k) => k !== key) : [...selected, key])}
              open={open}
              onOpen={setOpen}
              canAdmin={canAdmin}
              profileController={profileController}
              deletePending={remove.isPending}
              onEdit={(b) => setEditor({ mode: "edit", benchmark: b })}
              onAdd={(subject) => setEditor({ mode: "new", defaultSubject: subject })}
              onDelete={handleDelete}
            />
          )}
          <div className="bench-legend">{t("benchmarks.table.legend")}</div>

          {grouped.legacyOffering.length > 0 && (
            <details style={{ marginTop: 18 }}>
              <summary>{t("benchmarks.page.legacy_offering_title")} ({grouped.legacyOffering.length})</summary>
              <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
                {t("benchmarks.page.legacy_offering_hint")}
              </div>
              {grouped.legacyOffering.map((b) => (
                <div className="qrow" key={b.id} style={{ alignItems: "flex-start" }}>
                  <span style={{ width: 150, fontSize: 12, fontWeight: 600 }}>{b.metric}</span>
                  <span style={{ width: 70, fontFamily: "var(--mono)", fontSize: 12 }}>{b.value}</span>
                  <span style={{ width: 130, fontSize: 11, color: "var(--text-dim)" }}>
                    {subjectLabel(b.subject_type, b.subject_id, models.data ?? [], variants.data ?? [], configs.data ?? [], offerings.data ?? [])}
                  </span>
                  {canAdmin && (
                    <div style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                      <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => setEditor({ mode: "edit", benchmark: b })}>{t("benchmarks.page.edit_rescope")}</button>
                      <ConfirmButton
                        className="btn"
                        style={{ fontSize: 11, padding: "4px 8px" }}
                        pending={remove.isPending}
                        onConfirm={() => handleDelete(b)}
                        warning={t("benchmarks.page.delete_confirm", { metric: b.metric })}
                      />
                    </div>
                  )}
                </div>
              ))}
            </details>
          )}

          {grouped.orphans.length > 0 && (
            <details style={{ marginTop: 18 }}>
              <summary>{t("benchmarks.page.other_title")} ({grouped.orphans.length})</summary>
              <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
                {t("benchmarks.page.other_hint")}
              </div>
              {grouped.orphans.map((b) => (
                <div className="qrow" key={b.id} style={{ alignItems: "flex-start" }}>
                  <span style={{ width: 150, fontSize: 12, fontWeight: 600 }}>{b.metric}</span>
                  <span style={{ width: 70, fontFamily: "var(--mono)", fontSize: 12 }}>{b.value}</span>
                  <span style={{ width: 130, fontSize: 11, color: "var(--text-dim)" }}>{b.subject_type}: #{b.subject_id}</span>
                  {canAdmin && (
                    <div style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                      <ConfirmButton
                        className="btn"
                        style={{ fontSize: 11, padding: "4px 8px" }}
                        pending={remove.isPending}
                        onConfirm={() => handleDelete(b)}
                        warning={t("benchmarks.page.delete_confirm", { metric: b.metric })}
                      />
                    </div>
                  )}
                </div>
              ))}
            </details>
          )}
        </>
      )}

      {editor && (
        <Modal
          title={editor.mode === "edit" ? t("benchmarks.page.edit_heading", { metric: editor.benchmark.metric }) : t("benchmarks.page.new_heading")}
          onClose={closeEditor}
        >
          <BenchmarkForm
            key={editor.mode === "edit" ? editor.benchmark.id : "new"}
            existing={editor.mode === "edit" ? editor.benchmark : undefined}
            defaultSubject={editor.mode === "new" ? editor.defaultSubject : undefined}
            models={models.data ?? []}
            variants={variants.data ?? []}
            configs={configs.data ?? []}
            offerings={offerings.data ?? []}
            onSubmit={handleSubmit}
            onCancel={closeEditor}
            pending={create.isPending || update.isPending}
            isError={!!error}
          />
        </Modal>
      )}
    </div>
  );
}
