import { useState } from "react";
import { useTranslation } from "react-i18next";
import { AddSpecialistDialog } from "../components/AddSpecialistDialog";
import { BehaviorControl, SpecialistCard } from "../components/SpecialistCard";
import { StepUpModal } from "../components/StepUpModal";
import { apiErrorMessage } from "../lib/api";
import {
  useSpecialistAddSlot,
  useSpecialistBehavior,
  useSpecialistDefault,
  useSpecialistLoad,
  useSpecialists,
  useSpecialistUnload,
} from "../lib/queries";
import { useSession } from "../lib/session";
import { useStepUpGate } from "../lib/useStepUpGate";
import type { SpecialistBehavior, SpecialistFunction, SpecialistOccupant } from "../lib/types";

const MAX_SLOTS = 12; // ports 8100-8111; the server enforces the same cap

// Specialists — the specialist slot pool (ADR-0022): small, purpose-built models and services that stay loaded because latency matters. Separate
// from the LLM slots on the Console. Every specialist has one setting, Behavior (Always on / On demand / Disabled); Disabled unloads it and frees its
// slot but keeps it registered. Viewers read; admins change behavior and set the per-function defaults a0 answers with.
export function Specialists() {
  const { t } = useTranslation("specialists");
  const { canAdmin } = useSession();
  const q = useSpecialists();
  const load = useSpecialistLoad();
  const unload = useSpecialistUnload();
  const behavior = useSpecialistBehavior();
  const addSlot = useSpecialistAddSlot();
  const setDefault = useSpecialistDefault();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<{ key: string; opts: Record<string, string> } | null>(null);
  const [adding, setAdding] = useState(false); // the Add-specialist dialog is open

  const busy = load.isPending || unload.isPending || behavior.isPending || addSlot.isPending || setDefault.isPending;
  const data = q.data;

  function fail(e: unknown, retry: () => void) {
    if (!gate.handle(e, retry)) setError(apiErrorMessage(e));
  }
  function changeBehavior(o: SpecialistOccupant, b: SpecialistBehavior) {
    setError(null);
    setNotice(null);
    const run = () =>
      behavior.mutate({ name: o.name, behavior: b }, {
        onError: (e) => fail(e, run),
        onSuccess: (res) => setNotice({ key: res.state === "stopping" ? "notice.stopping" : `notice.${b}`, opts: { name: o.label || o.name } }),
      });
    run();
  }
  function doLoad(occupant: string, slot?: string) {
    setError(null);
    setNotice(null);
    load.mutate({ occupant, slot }, { onError: (e) => setError(apiErrorMessage(e)), });
  }
  function doUnload(slot: string) {
    setError(null);
    setNotice(null);
    unload.mutate(slot, {
      onError: (e) => setError(apiErrorMessage(e)),
      onSuccess: (res) => { if ((res as { state?: string }).state === "stopping") setNotice({ key: "notice.stopping_slot", opts: { slot } }); },
    });
  }
  function doAddSlot() {
    setError(null);
    setNotice(null);
    const run = () =>
      addSlot.mutate(undefined, {
        onError: (e) => fail(e, run),
        onSuccess: (r) => setNotice({ key: "notice.slot_added", opts: { slot: r.label } }),
      });
    run();
  }
  function changeDefault(fn: SpecialistFunction, occupant: string) {
    setError(null);
    const run = () => setDefault.mutate({ fn, occupant }, { onError: (e) => fail(e, run) });
    run();
  }

  if (q.isLoading) return <section className="page"><div className="empty-note">{t("loading")}</div></section>;
  if (!data) return <section className="page"><div className="empty-note">{t("load_failed")}</div></section>;

  const byName = new Map(data.occupants.map((o) => [o.name, o]));
  const inSlots = new Set(data.slots.filter((s) => s.state !== "empty").map((s) => s.occupant));
  const usedSlots = data.slots.filter((s) => s.state !== "empty");
  const freeSlots = data.slots.length - usedSlots.length;
  // One "On standby" list: on-demand engines that are not loaded right now, plus disabled ones.
  const standby = data.occupants
    .filter((o) => !o.enabled || (o.policy === "on_demand" && !inSlots.has(o.name)))
    .sort((x, y) => Number(!x.enabled) - Number(!y.enabled) || (x.label || x.name).localeCompare(y.label || y.name));

  return (
    <section className="page" data-tour-id="specialists">
      <div className="spec-bar">
        <div className="eyebrow">{t("title")}</div>
        <span className="spec-note spec-bar-note">
          {freeSlots > 0 && t("free_slots", { count: freeSlots })}
        </span>
        {canAdmin && (
          <>
            <button className="btn-mini" disabled={busy || data.slots.length >= MAX_SLOTS} onClick={doAddSlot}>
              {data.slots.length >= MAX_SLOTS ? t("capacity.at_limit") : t("capacity.add_slot")}
            </button>
            <button className="btn-mini primary" disabled={busy} onClick={() => setAdding(true)}>{t("add_specialist")}</button>
          </>
        )}
      </div>
      {error && <div className="spec-err" role="alert">{error}</div>}
      {notice && <div className="spec-ok" role="status">{t(notice.key, notice.opts)}</div>}

      {data.slots.length === 0 ? (
        <div className="card"><div className="empty-note">{t("no_slots")}</div></div>
      ) : (
        <>
          {usedSlots.length > 0 && <div className="spec-note spec-hint">{t("click_hint")}</div>}
          <div className="spec-grid">
            {usedSlots.map((s) => {
              const o = byName.get(s.occupant);
              return (
                <SpecialistCard
                  key={s.slot}
                  slot={s}
                  canAdmin={canAdmin}
                  busy={busy}
                  onBehavior={(b) => o && changeBehavior(o, b)}
                  onRetry={() => doLoad(s.occupant, s.slot)}
                  onUnload={() => doUnload(s.slot)}
                  onAdd={() => setAdding(true)}
                />
              );
            })}
          </div>

          {standby.length > 0 && (
            <>
              <div className="eyebrow eyebrow-wrap">{t("standby.title")} <span className="spec-note">{t("standby.note")}</span></div>
              <div className="spec-standby">
                {standby.map((o) => (
                  <div
                    className="spec-srow"
                    key={o.name}
                    data-waiting={o.enabled ? o.name : undefined}
                    data-disabled={o.enabled ? undefined : o.name}
                    title={o.enabled ? undefined : o.kind === "catalog-config" ? t("standby.disabled_catalog") : t("standby.disabled_service")}
                  >
                    <div className="spec-dname">
                      <b>{o.label || o.name}</b>
                      <div className="spec-note">
                        {o.enabled ? t("standby.state_waiting") : t("standby.state_disabled")}
                        {o.functions.length > 0 && ` · ${o.functions.map((f) => t(`function.${f}`)).join(", ")}`}
                      </div>
                    </div>
                    {canAdmin && <BehaviorControl value={o.enabled ? "on_demand" : "disabled"} disabled={busy} onPick={(b) => changeBehavior(o, b)} label={t("behavior.label")} />}
                  </div>
                ))}
              </div>
            </>
          )}

          <details className="spec-explainer">
            <summary>{t("explainer_toggle")}</summary>
            <div className="spec-callout">{t("behavior_explainer")}</div>
          </details>
        </>
      )}

      {adding && (
        <AddSpecialistDialog
          data={data}
          onClose={() => setAdding(false)}
          onDone={(name, slot) => {
            setAdding(false);
            setNotice({ key: slot ? "notice.added_always_on" : "notice.added_on_demand", opts: { name, slot } });
          }}
        />
      )}

      <div className="eyebrow">{t("defaults.title")}</div>
      <div className="card">
        <p className="spec-note">{t("defaults.explainer")}</p>
        <div className="spec-defaults">
          {data.functions.map((fn) => {
            const options = data.occupants.filter((o) => o.enabled && o.functions.includes(fn));
            return (
              <label className="form-row" key={fn}>
                {t(`function.${fn}`)}
                <select
                  value={data.defaults[fn] ?? ""}
                  disabled={!canAdmin || busy}
                  onChange={(e) => changeDefault(fn, e.target.value)}
                >
                  <option value="">{t("defaults.none")}</option>
                  {options.map((o) => <option key={o.name} value={o.name}>{o.label || o.name}</option>)}
                </select>
              </label>
            );
          })}
        </div>
      </div>
      <StepUpModal open={gate.open} requiredFactor={gate.factor} onSuccess={gate.onSuccess} onClose={gate.onClose} />
    </section>
  );
}
