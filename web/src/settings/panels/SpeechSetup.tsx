// settings/panels/SpeechSetup.tsx — what this server can do with speech and how to add what is missing (docs/speech-setup-plan.md, step 2).
//
// - SpeechSetupCard: one tile per capability (preset voices, cloning, voice design) from GET /api/v1/speech/readiness, with the one next action.
//   An engine whose files are already here gets a Register button (admin and step-up).
// - SpeechRecipesDialog ("+ Add engine"): every engine the server knows how to set up, what it has of each, and Register for the ones present.
//
// Registering uses files already on the server. Nothing here downloads or builds an engine: for the engines the Forge can build, the dialog shows the
// command to run on the server.
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { StepUpModal } from "../../components/StepUpModal";
import { apiErrorMessage } from "../../lib/api";
import { formatGB } from "../../lib/format";
import { useExternalTools, useRegisterSpeechRecipe, useSpeechReadiness, useSpeechRecipes } from "../../lib/queries";
import { voiceStudioLink } from "../../lib/speechModel";
import { useStepUpGate } from "../../lib/useStepUpGate";
import type { SpeechCapability, SpeechRecipe, SpeechRegisterResult } from "../../lib/types";

const PROBLEMS_SHOWN = 3;

/** Registering an engine, with the step-up prompt and the sentence that says what happened. */
function useRegistration() {
  const { t } = useTranslation("settings");
  const mutation = useRegisterSpeechRecipe();
  const gate = useStepUpGate();
  const [error, setError] = useState<string | null>(null);
  // The result is kept, not its sentences, so a change of language re-words it.
  const [done, setDone] = useState<SpeechRegisterResult | null>(null);

  function describe(r: SpeechRegisterResult): string[] {
    const lines = [t(r.created ? "voice.setup.registered" : "voice.setup.registered_already", { engine: r.engine })];
    lines.push(t(r.policy === "always_on" ? "voice.setup.kept_always_on" : "voice.setup.kept_on_demand"));
    if (r.slot_added) lines.push(t("voice.setup.slot_added", { slot: r.slot_added.toUpperCase() }));
    if (r.default_set) lines.push(t("voice.setup.default_set"));
    if (!r.enabled) lines.push(t("voice.setup.engine_off"));
    if (r.restart_required) lines.push(t("voice.setup.restart_required"));
    return lines;
  }

  function register(id: string) {
    setError(null);
    setDone(null);
    const attempt = () =>
      mutation.mutate(id, {
        onSuccess: setDone,
        onError: (e) => {
          if (!gate.handle(e, attempt)) setError(apiErrorMessage(e));
        },
      });
    attempt();
  }

  return { register, pending: mutation.isPending ? mutation.variables : undefined, error, done: done && describe(done), gate };
}

type Registration = ReturnType<typeof useRegistration>;

function RegistrationOutcome({ reg }: { reg: Registration }) {
  return (
    <>
      {reg.error && <div className="spec-err" role="alert">{reg.error}</div>}
      <div role="status">{reg.done && <div className="speech-done">{reg.done.join(" ")}</div>}</div>
    </>
  );
}

function RegistrationStepUp({ reg }: { reg: Registration }) {
  return <StepUpModal open={reg.gate.open} requiredFactor={reg.gate.factor} onSuccess={reg.gate.onSuccess} onClose={reg.gate.onClose} />;
}

function chipClass(c: SpeechCapability): string {
  if (c.state === "ready") return "chip st-ok";
  if (c.usable || c.state === "not_registered" || c.state === "starting") return "chip apply-restart";
  return "chip st-bad";
}

function CapabilityTile({ c, recipe, canAdmin, reg, onDetails }: {
  c: SpeechCapability;
  recipe: SpeechRecipe | undefined;
  canAdmin: boolean;
  reg: Registration;
  onDetails: () => void;
}) {
  const { t } = useTranslation("settings");
  const name = recipe?.label ?? c.recipe ?? "";
  return (
    <li className="speech-cap">
      <div className="speech-cap-head">
        <span className="speech-cap-name">{t(`voice.setup.cap_${c.id}`)}</span>
        <span className={chipClass(c)}>{t(`voice.setup.state_${c.state}`)}</span>
      </div>
      <p className="spec-note">{t(`voice.setup.what_${c.state}`, { engine: c.engine ?? "", name })}</p>
      {c.detail && <p className="speech-cap-detail">{c.detail}</p>}
      {c.state === "not_registered" && c.recipe && (
        canAdmin ? (
          <button type="button" className="btn-mini primary" disabled={reg.pending !== undefined} onClick={() => reg.register(c.recipe!)}>
            {reg.pending === c.recipe ? t("voice.setup.registering") : t("voice.setup.register_named", { name })}
          </button>
        ) : (
          <p className="spec-note">{t("voice.setup.admin_only")}</p>
        )
      )}
      {c.state === "no_slot" && canAdmin && recipe?.state === "registered" && (
        <button type="button" className="btn-mini primary" disabled={reg.pending !== undefined} onClick={() => reg.register(recipe.id)}>
          {t("voice.setup.add_slot")}
        </button>
      )}
      {c.state === "not_installed" && (
        <button type="button" className="btn-mini" onClick={onDetails}>{t("voice.setup.show_missing")}</button>
      )}
    </li>
  );
}

export function SpeechSetupCard({ canAdmin, onOpenRecipes }: { canAdmin: boolean; onOpenRecipes: () => void }) {
  const { t } = useTranslation("settings");
  const readiness = useSpeechReadiness();
  const recipes = useSpeechRecipes();
  const tools = useExternalTools();
  const reg = useRegistration();
  const r = readiness.data;
  const studio = voiceStudioLink(tools.data?.tools);

  // The recipe a tile talks about: the one the report names, or the one the engine it names is registered from.
  const recipeFor = (c: SpeechCapability) =>
    recipes.data?.recipes.find((x) => (c.recipe ? x.id === c.recipe : !!c.engine && x.engine === c.engine));

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>{t("voice.setup.title")}</span>
        {studio && (
          <a className="tab" style={{ marginLeft: "auto" }} href={studio.url} target="_blank" rel="noopener noreferrer">
            {t("voice.setup.open_studio", { name: studio.name })}
          </a>
        )}
      </div>
      <div className="card" data-tour-id="speech-setup">
        {readiness.isLoading && <div className="empty-note">{t("shared.loading", { resource: t("voice.resource_speech_engines") })}</div>}
        {readiness.isError && <div className="error-note">{apiErrorMessage(readiness.error)}</div>}
        {r?.front_door && r.front_door.state !== "ok" && (
          <div className="spec-warn" style={{ marginBottom: 10 }}>{t(`voice.setup.front_door_${r.front_door.state}`)}</div>
        )}
        {r && (
          <ul className="speech-caps">
            {r.capabilities.map((c) => (
              <CapabilityTile key={c.id} c={c} recipe={recipeFor(c)} canAdmin={canAdmin} reg={reg} onDetails={onOpenRecipes} />
            ))}
          </ul>
        )}
        <RegistrationOutcome reg={reg} />
      </div>
      <RegistrationStepUp reg={reg} />
    </>
  );
}

function RecipeItem({ r, canAdmin, reg }: { r: SpeechRecipe; canAdmin: boolean; reg: Registration }) {
  const { t } = useTranslation("settings");
  const problems = r.problems ?? [];
  return (
    <li className="speech-recipe">
      <div className="speech-cap-head">
        <span className="speech-cap-name">{r.label}</span>
        <span className={r.state === "registered" ? "chip st-ok" : r.state === "present" ? "chip apply-restart" : "chip"}>
          {t(`voice.setup.recipe_${r.state}`)}
        </span>
      </div>
      <p className="spec-note">
        {t(`voice.kind_${r.mode}`)} · {t("voice.setup.sizes", { download: formatGB(r.download_bytes, 1), memory: formatGB(r.memory_bytes, 1) })} · {r.license}
      </p>
      {r.state === "registered" && <p className="spec-note">{t("voice.setup.registered_as", { engine: r.engine })}</p>}
      {r.state === "absent" && problems.length > 0 && (
        <details>
          <summary>{t("voice.setup.missing_files", { count: problems.length })}</summary>
          <ul className="speech-problems">
            {problems.slice(0, PROBLEMS_SHOWN).map((p) => (
              <li key={p.path}><span className="mono">{p.path}</span>: {t(`voice.setup.problem_${p.problem}`)}</li>
            ))}
            {problems.length > PROBLEMS_SHOWN && <li>{t("voice.setup.more_files", { count: problems.length - PROBLEMS_SHOWN })}</li>}
          </ul>
        </details>
      )}
      {r.state === "absent" && (r.install_command ? (
        <div className="speech-install">
          <span className="spec-note">{t("voice.setup.install_with")}</span>
          <code>{r.install_command}</code>
        </div>
      ) : (
        <p className="spec-note">{t("voice.setup.cannot_install")}</p>
      ))}
      {r.state === "present" && canAdmin && (
        <button type="button" className="btn-mini primary" disabled={reg.pending !== undefined} onClick={() => reg.register(r.id)}>
          {reg.pending === r.id ? t("voice.setup.registering") : t("voice.setup.register_named", { name: r.label })}
        </button>
      )}
    </li>
  );
}

export function SpeechRecipesDialog({ canAdmin, onClose }: { canAdmin: boolean; onClose: () => void }) {
  const { t } = useTranslation("settings");
  const recipes = useSpeechRecipes();
  const reg = useRegistration();
  const boxRef = useRef<HTMLDivElement>(null);

  // Focus moves into the dialog when it opens and goes back to what opened it when it closes.
  useEffect(() => {
    const opener = document.activeElement as HTMLElement | null;
    boxRef.current?.focus();
    return () => opener?.focus?.();
  }, []);
  // Escape closes, unless the step-up prompt is on top (it handles its own).
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape" && !reg.gate.open) {
        e.stopPropagation();
        onClose();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose, reg.gate.open]);

  return (
    <>
    <div className="modal-backdrop" onClick={(e) => { e.stopPropagation(); onClose(); }}>
      <div ref={boxRef} tabIndex={-1} className="modal wide spec-wiz" role="dialog" aria-modal="true" aria-label={t("voice.setup.dialog_title")} onClick={(e) => e.stopPropagation()}>
        <h3>{t("voice.setup.dialog_title")}</h3>
        <p className="spec-note">{t("voice.setup.dialog_note", { root: recipes.data?.root ?? "…" })}</p>
        {recipes.isLoading && <div className="empty-note">{t("shared.loading", { resource: t("voice.resource_speech_engines") })}</div>}
        {recipes.isError && <div className="error-note">{apiErrorMessage(recipes.error)}</div>}
        <ul className="speech-recipes">
          {recipes.data?.recipes.map((r) => <RecipeItem key={r.id} r={r} canAdmin={canAdmin} reg={reg} />)}
        </ul>
        {!canAdmin && <p className="spec-note">{t("voice.setup.admin_only")}</p>}
        <RegistrationOutcome reg={reg} />
        <div className="spec-actions spec-wiz-foot">
          <span className="spec-grow" />
          <button type="button" className="btn-mini" onClick={onClose}>{t("voice.setup.close")}</button>
        </div>
      </div>
    </div>
    <RegistrationStepUp reg={reg} />
    </>
  );
}
