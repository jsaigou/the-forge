import { useTranslation } from "react-i18next";
import { useSpecialists } from "../lib/queries";
import type { SpecialistSlot } from "../lib/types";

// SpecialistStrip: a compact summary of the engines on the Console, right under the VRAM bar (ADR-0022). Design/QA 2026-11-26: one tile per
// engine that is actually in a slot, showing only its TYPE (Embedding, TTS, ...), never the slot number or the model name, and no empty slots.
// When several engines share a type they get a number prefix ("1 TTS", "2 TTS"). Renders nothing when no slot is occupied.
export function SpecialistStrip() {
  const { t } = useTranslation("specialists");
  const q = useSpecialists();
  const used = (q.data?.slots ?? []).filter((s) => s.state !== "empty");
  if (used.length === 0) return null;

  const typeOf = (s: SpecialistSlot) => s.functions[0] ?? "other";
  const total = new Map<string, number>();
  for (const s of used) total.set(typeOf(s), (total.get(typeOf(s)) ?? 0) + 1);
  const seen = new Map<string, number>();

  return (
    <div data-tour-id="specialist-strip">
      <div className="spec-strip">
        {used.map((s) => {
          const type = typeOf(s);
          const n = (seen.get(type) ?? 0) + 1;
          seen.set(type, n);
          const label = t(`strip.type.${type}`);
          return (
            <a className="spec-mini" key={s.slot} href="#specialists" title={t("strip.open")}>
              <div className="t">{(total.get(type) ?? 0) > 1 ? t("strip.numbered", { n, type: label }) : label}</div>
            </a>
          );
        })}
      </div>
    </div>
  );
}
