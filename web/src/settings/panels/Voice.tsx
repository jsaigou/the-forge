// settings/panels/Voice.tsx — Voice & Speech (docs/speech-routing-plan.md, P4). Five cards:
//
// - "What works": readiness per capability, with Register for engines whose files are here (SpeechSetup.tsx).
// - "Speech services": start/stop for STT / Embedding / Aligner (POST /api/v1/{name}/{start,stop}); unchanged.
// - "Speech engines": one row per speech engine in the pool, with the kind of voice it makes, its Behavior (Always on / On demand / Disabled, the same
//   setting as on the Engines tab; it applies at once and nothing is saved from here), its state and how many voices use it.
// - "Default engine for each kind of voice": the only speech setting that is saved (tts.defaults, PUT /api/v1/speech/defaults, admin + step-up).
// - "Voices": which engine makes each voice and whether it can be played. Read-only: a voice's engine is chosen in Voice Studio when the voice is made
//   and stays with it (decision 2 of the plan), so nothing here moves a voice to another engine.
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { BehaviorControl } from "../../components/SpecialistCard";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import { apiErrorMessage } from "../../lib/api";
import {
  useInfraServices,
  useSpeechDefaults,
  useSpecialistBehavior,
  useSpecialists,
  useStartInfraService,
  useStopInfraService,
  useUpdateSpeechDefaults,
  useVoiceList,
} from "../../lib/queries";
import {
  ENGINE_ROW_ORDER,
  SPEECH_KINDS,
  countVoicesByEngine,
  engineBehavior,
  enginesOfKind,
  normalizeDefaults,
  unplayableVoices,
  voiceUse,
} from "../../lib/speechModel";
import type { SpeechEngineInfo, SpeechKind, SpecialistBehavior, SpecialistSlot, VoiceListEntry } from "../../lib/types";
import { useSettingsGroup } from "../useSettingsGroup";
import { SpeechRecipesDialog, SpeechSetupCard } from "./SpeechSetup";
import { VoiceListModal } from "./VoiceListModal";

// matchLabel is the real backend service name to look up against
// GET /api/v1/infra-services — never translated. Display label is resolved
// separately, inside the component, via t().
const FIXED_SERVICES: { name: "stt" | "embedding" | "aligner"; matchLabel: string }[] = [
  { name: "stt", matchLabel: "STT" },
  { name: "embedding", matchLabel: "Embedding" },
  { name: "aligner", matchLabel: "Aligner" },
];

const VOICES_SHOWN = 12;

function SpeechServicesCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const infra = useInfraServices();
  const start = useStartInfraService();
  const stop = useStopInfraService();
  const busy = start.isPending || stop.isPending;

  const rows = FIXED_SERVICES.map(({ name, matchLabel }) => {
    const row = infra.data?.services.find((s) => s.name.toLowerCase() === matchLabel.toLowerCase());
    return { name, label: t(`voice.service_${name}`), active: row?.active ?? false, present: !!row };
  });

  return (
    <>
      <div className="eyebrow">{t("voice.speech_services_title")}</div>
      <div className="card">
        {infra.isLoading && <div className="empty-note">{t("voice.loading_status")}</div>}
        <div style={{ display: "flex", alignItems: "center", gap: 20, flexWrap: "wrap" }}>
          {rows.map((r) => (
            <span key={r.name} style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <span style={{ fontWeight: 600 }}>{r.label}</span>
              <span className={`chip ${r.active ? "apply-live" : "apply-restart"}`}>
                {r.present ? (r.active ? t("voice.status_active") : t("voice.status_stopped")) : t("voice.status_not_configured")}
              </span>
              {canAdmin && r.present && (
                <button
                  className="btn"
                  disabled={busy}
                  onClick={() => (r.active ? stop.mutate(r.name) : start.mutate(r.name))}
                >
                  {r.active ? t("voice.stop") : t("voice.start")}
                </button>
              )}
            </span>
          ))}
        </div>
      </div>
    </>
  );
}

// The state column: Off for a disabled engine, otherwise the slot state, with how long an On demand engine has been idle.
function EngineState({ e, slot }: { e: SpeechEngineInfo; slot: SpecialistSlot | undefined }) {
  const { t } = useTranslation("settings");
  if (!e.enabled) return <span className="chip">{t("voice.state_off")}</span>;
  const state = slot?.state ?? e.state;
  if (state === "running") {
    const mins = e.policy === "on_demand" && slot ? Math.floor(slot.idle_seconds / 60) : 0;
    return (
      <>
        <span className="chip st-ok">{t("voice.state_running")}</span>
        {mins >= 1 && <span className="spec-note"> {t("voice.state_idle_for", { count: mins })}</span>}
      </>
    );
  }
  if (state === "starting") return <span className="chip apply-restart">{t("voice.state_starting")}</span>;
  if (state === "failed") return <span className="chip st-bad">{t("voice.state_failed")}</span>;
  if (state === "stopping") return <span className="chip apply-restart">{t("voice.state_stopping")}</span>;
  return <span className="chip">{t("voice.state_not_loaded")}</span>;
}

function EnginesCard({ canAdmin, onAdd }: { canAdmin: boolean; onAdd: () => void }) {
  const { t } = useTranslation("settings");
  const speech = useSpeechDefaults();
  const pool = useSpecialists();
  const voices = useVoiceList(true);
  const behave = useSpecialistBehavior();
  const [error, setError] = useState<string | null>(null);

  const info = speech.data?.engine_info;
  const defaults = useMemo(() => normalizeDefaults(speech.data?.defaults), [speech.data]);
  const counts = useMemo(
    () => (info && voices.data ? countVoicesByEngine(voices.data.voices, defaults, info) : null),
    [info, voices.data, defaults],
  );
  const kokoroVoices = voices.data?.voices.filter((v) => v.kind === "kokoro").length ?? 0;
  const rows = useMemo(() => ENGINE_ROW_ORDER.flatMap((k) => enginesOfKind(info ?? [], k)), [info]);

  function pick(name: string, b: SpecialistBehavior) {
    setError(null);
    behave.mutate({ name, behavior: b }, { onError: (e) => setError(apiErrorMessage(e)) });
  }

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>{t("voice.engines_title")}</span>
        {canAdmin && (
          <button type="button" className="tab" style={{ marginLeft: "auto" }} onClick={onAdd}>
            {t("voice.add_engine")}
          </button>
        )}
      </div>
      <div className="card">
        {speech.isError && <div className="empty-note">{t("shared.role_required", { resource: t("voice.resource_speech_engines") })}</div>}
        {speech.isLoading && <div className="empty-note">{t("shared.loading", { resource: t("voice.resource_speech_engines") })}</div>}
        {error && <div className="spec-err" role="alert" style={{ marginBottom: 10 }}>{error}</div>}
        {info && rows.length === 0 && <div className="empty-note">{t("voice.no_engines")}</div>}
        {rows.length > 0 && (
          <div className="speech-table-wrap">
            <table className="speech-table">
              <thead>
                <tr>
                  <th scope="col">{t("voice.col_engine")}</th>
                  <th scope="col">{t("voice.col_makes")}</th>
                  <th scope="col">{t("voice.col_behavior")}</th>
                  <th scope="col">{t("voice.col_state")}</th>
                  <th className="num" scope="col">{t("voice.col_voices")}</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((e) => {
                  const slot = pool.data?.slots.find((s) => s.occupant === e.name);
                  const n = counts?.get(e.name) ?? 0;
                  return (
                    <tr key={e.name}>
                      <td className="name">
                        {e.label || e.name}
                        <small>{e.name}</small>
                      </td>
                      <td>{t(`voice.kind_${e.mode}`)}</td>
                      <td>
                        <BehaviorControl
                          value={engineBehavior(e)}
                          disabled={!canAdmin || behave.isPending}
                          onPick={(b) => pick(e.name, b)}
                          label={`${e.label || e.name}: ${t("voice.col_behavior")}`}
                        />
                      </td>
                      <td><EngineState e={e} slot={slot} /></td>
                      <td className="num">
                        {!counts ? "…" : e.mode === "kokoro" && n > 0 ? t("voice.voices_builtin", { count: kokoroVoices || n }) : n}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
        <div className="spec-note" style={{ marginTop: 10 }}>{t("voice.engines_note")}</div>
      </div>
    </>
  );
}

function DefaultsCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const speech = useSpeechDefaults();
  const update = useUpdateSpeechDefaults();
  const data = useMemo(() => (speech.data ? normalizeDefaults(speech.data.defaults) : undefined), [speech.data]);
  const g = useSettingsGroup(data, update);

  if (!g.active || !speech.data) {
    return (
      <>
        <div className="eyebrow">{t("voice.defaults_title")}</div>
        <div className="card">
          <div className="empty-note">
            {speech.isError ? t("shared.role_required", { resource: t("voice.resource_speech_engines") }) : t("shared.loading", { resource: t("voice.resource_speech_engines") })}
          </div>
        </div>
      </>
    );
  }
  const info = speech.data.engine_info;

  return (
    <>
      <div className="eyebrow">{t("voice.defaults_title")}</div>
      <div className="card">
        <div className="spec-note">{t("voice.defaults_note")}</div>
        {g.error && <div className="error-note" style={{ marginTop: 8 }}>{g.error}</div>}
        <div className="speech-defaults">
          {SPEECH_KINDS.map((k: SpeechKind) => {
            const options = enginesOfKind(info, k);
            const current = g.active![k];
            const known = options.some((o) => o.name === current);
            return (
              <label key={k}>
                <span>{t(`voice.kind_${k}`)}</span>
                <select
                  value={current}
                  disabled={!canAdmin}
                  aria-label={t("voice.default_for", { kind: t(`voice.kind_${k}`) })}
                  onChange={(ev) => g.setField(k, ev.target.value)}
                >
                  <option value="">{t("voice.default_none")}</option>
                  {current && !known && <option value={current}>{t("voice.default_unknown", { name: current })}</option>}
                  {options.map((o) => (
                    <option key={o.name} value={o.name}>
                      {o.enabled ? (o.label || o.name) : t("voice.default_off", { name: o.label || o.name })}
                    </option>
                  ))}
                </select>
              </label>
            );
          })}
        </div>
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} label={t("voice.save_defaults")} />
          </div>
        )}
        {!canAdmin && <div className="spec-note" style={{ marginTop: 10 }}>{t("voice.defaults_admin_only")}</div>}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
    </>
  );
}

function VoiceRow({ v, defaults, info }: { v: VoiceListEntry; defaults: ReturnType<typeof normalizeDefaults>; info: SpeechEngineInfo[] }) {
  const { t } = useTranslation("settings");
  const use = voiceUse(v, defaults, info);
  const label = info.find((e) => e.name === use.engine)?.label;
  const engine = !use.engine
    ? <span className="spec-note">{t("voice.engine_none")}</span>
    : use.viaDefault
      ? <span className="spec-note">{t("voice.engine_default", { name: label || use.engine })}</span>
      : use.engine;
  return (
    <tr>
      <td className="mono">{v.id}</td>
      <td>{t(`voice.kind_single_${v.kind}`, { defaultValue: v.kind })}</td>
      <td>{engine}</td>
      <td>
        <span className={`chip ${use.status === "ready" ? "st-ok" : "st-bad"}`}>{t(`voice.status_${use.status}`)}</span>
      </td>
    </tr>
  );
}

function VoicesCard() {
  const { t } = useTranslation("settings");
  const speech = useSpeechDefaults();
  const list = useVoiceList(true);
  const [showList, setShowList] = useState(false);
  const info = speech.data?.engine_info;
  const defaults = useMemo(() => normalizeDefaults(speech.data?.defaults), [speech.data]);

  const { shown, kokoro, registry } = useMemo(() => {
    const all = list.data?.voices ?? [];
    const reg = all.filter((v) => v.kind !== "kokoro");
    const bad = info ? unplayableVoices(all, defaults, info).filter((v) => v.kind !== "kokoro") : [];
    const rest = reg.filter((v) => !bad.includes(v)).sort((a, b) => a.id.localeCompare(b.id));
    return { shown: [...bad, ...rest].slice(0, VOICES_SHOWN), kokoro: all.length - reg.length, registry: reg.length };
  }, [list.data, info, defaults]);

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>{t("voice.voices_title")}</span>
        <button type="button" className="tab" style={{ marginLeft: "auto" }} onClick={() => setShowList(true)}>
          {t("voice.list_all_voices")}
        </button>
      </div>
      <div className="card">
        {list.isLoading && <div className="empty-note">{t("voice_list_modal.loading")}</div>}
        {list.isError && <div className="error-note">{t("voice_list_modal.error")}</div>}
        {list.data && info && (
          <>
            {shown.length === 0 ? (
              <div className="empty-note">{t("voice.voices_empty")}</div>
            ) : (
              <div className="speech-table-wrap">
                <table className="speech-table voices">
                  <thead>
                    <tr>
                      <th scope="col">{t("voice.col_voice")}</th>
                      <th scope="col">{t("voice.col_kind")}</th>
                      <th scope="col">{t("voice.col_voice_engine")}</th>
                      <th scope="col">{t("voice.col_status")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {shown.map((v) => <VoiceRow key={v.id} v={v} defaults={defaults} info={info} />)}
                  </tbody>
                </table>
              </div>
            )}
            <div className="spec-note" style={{ marginTop: 10 }}>
              {registry > shown.length ? t("voice.voices_showing", { shown: shown.length, total: registry }) + " " : ""}
              {kokoro > 0 ? t("voice.voices_kokoro_note", { count: kokoro }) + " " : ""}
              {t("voice.voices_note")}
            </div>
          </>
        )}
      </div>
      {showList && <VoiceListModal onClose={() => setShowList(false)} />}
    </>
  );
}

export function Voice({ canAdmin }: { canAdmin: boolean }) {
  const [adding, setAdding] = useState(false);
  return (
    <>
      <SpeechSetupCard canAdmin={canAdmin} onOpenRecipes={() => setAdding(true)} />
      <SpeechServicesCard canAdmin={canAdmin} />
      <EnginesCard canAdmin={canAdmin} onAdd={() => setAdding(true)} />
      <DefaultsCard canAdmin={canAdmin} />
      <VoicesCard />
      {adding && <SpeechRecipesDialog canAdmin={canAdmin} onClose={() => setAdding(false)} />}
    </>
  );
}
