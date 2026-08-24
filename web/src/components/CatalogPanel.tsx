// CatalogPanel.tsx — Sprint MODEL CATALOG Phase 5 (docs/v5-modes-config-editable.md §Phase 5).
// Settings → Catalog tab: CRUD UI for Configs, Offerings, Benchmarks, Notes,
// Services, Models, Variants, and Artifacts. Replaces the read-only Modes view.
//
// All mutating routes are requireRole(admin) + requireAssurance(page.settings)
// on the backend; non-admin viewers see a read-only render and the forms are
// hidden. The InvalidateConfig hook fires after every mutation so the
// merged-config seam picks up changes immediately.

import { useEffect, useState } from "react";
import { countryFlag, formatGB } from "../lib/format";
import { familyInheritedIcon, modelInheritedIcon } from "../lib/iconInheritance";
import { preferredOfferingIds } from "../lib/offeringPreference";
import { providerIconSlug } from "../lib/providerPresets";
import { useErrorState } from "../lib/useErrorState";
import { subjectLabel } from "./catalog/BenchmarkForm";
import { ConfirmButton } from "./ConfirmButton";
import { Icon } from "./Icon";
import { IconPicker } from "./IconPicker";
import { SaveButton } from "./SaveButton";
import {
  useCatalogArtifacts,
  useCatalogBuilds,
  useCatalogConfigs,
  useCatalogEngines,
  useCatalogFamilies,
  useCatalogFormats,
  useCatalogGenealogies,
  useCatalogModels,
  useCatalogNotes,
  useCatalogOfferings,
  useCatalogQuantizations,
  useCatalogServices,
  useCatalogVariants,
  useCreateCatalogConfig,
  useCreateCatalogFamily,
  useCreateCatalogGenealogy,
  useCreateCatalogModel,
  useCreateCatalogNote,
  useCreateCatalogOffering,
  useCreateCatalogService,
  useCreateCatalogVariant,
  useDeleteCatalogConfig,
  useDeleteCatalogFamily,
  useDeleteCatalogGenealogy,
  useDeleteCatalogModel,
  useDeleteCatalogNote,
  useDeleteCatalogOffering,
  useDeleteCatalogService,
  useDeleteCatalogVariant,
  useModelFiles,
  useProviders,
  useUpdateCatalogConfig,
  useUpdateCatalogFamily,
  useUpdateCatalogGenealogy,
  useUpdateCatalogModel,
  useUpdateCatalogOffering,
  useUpdateCatalogService,
  useUploadConfigIcon,
  useUploadFamilyIcon,
  useUploadGenealogyIcon,
  useUploadModelIcon,
} from "../lib/queries";
import type {
  CatalogArtifact,
  CatalogBuild,
  CatalogConfig,
  CatalogEngine,
  CatalogFamily,
  CatalogGenealogy,
  CatalogModel,
  CatalogModelFile,
  CatalogOffering,
  CatalogService,
  CatalogVariant,
} from "../lib/types";

export type SubTab = "configs" | "offerings" | "models" | "taxonomy" | "notes" | "services";

const SUB_TABS: { key: SubTab; label: string }[] = [
  { key: "configs", label: "Configs" },
  { key: "offerings", label: "Offerings" },
  { key: "models", label: "Models" },
  // Sprint I: genealogy/family editing — the backend has had full CRUD for
  // both since 2026-07-29, this is the frontend that was never built.
  { key: "taxonomy", label: "Model Family" },
  { key: "notes", label: "Notes" },
  { key: "services", label: "Services" },
  // Sprint 12 (was H) Phase 5: Benchmarks and Profiling promoted to their
  // own top-level Settings sections — both were buried two levels deep
  // here before. Phase 8 (pre-release feedback sprint) then merged the two
  // into one section (settings/panels/Benchmarks.tsx): BenchmarksSection
  // and BenchmarkForm both moved out of this file entirely (the latter to
  // components/catalog/BenchmarkForm.tsx), and the standalone
  // components/ProfilingPanel.tsx was deleted — see Benchmarks.tsx's
  // header comment for why.
];

// tab/onTabChange make this a controlled/uncontrolled hybrid (Sprint 12
// Phase 5): Settings.tsx passes an initial tab when redirecting a stale
// deep link (the two retired sub-tabs above) or deep-linking into a
// specific catalog area, and is notified of in-panel tab switches so the
// URL's anchor half can track them. Internal-click behavior is unchanged
// when neither prop is passed (every other consumer, and this component's
// own history before this phase).
export function CatalogPanel({ canAdmin, tab: propTab, onTabChange }: { canAdmin: boolean; tab?: SubTab; onTabChange?: (tab: SubTab) => void }) {
  const [internalTab, setInternalTab] = useState<SubTab>(propTab ?? "configs");
  const tab = propTab ?? internalTab;

  useEffect(() => {
    if (propTab && propTab !== internalTab) setInternalTab(propTab);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [propTab]);

  function selectTab(t: SubTab) {
    setInternalTab(t);
    onTabChange?.(t);
  }

  return (
    <>
      <div className="eyebrow">Catalog</div>
      <div className="card">
        <div style={{ fontSize: 12, color: "var(--text-dim)", marginBottom: 12, lineHeight: 1.55 }}>
          The model database — Configs (local launch recipes), Offerings (remote, per-Provider),
          Model Family, Notes, and managed Services. Replaces the read-only Modes view. Mutations
          require admin + Settings assurance.
        </div>
        <div className="tabs" style={{ marginBottom: 14, display: "inline-flex" }}>
          {SUB_TABS.map((t) => (
            <button
              key={t.key}
              className={`tab ${tab === t.key ? "active" : ""}`}
              onClick={() => selectTab(t.key)}
            >
              {t.label}
            </button>
          ))}
        </div>

        {tab === "configs" && <ConfigsSection canAdmin={canAdmin} />}
        {tab === "offerings" && <OfferingsSection canAdmin={canAdmin} />}
        {tab === "models" && <ModelsSection canAdmin={canAdmin} />}
        {tab === "taxonomy" && <TaxonomySection canAdmin={canAdmin} />}
        {tab === "notes" && <NotesSection canAdmin={canAdmin} />}
        {tab === "services" && <ServicesSection canAdmin={canAdmin} />}
      </div>
    </>
  );
}

// ── Helpers ──────────────────────────────────────────────────────────────────

// useErrorState moved to lib/useErrorState.ts (Sprint 12 Phase 0) so other
// settings sections can share it.

function extraArgsToText(args: string[]): string {
  return args.join("\n");
}

function extraArgsFromText(text: string): string[] {
  return text
    .split("\n")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

// ── Configs ──────────────────────────────────────────────────────────────────

function ConfigsSection({ canAdmin }: { canAdmin: boolean }) {
  const configs = useCatalogConfigs();
  const variants = useCatalogVariants();
  const artifacts = useCatalogArtifacts();
  const engines = useCatalogEngines();
  const builds = useCatalogBuilds();
  const models = useCatalogModels();
  const families = useCatalogFamilies();
  const genealogies = useCatalogGenealogies();
  const modelFiles = useModelFiles();
  const create = useCreateCatalogConfig();
  const update = useUpdateCatalogConfig();
  const remove = useDeleteCatalogConfig();
  const uploadIcon = useUploadConfigIcon();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const [showFiles, setShowFiles] = useState(false);
  const { error, showError, clearError } = useErrorState();

  const list = configs.data ?? [];
  const variantList = variants.data ?? [];
  const artifactList = artifacts.data ?? [];
  const engineList = engines.data ?? [];
  const buildList = builds.data ?? [];
  const modelList = models.data ?? [];
  const familyList = families.data ?? [];
  const genealogyList = genealogies.data ?? [];

  const editingConfig = editing === "new" ? null : list.find((c) => c.id === editing);

  function handleSubmit(draft: Partial<CatalogConfig>, id?: number) {
    clearError();
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    if (id) {
      update.mutate({ id, c: draft }, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    } else {
      create.mutate(draft, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    }
  }

  function handleIconUpload(id: number, file: File, dark = false) {
    uploadIcon.mutate({ id, file, dark }, { onError: showError });
  }
  // Same full-replace shape as ModelsSection's handleIconSelect/Clear —
  // UpdateConfig requires every field, so round-trip the existing config
  // (the same pattern ConfigEditView's submit() already follows for
  // parallel/fingerprint) with only logo/logo_dark changed.
  function handleIconSelect(id: number, c: CatalogConfig, slug: string, dark = false) {
    update.mutate({ id, c: dark ? { ...c, logo_dark: slug } : { ...c, logo: slug } }, { onError: showError });
  }
  function handleIconClear(id: number, c: CatalogConfig, dark = false) {
    update.mutate({ id, c: dark ? { ...c, logo_dark: "" } : { ...c, logo: "" } }, { onError: showError });
  }

  // Sprint K: the confirm() gate moved into ConfirmButton itself (arm →
  // confirm, replacing window.confirm at the operator's call) — this just
  // fires the mutation now. Also drops the `loaded`/force-delete parameter
  // this always had: its one real call site below never actually passed a
  // third argument, so `force` was dead on every invocation before this
  // change too — not something this migration introduces.
  function handleDelete(id: number) {
    remove.mutate({ id }, { onError: showError, onSuccess: () => setEditing(null) });
  }

  if (configs.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {editing !== null ? (
        <ConfigForm
          key={editingConfig?.id ?? "new"}
          existing={editingConfig ?? undefined}
          variants={variantList}
          artifacts={artifactList}
          engines={engineList}
          builds={buildList}
          models={modelList}
          families={familyList}
          genealogies={genealogyList}
          modelFiles={modelFiles.data ?? []}
          showFiles={showFiles}
          onToggleFiles={() => setShowFiles(!showFiles)}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
          onIconUpload={handleIconUpload}
          onIconSelect={handleIconSelect}
          onIconClear={handleIconClear}
        />
      ) : (
        <>
          <div style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em", padding: "8px 0 4px" }}>
            {list.length} config{list.length === 1 ? "" : "s"}
          </div>
          {list.length === 0 && <div className="empty-note">No configs. Create one to get started.</div>}
          {list.map((c) => {
            const v = variantList.find((v) => v.id === c.variant_id);
            const m = v ? modelList.find((m) => m.id === v.model_id) : undefined;
            return (
              <div className="qrow" key={c.id} style={{ flexDirection: "column", alignItems: "stretch", gap: 4 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 10 }}>
                  <span style={{ fontFamily: "var(--mono)", fontSize: 12.5 }}>
                    {m?.logo && <Icon slug={m.logo} name={m.name} />}
                    {c.name}
                  </span>
                  {c.is_default && <span className="chip">default</span>}
                  <span className="chip" style={{ color: c.status === "verified" ? "var(--ok)" : "var(--text-mute)" }}>
                    {c.status}
                  </span>
                  <span className="chip">{c.visibility}</span>
                  {canAdmin && (
                    <div className="actions" style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                      <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(c.id); clearError(); }}>Edit</button>
                      <ConfirmButton
                        className="btn"
                        style={{ fontSize: 11, padding: "4px 8px" }}
                        pending={remove.isPending}
                        onConfirm={() => handleDelete(c.id)}
                        warning={`Delete config "${c.name}"?`}
                      />
                    </div>
                  )}
                </div>
                <div style={{ fontSize: 11, color: "var(--text-dim)", fontFamily: "var(--mono)" }}>
                  {m ? `${m.name} / ` : ""}{v?.name ?? `#${c.variant_id}`}
                  {" · "}n_ctx {c.n_ctx.toLocaleString()}
                  {" · "}parallel {c.parallel}
                  {c.extra_args.length > 0 && <span style={{ color: "var(--text-mute)" }}>{" · "}{c.extra_args.join(" ")}</span>}
                </div>
              </div>
            );
          })}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); setShowFiles(false); }}>
              + New config
            </button>
          )}
        </>
      )}
    </>
  );
}

// Exported (Console usability pass #3, 2026-07-30): the config gallery's
// Edit button reuses this verbatim rather than duplicating a second form
// for the same fields — see ConfigEditModal.tsx.
export function ConfigForm({
  existing,
  variants,
  artifacts,
  engines,
  builds,
  models,
  families,
  genealogies,
  modelFiles,
  showFiles,
  onToggleFiles,
  onSubmit,
  onCancel,
  pending,
  isError = false,
  onIconUpload,
  onIconSelect,
  onIconClear,
}: {
  existing?: CatalogConfig;
  variants: CatalogVariant[];
  artifacts: CatalogArtifact[];
  engines: CatalogEngine[];
  builds: CatalogBuild[];
  models: CatalogModel[];
  families: CatalogFamily[];
  genealogies: CatalogGenealogy[];
  modelFiles: CatalogModelFile[];
  showFiles: boolean;
  onToggleFiles: () => void;
  onSubmit: (draft: Partial<CatalogConfig>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
  // Icon controls are optional — the config gallery's other ConfigForm
  // caller (if any) may not wire icon mutations; Sprint I's own caller
  // (ConfigsSection) always does.
  onIconUpload?: (id: number, file: File, dark?: boolean) => void;
  onIconSelect?: (id: number, c: CatalogConfig, slug: string, dark?: boolean) => void;
  onIconClear?: (id: number, c: CatalogConfig, dark?: boolean) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [variantId, setVariantId] = useState(existing?.variant_id ?? 0);
  const [weightArtifactId, setWeightArtifactId] = useState(existing?.weight_artifact_id ?? 0);
  const [engineId, setEngineId] = useState(existing?.engine_id ?? 0);
  const [buildId, setBuildId] = useState(existing?.build_id ?? 0);
  const [mmprojArtifactId, setMmprojArtifactId] = useState(existing?.mmproj_artifact_id ?? 0);
  const [nCtx, setNCtx] = useState(existing?.n_ctx ?? 0);
  const [parallel, setParallel] = useState(existing?.parallel ?? 1);
  const [extraArgsText, setExtraArgsText] = useState(existing ? extraArgsToText(existing.extra_args) : "");
  const [status, setStatus] = useState(existing?.status ?? "unverified");
  const [visibility, setVisibility] = useState(existing?.visibility ?? "visible");
  const [isDefault, setIsDefault] = useState(existing?.is_default ?? false);
  // Sprint J1: existing.modalities is undefined when the config has no
  // override (server omits the field via `omitzero` — see
  // configJSON.Modalities's doc comment) and an array (even []) when it
  // does. Default to inherit unless an override already exists.
  const [modalitiesMode, setModalitiesMode] = useState<"inherit" | "override">(
    existing?.modalities !== undefined ? "override" : "inherit",
  );
  const [overrideVision, setOverrideVision] = useState(existing?.modalities?.includes("vision") ?? false);
  const [overrideAudio, setOverrideAudio] = useState(existing?.modalities?.includes("audio") ?? false);

  const variant = variants.find((v) => v.id === variantId);
  const weightArtifacts = artifacts.filter((a) => a.variant_id === variantId && a.artifact_type === "weight");
  const mmprojArtifacts = artifacts.filter((a) => a.artifact_type === "mmproj");
  const engineBuilds = builds.filter((b) => b.engine_id === engineId);
  // Client-side mirror of registry.resolveModalities' derive branch (server
  // is still the source of truth on save) — lets the inherit-mode hint show
  // what this config would actually get without waiting on a round-trip.
  const model = variant ? models.find((m) => m.id === variant.model_id) : undefined;
  const derivedModalities = mmprojArtifactId === 0 ? ["text"] : (model?.modalities ?? ["text"]);

  function submit() {
    onSubmit(
      {
        name,
        variant_id: variantId,
        weight_artifact_id: weightArtifactId,
        engine_id: engineId,
        build_id: buildId,
        mmproj_artifact_id: mmprojArtifactId,
        n_ctx: nCtx,
        parallel,
        extra_args: extraArgsFromText(extraArgsText),
        status,
        visibility,
        is_default: isDefault,
        // Carried through unchanged — same reasoning as ModelForm's submit()
        // above (Sprint I; UpdateConfig is full-replace).
        logo: existing?.logo ?? "",
        logo_dark: existing?.logo_dark ?? "",
        modalities:
          modalitiesMode === "override"
            ? ["text", ...(overrideVision ? ["vision"] : []), ...(overrideAudio ? ["audio"] : [])]
            : undefined,
      },
      existing?.id,
    );
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Name *
        <input value={name} placeholder="qwen36" onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="form-row">Variant *
        <select value={variantId} onChange={(e) => { const v = Number(e.target.value); setVariantId(v); setWeightArtifactId(0); }}>
          <option value={0}>— select —</option>
          {variants.map((v) => {
            const m = models.find((m) => m.id === v.model_id);
            return (
              <option key={v.id} value={v.id}>
                {m ? `${m.name} / ` : ""}{v.name}
              </option>
            );
          })}
        </select>
      </label>
      <label className="form-row">Weight artifact *
        <div style={{ display: "flex", gap: 6 }}>
          <select value={weightArtifactId} onChange={(e) => setWeightArtifactId(Number(e.target.value))} style={{ flex: 1 }}>
            <option value={0}>— select —</option>
            {weightArtifacts.map((a) => (
              <option key={a.id} value={a.id}>
                {a.file_path}{a.shard_set_id ? " (shard set)" : ""}
              </option>
            ))}
            {weightArtifacts.length === 0 && variant && (
              <option value={0} disabled>No weight artifacts for this variant</option>
            )}
          </select>
          <button className="btn" style={{ fontSize: 11, padding: "4px 8px", whiteSpace: "nowrap" }} onClick={onToggleFiles}>
            {showFiles ? "Hide files" : "Browse files"}
          </button>
        </div>
      </label>
      <label className="form-row">Engine *
        <select value={engineId} onChange={(e) => { const v = Number(e.target.value); setEngineId(v); setBuildId(0); }}>
          <option value={0}>— select —</option>
          {engines.map((e) => <option key={e.id} value={e.id}>{e.name}</option>)}
        </select>
      </label>
      <label className="form-row">Build
        <select value={buildId} onChange={(e) => setBuildId(Number(e.target.value))}>
          <option value={0}>— none —</option>
          {engineBuilds.map((b) => <option key={b.id} value={b.id}>{b.name}</option>)}
        </select>
      </label>
      <label className="form-row">mmproj artifact
        <select value={mmprojArtifactId} onChange={(e) => setMmprojArtifactId(Number(e.target.value))}>
          <option value={0}>— none —</option>
          {mmprojArtifacts.map((a) => (
            <option key={a.id} value={a.id}>{a.file_path}</option>
          ))}
        </select>
      </label>
      <div className="form-row" style={{ gridColumn: "1 / -1" }}>
        Modalities
        <div style={{ display: "flex", gap: 14, alignItems: "center", flexWrap: "wrap" }}>
          <select value={modalitiesMode} onChange={(e) => setModalitiesMode(e.target.value as "inherit" | "override")}>
            <option value="inherit">Inherit from model</option>
            <option value="override">Override</option>
          </select>
          {modalitiesMode === "inherit" ? (
            <span className="chip" style={{ opacity: 0.7 }}>
              inherited: {derivedModalities.join(", ")}
              {mmprojArtifactId === 0 && model && model.modalities.some((m) => m !== "text") ? " (no mmproj linked)" : ""}
            </span>
          ) : (
            <>
              <span className="chip" style={{ opacity: 0.6 }}>Text (always)</span>
              <label style={{ display: "flex", alignItems: "center", gap: 6, fontWeight: 400 }}>
                <input type="checkbox" checked={overrideVision} onChange={(e) => setOverrideVision(e.target.checked)} />
                Vision
              </label>
              <label style={{ display: "flex", alignItems: "center", gap: 6, fontWeight: 400 }}>
                <input type="checkbox" checked={overrideAudio} onChange={(e) => setOverrideAudio(e.target.checked)} />
                Audio
              </label>
            </>
          )}
        </div>
      </div>
      <label className="form-row">Context (n_ctx)
        <input type="number" min={0} value={nCtx} placeholder="131072" onChange={(e) => setNCtx(Number(e.target.value))} />
      </label>
      <label className="form-row">Parallel
        <input type="number" min={1} value={parallel} onChange={(e) => setParallel(Number(e.target.value))} />
      </label>
      <label className="form-row">Status
        <select value={status} onChange={(e) => setStatus(e.target.value)}>
          <option value="unverified">unverified</option>
          <option value="verified">verified</option>
        </select>
      </label>
      <label className="form-row">Visibility
        <select value={visibility} onChange={(e) => setVisibility(e.target.value)}>
          <option value="visible">visible</option>
          <option value="hidden">hidden</option>
        </select>
      </label>
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>Extra args (one ARGV TOKEN per line — not one flag+value per line)
        <textarea
          value={extraArgsText}
          placeholder="--ctx-checkpoints&#10;16&#10;--swa-full"
          rows={3}
          style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontFamily: "var(--mono)", fontSize: 12, resize: "vertical" }}
          onChange={(e) => setExtraArgsText(e.target.value)}
        />
        {/* BUG-2 (Sprint B, docs/v5-prerelease-readiness.md): the launcher
            (/usr/local/lib/forge/start-a1.sh) reads this file with
            `mapfile -t` — one argv element per line, no shell word-splitting.
            "--ctx-checkpoints 16" on ONE line becomes a single malformed
            token llama-server rejects at startup, not two flags. A flag and
            its value must each be their own line. */}
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 4 }}>
          The launcher reads this file one argv token per line — a flag and its value must each be on their own line (e.g. "--parallel" then "1" on the next line), not "--parallel 1" together.
        </div>
      </label>
      <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
        <input type="checkbox" checked={isDefault} onChange={(e) => setIsDefault(e.target.checked)} />
        Default config for this variant
      </label>

      {existing && onIconSelect && onIconClear && onIconUpload && (
        <div className="form-row" style={{ gridColumn: "1 / -1" }}>
          Icon (override — inherits from the model/family/genealogy chain when unset)
          <IconPicker
            value={existing.logo}
            valueDark={existing.logo_dark}
            inherited={variant ? modelInheritedIcon(variant.model_id, models, families, genealogies) : null}
            onSelect={(slug, dark) => onIconSelect(existing.id, existing, slug, dark)}
            onClear={(dark) => onIconClear(existing.id, existing, dark)}
            onUpload={(file, dark) => onIconUpload(existing.id, file, dark)}
          />
        </div>
      )}

      {showFiles && (
        <div style={{ gridColumn: "1 / -1", maxHeight: 280, overflow: "auto", border: "1px solid var(--border)", borderRadius: 8, padding: 8 }}>
          <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 6, textTransform: "uppercase", letterSpacing: ".06em" }}>
            Model files on disk ({modelFiles.length})
          </div>
          {modelFiles.length === 0 && <div className="empty-note">No GGUF files found in models dir.</div>}
          {modelFiles.map((f) => (
            <div key={f.path} style={{ display: "flex", gap: 8, fontSize: 11, padding: "4px 0", borderBottom: "1px solid var(--border)", fontFamily: "var(--mono)" }}>
              <span style={{ flex: 1, color: "var(--text-dim)" }}>{f.path}</span>
              <span style={{ color: "var(--text-mute)" }}>{formatGB(f.size_bytes, 1)} GB</span>
              {f.arch && <span style={{ color: "var(--cool)" }}>{f.arch}</span>}
              {f.trained_ctx > 0 && <span style={{ color: "var(--text-mute)" }}>ctx:{f.trained_ctx.toLocaleString()}</span>}
              {f.is_shard_set && <span className="chip">shard set</span>}
            </div>
          ))}
          {variant && (
            <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 8 }}>
              Create artifacts for these files under the Variants section (select variant #{variant.id}).
            </div>
          )}
        </div>
      )}

      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton
          pending={pending}
          isError={isError}
          disabled={pending || !name || !variantId || !weightArtifactId || !engineId}
          onClick={submit}
          label={existing ? "Save" : "Create"}
        />
      </div>
    </div>
  );
}

// ── Offerings ────────────────────────────────────────────────────────────────

function OfferingsSection({ canAdmin }: { canAdmin: boolean }) {
  const offerings = useCatalogOfferings();
  const models = useCatalogModels();
  const variants = useCatalogVariants();
  const providers = useCatalogProviders();
  const create = useCreateCatalogOffering();
  const update = useUpdateCatalogOffering();
  const remove = useDeleteCatalogOffering();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = offerings.data ?? [];
  const modelList = models.data ?? [];
  const variantList = variants.data ?? [];
  const providerList = providers;

  const editingOffering = editing === "new" ? null : list.find((o) => o.id === editing);

  // Which offering a0 presents for each model (the router's PRIMARY) —
  // shared with Settings → Routing's model→provider map so the badge can
  // never drift between the two UIs (see lib/offeringPreference.ts).
  const preferredIds = preferredOfferingIds(list, (name) => providerList.find((p) => p.name === name)?.enabled ?? true);

  function handleSubmit(draft: Partial<CatalogOffering>, id?: number) {
    clearError();
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    if (id) {
      update.mutate({ id, o: draft }, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    } else {
      create.mutate(draft, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    }
  }

  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (offerings.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {editing !== null ? (
        <OfferingForm
          key={editingOffering?.id ?? "new"}
          existing={editingOffering ?? undefined}
          models={modelList}
          variants={variantList}
          providers={providerList}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
            <span style={{ width: 140 }}>Provider</span>
            <span style={{ width: 180 }}>Wire model</span>
            <span style={{ width: 140 }}>Model</span>
            <span style={{ width: 100 }}>Price in/out</span>
            <span style={{ width: 64 }}>Priority</span>
            <span style={{ width: 64 }}>Currency</span>
            <span style={{ width: 72 }}>Context</span>
            <span>Enabled</span>
          </div>
          {list.length === 0 && <div className="empty-note">No offerings. Create one to route to a remote provider.</div>}
          {list.map((o) => {
            const m = modelList.find((m) => m.id === o.model_id);
            const prov = providerList.find((p) => p.name === o.provider);
            const providerDisabled = prov !== undefined && !prov.enabled;
            return (
              <div className="qrow" key={o.id} style={{ alignItems: "flex-start", opacity: providerDisabled ? 0.55 : undefined }}>
                <span style={{ width: 140, fontSize: 12, fontWeight: 600 }}>
                  <Icon slug={providerIconSlug(o.provider)} name={o.provider} />
                  {o.provider}
                  {prov?.country && <span style={{ marginLeft: 6, fontSize: 14 }}>{countryFlag(prov.country)}</span>}
                  {providerDisabled && <span className="chip" style={{ marginLeft: 4, color: "var(--warn)" }}>off</span>}
                </span>
                <span style={{ width: 180, fontFamily: "var(--mono)", fontSize: 11, color: "var(--text-dim)" }}>
                  {o.wire_model}
                  {preferredIds.has(o.id) && (
                    <span className="chip" style={{ marginLeft: 6, color: "var(--ok)" }} title="The provider a0 presents for this model (lowest priority value among enabled offerings of enabled providers)">
                      preferred
                    </span>
                  )}
                </span>
                <span style={{ width: 140, fontSize: 11, color: "var(--text-dim)" }}>
                  {m?.logo && <Icon slug={m.logo} name={m.name} />}
                  {m?.name ?? `#${o.model_id}`}
                </span>
                <span style={{ width: 100, fontFamily: "var(--mono)", fontSize: 11 }}>
                  {o.price_in_per_1m}/{o.price_out_per_1m}
                  {o.price_cached_in_per_1m != null && (
                    <span style={{ color: "var(--text-dim)" }}> (cached {o.price_cached_in_per_1m})</span>
                  )}
                </span>
                <span style={{ width: 64, fontFamily: "var(--mono)", fontSize: 11 }} title="Lower wins — among this model's enabled offerings, the lowest value is served via a0">{o.priority}</span>
                <span style={{ width: 64, fontSize: 11 }}>{o.currency}</span>
                <span style={{ width: 72, fontFamily: "var(--mono)", fontSize: 11 }}>{o.context_length.toLocaleString()}</span>
                <span style={{ fontSize: 11 }}>
                  <span className={`chip ${o.enabled ? "" : ""}`} style={{ color: o.enabled ? "var(--ok)" : "var(--text-mute)" }}>
                    {o.enabled ? "enabled" : "disabled"}
                  </span>
                </span>
                {canAdmin && (
                  <div className="actions" style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                    <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(o.id); clearError(); }}>Edit</button>
                    <ConfirmButton
                      className="btn"
                      style={{ fontSize: 11, padding: "4px 8px" }}
                      pending={remove.isPending}
                      onConfirm={() => handleDelete(o.id)}
                      warning={`Delete offering "${o.wire_model}"?`}
                    />
                  </div>
                )}
              </div>
            );
          })}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              + New offering
            </button>
          )}
        </>
      )}
    </>
  );
}

// Provider list for the Offering form dropdown + the offerings table. Uses
// the /api/v1/providers endpoint (router_providers table). country /
// data_residency_group are Provider-level facts (ADR-0003), exposed by the
// endpoint since the multi-provider sprint (2026-08-06); enabled drives the
// "provider disabled" styling + the preferred-primary computation.
interface CatalogProviderRef {
  name: string;
  country: string;
  data_residency_group: string;
  enabled: boolean;
}

function useCatalogProviders(): CatalogProviderRef[] {
  const providers = useProviders();
  return (providers.data?.providers ?? []).map((p) => ({
    name: p.name,
    country: p.country ?? "",
    data_residency_group: p.data_residency_group ?? "",
    enabled: p.enabled,
  }));
}

function OfferingForm({
  existing,
  models,
  variants,
  providers,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogOffering;
  models: CatalogModel[];
  variants: CatalogVariant[];
  providers: CatalogProviderRef[];
  onSubmit: (draft: Partial<CatalogOffering>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const [modelId, setModelId] = useState(existing?.model_id ?? 0);
  const [variantId, setVariantId] = useState(existing?.variant_id ?? 0);
  const [provider, setProvider] = useState(existing?.provider ?? "");
  const [wireModel, setWireModel] = useState(existing?.wire_model ?? "");
  const [priceIn, setPriceIn] = useState(existing?.price_in_per_1m ?? 0);
  const [priceOut, setPriceOut] = useState(existing?.price_out_per_1m ?? 0);
  const [priceCachedIn, setPriceCachedIn] = useState(existing?.price_cached_in_per_1m ?? null);
  const [currency, setCurrency] = useState(existing?.currency ?? "USD");
  const [contextLength, setContextLength] = useState(existing?.context_length ?? 0);
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [priority, setPriority] = useState(existing?.priority ?? 100);

  const modelVariants = variants.filter((v) => v.model_id === modelId);
  const selectedProvider = providers.find((p) => p.name === provider);

  function submit() {
    onSubmit(
      {
        model_id: modelId,
        variant_id: variantId,
        provider,
        wire_model: wireModel,
        price_in_per_1m: priceIn,
        price_out_per_1m: priceOut,
        price_cached_in_per_1m: priceCachedIn,
        currency,
        context_length: contextLength,
        enabled,
        priority,
      },
      existing?.id,
    );
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Model *
        <select value={modelId} onChange={(e) => { setModelId(Number(e.target.value)); setVariantId(0); }}>
          <option value={0}>— select —</option>
          {models.map((m) => <option key={m.id} value={m.id}>{m.name}</option>)}
        </select>
      </label>
      <label className="form-row">Variant (optional)
        <select value={variantId} onChange={(e) => setVariantId(Number(e.target.value))}>
          <option value={0}>— none —</option>
          {modelVariants.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
        </select>
      </label>
      <label className="form-row">Provider *
        <select value={provider} onChange={(e) => setProvider(e.target.value)}>
          <option value="">— select —</option>
          {providers.map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}{p.country ? ` ${countryFlag(p.country)}` : ""}{p.data_residency_group ? ` (${p.data_residency_group})` : ""}
            </option>
          ))}
        </select>
      </label>
      <label className="form-row">Wire model *
        <input value={wireModel} placeholder="deepseek-chat" onChange={(e) => setWireModel(e.target.value)} />
      </label>
      <label className="form-row">Price in / 1M
        <input type="number" step="0.01" min={0} value={priceIn} onChange={(e) => setPriceIn(Number(e.target.value))} />
      </label>
      <label className="form-row">Price out / 1M
        <input type="number" step="0.01" min={0} value={priceOut} onChange={(e) => setPriceOut(Number(e.target.value))} />
      </label>
      <label className="form-row">Cached price in / 1M (optional)
        <input
          type="number"
          step="0.001"
          min={0}
          value={priceCachedIn ?? ""}
          placeholder="unmodelled — full price applies to cache hits"
          onChange={(e) => setPriceCachedIn(e.target.value === "" ? null : Number(e.target.value))}
        />
      </label>
      <label className="form-row">Currency
        <input value={currency} maxLength={3} placeholder="USD" onChange={(e) => setCurrency(e.target.value.toUpperCase())} />
      </label>
      <label className="form-row">Context length
        <input type="number" min={0} value={contextLength} placeholder="65536" onChange={(e) => setContextLength(Number(e.target.value))} />
      </label>
      <label className="form-row">Priority
        <input type="number" min={0} value={priority} onChange={(e) => setPriority(Number(e.target.value))} />
        <span style={{ fontSize: 11, color: "var(--text-dim)" }}>
          When several providers offer this model, the LOWEST value is served via a0 (100 = no preference; ties break by provider name).
        </span>
      </label>
      <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
        <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
        Enabled (route to this offering)
      </label>
      {selectedProvider?.country && (
        <div style={{ gridColumn: "1 / -1", fontSize: 11, color: "var(--text-dim)" }}>
          {countryFlag(selectedProvider.country)} Data residency: {selectedProvider.country}
          {selectedProvider.data_residency_group ? ` · group: ${selectedProvider.data_residency_group}` : ""}
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton
          pending={pending}
          isError={isError}
          disabled={pending || !modelId || !provider || !wireModel}
          onClick={submit}
          label={existing ? "Save" : "Create"}
        />
      </div>
    </div>
  );
}

// ── Models ───────────────────────────────────────────────────────────────────

// ── Taxonomy (Sprint I — genealogy/family editing + icon hierarchy) ────────
// The backend has had full genealogy/family CRUD since the product/QA
// sprint (2026-07-29) — this was the missing frontend. Genealogies render
// above families since a family optionally belongs to one genealogy
// (nullable FK); the icon on each level is what the config/model/family
// chain inherits from when its own logo is unset (registry.resolveLogo).

function TaxonomySection({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <div className="eyebrow" style={{ marginTop: 0 }}>Genealogies</div>
      <div style={{ fontSize: 11.5, color: "var(--text-mute)", marginBottom: 10 }}>
        A vendor's own release lineage (Qwen, Gemma, Nemotron, …). The icon set here is the top
        of the inheritance chain — every family/model/config under it inherits unless it sets
        its own.
      </div>
      <GenealogiesSection canAdmin={canAdmin} />

      <div className="eyebrow" style={{ marginTop: 22 }}>Families</div>
      <div style={{ fontSize: 11.5, color: "var(--text-mute)", marginBottom: 10 }}>
        One generation within a genealogy (Gemma 4, Qwen 3.6, …). Optionally re-parented onto a
        genealogy above.
      </div>
      <FamiliesSection canAdmin={canAdmin} />
    </>
  );
}

function GenealogiesSection({ canAdmin }: { canAdmin: boolean }) {
  const genealogies = useCatalogGenealogies();
  const families = useCatalogFamilies();
  const create = useCreateCatalogGenealogy();
  const update = useUpdateCatalogGenealogy();
  const remove = useDeleteCatalogGenealogy();
  const uploadIcon = useUploadGenealogyIcon();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = genealogies.data ?? [];
  const familyList = families.data ?? [];
  const editingGenealogy = editing === "new" ? null : list.find((g) => g.id === editing);

  function handleSubmit(draft: Partial<CatalogGenealogy>, id?: number) {
    clearError();
    if (!id && list.some((g) => g.name.trim().toLowerCase() === (draft.name ?? "").trim().toLowerCase())) {
      // CreateGenealogy is INSERT OR IGNORE — a name collision silently
      // returns the *existing* row instead of erroring, which would make
      // "New genealogy" quietly discard this icon/name onto someone else's
      // row. Block it client-side instead.
      showError(`A genealogy named "${draft.name}" already exists — edit it instead of creating a duplicate.`);
      return;
    }
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    if (id) {
      update.mutate({ id, g: draft }, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    }
  }

  function handleIconSelect(id: number, g: CatalogGenealogy, slug: string, dark = false) {
    update.mutate({ id, g: dark ? { name: g.name, logo: g.logo, logo_dark: slug } : { name: g.name, logo: slug, logo_dark: g.logo_dark } }, { onError: showError });
  }
  function handleIconClear(id: number, g: CatalogGenealogy, dark = false) {
    update.mutate({ id, g: dark ? { name: g.name, logo: g.logo, logo_dark: "" } : { name: g.name, logo: "", logo_dark: g.logo_dark } }, { onError: showError });
  }
  function handleIconUpload(id: number, file: File, dark = false) {
    uploadIcon.mutate({ id, file, dark }, { onError: showError });
  }

  // Sprint K: confirm() gate moved into ConfirmButton — deleteWarning still
  // computes the same dependent-count text, now passed as ConfirmButton's
  // `warning` prop instead of a dialog message.
  function deleteWarning(id: number, name: string) {
    const dependents = familyList.filter((f) => f.genealogy_id === id).length;
    return dependents > 0
      ? `Delete genealogy "${name}"? ${dependents} famil${dependents === 1 ? "y" : "ies"} will lose this genealogy (and any icon inherited from it) — they are not deleted, just unparented.`
      : `Delete genealogy "${name}"?`;
  }
  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (genealogies.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {editing !== null ? (
        <GenealogyForm
          key={editing === "new" ? "new" : editing}
          existing={editingGenealogy ?? undefined}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
          onIconSelect={handleIconSelect}
          onIconClear={handleIconClear}
          onIconUpload={handleIconUpload}
        />
      ) : (
        <>
          {list.length === 0 && <div className="empty-note">No genealogies yet.</div>}
          {list.map((g) => (
            <div className="qrow" key={g.id}>
              {g.logo && <Icon slug={g.logo} name={g.name} sm />}
              <span style={{ fontSize: 12.5, fontWeight: 600, flex: 1 }}>{g.name}</span>
              <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
                {familyList.filter((f) => f.genealogy_id === g.id).length} famil{familyList.filter((f) => f.genealogy_id === g.id).length === 1 ? "y" : "ies"}
              </span>
              {canAdmin && (
                <div className="actions" style={{ marginLeft: 12, display: "flex", gap: 6 }}>
                  <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(g.id); clearError(); }}>Edit</button>
                  <ConfirmButton
                    className="btn"
                      style={{ fontSize: 11, padding: "4px 8px" }}
                    pending={remove.isPending}
                    onConfirm={() => handleDelete(g.id)}
                    warning={deleteWarning(g.id, g.name)}
                  />
                </div>
              )}
            </div>
          ))}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              + New genealogy
            </button>
          )}
        </>
      )}
    </>
  );
}

function GenealogyForm({
  existing,
  onSubmit,
  onCancel,
  pending,
  isError = false,
  onIconSelect,
  onIconClear,
  onIconUpload,
}: {
  existing?: CatalogGenealogy;
  onSubmit: (draft: Partial<CatalogGenealogy>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
  onIconSelect: (id: number, g: CatalogGenealogy, slug: string, dark?: boolean) => void;
  onIconClear: (id: number, g: CatalogGenealogy, dark?: boolean) => void;
  onIconUpload: (id: number, file: File, dark?: boolean) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Name *
        <input value={name} placeholder="Qwen" onChange={(e) => setName(e.target.value)} />
      </label>
      {existing && (
        <div className="form-row" style={{ gridColumn: "1 / -1" }}>
          Icon
          <IconPicker
            value={existing.logo}
            valueDark={existing.logo_dark}
            onSelect={(slug, dark) => onIconSelect(existing.id, existing, slug, dark)}
            onClear={(dark) => onIconClear(existing.id, existing, dark)}
            onUpload={(file, dark) => onIconUpload(existing.id, file, dark)}
          />
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton
          className="go"
          pending={pending}
          isError={isError}
          disabled={pending || !name}
          onClick={() => onSubmit({ name, logo: existing?.logo ?? "", logo_dark: existing?.logo_dark ?? "" }, existing?.id)}
        />
      </div>
    </div>
  );
}

function FamiliesSection({ canAdmin }: { canAdmin: boolean }) {
  const families = useCatalogFamilies();
  const genealogies = useCatalogGenealogies();
  const models = useCatalogModels();
  const create = useCreateCatalogFamily();
  const update = useUpdateCatalogFamily();
  const remove = useDeleteCatalogFamily();
  const uploadIcon = useUploadFamilyIcon();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = families.data ?? [];
  const genealogyList = genealogies.data ?? [];
  const modelList = models.data ?? [];
  const editingFamily = editing === "new" ? null : list.find((f) => f.id === editing);

  function handleSubmit(draft: Partial<CatalogFamily>, id?: number) {
    clearError();
    if (!id && list.some((f) => f.name.trim().toLowerCase() === (draft.name ?? "").trim().toLowerCase())) {
      // Same INSERT-OR-IGNORE collision as genealogies — see that comment.
      showError(`A family named "${draft.name}" already exists — edit it instead of creating a duplicate.`);
      return;
    }
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    if (id) {
      update.mutate({ id, f: draft }, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    } else {
      create.mutate(draft, { onSuccess: () => setTimeout(() => setEditing(null), 700), onError: showError });
    }
  }

  function handleIconSelect(id: number, f: CatalogFamily, slug: string, dark = false) {
    update.mutate({ id, f: dark ? { name: f.name, genealogy_id: f.genealogy_id, logo: f.logo, logo_dark: slug } : { name: f.name, genealogy_id: f.genealogy_id, logo: slug, logo_dark: f.logo_dark } }, { onError: showError });
  }
  function handleIconClear(id: number, f: CatalogFamily, dark = false) {
    update.mutate({ id, f: dark ? { name: f.name, genealogy_id: f.genealogy_id, logo: f.logo, logo_dark: "" } : { name: f.name, genealogy_id: f.genealogy_id, logo: "", logo_dark: f.logo_dark } }, { onError: showError });
  }
  function handleIconUpload(id: number, file: File, dark = false) {
    uploadIcon.mutate({ id, file, dark }, { onError: showError });
  }

  // Sprint K: confirm() gate moved into ConfirmButton.
  function deleteWarning(id: number, name: string) {
    const dependents = modelList.filter((m) => m.family_id === id).length;
    return dependents > 0
      ? `Delete family "${name}"? ${dependents} model${dependents === 1 ? "" : "s"} will lose this family (and any icon inherited from it) — they are not deleted, just unparented.`
      : `Delete family "${name}"?`;
  }
  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (families.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {editing !== null ? (
        <FamilyForm
          key={editing === "new" ? "new" : editing}
          existing={editingFamily ?? undefined}
          genealogies={genealogyList}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
          onIconSelect={handleIconSelect}
          onIconClear={handleIconClear}
          onIconUpload={handleIconUpload}
        />
      ) : (
        <>
          {list.length === 0 && <div className="empty-note">No families yet.</div>}
          {list.map((f) => {
            const genealogy = genealogyList.find((g) => g.id === f.genealogy_id);
            return (
              <div className="qrow" key={f.id}>
                {f.logo && <Icon slug={f.logo} name={f.name} sm />}
                <span style={{ fontSize: 12.5, fontWeight: 600, flex: 1 }}>{f.name}</span>
                <span style={{ fontSize: 11, color: "var(--text-dim)", width: 140 }}>{genealogy?.name ?? "—"}</span>
                <span style={{ fontSize: 11, color: "var(--text-mute)" }}>
                  {modelList.filter((m) => m.family_id === f.id).length} model{modelList.filter((m) => m.family_id === f.id).length === 1 ? "" : "s"}
                </span>
                {canAdmin && (
                  <div className="actions" style={{ marginLeft: 12, display: "flex", gap: 6 }}>
                    <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(f.id); clearError(); }}>Edit</button>
                    <ConfirmButton
                      className="btn"
                      style={{ fontSize: 11, padding: "4px 8px" }}
                      pending={remove.isPending}
                      onConfirm={() => handleDelete(f.id)}
                      warning={deleteWarning(f.id, f.name)}
                    />
                  </div>
                )}
              </div>
            );
          })}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              + New family
            </button>
          )}
        </>
      )}
    </>
  );
}

function FamilyForm({
  existing,
  genealogies,
  onSubmit,
  onCancel,
  pending,
  isError = false,
  onIconSelect,
  onIconClear,
  onIconUpload,
}: {
  existing?: CatalogFamily;
  genealogies: CatalogGenealogy[];
  onSubmit: (draft: Partial<CatalogFamily>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
  onIconSelect: (id: number, f: CatalogFamily, slug: string, dark?: boolean) => void;
  onIconClear: (id: number, f: CatalogFamily, dark?: boolean) => void;
  onIconUpload: (id: number, file: File, dark?: boolean) => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [genealogyId, setGenealogyId] = useState(existing?.genealogy_id ?? 0);

  const genForInherit = genealogies.find((g) => g.id === genealogyId);
  const inherited = genealogyId && genForInherit
    ? { logo: genForInherit.logo, logoDark: genForInherit.logo_dark || genForInherit.logo, label: genForInherit.name }
    : null;

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Name *
        <input value={name} placeholder="Gemma 4" onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="form-row">Genealogy
        <select value={genealogyId} onChange={(e) => setGenealogyId(Number(e.target.value))}>
          <option value={0}>— none —</option>
          {genealogies.map((g) => <option key={g.id} value={g.id}>{g.name}</option>)}
        </select>
      </label>
      {existing && (
        <div className="form-row" style={{ gridColumn: "1 / -1" }}>
          Icon
          <IconPicker
            value={existing.logo}
            valueDark={existing.logo_dark}
            inherited={inherited}
            onSelect={(slug, dark) => onIconSelect(existing.id, existing, slug, dark)}
            onClear={(dark) => onIconClear(existing.id, existing, dark)}
            onUpload={(file, dark) => onIconUpload(existing.id, file, dark)}
          />
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton
          className="go"
          pending={pending}
          isError={isError}
          disabled={pending || !name}
          onClick={() => onSubmit({ name, genealogy_id: genealogyId, logo: existing?.logo ?? "", logo_dark: existing?.logo_dark ?? "" }, existing?.id)}
        />
      </div>
    </div>
  );
}

function ModelsSection({ canAdmin }: { canAdmin: boolean }) {
  const models = useCatalogModels();
  const families = useCatalogFamilies();
  const genealogies = useCatalogGenealogies();
  const variants = useCatalogVariants();
  const artifacts = useCatalogArtifacts();
  const quantizations = useCatalogQuantizations();
  const formats = useCatalogFormats();
  const create = useCreateCatalogModel();
  const update = useUpdateCatalogModel();
  const remove = useDeleteCatalogModel();
  const uploadIcon = useUploadModelIcon();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const [variantFor, setVariantFor] = useState<number | null>(null);
  const { error, showError, clearError } = useErrorState();

  const modelList = models.data ?? [];
  const familyList = families.data ?? [];
  const genealogyList = genealogies.data ?? [];
  const variantList = variants.data ?? [];
  const artifactList = artifacts.data ?? [];

  function handleSubmit(draft: Partial<CatalogModel>, id?: number) {
    clearError();
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    if (id) {
      update.mutate({ id, m: draft }, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    } else {
      create.mutate(draft, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    }
  }

  function handleIconUpload(id: number, file: File, dark = false) {
    uploadIcon.mutate({ id, file, dark }, { onError: showError });
  }

  // handleIconSelect/Clear set the model's own logo directly via UpdateModel
  // (full-replace, so every other field must round-trip unchanged) — for
  // picking a vendored slug (no file to upload) or resetting to inherit from
  // the family. Mirrors uploadIcon's mutation but through the plain body path.
  function handleIconSelect(id: number, m: CatalogModel, slug: string, dark = false) {
    update.mutate({ id, m: dark ? { ...m, logo_dark: slug } : { ...m, logo: slug } }, { onError: showError });
  }
  function handleIconClear(id: number, m: CatalogModel, dark = false) {
    update.mutate({ id, m: dark ? { ...m, logo_dark: "" } : { ...m, logo: "" } }, { onError: showError });
  }

  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (models.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {editing !== null ? (
        <ModelForm
          key={editing === "new" ? "new" : editing}
          existing={editing === "new" ? undefined : modelList.find((m) => m.id === editing)}
          families={familyList}
          genealogies={genealogyList}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
          onIconUpload={handleIconUpload}
          onIconSelect={handleIconSelect}
          onIconClear={handleIconClear}
        />
      ) : (
        <>
          <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
            <span style={{ width: 180 }}>Model</span>
            <span style={{ width: 120 }}>Family</span>
            <span style={{ width: 100 }}>Architecture</span>
            <span style={{ width: 80 }}>Params</span>
            <span style={{ width: 60 }}>Variants</span>
            <span>Description</span>
          </div>
          {modelList.length === 0 && <div className="empty-note">No models. Create one to start the catalog hierarchy.</div>}
          {modelList.map((m) => {
            const f = familyList.find((f) => f.id === m.family_id);
            const modelVariants = variantList.filter((v) => v.model_id === m.id);
            return (
              <div key={m.id}>
                <div className="qrow" style={{ alignItems: "flex-start" }}>
                  <span style={{ width: 180, fontWeight: 600, fontSize: 12.5 }}>
                    {m.logo && <Icon slug={m.logo} name={m.name} />}
                    {m.name}
                    {m.visibility === "hidden" && <span className="chip" style={{ marginLeft: 6, opacity: 0.7 }}>hidden</span>}
                  </span>
                  <span style={{ width: 120, fontSize: 11, color: "var(--text-dim)" }}>{f?.name ?? "—"}</span>
                  <span style={{ width: 100, fontSize: 11, fontFamily: "var(--mono)", color: "var(--cool)" }}>{m.architecture || "—"}</span>
                  <span style={{ width: 80, fontSize: 11, fontFamily: "var(--mono)" }}>{m.parameter_count || "—"}</span>
                  <span style={{ width: 60, fontSize: 11 }}>
                    <button className="chip" style={{ cursor: "pointer", background: "transparent" }} onClick={() => setVariantFor(variantFor === m.id ? null : m.id)}>
                      {modelVariants.length} variant{modelVariants.length !== 1 ? "s" : ""}
                    </button>
                  </span>
                  <span style={{ fontSize: 11, color: "var(--text-dim)", flex: 1 }}>{m.description || "—"}</span>
                  {canAdmin && (
                    <div className="actions" style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                      <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(m.id); clearError(); }}>Edit</button>
                      <ConfirmButton
                        className="btn"
                      style={{ fontSize: 11, padding: "4px 8px" }}
                        pending={remove.isPending}
                        onConfirm={() => handleDelete(m.id)}
                        warning={`Delete model "${m.name}"? Its variants and configs must be deleted first.`}
                      />
                    </div>
                  )}
                </div>
                {variantFor === m.id && (
                  <VariantsSubList
                    modelId={m.id}
                    variants={modelVariants}
                    artifacts={artifactList}
                    quantizations={quantizations.data ?? []}
                    formats={formats.data ?? []}
                    canAdmin={canAdmin}
                  />
                )}
              </div>
            );
          })}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              + New model
            </button>
          )}
        </>
      )}
    </>
  );
}

function ModelForm({
  existing,
  families,
  genealogies,
  onSubmit,
  onCancel,
  pending,
  isError = false,
  onIconUpload,
  onIconSelect,
  onIconClear,
}: {
  existing?: CatalogModel;
  families: CatalogFamily[];
  genealogies: CatalogGenealogy[];
  onSubmit: (draft: Partial<CatalogModel>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
  onIconUpload: (id: number, file: File, dark?: boolean) => void;
  onIconSelect: (id: number, m: CatalogModel, slug: string, dark?: boolean) => void;
  onIconClear: (id: number, m: CatalogModel, dark?: boolean) => void;
}) {
  const [familyId, setFamilyId] = useState(existing?.family_id ?? 0);
  const [name, setName] = useState(existing?.name ?? "");
  const [architecture, setArchitecture] = useState(existing?.architecture ?? "");
  const [parameterCount, setParameterCount] = useState(existing?.parameter_count ?? "");
  const [description, setDescription] = useState(existing?.description ?? "");
  const [creator, setCreator] = useState(existing?.creator ?? "");
  const [licenseName, setLicenseName] = useState(existing?.license_name ?? "");
  const [licenseUrl, setLicenseUrl] = useState(existing?.license_url ?? "");
  const [hfRepo, setHfRepo] = useState(existing?.hf_repo ?? "");
  const [keyFeaturesText, setKeyFeaturesText] = useState(existing ? existing.key_features.join("\n") : "");
  // Sprint J1: "text" is always implicit (every config can do plain text) —
  // only vision/audio are operator-toggled. See registry.resolveModalities
  // for how a config's own modalities narrow this default.
	const [hasVision, setHasVision] = useState(existing?.modalities.includes("vision") ?? false);
	const [hasAudio, setHasAudio] = useState(existing?.modalities.includes("audio") ?? false);
	const [visibility, setVisibility] = useState(existing?.visibility ?? "visible");

  function submit() {
    onSubmit(
      {
        family_id: familyId,
        name,
        architecture,
        parameter_count: parameterCount,
        description,
        creator,
        license_name: licenseName,
        license_url: licenseUrl,
        hf_repo: hfRepo,
        key_features: keyFeaturesText.split("\n").map((s) => s.trim()).filter((s) => s.length > 0),
			modalities: ["text", ...(hasVision ? ["vision"] : []), ...(hasAudio ? ["audio"] : [])],
			visibility,
        // Carried through unchanged — UpdateModel is full-replace, and icon
        // changes go through the immediate onIconSelect/Upload/Clear
        // mutations below, not this form's own Save (Sprint I; the same gap
        // existed pre-Sprint-I — editing any other field here silently
        // wiped the model's icon since this draft never included it).
        logo: existing?.logo ?? "",
        logo_dark: existing?.logo_dark ?? "",
      },
      existing?.id,
    );
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Name *
        <input value={name} placeholder="Qwen3-Coder-Next" onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="form-row">Family
        <select value={familyId} onChange={(e) => setFamilyId(Number(e.target.value))}>
          <option value={0}>— none —</option>
          {families.map((f) => <option key={f.id} value={f.id}>{f.name}</option>)}
        </select>
      </label>
      <label className="form-row">Architecture
        <input value={architecture} placeholder="llama" onChange={(e) => setArchitecture(e.target.value)} />
      </label>
      <label className="form-row">Parameter count
        <input value={parameterCount} placeholder="31B" onChange={(e) => setParameterCount(e.target.value)} />
      </label>
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>Description
        <textarea value={description} rows={2} style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 13, resize: "vertical" }} onChange={(e) => setDescription(e.target.value)} />
      </label>
      <label className="form-row">Creator
        <input value={creator} placeholder="Qwen" onChange={(e) => setCreator(e.target.value)} />
      </label>
      <label className="form-row">HF repo
        <input value={hfRepo} placeholder="Qwen/Qwen3-Coder-Next" onChange={(e) => setHfRepo(e.target.value)} />
      </label>
      <label className="form-row">License name
        <input value={licenseName} placeholder="Apache-2.0" onChange={(e) => setLicenseName(e.target.value)} />
      </label>
      <label className="form-row">License URL
        <input value={licenseUrl} placeholder="https://…" onChange={(e) => setLicenseUrl(e.target.value)} />
      </label>
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>Key features (one per line)
        <textarea value={keyFeaturesText} rows={2} style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 12, resize: "vertical" }} onChange={(e) => setKeyFeaturesText(e.target.value)} />
      </label>
	<div className="form-row" style={{ gridColumn: "1 / -1" }}>
		Modalities
		<div style={{ display: "flex", gap: 14, alignItems: "center" }}>
			<span className="chip" style={{ opacity: 0.6 }}>Text (always)</span>
			<label style={{ display: "flex", alignItems: "center", gap: 6, fontWeight: 400 }}>
				<input type="checkbox" checked={hasVision} onChange={(e) => setHasVision(e.target.checked)} />
				Vision
			</label>
			<label style={{ display: "flex", alignItems: "center", gap: 6, fontWeight: 400 }}>
				<input type="checkbox" checked={hasAudio} onChange={(e) => setHasAudio(e.target.checked)} />
				Audio
			</label>
		</div>
	</div>
	<label className="form-row">Visibility
		<select value={visibility} onChange={(e) => setVisibility(e.target.value)}>
			<option value="visible">visible</option>
			<option value="hidden">hidden</option>
		</select>
	</label>
	{existing && (
        <div className="form-row" style={{ gridColumn: "1 / -1" }}>
          Icon
          <IconPicker
            value={existing.logo}
            valueDark={existing.logo_dark}
            inherited={familyInheritedIcon(familyId, families, genealogies)}
            onSelect={(slug, dark) => onIconSelect(existing.id, existing, slug, dark)}
            onClear={(dark) => onIconClear(existing.id, existing, dark)}
            onUpload={(file, dark) => onIconUpload(existing.id, file, dark)}
          />
        </div>
      )}
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton pending={pending} isError={isError} disabled={pending || !name} onClick={submit} label={existing ? "Save" : "Create"} />
      </div>
    </div>
  );
}

// ── Variants sub-list (inline under Models) ─────────────────────────────────

function VariantsSubList({
  modelId,
  variants,
  artifacts,
  quantizations,
  formats,
  canAdmin,
}: {
  modelId: number;
  variants: CatalogVariant[];
  artifacts: CatalogArtifact[];
  quantizations: { id: number; name: string }[];
  formats: { id: number; name: string }[];
  canAdmin: boolean;
}) {
  const createVar = useCreateCatalogVariant();
  const deleteVar = useDeleteCatalogVariant();
  const [creating, setCreating] = useState(false);
  const { error, showError, clearError } = useErrorState();

  const [varName, setVarName] = useState("");
  const [derivationType, setDerivationType] = useState("base");
  const [sourceVariantId, setSourceVariantId] = useState(0);
  const [trainedCtx, setTrainedCtx] = useState(0);
  const [isAbliterated, setIsAbliterated] = useState(false);
  const [abliterationQuality, setAbliterationQuality] = useState("");

  function submitVariant() {
    clearError();
    createVar.mutate(
      {
        model_id: modelId,
        name: varName,
        derivation_type: derivationType,
        source_variant_id: sourceVariantId,
        trained_ctx: trainedCtx,
        is_abliterated: isAbliterated,
        abliteration_quality: abliterationQuality,
      },
      {
        onSuccess: () => { setCreating(false); setVarName(""); setDerivationType("base"); setSourceVariantId(0); setTrainedCtx(0); setIsAbliterated(false); setAbliterationQuality(""); },
        onError: showError,
      },
    );
  }

  function deleteVariant(id: number) {
    deleteVar.mutate(id, { onError: showError });
  }

  return (
    <div style={{ marginLeft: 24, marginBottom: 10, padding: "8px 12px", borderLeft: "2px solid var(--border)", fontSize: 12 }}>
      {error && <div className="error-note" style={{ marginBottom: 8 }}>{error}</div>}
      {variants.length === 0 && !creating && <div className="empty-note" style={{ padding: "6px" }}>No variants.</div>}
      {variants.map((v) => {
        const vArtifacts = artifacts.filter((a) => a.variant_id === v.id);
        return (
          <div key={v.id} style={{ padding: "6px 0", borderBottom: "1px solid var(--border)" }}>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <span style={{ fontWeight: 600 }}>{v.name}</span>
              {v.derivation_type && v.derivation_type !== "base" && <span className="chip">{v.derivation_type}</span>}
              {v.is_abliterated && <span className="chip" style={{ color: "var(--warn)" }}>abliterated</span>}
              {v.trained_ctx > 0 && <span style={{ fontFamily: "var(--mono)", fontSize: 10.5, color: "var(--text-mute)" }}>ctx:{v.trained_ctx.toLocaleString()}</span>}
              {canAdmin && (
                <ConfirmButton
                  className="btn"
                  style={{ fontSize: 10.5, padding: "2px 6px", marginLeft: "auto" }}
                  pending={deleteVar.isPending}
                  onConfirm={() => deleteVariant(v.id)}
                  warning={`Delete variant "${v.name}"? Its configs and artifacts must be deleted first.`}
                />
              )}
            </div>
            {vArtifacts.length > 0 && (
              <div style={{ marginLeft: 12, marginTop: 4, fontSize: 11, color: "var(--text-dim)" }}>
                {vArtifacts.map((a) => {
                  const q = quantizations.find((q) => q.id === a.quantization_id);
                  const f = formats.find((f) => f.id === a.format_id);
                  return (
                    <div key={a.id} style={{ display: "flex", gap: 6, fontFamily: "var(--mono)", fontSize: 10.5, padding: "2px 0" }}>
                      <span style={{ color: "var(--text-mute)" }}>{a.artifact_type}:</span>
                      <span style={{ flex: 1, color: "var(--text-dim)" }}>{a.file_path}</span>
                      {q && <span className="chip" style={{ fontSize: 9.5 }}>{q.name}</span>}
                      {f && <span className="chip" style={{ fontSize: 9.5 }}>{f.name}</span>}
                      {a.missing && <span style={{ color: "var(--crit)" }}>missing</span>}
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        );
      })}
      {creating && (
        <div className="form-grid" style={{ marginTop: 8, gridTemplateColumns: "1fr 1fr 1fr" }}>
          <label className="form-row">Name *
            <input value={varName} placeholder="base instruct" onChange={(e) => setVarName(e.target.value)} />
          </label>
          <label className="form-row">Derivation
            <select value={derivationType} onChange={(e) => setDerivationType(e.target.value)}>
              {["base", "abliteration", "finetune", "merge", "mtp-head-add", "uncensor"].map((d) => <option key={d} value={d}>{d}</option>)}
            </select>
          </label>
          <label className="form-row">Source variant
            <select value={sourceVariantId} onChange={(e) => setSourceVariantId(Number(e.target.value))}>
              <option value={0}>— none —</option>
              {variants.map((v) => <option key={v.id} value={v.id}>{v.name}</option>)}
            </select>
          </label>
          <label className="form-row">Trained ctx
            <input type="number" min={0} value={trainedCtx} placeholder="131072" onChange={(e) => setTrainedCtx(Number(e.target.value))} />
          </label>
          <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8 }}>
            <input type="checkbox" checked={isAbliterated} onChange={(e) => setIsAbliterated(e.target.checked)} />
            Abliterated
          </label>
          <label className="form-row">Abliteration quality
            <input value={abliterationQuality} placeholder="high" onChange={(e) => setAbliterationQuality(e.target.value)} />
          </label>
          <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
            <button className="btn" onClick={() => { setCreating(false); clearError(); }}>Cancel</button>
            <button className="btn primary" disabled={createVar.isPending || !varName} onClick={submitVariant}>Create</button>
          </div>
        </div>
      )}
      {canAdmin && !creating && (
        <button className="btn" style={{ fontSize: 11, padding: "4px 8px", marginTop: 6 }} onClick={() => { setCreating(true); clearError(); }}>+ Add variant</button>
      )}
    </div>
  );
}

// ── Notes ────────────────────────────────────────────────────────────────────

function NotesSection({ canAdmin }: { canAdmin: boolean }) {
  const notes = useCatalogNotes();
  const models = useCatalogModels();
  const configs = useCatalogConfigs();
  const offerings = useCatalogOfferings();
  const create = useCreateCatalogNote();
  const remove = useDeleteCatalogNote();

  const [creating, setCreating] = useState(false);
  const { error, showError, clearError } = useErrorState();

  const [subjectType, setSubjectType] = useState("model");
  const [subjectId, setSubjectId] = useState(0);
  const [author, setAuthor] = useState("");
  const [body, setBody] = useState("");

  const list = notes.data ?? [];

  function submit() {
    clearError();
    create.mutate(
      { subject_type: subjectType, subject_id: subjectId, author, body },
      {
        onSuccess: () => { setCreating(false); setSubjectType("model"); setSubjectId(0); setAuthor(""); setBody(""); },
        onError: showError,
      },
    );
  }

  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (notes.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  const subjectOptions: { id: number; name: string }[] = (() => {
    switch (subjectType) {
      case "model": return (models.data ?? []).map((m: CatalogModel) => ({ id: m.id, name: m.name }));
      case "config": return (configs.data ?? []).map((c: CatalogConfig) => ({ id: c.id, name: c.name }));
      case "offering": return (offerings.data ?? []).map((o: CatalogOffering) => ({ id: o.id, name: o.wire_model }));
      default: return [];
    }
  })();

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {creating && (
        <div className="form-grid" style={{ marginTop: 4 }}>
          <label className="form-row">Subject type *
            <select value={subjectType} onChange={(e) => { setSubjectType(e.target.value); setSubjectId(0); }}>
              <option value="model">model</option>
              <option value="config">config</option>
              <option value="offering">offering</option>
            </select>
          </label>
          <label className="form-row">Subject *
            <select value={subjectId} onChange={(e) => setSubjectId(Number(e.target.value))}>
              <option value={0}>— select —</option>
              {subjectOptions.map((s) => <option key={s.id} value={s.id}>{s.name}</option>)}
            </select>
          </label>
          <label className="form-row">Author *
            <input value={author} placeholder="operator" onChange={(e) => setAuthor(e.target.value)} />
          </label>
          <label className="form-row" style={{ gridColumn: "1 / -1" }}>Body *
            <textarea value={body} rows={3} style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontSize: 12, resize: "vertical" }} onChange={(e) => setBody(e.target.value)} />
          </label>
          <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
            <button className="btn" onClick={() => { setCreating(false); clearError(); }}>Cancel</button>
            <button className="btn primary" disabled={create.isPending || !subjectId || !author || !body} onClick={submit}>Create</button>
          </div>
        </div>
      )}

      {!creating && (
        <>
          <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
            <span style={{ width: 100 }}>Subject</span>
            <span style={{ width: 100 }}>Author</span>
            <span>Body</span>
          </div>
          {list.length === 0 && <div className="empty-note">No notes recorded.</div>}
          {list.map((n) => {
            const subj = subjectLabel(n.subject_type, n.subject_id, models.data ?? [], [], configs.data ?? [], offerings.data ?? []);
            return (
              <div className="qrow" key={n.id} style={{ alignItems: "flex-start", flexDirection: "column", gap: 4 }}>
                <div style={{ display: "flex", gap: 10, width: "100%" }}>
                  <span style={{ width: 100, fontSize: 11, color: "var(--text-dim)" }}>{n.subject_type}: {subj}</span>
                  <span style={{ width: 100, fontSize: 11, color: "var(--cool)" }}>{n.author}</span>
                  <span style={{ flex: 1, fontSize: 11, color: "var(--text-dim)" }}>{n.body}</span>
                  {canAdmin && (
                    <ConfirmButton
                      className="btn"
                      style={{ fontSize: 10.5, padding: "2px 6px" }}
                      pending={remove.isPending}
                      onConfirm={() => handleDelete(n.id)}
                      warning="Delete this note?"
                    />
                  )}
                </div>
              </div>
            );
          })}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setCreating(true); clearError(); }}>+ Add note</button>
          )}
        </>
      )}
    </>
  );
}

// ── Services ─────────────────────────────────────────────────────────────────

function ServicesSection({ canAdmin }: { canAdmin: boolean }) {
  const services = useCatalogServices();
  const create = useCreateCatalogService();
  const update = useUpdateCatalogService();
  const remove = useDeleteCatalogService();

  const [editing, setEditing] = useState<number | "new" | null>(null);
  const { error, showError, clearError } = useErrorState();

  const list = services.data ?? [];
  const editingService = editing === "new" ? null : list.find((s) => s.id === editing);

  function handleSubmit(draft: Partial<CatalogService>, id?: number) {
    clearError();
    // Sprint K: delayed close so SaveButton's flash has time to paint.
    if (id) {
      update.mutate({ id, s: draft }, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    } else {
      create.mutate(draft, {
        onSuccess: () => setTimeout(() => setEditing(null), 700),
        onError: showError,
      });
    }
  }

  function handleDelete(id: number) {
    remove.mutate(id, { onError: showError });
  }

  if (services.isError) {
    return <div className="empty-note">Catalog not available (503 — store may not be wired).</div>;
  }

  return (
    <>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      {editing !== null ? (
        <ServiceForm
          key={editingService?.id ?? "new"}
          existing={editingService ?? undefined}
          onSubmit={handleSubmit}
          onCancel={() => { setEditing(null); clearError(); }}
          pending={create.isPending || update.isPending}
          isError={!!error}
        />
      ) : (
        <>
          <div className="qrow" style={{ color: "var(--text-mute)", fontSize: 10.5, textTransform: "uppercase", letterSpacing: ".06em" }}>
            <span style={{ width: 120 }}>Name</span>
            <span style={{ width: 120 }}>Label</span>
            <span style={{ width: 60 }}>Unit</span>
            <span>Description</span>
          </div>
          {list.length === 0 && <div className="empty-note">No services defined.</div>}
          {list.map((s) => (
            <div className="qrow" key={s.id} style={{ alignItems: "flex-start" }}>
              <span style={{ width: 120, fontFamily: "var(--mono)", fontSize: 12 }}>
                {s.icon && <span style={{ marginRight: 4 }}>{s.icon}</span>}
                {s.name}
              </span>
              <span style={{ width: 120, fontSize: 12, fontWeight: 600 }}>{s.label}</span>
              <span style={{ width: 60, fontSize: 11, color: "var(--text-dim)" }}>{s.unit || "—"}</span>
              <span style={{ fontSize: 11, color: "var(--text-dim)", flex: 1 }}>{s.description || "—"}</span>
              {canAdmin && (
                <div className="actions" style={{ marginLeft: "auto", display: "flex", gap: 6 }}>
                  <button className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={() => { setEditing(s.id); clearError(); }}>Edit</button>
                  <ConfirmButton
                    className="btn"
                    style={{ fontSize: 11, padding: "4px 8px" }}
                    pending={remove.isPending}
                    onConfirm={() => handleDelete(s.id)}
                    warning={`Delete service "${s.name}"?`}
                  />
                </div>
              )}
            </div>
          ))}
          {canAdmin && (
            <button className="btn" style={{ marginTop: 14 }} onClick={() => { setEditing("new"); clearError(); }}>
              + New service
            </button>
          )}
        </>
      )}
    </>
  );
}

function ServiceForm({
  existing,
  onSubmit,
  onCancel,
  pending,
  isError = false,
}: {
  existing?: CatalogService;
  onSubmit: (draft: Partial<CatalogService>, id?: number) => void;
  onCancel: () => void;
  pending: boolean;
  isError?: boolean;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [label, setLabel] = useState(existing?.label ?? "");
  const [description, setDescription] = useState(existing?.description ?? "");
  const [icon, setIcon] = useState(existing?.icon ?? "");
  const [color, setColor] = useState(existing?.color ?? "");
  const [unit, setUnit] = useState(existing?.unit ?? "");
  const [healthCheck, setHealthCheck] = useState(existing?.health_check ?? "{}");

  function submit() {
    onSubmit(
      { name, label, description, icon, color, unit, health_check: healthCheck },
      existing?.id,
    );
  }

  return (
    <div className="form-grid" style={{ marginTop: 4 }}>
      <label className="form-row">Name *
        <input value={name} placeholder="comfyui" onChange={(e) => setName(e.target.value)} />
      </label>
      <label className="form-row">Label *
        <input value={label} placeholder="ComfyUI" onChange={(e) => setLabel(e.target.value)} />
      </label>
      <label className="form-row" style={{ gridColumn: "1 / -1" }}>Description
        <input value={description} placeholder="Standalone image generation service" onChange={(e) => setDescription(e.target.value)} />
      </label>
      <label className="form-row">Icon (slug or emoji)
        <input value={icon} placeholder="🎨" onChange={(e) => setIcon(e.target.value)} />
      </label>
      <label className="form-row">Color
        <input value={color} placeholder="#76b900" onChange={(e) => setColor(e.target.value)} />
      </label>
      <label className="form-row">Unit
        <input value={unit} placeholder="port" onChange={(e) => setUnit(e.target.value)} />
      </label>
      <label className="form-row">Health check (JSON)
        <input value={healthCheck} placeholder="{}" onChange={(e) => setHealthCheck(e.target.value)} />
      </label>
      <div className="form-actions" style={{ gridColumn: "1 / -1" }}>
        <button className="btn" onClick={onCancel}>Cancel</button>
        <SaveButton pending={pending} isError={isError} disabled={pending || !name || !label} onClick={submit} label={existing ? "Save" : "Create"} />
      </div>
    </div>
  );
}
