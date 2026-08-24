// profileFormat — salvaged from the deleted ProfilingPanel.tsx (Phase 8,
// pre-release feedback sprint, profiling merged into Benchmarks). Shared by
// ProfileRunCard and ConfigBenchmarkGroup so both read the same wording.

// Why a profile is "stale": the fingerprint (model path+size, quant, n_ctx,
// backend, llama.cpp binary path+mtime, extra args) no longer matches the
// live config — this is an invalidated cache, not an error. Shown next to
// the chip so the word "stale" is never left unexplained (product/QA sprint,
// 2026-07-29).
export const STALE_EXPLANATION = "config changed since measurement (context, backend, or model file differs)";

export function depthLabel(depthTokens: number, nCtx: number): string {
  if (nCtx <= 0) return `${depthTokens} tok`;
  const pct = Math.round((depthTokens / nCtx) * 100);
  return depthTokens === 0 ? "empty" : `~${pct}% ctx`;
}
