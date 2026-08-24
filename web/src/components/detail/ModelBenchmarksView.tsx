import { useState } from "react";
import { apiErrorMessage } from "../../lib/api";
import {
  useCatalogBenchmarks,
  useCatalogConfigs,
  useCatalogModels,
  useCatalogOfferings,
  useCatalogVariants,
  useCreateCatalogBenchmark,
  useDeleteCatalogBenchmark,
  useModelCards,
  useUpdateCatalogBenchmark,
} from "../../lib/queries";
import type { CatalogBenchmark } from "../../lib/types";
import { BenchmarkForm } from "../catalog/BenchmarkForm";
import { ConfirmButton } from "../ConfirmButton";

function useErrorState() {
  const [error, setError] = useState<string | null>(null);
  return {
    error,
    showError: (e: unknown) => setError(apiErrorMessage(e)),
    clearError: () => setError(null),
  };
}

// ModelBenchmarksView — Sprint D: benchmark add/edit reachable from the
// model detail view's Capabilities section, not just the buried Settings →
// Catalog → Benchmarks tab. Reuses CatalogPanel's BenchmarkForm (same
// preset dropdown, same F7 gate) scoped to this one model via
// useCatalogBenchmarks's server-side subject filter, pre-seeding new rows
// to subject_type="model" — the scope card-building actually reads
// (registry.go's benchesFor union, the Sprint D subject_type-trap fix).
export function ModelBenchmarksView({ modelId }: { modelId: string }) {
  const numericModelId = Number(modelId);
  const modelCards = useModelCards("7d");
  const modelName = modelCards.data?.cards.find((c) => c.id === modelId)?.name ?? `Model #${modelId}`;

  const benchmarks = useCatalogBenchmarks("model", numericModelId);
  const models = useCatalogModels();
  const variants = useCatalogVariants();
  const configs = useCatalogConfigs();
  const offerings = useCatalogOfferings();
  const create = useCreateCatalogBenchmark();
  const update = useUpdateCatalogBenchmark();
  const remove = useDeleteCatalogBenchmark();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = benchmarks.data ?? [];
  const editingBenchmark = editing === "new" ? null : list.find((b) => b.id === editing);

  function handleSubmit(draft: Partial<CatalogBenchmark>, id?: number) {
    clearError();
    // Sprint K: delayed close so BenchmarkForm's SaveButton flash has time
    // to paint — see the matching comment on CatalogPanel.tsx's forms.
    if (id) {
      update.mutate({ id, b: draft }, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    }
  }

  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  return (
    <div className="detail-view">
      <h3 style={{ marginBottom: 2 }}>Capability benchmarks</h3>
      <div className="mmaker" style={{ marginBottom: 16 }}>{modelName}</div>

      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {editing !== null ? (
        <BenchmarkForm
          key={editingBenchmark?.id ?? "new"}
          existing={editingBenchmark ?? undefined}
          defaultSubject={{ type: "model", id: numericModelId }}
          models={models.data ?? []}
          variants={variants.data ?? []}
          configs={configs.data ?? []}
          offerings={offerings.data ?? []}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          {benchmarks.isError ? (
            <div className="empty-note">Catalog not available (503 — store may not be wired).</div>
          ) : list.length === 0 ? (
            <div className="empty-note">No capability benchmarks recorded for this model yet.</div>
          ) : (
            list.map((b) => (
              <div className="qrow" key={b.id} style={{ alignItems: "flex-start" }}>
                <span style={{ width: 140, fontSize: 12, fontWeight: 600 }}>{b.metric}</span>
                <span style={{ width: 70, fontFamily: "var(--mono)", fontSize: 12 }}>{b.value}</span>
                <span style={{ width: 140, fontSize: 11, color: "var(--text-dim)" }}>{b.notes || "—"}</span>
                <span style={{ fontSize: 10.5, color: "var(--text-mute)", flex: 1, fontFamily: "var(--mono)" }}>
                  {b.source_url ? <a href={b.source_url} target="_blank" rel="noopener noreferrer" style={{ color: "var(--cool)" }}>{b.source_date || "link"}</a> : b.source_date || "—"}
                </span>
                <div className="actions" style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                  <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(b.id); clearError(); }}>Edit</button>
                  <ConfirmButton
                    className="btn"
                    style={{ fontSize: 11, padding: "4px 8px" }}
                    pending={remove.isPending}
                    onConfirm={() => handleDelete(b.id)}
                    warning={`Delete benchmark "${b.metric}"?`}
                  />
                </div>
              </div>
            ))
          )}
          <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
            + Add benchmark
          </button>
        </>
      )}
    </div>
  );
}
