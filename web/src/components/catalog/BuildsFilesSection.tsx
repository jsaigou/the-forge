// SPDX-License-Identifier: Apache-2.0

// BuildsFilesSection — the Builds & files catalog tab (ADR-0020 F8). Builds (the llama-server binaries configs run on)
// and artifacts (the weight files variants ship as) had no write path in the UI; the API now has one, with the same
// guards smith applies, so a binary or a file already on the host can be registered here without a deploy.

import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useErrorState } from "../../lib/useErrorState";
import {
  useCatalogArtifacts,
  useCatalogBuilds,
  useCatalogEngines,
  useCatalogFormats,
  useCatalogModels,
  useCatalogQuantizations,
  useCatalogVariants,
  useCreateCatalogArtifact,
  useCreateCatalogBuild,
  useDeleteCatalogArtifact,
  useRetireCatalogBuild,
} from "../../lib/queries";
import { ConfirmButton } from "../ConfirmButton";

const BACKENDS = ["rocm", "vulkan", "vllm"] as const;
const ARTIFACT_TYPES = ["weight", "mmproj", "tokenizer"] as const;

export function BuildsFilesSection({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <BuildsBlock canAdmin={canAdmin} />
      <div style={{ height: 22 }} />
      <ArtifactsBlock canAdmin={canAdmin} />
    </>
  );
}

function BuildsBlock({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const builds = useCatalogBuilds();
  const engines = useCatalogEngines();
  const create = useCreateCatalogBuild();
  const retire = useRetireCatalogBuild();
  const { error, showError, clearError } = useErrorState();
  const [adding, setAdding] = useState(false);
  const [engineId, setEngineId] = useState(0);
  const [name, setName] = useState("");
  const [backend, setBackend] = useState<string>("rocm");
  const [path, setPath] = useState("");
  const [reason, setReason] = useState("");
  const [probe, setProbe] = useState<string | null>(null);

  const list = (builds.data ?? []).filter((b) => !b.retired);
  const retired = (builds.data ?? []).filter((b) => b.retired);
  const engineList = engines.data ?? [];
  const engineName = (id: number) => engineList.find((e) => e.id === id)?.name ?? `#${id}`;

  function submit() {
    clearError();
    setProbe(null);
    create.mutate(
      { engine_id: engineId || engineList[0]?.id || 0, name, binary_path: path, backend, reason },
      {
        onSuccess: (b) => {
          setProbe(
            b.load_mode_supported === undefined
              ? t("catalog.builds_files.probe_unknown")
              : b.load_mode_supported
                ? t("catalog.builds_files.probe_load_mode")
                : t("catalog.builds_files.probe_no_mmap"),
          );
          setAdding(false);
          setName("");
          setPath("");
          setReason("");
        },
        onError: showError,
      },
    );
  }

  if (builds.isError) return <div className="empty-note">{t("catalog.catalog_unavailable")}</div>;

  return (
    <>
      <div className="eyebrow" style={{ marginBottom: 8 }}>{t("catalog.builds_files.builds_title")}</div>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {probe && <div className="empty-note" style={{ marginBottom: 10 }}>{probe}</div>}
      {list.length === 0 && <div className="empty-note">{t("catalog.builds_files.builds_empty")}</div>}
      {list.map((b) => (
        <div className="qrow" key={b.id} style={{ alignItems: "flex-start" }}>
          <span style={{ width: 220, fontFamily: "var(--mono)", fontSize: 12 }}>{b.name}</span>
          <span style={{ width: 70, fontSize: 11, color: "var(--text-dim)" }}>{b.backend || "—"}</span>
          <span style={{ width: 90, fontSize: 11, color: "var(--text-dim)" }}>{engineName(b.engine_id)}</span>
          <span style={{ flex: 1, fontFamily: "var(--mono)", fontSize: 11, color: "var(--text-dim)", wordBreak: "break-all" }}>
            {b.binary_path || "—"}
          </span>
          {canAdmin && (
            <ConfirmButton
              className="btn"
              style={{ fontSize: 11, padding: "4px 8px", marginLeft: "auto" }}
              label={t("catalog.builds_files.retire")}
              pending={retire.isPending}
              onConfirm={() => { clearError(); retire.mutate(b.id, { onError: showError }); }}
              warning={t("catalog.builds_files.retire_confirm", { name: b.name })}
            />
          )}
        </div>
      ))}
      {retired.length > 0 && (
        <details style={{ marginTop: 10 }}>
          <summary style={{ fontSize: 11.5, color: "var(--text-dim)", cursor: "pointer" }}>
            {t("catalog.builds_files.retired_count", { count: retired.length })}
          </summary>
          {retired.map((b) => (
            <div className="qrow" key={b.id} style={{ fontSize: 11, color: "var(--text-mute)" }}>
              <span style={{ fontFamily: "var(--mono)" }}>{b.name.replace(/^\(retired\) /, "")}</span>
            </div>
          ))}
        </details>
      )}
      {canAdmin && !adding && (
        <button className="btn" style={{ marginTop: 14 }} onClick={() => { setAdding(true); clearError(); setProbe(null); }}>
          {t("catalog.builds_files.register_build")}
        </button>
      )}
      {canAdmin && adding && (
        <div className="form-grid" style={{ marginTop: 14 }}>
          <label className="form-row">{t("catalog.builds_files.engine_label")}
            <select value={engineId || engineList[0]?.id || 0} onChange={(e) => setEngineId(Number(e.target.value))}>
              {engineList.map((e) => <option key={e.id} value={e.id}>{e.name}</option>)}
            </select>
          </label>
          <label className="form-row">{t("catalog.builds_files.name_label")}
            <input value={name} onChange={(e) => setName(e.target.value)} />
          </label>
          <label className="form-row">{t("catalog.builds_files.backend_label")}
            <select value={backend} onChange={(e) => setBackend(e.target.value)}>
              {BACKENDS.map((b) => <option key={b} value={b}>{b}</option>)}
            </select>
          </label>
          <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("catalog.builds_files.path_label")}
            <input value={path} placeholder="/opt/forge/llama.cpp-…/build-…/bin/llama-server" onChange={(e) => setPath(e.target.value)} />
          </label>
          <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("catalog.builds_files.reason_label")}
            <input value={reason} onChange={(e) => setReason(e.target.value)} />
          </label>
          <div style={{ gridColumn: "1 / -1", display: "flex", gap: 8 }}>
            <button className="btn primary" disabled={create.isPending || !name} onClick={submit}>{t("catalog.builds_files.register")}</button>
            <button className="btn" onClick={() => { setAdding(false); clearError(); }}>{t("catalog.builds_files.cancel")}</button>
          </div>
        </div>
      )}
    </>
  );
}

function ArtifactsBlock({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const artifacts = useCatalogArtifacts();
  const variants = useCatalogVariants();
  const models = useCatalogModels();
  const formats = useCatalogFormats();
  const quants = useCatalogQuantizations();
  const create = useCreateCatalogArtifact();
  const remove = useDeleteCatalogArtifact();
  const { error, showError, clearError } = useErrorState();
  const [adding, setAdding] = useState(false);
  const [variantId, setVariantId] = useState(0);
  const [formatId, setFormatId] = useState(0);
  const [quantId, setQuantId] = useState(0);
  const [type, setType] = useState<string>("weight");
  const [file, setFile] = useState("");

  const variantList = variants.data ?? [];
  const modelName = (id: number) => (models.data ?? []).find((m) => m.id === id)?.name ?? `#${id}`;
  const variantLabel = (id: number) => {
    const v = variantList.find((x) => x.id === id);
    return v ? `${modelName(v.model_id)} / ${v.name}` : `#${id}`;
  };
  const formatList = formats.data ?? [];
  const quantList = quants.data ?? [];

  function submit() {
    clearError();
    create.mutate(
      {
        variant_id: variantId || variantList[0]?.id || 0,
        format_id: formatId || formatList[0]?.id || 0,
        quantization_id: quantId,
        file_path: file,
        artifact_type: type,
      },
      { onSuccess: () => { setAdding(false); setFile(""); }, onError: showError },
    );
  }

  if (artifacts.isError) return <div className="empty-note">{t("catalog.catalog_unavailable")}</div>;
  const list = artifacts.data ?? [];

  return (
    <>
      <div className="eyebrow" style={{ marginBottom: 8 }}>{t("catalog.builds_files.artifacts_title")}</div>
      <div style={{ fontSize: 11.5, color: "var(--text-dim)", marginBottom: 10, lineHeight: 1.5 }}>
        {t("catalog.builds_files.artifacts_intro")}
      </div>
      {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
      {list.length === 0 && <div className="empty-note">{t("catalog.builds_files.artifacts_empty")}</div>}
      {list.map((a) => (
        <div className="qrow" key={a.id} style={{ alignItems: "flex-start" }}>
          <span style={{ width: 220, fontSize: 12 }}>{variantLabel(a.variant_id)}</span>
          <span style={{ width: 70, fontSize: 11, color: "var(--text-dim)" }}>{a.artifact_type}</span>
          <span style={{ flex: 1, fontFamily: "var(--mono)", fontSize: 11, color: "var(--text-dim)", wordBreak: "break-all" }}>
            {a.file_path}{a.missing ? ` — ${t("catalog.builds_files.missing")}` : ""}
          </span>
          {canAdmin && (
            <ConfirmButton
              className="btn"
              style={{ fontSize: 11, padding: "4px 8px", marginLeft: "auto" }}
              pending={remove.isPending}
              onConfirm={() => { clearError(); remove.mutate(a.id, { onError: (e) => showError(e) }); }}
              warning={t("catalog.builds_files.delete_confirm", { file: a.file_path })}
            />
          )}
        </div>
      ))}
      {canAdmin && !adding && (
        <button className="btn" style={{ marginTop: 14 }} onClick={() => { setAdding(true); clearError(); }}>
          {t("catalog.builds_files.register_artifact")}
        </button>
      )}
      {canAdmin && adding && (
        <div className="form-grid" style={{ marginTop: 14 }}>
          <label className="form-row">{t("catalog.builds_files.variant_label")}
            <select value={variantId || variantList[0]?.id || 0} onChange={(e) => setVariantId(Number(e.target.value))}>
              {variantList.map((v) => <option key={v.id} value={v.id}>{variantLabel(v.id)}</option>)}
            </select>
          </label>
          <label className="form-row">{t("catalog.builds_files.type_label")}
            <select value={type} onChange={(e) => setType(e.target.value)}>
              {ARTIFACT_TYPES.map((x) => <option key={x} value={x}>{x}</option>)}
            </select>
          </label>
          <label className="form-row">{t("catalog.builds_files.format_label")}
            <select value={formatId || formatList[0]?.id || 0} onChange={(e) => setFormatId(Number(e.target.value))}>
              {formatList.map((f) => <option key={f.id} value={f.id}>{f.name}</option>)}
            </select>
          </label>
          <label className="form-row">{t("catalog.builds_files.quant_label")}
            <select value={quantId} onChange={(e) => setQuantId(Number(e.target.value))}>
              <option value={0}>—</option>
              {quantList.map((q) => <option key={q.id} value={q.id}>{q.name}</option>)}
            </select>
          </label>
          <label className="form-row" style={{ gridColumn: "1 / -1" }}>{t("catalog.builds_files.file_label")}
            <input value={file} placeholder="qwen/model-Q6_K.gguf" onChange={(e) => setFile(e.target.value)} />
          </label>
          <div style={{ gridColumn: "1 / -1", display: "flex", gap: 8 }}>
            <button className="btn primary" disabled={create.isPending || !file} onClick={submit}>{t("catalog.builds_files.register")}</button>
            <button className="btn" onClick={() => { setAdding(false); clearError(); }}>{t("catalog.builds_files.cancel")}</button>
          </div>
        </div>
      )}
    </>
  );
}

