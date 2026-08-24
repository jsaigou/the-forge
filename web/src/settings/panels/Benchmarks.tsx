import { useState } from "react";
import { ConfigBenchmarkGroup } from "../../components/benchmarks/ConfigBenchmarkGroup";
import { ProfileRunCard, useProfileRunController } from "../../components/benchmarks/ProfileRunCard";
import { subjectLabel, BenchmarkForm } from "../../components/catalog/BenchmarkForm";
import { ConfirmButton } from "../../components/ConfirmButton";
import { Icon } from "../../components/Icon";
import { groupBenchmarks } from "../../lib/benchmarkGrouping";
import { useErrorState } from "../../lib/useErrorState";
import {
  useCatalogBenchmarks,
  useCatalogConfigs,
  useCatalogModels,
  useCatalogOfferings,
  useCatalogVariants,
  useCreateCatalogBenchmark,
  useDeleteCatalogBenchmark,
  useProfiles,
  useUpdateCatalogBenchmark,
} from "../../lib/queries";
import type { CatalogBenchmark } from "../../lib/types";

// settings/panels/Benchmarks.tsx — Phase 8 (pre-release feedback sprint):
// "Benchmarks" merged with "Profiling" into one section. The two used to be
// unrelated Settings pages even though profiling PRODUCES exactly the kind
// of data (measured memory, prefill/decode T/s) that belongs next to the
// curated benchmarks it should be compared against, and both are naturally
// organized by the same unit — a config. See docs/investigations at
// /home/testuser/.claude/plans/giggly-singing-parasol.md for the full design.
//
// Regrouped from a flat, all-subject-types list to one block per config
// (own benchmarks + what it inherits from its variant/model, most-specific
// first) — mirrors the Go registry's benchesForConfig union exactly, so
// this view can never show something a card-building call wouldn't also
// show. Models with no local config (remote-only), offering-scoped rows
// (legacy — never reach any card), and orphans (a deleted parent) are
// never silently dropped — see benchmarkGrouping.ts's doc comment.

type EditorState =
  | { mode: "new"; defaultSubject: { type: string; id: number } }
  | { mode: "edit"; benchmark: CatalogBenchmark }
  | null;

type Filter = "all" | "profiled" | "has-benchmarks" | "stale";

export function Benchmarks({ canAdmin }: { canAdmin: boolean }) {
  const benchmarks = useCatalogBenchmarks();
  const configs = useCatalogConfigs();
  const variants = useCatalogVariants();
  const models = useCatalogModels();
  const offerings = useCatalogOfferings();
  const profiles = useProfiles();

  const create = useCreateCatalogBenchmark();
  const update = useUpdateCatalogBenchmark();
  const remove = useDeleteCatalogBenchmark();
  const profileController = useProfileRunController();
  const { error, showError, clearError } = useErrorState();

  const [editor, setEditor] = useState<EditorState>(null);
  const [filter, setFilter] = useState<Filter>("all");

  const grouped = groupBenchmarks({
    configs: configs.data ?? [],
    variants: variants.data ?? [],
    models: models.data ?? [],
    offerings: offerings.data ?? [],
    benchmarks: benchmarks.data ?? [],
    profiles: profiles.data?.profiles ?? [],
  });

  const sortedGroups = [...grouped.groups].sort((a, b) => a.config.name.localeCompare(b.config.name));
  const visibleGroups = sortedGroups.filter((g) => {
    switch (filter) {
      case "profiled": return !!g.profile && !g.profile.stale && g.profile.measured_at > 0;
      case "has-benchmarks": return g.benchmarks.length > 0;
      case "stale": return !!g.profile?.stale;
      default: return true;
    }
  });

  const profiledCount = sortedGroups.filter((g) => g.profile && !g.profile.stale && g.profile.measured_at > 0).length;
  const staleCount = sortedGroups.filter((g) => g.profile?.stale).length;

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

  const anyError = benchmarks.isError || configs.isError;

  return (
    <>
      <div className="eyebrow" id="benchmarks">Benchmarks & Profiling</div>

      <ProfileRunCard controller={profileController} />

      <div className="card" style={{ borderLeft: "3px solid var(--warn, var(--accent))", marginBottom: 12 }}>
        <div style={{ fontSize: 12, color: "var(--text-dim)", lineHeight: 1.55 }}>
          Curated performance/capability scores per config, grouped with the config's own measured
          profile. <b style={{ color: "var(--warn)" }}>Profiling is destructive</b> — if the target
          model isn't already loaded, other running models will be evicted to make room. Mutations
          require admin + Settings assurance.
        </div>
      </div>

      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {anyError ? (
        <div className="empty-note">Catalog not available (503 — store may not be wired).</div>
      ) : (
        <>
          <div style={{ display: "flex", alignItems: "center", gap: 10, marginBottom: 10, flexWrap: "wrap" }}>
            <div style={{ fontSize: 11.5, color: "var(--text-dim)" }}>
              {profiledCount} of {sortedGroups.length} configs profiled · {staleCount} stale · {(benchmarks.data ?? []).length} curated benchmarks
            </div>
            <div style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
              {(["all", "profiled", "has-benchmarks", "stale"] as Filter[]).map((f) => (
                <button
                  key={f}
                  className={`tab ${filter === f ? "active" : ""}`}
                  style={{ fontSize: 11 }}
                  onClick={() => setFilter(f)}
                >
                  {f === "all" ? "All" : f === "profiled" ? "Profiled" : f === "has-benchmarks" ? "Has benchmarks" : "Stale"}
                </button>
              ))}
            </div>
          </div>

          {sortedGroups.length === 0 ? (
            <div className="empty-note">No configs in the catalog — add one in Catalog → Configs.</div>
          ) : visibleGroups.length === 0 ? (
            <div className="empty-note">No configs match this filter.</div>
          ) : (
            visibleGroups.map((g) => (
              <ConfigBenchmarkGroup
                key={g.config.id}
                group={g}
                canAdmin={canAdmin}
                profileController={profileController}
                deletePending={remove.isPending}
                onEditBenchmark={(b) => setEditor({ mode: "edit", benchmark: b })}
                onAddBenchmark={(configId) => setEditor({ mode: "new", defaultSubject: { type: "config", id: configId } })}
                onDeleteBenchmark={handleDelete}
              />
            ))
          )}

          {grouped.remoteOnly.length > 0 && (
            <div style={{ marginTop: 18 }}>
              <div className="eyebrow" style={{ fontSize: 11 }}>Remote-only models</div>
              <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
                No local config — nothing to profile. Benchmarks here are model-scoped only.
              </div>
              {grouped.remoteOnly.map((mg) => (
                <div className="card" key={mg.model.id} style={{ marginBottom: 10 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                    {mg.model.logo && <Icon slug={mg.model.logo} name={mg.model.name} />}
                    <span style={{ fontWeight: 600, fontSize: 13 }}>{mg.model.name}</span>
                    {mg.offerings.length > 0 && (
                      <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
                        served by {mg.offerings.map((o) => o.provider).join(", ")}
                      </span>
                    )}
                  </div>
                  {mg.benchmarks.length === 0 ? (
                    <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 6 }}>No benchmarks recorded.</div>
                  ) : (
                    <div style={{ marginTop: 6 }}>
                      {mg.benchmarks.map((row) => (
                        <div className="qrow" key={row.benchmark.id} style={{ alignItems: "flex-start" }}>
                          <span style={{ width: 150, fontSize: 12, fontWeight: 600 }}>{row.benchmark.metric}</span>
                          <span style={{ width: 70, fontFamily: "var(--mono)", fontSize: 12 }}>{row.benchmark.value}</span>
                          <span style={{ fontSize: 10.5, color: "var(--text-mute)", flex: 1 }}>{row.benchmark.notes || "—"}</span>
                          {canAdmin && (
                            <div style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                              <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => setEditor({ mode: "edit", benchmark: row.benchmark })}>Edit</button>
                              <ConfirmButton
                                className="btn"
                                style={{ fontSize: 11, padding: "4px 8px" }}
                                pending={remove.isPending}
                                onConfirm={() => handleDelete(row.benchmark)}
                                warning={`Delete "${row.benchmark.metric}"?`}
                              />
                            </div>
                          )}
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}

          {grouped.legacyOffering.length > 0 && (
            <div style={{ marginTop: 18 }}>
              <div className="eyebrow" style={{ fontSize: 11 }}>Offering-scoped (legacy)</div>
              <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
                These predate the config-scoped card fix and never surfaced on any card — re-scope to
                model, variant, or config via Edit to make one visible, or leave as historical record.
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
                      <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => setEditor({ mode: "edit", benchmark: b })}>Edit / re-scope</button>
                      <ConfirmButton
                        className="btn"
                        style={{ fontSize: 11, padding: "4px 8px" }}
                        pending={remove.isPending}
                        onConfirm={() => handleDelete(b)}
                        warning={`Delete "${b.metric}"?`}
                      />
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}

          {grouped.orphans.length > 0 && (
            <div style={{ marginTop: 18 }}>
              <div className="eyebrow" style={{ fontSize: 11 }}>Other</div>
              <div style={{ fontSize: 11, color: "var(--text-mute)", marginBottom: 8 }}>
                Points at a subject that no longer exists in the catalog.
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
                        warning={`Delete "${b.metric}"?`}
                      />
                    </div>
                  )}
                </div>
              ))}
            </div>
          )}
        </>
      )}

      {editor && (
        <div className="modal-backdrop" onClick={(e) => { e.stopPropagation(); setEditor(null); clearError(); }}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <h3>{editor.mode === "edit" ? `Edit "${editor.benchmark.metric}"` : "New benchmark"}</h3>
            <BenchmarkForm
              key={editor.mode === "edit" ? editor.benchmark.id : "new"}
              existing={editor.mode === "edit" ? editor.benchmark : undefined}
              defaultSubject={editor.mode === "new" ? editor.defaultSubject : undefined}
              models={models.data ?? []}
              variants={variants.data ?? []}
              configs={configs.data ?? []}
              offerings={offerings.data ?? []}
              onSubmit={handleSubmit}
              onCancel={() => { setEditor(null); clearError(); }}
              pending={create.isPending || update.isPending}
              isError={!!error}
            />
          </div>
        </div>
      )}
    </>
  );
}
