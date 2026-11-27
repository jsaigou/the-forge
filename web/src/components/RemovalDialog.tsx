// Removing a model or a config: shows exactly what goes and what stands in the way, then asks once. Deleting files from disk is opt-in and lists
// every file, including the ones that stay and why. Opened from Settings → Model catalog and from the config and model editors.
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api";
import { formatGB } from "../lib/format";
import { useCatalogRemovalPlan, useCatalogRemove } from "../lib/queries";
import type { RemovalFile, RemovalKind, RemovalResult } from "../lib/types";
import { useStepUpGate } from "../lib/useStepUpGate";
import { Modal } from "./Modal";
import { StepUpModal } from "./StepUpModal";

export function RemovalDialog({ kind: initialKind, id: initialId, onClose, onRemoved }: {
  kind: RemovalKind;
  id: number;
  onClose: () => void;
  onRemoved?: () => void;
}) {
  const { t } = useTranslation("common");
  // Removing a model's last config can widen into removing the model.
  const [target, setTarget] = useState<{ kind: RemovalKind; id: number }>({ kind: initialKind, id: initialId });
  const [widenFrom, setWidenFrom] = useState<number | null>(null);
  const [deleteFiles, setDeleteFiles] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [result, setResult] = useState<RemovalResult | null>(null);
  const plan = useCatalogRemovalPlan(target.kind, target.id, result === null);
  const remove = useCatalogRemove();
  const gate = useStepUpGate();
  const p = plan.data;

  const deletable = p?.files.filter((f) => f.delete) ?? [];
  const kept = p?.files.filter((f) => !f.delete) ?? [];
  const bytes = deletable.reduce((sum, f) => sum + f.bytes, 0);
  const blocked = (p?.blockers.length ?? 0) > 0;

  function finish() {
    onRemoved?.();
    onClose();
  }

  function submit() {
    if (!p) return;
    setError(null);
    remove.mutate(
      { kind: target.kind, id: target.id, deleteFiles: deleteFiles && deletable.length > 0, token: p.token },
      {
        onSuccess: (r) => {
          if (Object.keys(r.file_errors).length > 0) setResult(r);
          else finish();
        },
        onError: (e) => {
          if (gate.handle(e, submit)) return;
          setError(apiErrorMessage(e));
          void plan.refetch();
        },
      },
    );
  }

  function widen(on: boolean) {
    if (!p) return;
    setError(null);
    setDeleteFiles(false);
    if (on) {
      setWidenFrom(target.id);
      setTarget({ kind: "model", id: p.model_id });
    } else if (widenFrom !== null) {
      setTarget({ kind: "config", id: widenFrom });
      setWidenFrom(null);
    }
  }

  const fileLine = (f: RemovalFile) => (
    <li key={f.path} style={{ overflowWrap: "anywhere" }}>
      <span style={{ fontFamily: "var(--mono)" }}>{f.path}</span>
      {f.is_dir && ` ${t("removal.folder")}`}
      {f.bytes > 0 && ` · ${formatGB(f.bytes, 1)} GB`}
      {!f.delete && ` — ${t(`removal.keep.${f.keep}`, { by: f.kept_by })}`}
    </li>
  );

  if (result) {
    return (
      <Modal title={t("removal.partial_title")} onClose={finish} wide>
        <div className="error-note" style={{ marginBottom: 12 }}>{t("removal.partial_body")}</div>
        <ul style={{ fontSize: 12, paddingLeft: 18 }}>
          {Object.entries(result.file_errors).map(([path, why]) => (
            <li key={path} style={{ overflowWrap: "anywhere" }}><span style={{ fontFamily: "var(--mono)" }}>{path}</span> — {why}</li>
          ))}
        </ul>
        <div className="form-actions"><button type="button" className="btn" onClick={finish}>{t("removal.close")}</button></div>
      </Modal>
    );
  }

  const list = { margin: "4px 0 12px 18px", padding: 0, fontSize: 12, lineHeight: 1.6 } as const;
  return (
    <>
      <Modal title={p ? t(`removal.title_${target.kind}`, { name: p.name }) : t("removal.loading")} onClose={() => { if (!remove.isPending) onClose(); }} wide>
        {plan.isError && <div className="error-note">{apiErrorMessage(plan.error)}</div>}
        {p && (
          <>
            {blocked && (
              <div className="error-note" style={{ marginBottom: 12 }}>
                <b>{t("removal.blocked")}</b>
                <ul style={list}>
                  {p.blockers.map((b, i) => (
                    <li key={i}>{t(`removal.blocker.${b.code}`, { config: b.config, name: b.name })}</li>
                  ))}
                </ul>
              </div>
            )}

            <div style={{ fontSize: 13 }}>{t("removal.will_remove")}</div>
            <ul style={list}>
              {target.kind === "model" && <li>{t("removal.the_model", { name: p.model_name })}</li>}
              {p.configs.length > 0 ? (
                <li>
                  {t("removal.configs", { count: p.configs.length })}{" "}
                  <span style={{ fontFamily: "var(--mono)" }}>{p.configs.map((c) => c.name).join(", ")}</span>
                </li>
              ) : (
                <li>{t("removal.no_configs")}</li>
              )}
              {p.aliases.length > 0 && <li>{t("removal.aliases", { names: p.aliases.join(", ") })}</li>}
              {p.offerings.length > 0 && <li>{t("removal.offerings", { names: p.offerings.join(", ") })}</li>}
              {p.benchmarks > 0 && <li>{t("removal.benchmarks", { count: p.benchmarks })}</li>}
              {p.notes > 0 && <li>{t("removal.notes", { count: p.notes })}</li>}
              {p.configs.length > 0 && <li>{t("removal.history")}</li>}
            </ul>

            {(p.last_config_of_model || widenFrom !== null) && (
              <label style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: 12.5, marginBottom: 10 }}>
                <input type="checkbox" checked={widenFrom !== null} onChange={(e) => widen(e.target.checked)} disabled={remove.isPending} />
                <span>{t("removal.also_model", { name: p.model_name })}</span>
              </label>
            )}

            {deletable.length > 0 && (
              <label style={{ display: "flex", gap: 8, alignItems: "flex-start", fontSize: 12.5, marginBottom: 4 }}>
                <input type="checkbox" checked={deleteFiles} onChange={(e) => setDeleteFiles(e.target.checked)} disabled={remove.isPending || blocked} />
                <span>{t("removal.delete_files", { count: deletable.length, size: formatGB(bytes, 1) })}</span>
              </label>
            )}
            {deletable.length > 0 && <ul style={{ ...list, opacity: deleteFiles ? 1 : 0.6 }}>{deletable.map(fileLine)}</ul>}
            {kept.length > 0 && (
              <>
                <div style={{ fontSize: 12, color: "var(--text-dim)" }}>{t("removal.kept_files")}</div>
                <ul style={{ ...list, color: "var(--text-dim)" }}>{kept.map(fileLine)}</ul>
              </>
            )}

            <div style={{ fontSize: 12, color: "var(--warn)", marginBottom: 12 }}>
              {deleteFiles ? t("removal.warn_files") : t("removal.warn_catalog")}
            </div>
          </>
        )}
        {error && <div className="error-note" style={{ marginBottom: 12 }}>{error}</div>}
        <div className="form-actions">
          <button type="button" className="btn" onClick={onClose} disabled={remove.isPending}>{t("removal.cancel")}</button>
          <button type="button" className="btn danger" onClick={submit} disabled={!p || blocked || remove.isPending || plan.isFetching}>
            {remove.isPending
              ? t("removal.removing")
              : p
                ? target.kind === "model" && p.configs.length === 0
                  ? t(deleteFiles ? "removal.confirm_bare_model_files" : "removal.confirm_bare_model")
                  : t(deleteFiles ? `removal.confirm_${target.kind}_files` : `removal.confirm_${target.kind}`, { count: p.configs.length })
                : t("removal.loading")}
          </button>
        </div>
      </Modal>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </>
  );
}
