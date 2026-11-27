// profileFormat — salvaged from the deleted ProfilingPanel.tsx (Phase 8,
// pre-release feedback sprint, profiling merged into Benchmarks). Shared by
// ProfileRunCard and BenchmarkTable so both read the same wording.
//
// A measurement's state (never | current | outdated | unsupported) comes from the server (go/internal/profile/status.go).

export function depthLabel(depthTokens: number, nCtx: number, t: (key: string, opts?: Record<string, unknown>) => string): string {
  if (nCtx <= 0) return t("benchmarks.depth_label.tokens", { tokens: depthTokens });
  const pct = Math.round((depthTokens / nCtx) * 100);
  return depthTokens === 0 ? t("benchmarks.depth_label.empty") : t("benchmarks.depth_label.pct", { pct });
}
