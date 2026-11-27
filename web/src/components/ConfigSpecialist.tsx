import { useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api";
import { useSpecialistBehavior, useSpecialists } from "../lib/queries";
import { useSession } from "../lib/session";
import type { ConfigCard, SpecialistBehavior } from "../lib/types";
import { AddSpecialistDialog } from "./AddSpecialistDialog";
import { BehaviorControl } from "./SpecialistCard";

// useCardSpecialist — whether a catalog config is a specialist (ADR-0022) and how it behaves. One owner per model: while it is a specialist it
// never loads into an LLM slot, so the Load buttons give way to a badge.
export function useCardSpecialist(card: ConfigCard) {
  const q = useSpecialists();
  const data = q.data;
  const occ = data?.occupants.find((o) => o.kind === "catalog-config" && o.params.config === card.name);
  const behavior: SpecialistBehavior = occ && occ.enabled ? occ.policy : "disabled";
  const slot = occ && behavior !== "disabled" ? data?.slots.find((s) => s.occupant === occ.name) : undefined;
  return { data, occ, behavior, slot, isSpecialist: !!occ && occ.enabled };
}

// SpecialistSection — "Specialist: Disabled / On demand / Always on" on a model's detail view: the same three words as the Specialists tab. Disabled
// (the default) means the model works as it always has. Choosing another value for a model that is not a specialist yet opens the Add dialog on its
// confirm step, which picks the slot, shows the memory cost and warns about a big model.
export function SpecialistSection({ card }: { card: ConfigCard }) {
  const { t } = useTranslation("specialists");
  const { canAdmin } = useSession();
  const { data, occ, behavior, slot, isSpecialist } = useCardSpecialist(card);
  const behave = useSpecialistBehavior();
  const [error, setError] = useState<string | null>(null);
  const [adding, setAdding] = useState<"always_on" | "on_demand" | null>(null);
  if (!data || data.slots.length === 0) return null;

  function pick(b: SpecialistBehavior) {
    setError(null);
    if (!occ && b !== "disabled") { setAdding(b); return; }
    if (!occ) return;
    behave.mutate({ name: occ.name, behavior: b }, { onError: (e) => setError(apiErrorMessage(e)) });
  }

  return (
    <div className="spec-onmodel" style={{ marginTop: 16 }}>
      <div className="eyebrow" style={{ fontSize: 11, flexBasis: "100%" }}>{t("modelcard.label")}</div>
      {canAdmin ? (
        <BehaviorControl value={behavior} disabled={behave.isPending} onPick={pick} label={t("modelcard.label")} />
      ) : (
        <span className="spec-pill on">{t(`behavior.${behavior}`)}</span>
      )}
      {isSpecialist && (
        <span className="spec-note">
          {slot && slot.state === "running" ? t("modelcard.running_in", { slot: slot.label || slot.slot.toUpperCase() }) : ""} <a href="#specialists">{t("modelcard.open")}</a>
        </span>
      )}
      <div className="spec-note" style={{ flexBasis: "100%" }}>{isSpecialist ? t("modelcard.managed_note") : t("modelcard.disabled_note")}</div>
      {error && <div className="spec-err" role="alert">{error}</div>}
      {adding && <AddSpecialistDialog data={data} initial={{ name: card.name, behavior: adding }} onClose={() => setAdding(null)} onDone={() => setAdding(null)} />}
    </div>
  );
}
