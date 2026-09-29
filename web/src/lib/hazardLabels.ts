import { canonicalFlag, hazardsFor, parseLoadOptions, type Hazard } from "./llamaFlags";
import type { ConfigCard } from "./types";

// translatedHazardsFor wraps llamaFlags.ts's hazardsFor with i18n. hazardsFor
// itself stays English-only (it's also read by non-UI code paths) — this
// re-derives the same two known hazard types' interpolation values
// (nCtx/flag/n/perSlot) from the same inputs and looks up a translated
// template by canonical flag name, falling back to the original English
// message via `defaultValue` for any future hazard type this doesn't know
// about yet.
export interface TranslatedHazard extends Hazard {
  translated: string;
}

export function translatedHazardsFor(
  t: (key: string, opts?: Record<string, unknown>) => string,
  card: Pick<ConfigCard, "extra_args" | "n_ctx">,
): TranslatedHazard[] {
  const hazards = hazardsFor(card);
  const opts = parseLoadOptions(card.extra_args);

  return hazards.map((h) => {
    const canon = canonicalFlag(h.flag);
    if (canon === "--parallel") {
      const row = opts.find((o) => canonicalFlag(o.flag) === "--parallel");
      const n = row?.value != null ? parseInt(row.value, 10) : NaN;
      const perSlot = Number.isFinite(n) && n > 0 ? Math.floor(card.n_ctx / n) : 0;
      return {
        ...h,
        translated: t("common:hazards.parallel", {
          nCtx: card.n_ctx.toLocaleString(),
          flag: h.flag,
          n,
          perSlot: perSlot.toLocaleString(),
          defaultValue: h.message,
        }),
      };
    }
    if (canon === "--ctx-checkpoints") {
      return { ...h, translated: t("common:hazards.ctx_checkpoints", { defaultValue: h.message }) };
    }
    return { ...h, translated: h.message };
  });
}
