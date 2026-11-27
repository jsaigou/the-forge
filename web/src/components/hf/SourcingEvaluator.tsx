// components/hf/SourcingEvaluator.tsx — the "does this repo fit my memory?"
// tool (smith sourcing, P6 FR4, docs/v5-smith.md §4.9). It used to live at the
// bottom of Settings → Smith, but it is a one-shot research tool for model
// acquisition, not a setting (review §A3), so it now sits under Models → Add
// model beside the HF search it complements. Enter a HF repo, get back ranked
// GGUF candidates against a memory budget; a blank budget tells the backend to
// fall back to the live collector snapshot's GTT total (smith.Evaluate's own
// default). Read-only: nothing is downloaded or added to the catalog.
import { useId, useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../../lib/api";
import { formatGB } from "../../lib/format";
import { useSmithSourcingEvaluate } from "../../lib/queries";
import type { SmithSourcingEvaluation } from "../../lib/types";

export function SourcingEvaluator() {
  const { t } = useTranslation("models");
  const descId = useId();
  const [repo, setRepo] = useState("");
  const [budgetGB, setBudgetGB] = useState("");
  const [result, setResult] = useState<SmithSourcingEvaluation | null>(null);
  const [error, setError] = useState<string | null>(null);
  const evaluate = useSmithSourcingEvaluate();

  function run() {
    setError(null);
    setResult(null);
    const budgetBytes = budgetGB.trim() ? Math.round(parseFloat(budgetGB) * (1024 ** 3)) : undefined;
    evaluate.mutate(
      { hfRepo: repo.trim(), budgetBytes },
      {
        onSuccess: (data) => setResult(data.evaluation),
        onError: (e) => setError(apiErrorMessage(e)),
      },
    );
  }

  return (
    <>
      <div className="eyebrow" id="models-sourcing">{t("add_model.sourcing.heading")}</div>
      <div className="card">
        <div id={descId} className="src-desc">{t("add_model.sourcing.description")}</div>
        <div className="src-controls">
          <label className="form-row src-repo">
            <span className="sr-only">{t("add_model.sourcing.repo_label")}</span>
            <input
              aria-describedby={descId}
              placeholder={t("add_model.sourcing.repo_placeholder")}
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && repo.trim() && run()}
            />
          </label>
          <label className="form-row src-budget">
            <span className="sr-only">{t("add_model.sourcing.budget_label")}</span>
            <input
              inputMode="decimal"
              placeholder={t("add_model.sourcing.budget_placeholder")}
              value={budgetGB}
              onChange={(e) => setBudgetGB(e.target.value)}
            />
          </label>
          <button type="button" className="btn primary" disabled={!repo.trim() || evaluate.isPending} onClick={run}>
            {evaluate.isPending ? t("add_model.sourcing.evaluating") : t("add_model.sourcing.evaluate")}
          </button>
        </div>

        {error && <div className="error-note src-error">{error}</div>}

        {result && (
          <div className="src-result">
            <div className="src-summary">
              {t("add_model.sourcing.result_summary", { repo: result.repo, budget: formatGB(result.budget_bytes, 1) })}
              {result.cached && t("add_model.sourcing.cached_suffix")}
            </div>
            {result.candidates.length === 0 ? (
              <div className="empty-note">{t("add_model.sourcing.no_gguf")}</div>
            ) : (
              <table className="src-table">
                <thead>
                  <tr>
                    <th scope="col">{t("add_model.sourcing.col_file")}</th>
                    <th scope="col">{t("add_model.sourcing.col_quant")}</th>
                    <th scope="col" className="num">{t("add_model.sourcing.col_size")}</th>
                    <th scope="col" className="num">{t("add_model.sourcing.col_vram")}</th>
                    <th scope="col">{t("add_model.sourcing.col_fits")}</th>
                  </tr>
                </thead>
                <tbody>
                  {result.candidates.map((c) => (
                    <tr key={c.filename} className={c.recommended ? "recommended" : undefined}>
                      <td className="file">
                        {c.recommended && <span title={t("add_model.sourcing.recommended_title")}>★ </span>}
                        {c.filename}
                      </td>
                      <td className="dim">{c.quant || "—"}</td>
                      <td className="num dim">{formatGB(c.size_bytes, 1)} GB</td>
                      <td className="num dim">{formatGB(c.estimated_vram_bytes, 1)} GB</td>
                      <td className={c.fits_budget ? "fits" : "nofit"}>
                        {c.fits_budget ? t("add_model.sourcing.fits_yes") : t("add_model.sourcing.fits_no")}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {result.download_steps.length > 0 && (
              <div className="src-downloads">
                <div className="src-downloads-title">{t("add_model.sourcing.download_heading")}</div>
                {result.download_steps.map((step, i) => (
                  <pre key={`${step.command}-${i}`} className="src-cmd">{step.command}</pre>
                ))}
              </div>
            )}
          </div>
        )}
      </div>
    </>
  );
}
