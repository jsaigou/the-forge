import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../../lib/api";
import type { ConfigWritePayload } from "../../lib/configPayload";
import { formatGB } from "../../lib/format";
import { modelInheritedIcon } from "../../lib/iconInheritance";
import {
  useCatalogArtifacts,
  useCatalogBuilds,
  useCatalogConfigs,
  useCatalogEngines,
  useCatalogFamilies,
  useCatalogGenealogies,
  useCatalogModels,
  useCatalogCapabilityTiers,
  useCatalogVariants,
  useModelFiles,
  useUpdateCatalogConfig,
  useUploadConfigIcon,
} from "../../lib/queries";
import type { CatalogArtifact, CatalogBuild, CatalogConfig, CatalogEngine, CatalogFamily, CatalogGenealogy, CatalogModel, CatalogModelFile, CatalogCapabilityTier, CatalogVariant } from "../../lib/types";
import type { CapsOverride } from "../ChatTemplateCapsOverrideEditor";
import { ChatTemplateCapsOverrideEditor } from "../ChatTemplateCapsOverrideEditor";
import { IconPicker } from "../IconPicker";
import { LoadOptionsEditor } from "../LoadOptionsEditor";
import { SaveButton } from "../SaveButton";

// ConfigEditView (Sprint C) — the premium in-place config editor, pushed
// into DetailModal's view stack from ConfigDetailView's Edit button
// (replacing Sprint B's ConfigEditModal, which stacked a second
// .modal-backdrop over the detail view and just reused CatalogPanel's
// dense admin ConfigForm as-is). Covers every field CatalogPanel's admin
// form does — including the structural selectors (variant/weight
// artifact/engine/build/mmproj) — so there is one real place to edit a
// config; CatalogPanel keeps ConfigForm only for create/delete.
//
// Deliberately drops the Parallel number input (Sprint B's BUG-1,
// docs/v5-prerelease-readiness.md): store.Config.Parallel is validated and
// persisted but never read anywhere in the launch path — only extra_args
// reaches llama-server — so --parallel now lives in the load-options
// picker below like any other real flag. The column's current value is
// still round-tripped unchanged on every save (never zeroed out) so a
// direct DB inspection doesn't see it silently reset.
export function ConfigEditView({ configId, onDone, onCancel }: { configId: number; onDone: () => void; onCancel: () => void }) {
  const configs = useCatalogConfigs();
  const variants = useCatalogVariants();
  const artifacts = useCatalogArtifacts();
  const engines = useCatalogEngines();
  const builds = useCatalogBuilds();
  const models = useCatalogModels();
  const families = useCatalogFamilies();
  const genealogies = useCatalogGenealogies();
  const capabilityTiers = useCatalogCapabilityTiers();
  const modelFiles = useModelFiles();
  const update = useUpdateCatalogConfig();
  const uploadIcon = useUploadConfigIcon();
  const [showFiles, setShowFiles] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const { t } = useTranslation("models");
  const existing = configs.data?.find((c) => c.id === configId);

  if (!existing) {
    return <div className="empty-note">{t("config_edit.loading")}</div>;
  }

  // id/chat_template_caps/chat_template_caps_probed_at are excluded from
  // every icon-select/clear body below: id travels in the URL
  // (update.mutate's own `id: configId`), and configBody's decode rejects
  // unknown fields (DisallowUnknownFields) — a body carrying any of the
  // three 400s as "unknown field" (found live 2026-09-14; see
  // lib/configPayload.ts's doc comment for why ConfigWritePayload's type
  // alone can't catch this at a spread site like this one — Routing.tsx's
  // submitPatch documents the same id trap for offerings).
  const { id: _id, chat_template_caps: _caps, chat_template_caps_probed_at: _probedAt, ...existingBody } = existing;

  return (
    <ConfigEditForm
      existing={existing}
      variants={variants.data ?? []}
      artifacts={artifacts.data ?? []}
      engines={engines.data ?? []}
      builds={builds.data ?? []}
      models={models.data ?? []}
      families={families.data ?? []}
      genealogies={genealogies.data ?? []}
      capabilityTiers={capabilityTiers.data ?? []}
      modelFiles={modelFiles.data ?? []}
      showFiles={showFiles}
      onToggleFiles={() => setShowFiles(!showFiles)}
      pending={update.isPending}
      error={error}
      onCancel={onCancel}
      onUploadIcon={(file, dark) => uploadIcon.mutate({ id: configId, file, dark }, { onError: (e) => setError(apiErrorMessage(e)) })}
      onSelectIcon={(slug, dark) => update.mutate({ id: configId, c: dark ? { ...existingBody, logo_dark: slug } : { ...existingBody, logo: slug } }, { onError: (e) => setError(apiErrorMessage(e)) })}
      onClearIcon={(dark) => update.mutate({ id: configId, c: dark ? { ...existingBody, logo_dark: "" } : { ...existingBody, logo: "" } }, { onError: (e) => setError(apiErrorMessage(e)) })}
      onSubmit={(draft, reason) => {
        setError(null);
        update.mutate(
          { id: configId, c: draft, reason },
          // Sprint K: delayed onDone so SaveButton's flash has time to
          // paint before this view pops off DetailModal's stack.
          { onSuccess: () => setTimeout(onDone, 700), onError: (e) => setError(apiErrorMessage(e)) },
        );
      }}
    />
  );
}

// Split out so it only needs the plain CatalogConfig-shaped fields
// ConfigDetailView's ConfigCard already carries most of, but takes the
// real CatalogConfig record (queried above) since editing needs the raw
// foreign keys a card's derived/display fields don't expose.
function ConfigEditForm({
  existing,
  variants,
  artifacts,
  engines,
  builds,
  models,
  families,
  genealogies,
  capabilityTiers,
  modelFiles,
  showFiles,
  onToggleFiles,
  onSubmit,
  onCancel,
  onUploadIcon,
  onSelectIcon,
  onClearIcon,
  pending,
  error,
}: {
  existing: CatalogConfig;
  variants: CatalogVariant[];
  artifacts: CatalogArtifact[];
  engines: CatalogEngine[];
  builds: CatalogBuild[];
  models: CatalogModel[];
  families: CatalogFamily[];
  genealogies: CatalogGenealogy[];
  capabilityTiers: CatalogCapabilityTier[];
  modelFiles: CatalogModelFile[];
  showFiles: boolean;
  onToggleFiles: () => void;
  onSubmit: (draft: ConfigWritePayload, reason: string) => void;
  onCancel: () => void;
  onUploadIcon: (file: File, dark?: boolean) => void;
  onSelectIcon: (slug: string, dark?: boolean) => void;
  onClearIcon: (dark?: boolean) => void;
  pending: boolean;
  error: string | null;
}) {
  const { t } = useTranslation("models");
  const [reason, setReason] = useState("");
  const [name, setName] = useState(existing.name);
  const [variantId, setVariantId] = useState(existing.variant_id);
  const [weightArtifactId, setWeightArtifactId] = useState(existing.weight_artifact_id);
  const [engineId, setEngineId] = useState(existing.engine_id);
  const [buildId, setBuildId] = useState(existing.build_id);
  const [mmprojArtifactId, setMmprojArtifactId] = useState(existing.mmproj_artifact_id);
  const [nCtx, setNCtx] = useState(existing.n_ctx);
  const [extraArgs, setExtraArgs] = useState(existing.extra_args);
  const [status, setStatus] = useState(existing.status);
  const [visibility, setVisibility] = useState(existing.visibility);
  const [isDefault, setIsDefault] = useState(existing.is_default);
  const [reasoningEffortDefault, setReasoningEffortDefault] = useState(existing.reasoning_effort_default);
  const [capabilityTierId, setCapabilityTierId] = useState(existing.capability_tier_id);
  const [capabilityRank, setCapabilityRank] = useState(existing.capability_rank);
  const [capsOverride, setCapsOverride] = useState<CapsOverride>(existing.chat_template_caps_override);

  const variant = variants.find((v) => v.id === variantId);
  const weightArtifacts = artifacts.filter((a) => a.variant_id === variantId && a.artifact_type === "weight");
  const mmprojArtifacts = artifacts.filter((a) => a.artifact_type === "mmproj");
  const engineBuilds = builds.filter((b) => b.engine_id === engineId);

  // Vulkan vs ROCm both speak llama.cpp's flag set; only a vLLM build's
  // config needs VLLM_FLAGS (ConfigDetailView.backend, derived server-side
  // from the linked Build, is the source of truth for a loaded card — here,
  // before any save, the config's own name is still the only reliable
  // signal, since Build has no backend field in the frontend catalog types
  // yet). Good enough for a curated-flag lookup; a wrong guess only means a
  // flag renders without its tooltip, never a wrong value.
  const backend = existing.name === "carbon-8b" || existing.name === "hy-mt2" ? "vllm" : "llama.cpp";

  function submit() {
    const payload: ConfigWritePayload = {
      name,
      variant_id: variantId,
      weight_artifact_id: weightArtifactId,
      engine_id: engineId,
      build_id: buildId,
      mmproj_artifact_id: mmprojArtifactId,
      n_ctx: nCtx,
      // Carried through unchanged, not user-editable here — see this
      // file's doc comment on why Parallel has no input.
      parallel: existing.parallel,
      extra_args: extraArgs,
      status,
      visibility,
      is_default: isDefault,
      fingerprint: existing.fingerprint,
      // Carried through unchanged, same as parallel/fingerprint above —
      // the icon override lives on IconPicker's own save path (Sprint I),
      // not this form, and UpdateConfig is full-replace.
      logo: existing.logo,
      logo_dark: existing.logo_dark,
      // Carried through unchanged — this form has no modalities control
      // (only CatalogPanel's ConfigForm does); omitting it entirely, as
      // this form did before, sends a nil override and silently resets an
      // existing modalities override back to "derive" on every save
      // (store.UpdateConfig writes the modalities column unconditionally —
      // found while fixing the same class of bug for capability_tier_id/
      // chat_template_caps_override below).
      modalities: existing.modalities,
      capability_tier_id: capabilityTierId,
      capability_rank: capabilityRank,
      chat_template_caps_override: capsOverride,
      reasoning_effort_default: reasoningEffortDefault,
    };
    onSubmit(payload, reason);
  }

  return (
    <div className="config-edit-view">
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}

      <div className="eyebrow" style={{ marginTop: 0 }}>{t("config_edit.identity")}</div>
      <div className="form-grid">
        <label className="form-row">{t("config_edit.name")}
          <input value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <label className="form-row">{t("config_edit.variant")}
          <select value={variantId} onChange={(e) => { const v = Number(e.target.value); setVariantId(v); setWeightArtifactId(0); }}>
            <option value={0}>{t("config_edit.variant_select")}</option>
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
        <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("config_edit.weight_artifact")}
          <div style={{ display: "flex", gap: 6 }}>
            <select value={weightArtifactId} onChange={(e) => setWeightArtifactId(Number(e.target.value))} style={{ flex: 1 }}>
              <option value={0}>{t("config_edit.variant_select")}</option>
              {weightArtifacts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.file_path}{a.shard_set_id ? t("config_edit.shard_set_suffix") : ""}
                </option>
              ))}
              {weightArtifacts.length === 0 && variant && (
                <option value={0} disabled>{t("config_edit.no_weight_artifacts")}</option>
              )}
            </select>
            <button type="button" className="btn" style={{ fontSize: 11, padding: "4px 8px", whiteSpace: "nowrap" }} onClick={onToggleFiles}>
              {showFiles ? t("config_edit.hide_files") : t("config_edit.browse_files")}
            </button>
          </div>
        </label>
        <label className="form-row">{t("config_edit.mmproj")}
          <select value={mmprojArtifactId} onChange={(e) => setMmprojArtifactId(Number(e.target.value))}>
            <option value={0}>{t("config_edit.mmproj_none")}</option>
            {mmprojArtifacts.map((a) => (
              <option key={a.id} value={a.id}>{a.file_path}</option>
            ))}
          </select>
        </label>
      </div>

      {showFiles && (
        <div style={{ maxHeight: 220, overflow: "auto", border: "1px solid var(--border)", borderRadius: 8, padding: 8, marginBottom: 14 }}>
          <div style={{ fontSize: 10.5, color: "var(--text-mute)", marginBottom: 6, textTransform: "uppercase", letterSpacing: ".06em" }}>
            {t("config_edit.model_files_on_disk", { count: modelFiles.length })}
          </div>
          {modelFiles.length === 0 && <div className="empty-note">{t("config_edit.no_gguf_files")}</div>}
          {modelFiles.map((f) => (
            <div key={f.path} style={{ display: "flex", gap: 8, fontSize: 11, padding: "4px 0", borderBottom: "1px solid var(--border)", fontFamily: "var(--mono)" }}>
              <span style={{ flex: 1, color: "var(--text-dim)" }}>{f.path}</span>
              <span style={{ color: "var(--text-mute)" }}>{formatGB(f.size_bytes, 1)} GB</span>
              {f.arch && <span style={{ color: "var(--cool)" }}>{f.arch}</span>}
              {f.trained_ctx > 0 && <span style={{ color: "var(--text-mute)" }}>ctx:{f.trained_ctx.toLocaleString()}</span>}
              {f.is_shard_set && <span className="chip">{t("config_edit.shard_set_chip")}</span>}
            </div>
          ))}
        </div>
      )}

      <div className="eyebrow">{t("config_edit.engine")}</div>
      <div className="form-grid">
        <label className="form-row">{t("config_edit.engine_select")}
          <select value={engineId} onChange={(e) => { const v = Number(e.target.value); setEngineId(v); setBuildId(0); }}>
            <option value={0}>{t("config_edit.variant_select")}</option>
            {engines.map((e) => <option key={e.id} value={e.id}>{e.name}</option>)}
          </select>
        </label>
        <label className="form-row">{t("config_edit.build")}
          <select value={buildId} onChange={(e) => setBuildId(Number(e.target.value))}>
            <option value={0}>{t("config_edit.build_none")}</option>
            {engineBuilds.map((b) => <option key={b.id} value={b.id}>{b.name}</option>)}
          </select>
        </label>
      </div>

      <div className="eyebrow">{t("config_edit.runtime")}</div>
      <div className="form-grid">
        <label className="form-row">{t("config_edit.context")}
          <input type="number" min={0} value={nCtx} onChange={(e) => setNCtx(Number(e.target.value))} />
        </label>
        <label className="form-row">{t("config_edit.capability_tier")}
          <select value={capabilityTierId} onChange={(e) => setCapabilityTierId(Number(e.target.value))}>
            <option value={0}>{t("config_edit.capability_tier_none")}</option>
            {capabilityTiers.map((p) => (
              <option key={p.id} value={p.id}>{p.name}</option>
            ))}
          </select>
          <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
            {t("config_edit.capability_tier_hint")}
          </span>
        </label>
        {capabilityTierId !== 0 && (
          <label className="form-row">{t("config_edit.capability_rank")}
            <input type="number" value={capabilityRank} onChange={(e) => setCapabilityRank(Number(e.target.value))} />
            <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>{t("config_edit.capability_rank_hint")}</span>
          </label>
        )}
        <label className="form-row">{t("config_edit.default_reasoning_effort")}
          <select value={reasoningEffortDefault} onChange={(e) => setReasoningEffortDefault(e.target.value as typeof reasoningEffortDefault)}>
            <option value="">{t("config_edit.reasoning_none")}</option>
            <option value="none">{t("config_edit.reasoning_off")}</option>
            <option value="low">{t("config_edit.reasoning_low")}</option>
            <option value="medium">{t("config_edit.reasoning_medium")}</option>
            <option value="high">{t("config_edit.reasoning_high")}</option>
          </select>
          <span style={{ fontSize: 10.5, color: "var(--text-mute)" }}>
            {t("config_edit.default_reasoning_hint")}
          </span>
        </label>
        <ChatTemplateCapsOverrideEditor
          probed={existing.chat_template_caps}
          override={capsOverride}
          onChange={setCapsOverride}
        />
      </div>
      <LoadOptionsEditor value={extraArgs} onChange={setExtraArgs} backend={backend} nCtx={nCtx} />

      <div className="eyebrow">{t("config_edit.publishing")}</div>
      <div className="form-grid">
        <label className="form-row">{t("config_edit.status")}
          <select value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="unverified">{t("config_edit.status_unverified")}</option>
            <option value="verified">{t("config_edit.status_verified")}</option>
          </select>
        </label>
        <label className="form-row">{t("config_edit.visibility")}
          <select value={visibility} onChange={(e) => setVisibility(e.target.value)}>
            <option value="visible">{t("config_edit.visibility_visible")}</option>
            <option value="hidden">{t("config_edit.visibility_hidden")}</option>
          </select>
        </label>
        <label className="form-row" style={{ flexDirection: "row", alignItems: "center", gap: 8, gridColumn: "1 / -1" }}>
          <input type="checkbox" checked={isDefault} onChange={(e) => setIsDefault(e.target.checked)} />
          {t("config_edit.is_default")}
        </label>
      </div>

      <div className="eyebrow">{t("config_edit.icon")}</div>
      <IconPicker
        value={existing.logo}
        valueDark={existing.logo_dark}
        inherited={variant ? modelInheritedIcon(variant.model_id, models, families, genealogies) : null}
        onSelect={onSelectIcon}
        onClear={onClearIcon}
        onUpload={onUploadIcon}
      />

      <label className="form-row">{t("config_edit.reason_label")}
        <input value={reason} placeholder={t("config_edit.reason_placeholder")} onChange={(e) => setReason(e.target.value)} />
      </label>

      <div className="form-actions" style={{ marginTop: 20 }}>
        <button type="button" className="btn" onClick={onCancel}>{t("config_edit.cancel")}</button>
        <SaveButton className="go" pending={pending} isError={!!error} disabled={pending || !name} onClick={submit} />
      </div>
    </div>
  );
}
