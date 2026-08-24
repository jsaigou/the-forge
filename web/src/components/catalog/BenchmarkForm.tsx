import { useState } from "react";
import { BENCHMARK_REFS } from "../../lib/benchmarks";
import type { CatalogBenchmark, CatalogConfig, CatalogModel, CatalogOffering, CatalogVariant } from "../../lib/types";
import { SaveButton } from "../SaveButton";

// subjectLabel resolves a benchmark's (subject_type, subject_id) pair to a
// human name, falling back to a bare #id when the subject was deleted out
// from under an orphaned benchmark row.
export function subjectLabel(
  subjectType: string,
  subjectId: number,
  models: CatalogModel[],
  variants: CatalogVariant[],
  configs: CatalogConfig[],
  offerings: CatalogOffering[],
): string {
  switch (subjectType) {
    case "model":
      return models.find((m) => m.id === subjectId)?.name ?? `#${subjectId}`;
    case "variant":
      return variants.find((v) => v.id === subjectId)?.name ?? `#${subjectId}`;
    case "config":
      return configs.find((c) => c.id === subjectId)?.name ?? `#${subjectId}`;
    case "offering":
      return offerings.find((o) => o.id === subjectId)?.wire_model ?? `#${subjectId}`;
    default:
      return `#${subjectId}`;
  }
}

// BenchmarkForm — moved out of CatalogPanel.tsx (Phase 8, pre-release
// feedback sprint) so the new merged Benchmarks & Profiling settings panel
// doesn't have to import a 2000+ line catalog CRUD file just for this form.
// Reused verbatim (by import path only) from ModelBenchmarksView and the
// new per-config benchmark groups.
//
// defaultSubject lets a caller (e.g. ModelBenchmarksView, Sprint D; the new
// per-config groups, Phase 8) preset the subject picker for a new
// benchmark — only used when existing is unset; editing an existing row
// always keeps its own subject.
//
// subject_type "offering" is deliberately NOT a selectable option for new
// rows — registry.go's loadSnapshot never indexed offering-scoped
// benchmarks, so they reach no card and never will (Phase 8's config-scoped
// union only widened model/variant/config). It stays selectable ONLY when
// editing a row that is already offering-scoped (the backend's
// validateBenchmarkUpdate grandfathers exactly this case), so a
// pre-existing legacy row doesn't become permanently unsavable through the
// only UI that can reach it.
export function BenchmarkForm({
  existing,
  defaultSubject,
  models,
  variants,
  configs,
  offerings,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogBenchmark;
  defaultSubject?: { type: string; id: number };
  models: CatalogModel[];
  variants: CatalogVariant[];
  configs: CatalogConfig[];
  offerings: CatalogOffering[];
  onSubmit: (draft: Partial<CatalogBenchmark>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const [metric, setMetric] = useState(existing?.metric ?? "");
  const [value, setValue] = useState(existing?.value ?? "");
  const [source, setSource] = useState(existing?.source ?? "published");
  const [sourceUrl, setSourceUrl] = useState(existing?.source_url ?? "");
  const [sourceDate, setSourceDate] = useState(existing?.source_date ?? "");
  const [subjectType, setSubjectType] = useState(existing?.subject_type ?? defaultSubject?.type ?? "model");
  const [subjectId, setSubjectId] = useState(existing?.subject_id ?? defaultSubject?.id ?? 0);
  const [notes, setNotes] = useState(existing?.notes ?? "");

  const isLegacyOffering = existing?.subject_type === "offering";
  const f7Violation = source === "published" && (!sourceUrl || !sourceDate);

  // Sprint D: notes is what CapabilityBar's normalizeScore() actually keys
  // off (not metric — metric only drives the visible label/ID), so a
  // hand-typed name that doesn't exactly match a BENCHMARK_REFS key
  // silently renders unnormalized with no feedback. The preset select below
  // writes the exact key into notes so that trap can't happen for the
  // benchmarks we have a cited ceiling for.
  const presetKeys = Object.keys(BENCHMARK_REFS);
  const activePreset = presetKeys.includes(notes) ? notes : "__custom__";
  const activeRef = activePreset !== "__custom__" ? BENCHMARK_REFS[activePreset] : undefined;

  function selectPreset(key: string) {
    if (key === "__custom__") return;
    setNotes(key);
    if (!metric.trim()) setMetric(BENCHMARK_REFS[key].defaultMetric);
  }

  function submit() {
    onSubmit(
      {
        metric,
        value,
        source,
        source_url: sourceUrl,
        source_date: sourceDate,
        subject_type: subjectType,
        subject_id: subjectId,
        notes,
      },
      existing?.id,
    );
  }

  const subjectOptions = (() => {
    switch (subjectType) {
      case "model": return models.map((m) => ({ id: m.id, label: m.name }));
      case "variant": return variants.map((v) => ({ id: v.id, label: v.name }));
      case "config": return configs.map((c) => ({ id: c.id, label: c.name }));
      case "offering": return offerings.map((o) => ({ id: o.id, label: o.wire_model }));
      default: return [];
    }
  })();

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Benchmark
        <select value={activePreset} onChange={(e) => selectPreset(e.target.value)}>
          {presetKeys.map((k) => <option key={k} value={k}>{k}</option>)}
          <option value="__custom__">Custom…</option>
        </select>
      </label>
      {activeRef ? (
        <div className="empty-note" style={{ gridColumn: "1 / -1", marginTop: -4 }}>
          Frontier ceiling ~{(activeRef.ceiling * 100).toFixed(0)}% ({activeRef.source}, {activeRef.asOf}) — scores normalize against this.
        </div>
      ) : (
        <div className="empty-note" style={{ gridColumn: "1 / -1", marginTop: -4 }}>
          Custom benchmark name — no cited frontier ceiling, so the card will show the raw score unnormalized.
        </div>
      )}
      <label className="form-row">Metric *
        <input value={metric} placeholder="reasoning" onChange={(e) => setMetric(e.target.value)} />
      </label>
      <label className="form-row">Value *
        <input value={value} placeholder="0.843" onChange={(e) => setValue(e.target.value)} />
      </label>
      <label className="form-row">Source *
        <select value={source} onChange={(e) => setSource(e.target.value)}>
          <option value="published">published</option>
          <option value="self_measured">self_measured</option>
          <option value="provider_reported">provider_reported</option>
        </select>
      </label>
      <label className="form-row">Subject type *
        <select value={subjectType} onChange={(e) => { setSubjectType(e.target.value); setSubjectId(0); }}>
          <option value="model">model — capability scores, shown on model + config cards</option>
          <option value="variant">variant — performance metrics (decode_tps, prefill_tps, safe_memory_bytes), shown on config cards</option>
          <option value="config">config — capability or performance scores specific to this launch recipe, shown on this config's card</option>
          {isLegacyOffering && (
            <option value="offering">offering — legacy, not selectable for new rows; never shown on a card</option>
          )}
        </select>
      </label>
      {subjectType === "offering" && (
        <div className="empty-note" style={{ gridColumn: "1 / -1", marginTop: -4 }}>
          Offering-scoped benchmarks predate the config-scoped card fix and never surfaced on any card — re-scope this row to model, variant, or config to make it visible, or leave it as-is to keep the historical record.
        </div>
      )}
      {subjectType === "variant" && (
        <div className="empty-note" style={{ gridColumn: "1 / -1", marginTop: -4 }}>
          Known performance metric names: <code>decode_tps</code>, <code>prefill_tps</code> (both tok/s), <code>safe_memory_bytes</code> — type one exactly into Metric below. Unlike the capability preset above, these are raw measurements, not normalized against a ceiling.
        </div>
      )}
      <label className="form-row">Subject *
        <select value={subjectId} onChange={(e) => setSubjectId(Number(e.target.value))}>
          <option value={0}>— select —</option>
          {subjectOptions.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
        </select>
      </label>
      <label className="form-row">Source URL {source === "published" && "*"}
        <input value={sourceUrl} placeholder="https://…" onChange={(e) => setSourceUrl(e.target.value)} />
      </label>
      <label className="form-row">Source date {source === "published" && "*"}
        <input type="date" value={sourceDate} onChange={(e) => setSourceDate(e.target.value)} />
      </label>
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>
        {activePreset === "__custom__" ? "Benchmark name (custom)" : "Notes"}
        <textarea value={notes} rows={2} style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 12, resize: "vertical" }} onChange={(e) => setNotes(e.target.value)} />
      </label>
      {f7Violation && (
        <div className="error-note" style={{ gridColumn: "1 / -1" }}>
          F7 gate: published benchmarks require both source URL and source date.
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton
          pending={pending}
          isError={isError}
          disabled={pending || !metric || !value || !subjectId || f7Violation}
          onClick={submit}
          label={existing ? "Save" : "Create"}
        />
      </div>
    </div>
  );
}
