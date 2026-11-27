import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { apiErrorMessage } from "../lib/api";
import { formatGB } from "../lib/format";
import {
  useConfigCards,
  useSchedulerStatus,
  useSpecialistAddSlot,
  useSpecialistBehavior,
  useSpecialistCreate,
} from "../lib/queries";
import type { ConfigCard, SpecialistFunction, SpecialistOccupant, SpecialistsResponse } from "../lib/types";

type Source = "catalog" | "service";
type Choice = { source: Source; name: string; label: string; summary: string; bytes: number; functions: SpecialistFunction[] };

const MAX_SLOTS = 12;
const BIG_SHARE = 0.1; // warn when "always on" would hold at least this share of the machine's memory

// What a catalog model can serve, from what it takes in and puts out. A chat-only model serves no named function: it is simply kept loaded.
function functionsOf(c: Pick<ConfigCard, "modalities" | "output_modalities">): SpecialistFunction[] {
  const out: SpecialistFunction[] = [];
  if (c.output_modalities.includes("decisions")) out.push("decisions");
  if (c.output_modalities.includes("audio")) out.push("speech");
  if (c.modalities.includes("audio")) out.push("transcription");
  return out;
}

// AddSpecialistDialog — the three-step way to keep something loaded: what, how it should behave, confirm. No slots, kinds, ports or launch
// parameters appear; the slot is chosen automatically and the memory figure comes from the catalog. If the pool is full the dialog says so and
// offers to add a slot. Only a model that cannot fit the machine at all is refused; a big one gets a plain warning with its share of memory.
export function AddSpecialistDialog({
  data,
  initial,
  onClose,
  onDone,
}: {
  data: SpecialistsResponse;
  /** Open on the confirm step for this catalog model (the Models page control uses this). */
  initial?: { name: string; behavior: "always_on" | "on_demand" };
  onClose: () => void;
  onDone: (name: string, where: string) => void;
}) {
  const { t } = useTranslation("specialists");
  const cards = useConfigCards("7d");
  const sched = useSchedulerStatus();
  const create = useSpecialistCreate();
  const behave = useSpecialistBehavior();
  const addSlot = useSpecialistAddSlot();
  const [step, setStep] = useState<1 | 2 | 3>(initial ? 3 : 1);
  const [source, setSource] = useState<Source>("catalog");
  const [query, setQuery] = useState("");
  const [chosen, setChosen] = useState<Choice | null>(null);
  const [behavior, setBehavior] = useState<"always_on" | "on_demand">(initial?.behavior ?? "always_on");
  const [error, setError] = useState<string | null>(null);
  const boxRef = useRef<HTMLDivElement>(null);
  // What an earlier attempt already did: a retry after a later step failed must not repeat a step that succeeded (a second create would be refused
  // as already existing, and a second add-slot would add another slot).
  const done = useRef({ slot: false, created: "" });

  const haveConfigs = new Set(data.occupants.filter((o) => o.kind === "catalog-config").map((o) => o.params.config));
  const list: Choice[] = useMemo(() => {
    if (source === "service") {
      return data.occupants
        .filter((o: SpecialistOccupant) => !o.enabled && o.kind !== "catalog-config")
        .map((o) => ({ source: "service" as const, name: o.name, label: o.label || o.name, summary: o.functions.map((f) => t(`function.${f}`)).join(", "), bytes: o.memory_bytes, functions: o.functions }));
    }
    return (cards.data?.cards ?? [])
      .filter((c) => c.visibility === "visible" && !haveConfigs.has(c.name))
      .map((c) => ({
        source: "catalog" as const,
        name: c.name,
        label: c.name,
        summary: [c.creator, c.model_name].filter(Boolean).join(" · "),
        bytes: c.derived.memory_req_bytes ?? 0,
        functions: functionsOf(c),
      }));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [source, cards.data, data.occupants, t]);
  useEffect(() => {
    if (initial && !chosen) {
      const hit = list.find((x) => x.name === initial.name);
      if (hit) setChosen(hit);
    }
  }, [initial, chosen, list]);
  const shown = list
    .filter((x) => `${x.label} ${x.summary}`.toLowerCase().includes(query.toLowerCase()));

  const total = sched.data?.memory_budget?.total_bytes ?? 0;
  const free = sched.data?.memory_budget?.free_bytes ?? 0;
  const bytes = chosen?.bytes ?? 0;
  const cannotFit = total > 0 && bytes > total;
  const bigShare = total > 0 && bytes / total >= BIG_SHARE;
  // Where an always-on member would go: the first slot with nothing in it (what the server picks too).
  const freeSlot = data.slots.find((s) => s.state === "empty" && !data.occupants.some((o) => o.enabled && o.policy === "always_on" && o.home_slot === s.slot));
  const needSlot = behavior === "always_on" && !freeSlot;
  const busy = create.isPending || behave.isPending || addSlot.isPending;

  async function submit() {
    if (!chosen) return;
    setError(null);
    try {
      if (needSlot && !done.current.slot) {
        await addSlot.mutateAsync();
        done.current.slot = true;
      }
      if (chosen.source === "catalog" && done.current.created !== chosen.name) {
        await create.mutateAsync({
          name: chosen.name, label: chosen.label, kind: "catalog-config", params: { config: chosen.name }, functions: chosen.functions,
          policy: "on_demand", enabled: true,
        });
        done.current.created = chosen.name;
      }
      const got = await behave.mutateAsync({ name: chosen.name, behavior });
      onDone(chosen.label, behavior === "always_on" ? got.home_slot.toUpperCase() : "");
    } catch (e) {
      setError(apiErrorMessage(e));
    }
  }

  // Escape closes (unless a write is in flight); focus moves into the dialog when it opens and goes back to what opened it when it closes.
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    boxRef.current?.focus();
    return () => opener?.focus?.();
  }, []);
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape" && !busy) {
        e.stopPropagation();
        onClose();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [busy, onClose]);

  const next = () => setStep((s) => (s < 3 ? ((s + 1) as 2 | 3) : s));
  const back = () => setStep((s) => (s > 1 ? ((s - 1) as 1 | 2) : s));
  const slotLabel = freeSlot ? (freeSlot.label || freeSlot.slot.toUpperCase()) : "";

  return (
    <div className="modal-backdrop" onClick={(e) => { e.stopPropagation(); onClose(); }}>
      <div ref={boxRef} tabIndex={-1} className="modal wide spec-wiz" role="dialog" aria-modal="true" aria-label={t("wizard.title")} onClick={(e) => e.stopPropagation()}>
        <div className="spec-steps" aria-hidden="true">
          {(["what", "how", "confirm"] as const).map((k, i) => (
            <span key={k} className={step === i + 1 ? "on" : step > i + 1 ? "done" : ""}>{i + 1} · {t(`wizard.step_${k}`)}</span>
          ))}
        </div>

        {step === 1 && (
          <div className="spec-wiz-body">
            <h3>{t("wizard.what_q")}</h3>
            {(["catalog", "service"] as const).map((s) => (
              <button key={s} className="spec-opt" aria-pressed={source === s} onClick={() => { setSource(s); setChosen(null); }}>
                <span className="t">{t(`wizard.src_${s}`)}</span>
                <span className="d">{t(`wizard.src_${s}_d`)}</span>
              </button>
            ))}
            <input type="text" value={query} onChange={(e) => setQuery(e.target.value)} placeholder={t("wizard.search")} aria-label={t("wizard.search")} />
            <div className="spec-pick" role="group" aria-label={t("wizard.what_q")}>
              {cards.isLoading && source === "catalog" ? <div className="spec-note">{t("loading")}</div> : shown.length === 0 ? (
                <div className="spec-note">{query.trim() ? t("wizard.no_match") : source === "service" ? t("wizard.none_service") : t("wizard.none_catalog")}</div>
              ) : shown.map((x) => (
                <button key={x.name} className="spec-it" aria-pressed={chosen?.name === x.name} onClick={() => setChosen(x)}>
                  <span className="nm">{x.label}</span>
                  {x.functions.length > 0 && <span className="chip">{x.functions.map((f) => t(`function.${f}`)).join(", ")}</span>}
                  <span className="sz">{x.bytes > 0 ? `${formatGB(x.bytes, 1)} GB` : ""}</span>
                </button>
              ))}
            </div>
          </div>
        )}

        {step === 2 && (
          <div className="spec-wiz-body">
            <h3>{t("wizard.how_q")}</h3>
            {(["always_on", "on_demand"] as const).map((b) => (
              <button key={b} className="spec-opt" aria-pressed={behavior === b} onClick={() => setBehavior(b)}>
                <span className="t">{t(`behavior.${b}`)}</span>
                <span className="d">{t(`wizard.how_${b}`)}</span>
              </button>
            ))}
          </div>
        )}

        {step === 3 && !chosen && <div className="spec-wiz-body spec-note">{t("loading")}</div>}
        {step === 3 && chosen && (
          <div className="spec-wiz-body">
            <h3>{t("wizard.ready")}</h3>
            <div className="spec-sum">
              <div><b>{chosen.label}</b> · {t(`behavior.${behavior}`)}</div>
              <div className="spec-note">
                {behavior === "always_on"
                  ? freeSlot ? t("wizard.goes_in", { slot: slotLabel }) : t("wizard.goes_in_new")
                  : t("wizard.loads_on_use")}
                {bytes > 0 && <> {t("wizard.about", { gb: formatGB(bytes, 1) })}{free > 0 && <> {t("wizard.free", { gb: formatGB(free, 0) })}</>}</>}
              </div>
            </div>
            {needSlot && data.slots.length < MAX_SLOTS && <div className="spec-warn">{t("wizard.pool_full", { n: data.slots.length })}</div>}
            {needSlot && data.slots.length >= MAX_SLOTS && <div className="spec-warn">{t("wizard.pool_limit")}</div>}
            {behavior === "always_on" && bigShare && !cannotFit && (
              <div className="spec-warn">{t("wizard.big", { name: chosen.label, gb: formatGB(bytes, 0), pct: Math.round((bytes / total) * 100), free: formatGB(free, 0) })}</div>
            )}
            {cannotFit && <div className="spec-err" role="alert">{t("wizard.cannot_fit", { name: chosen.label, gb: formatGB(bytes, 0), total: formatGB(total, 0) })}</div>}
            {chosen.source === "catalog" && <p className="spec-note">{t("wizard.one_owner")}</p>}
          </div>
        )}

        {error && <div className="spec-err" role="alert">{error}</div>}
        <div className="spec-actions spec-wiz-foot">
          <button className="btn-mini" disabled={step === 1 || busy} onClick={back}>{t("wizard.back")}</button>
          <span className="spec-grow" />
          <button className="btn-mini" disabled={busy} onClick={onClose}>{t("add.cancel")}</button>
          {step < 3 ? (
            <button className="btn-mini primary" disabled={!chosen} onClick={next}>{t("wizard.next")}</button>
          ) : (
            <button className="btn-mini primary" disabled={busy || cannotFit || (needSlot && data.slots.length >= MAX_SLOTS)} onClick={() => void submit()}>
              {needSlot && data.slots.length < MAX_SLOTS ? t("wizard.add_slot_and_go") : t("wizard.add")}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
