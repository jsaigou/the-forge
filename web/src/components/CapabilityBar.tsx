import { BENCHMARK_REFS, normalizeScore } from "../lib/benchmarks";
import type { ModelCapability } from "../lib/types";

// CapabilityBar — one capability row (label, normalized bar, raw/normalized
// tooltip). Extracted (Sprint B) from three identical private copies
// (ModelCardView's compact top-3, ConfigCardView's compact top-3, and
// ConfigCardView's expanded full list) — same rendering, same
// normalization rationale, no reason for three copies to drift.
//
// cap.score is a 0-1 fraction (registry.py: "score 0..1") of the raw
// published benchmark result. Bug fix 2026-07-23: the bar previously used
// cap.score directly as a % (width: 0.809%), rendering every bar as an
// invisible sliver.
// Normalization 2026-07-23 (F7 follow-up): a raw score isn't comparable
// across benchmark rows within one card — Humanity's Last Exam is designed
// so frontier models mostly score under 50%, while GPQA Diamond's frontier
// ceiling is ~95%, so a 43% HLE score can represent stronger relative
// capability than an 80% GPQA score. normalizeScore() rescales each raw
// score onto a 0-1 "relative-to-today's-frontier" scale using per-benchmark
// floor/ceiling references (see lib/benchmarks.ts) — that's what drives the
// bar width and the displayed number; the raw published score + its source
// citation is in the tooltip.
export function CapabilityBar({ cap }: { cap: ModelCapability }) {
  const rawPct = Math.min(100, Math.max(0, cap.score * 100));
  const normPct = normalizeScore(cap.score, cap.benchmark) * 100;
  const ref = BENCHMARK_REFS[cap.benchmark];
  const tooltip = ref
    ? `${cap.benchmark}: ${rawPct.toFixed(1)}% raw · frontier ceiling ~${(ref.ceiling * 100).toFixed(0)}% (${ref.source}, ${ref.asOf}) · ${normPct.toFixed(0)}% relative-to-frontier`
    : `${cap.benchmark}: ${rawPct.toFixed(1)}% (no frontier reference available)`;
  return (
    <div className="cap" title={tooltip}>
      <span className="track">
        <i style={{ height: `${normPct}%` }} />
      </span>
      <span className="lab">{cap.label}</span>
      <span className="pct">{normPct.toFixed(0)}%</span>
    </div>
  );
}
