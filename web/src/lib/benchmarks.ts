// Reference floor/ceiling per published academic benchmark, used to
// normalize model-card capability scores for cross-benchmark visual
// comparison (docs/v5-review-fixes.md F7 follow-up, 2026-07-23).
//
// Model cards show curated `capabilities` scores (config/models.toml) from
// several different benchmarks with wildly different achievable ranges —
// e.g. Humanity's Last Exam is designed so even frontier models mostly
// score under 50%, while GPQA Diamond's current frontier ceiling is ~95%.
// A raw score comparison across benchmark rows within one card is
// misleading without this: GPT-OSS 120B's 43.1% on HLE actually represents
// stronger relative capability than its 80.9% on GPQA Diamond once each is
// weighed against what's achievable on that specific benchmark today.
//
//   normalized = clamp((raw - floor) / (ceiling - floor), 0, 1)
//
// floor: the benchmark's random-chance/near-zero baseline.
// ceiling: the highest publicly reported score by any model, AS OF the
// date below — not a theoretical maximum. Leaderboards move; a stale
// ceiling makes every model look artificially strong as new frontier
// models surpass it. Re-verify periodically (e.g. every polish sprint that
// touches model cards) rather than treating these as fixed constants.
//
// Unknown benchmark names fall back to floor=0/ceiling=1 (no normalization
// — the raw score is used as-is) rather than guessing a reference range.
//
// defaultMetric is the capability ID a Sprint D preset selection prefills
// (web/src/components/CatalogPanel.tsx's BenchmarkForm) — matches the
// convention the models.toml registry used before the models.toml ->
// catalog migration dropped it (see migration 0026_capability_benchmarks.sql,
// which restores those rows). It's a default, not a constraint: metric stays
// operator-editable, since one benchmark can legitimately back differently
// labelled capabilities on different cards.
export interface BenchmarkRef {
  floor: number;
  ceiling: number;
  source: string; // short citation for the ceiling figure
  asOf: string; // YYYY-MM, for staleness tracking
  defaultMetric: string;
}

export const BENCHMARK_REFS: Record<string, BenchmarkRef> = {
  "GPQA Diamond": {
    floor: 0.25, // 4-option multiple choice random chance
    ceiling: 0.955, // Sakana Fugu-Ultra, benchlm.ai GPQA-D leaderboard — re-verified 2026-08, unchanged
    source: "Sakana Fugu-Ultra 95.5%",
    asOf: "2026-08",
    defaultMetric: "reasoning",
  },
  "Humanity's Last Exam": {
    floor: 0,
    ceiling: 0.533, // Claude Fable 5 — re-verified 2026-08, unchanged (a
    // higher 64.7% figure exists for Claude Opus 5 but under a "with tools"
    // agentic variant of the benchmark, not the standard leaderboard this
    // ceiling tracks — not a like-for-like comparison)
    source: "Claude Fable 5 53.3%",
    asOf: "2026-08",
    defaultMetric: "knowledge",
  },
  "SWE-bench Verified": {
    floor: 0,
    ceiling: 0.96, // Claude Opus 5 — new leader as of 2026-08-02, surpassing
    // the prior 95.5% (Claude Mythos 5); same data-contamination concerns
    // reported industry-wide at this score range apply here too.
    source: "Claude Opus 5 96% (contamination concerns reported at this range industry-wide)",
    asOf: "2026-08",
    defaultMetric: "coding",
  },
  "Terminal-Bench 2.0": {
    floor: 0,
    ceiling: 0.919, // GPT-5.6 Sol — re-verified 2026-08, unchanged
    source: "GPT-5.6 Sol 91.9%",
    asOf: "2026-08",
    defaultMetric: "agentic_logic",
  },
  AIME: {
    floor: 0, // integer-answer (000-999) competition math; near-zero
    // random-guess baseline
    ceiling: 1.0, // saturated — multiple frontier models (GPT-5.2 Pro,
    // Kimi K2-Thinking-0905) report 100% on AIME 2025 as of 2026-08
    source: "GPT-5.2 Pro / Kimi K2-Thinking-0905 100% (saturated)",
    asOf: "2026-08",
    defaultMetric: "math",
  },
};

// normalizeScore maps a raw 0-1 capability score onto a 0-1
// "relative-to-frontier" scale for the given benchmark name. Falls back to
// the raw score (clamped) when the benchmark has no reference entry.
export function normalizeScore(rawScore: number, benchmark: string): number {
  const ref = BENCHMARK_REFS[benchmark];
  if (!ref || ref.ceiling <= ref.floor) {
    return Math.min(1, Math.max(0, rawScore));
  }
  return Math.min(1, Math.max(0, (rawScore - ref.floor) / (ref.ceiling - ref.floor)));
}
