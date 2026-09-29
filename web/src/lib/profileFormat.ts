// profileFormat — salvaged from the deleted ProfilingPanel.tsx (Phase 8,
// pre-release feedback sprint, profiling merged into Benchmarks). Shared by
// ProfileRunCard and ConfigBenchmarkGroup so both read the same wording.

// Why a profile is "stale": the fingerprint (model path+size, quant, n_ctx,
// backend, llama.cpp binary path+mtime, extra args) no longer matches the
// live config — this is an invalidated cache, not an error. Shown next to
// the chip so the word "stale" is never left unexplained (product/QA sprint,
// 2026-07-29).
// t must be a "settings"-namespaced translate function (useTranslation("settings")).
export function staleExplanation(t: (key: string) => string): string {
  return t("benchmarks.stale_explanation");
}

export function depthLabel(depthTokens: number, nCtx: number, t: (key: string, opts?: Record<string, unknown>) => string): string {
  if (nCtx <= 0) return t("benchmarks.depth_label.tokens", { tokens: depthTokens });
  const pct = Math.round((depthTokens / nCtx) * 100);
  return depthTokens === 0 ? t("benchmarks.depth_label.empty") : t("benchmarks.depth_label.pct", { pct });
}
