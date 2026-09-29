// settings/panels/Voice.tsx — Tier 1 Sprint 2, Voice & Speech settings
// (2026-08-27). Two cards:
//
// - "Speech services" — simple start/stop for STT/Embedding/Aligner, which
//   had no start/stop route at all before this sprint (only TTS did). No
//   settings key of their own; just POST /api/v1/{name}/{start,stop} against
//   the live infra-services snapshot.
// - "Voice engines" — the tts.engines settings key (four sub-sections,
//   kokoro + the three Qwen sub-variants). Unlike Smith.tsx's convention of
//   one useSettingsGroup PER independent settings-key group, this is a
//   SINGLE useSettingsGroup for the whole card: tts.engines is one object at
//   the wire level (PUT always replaces all four engines together), so
//   there is no independent-group leak risk to guard against — splitting it
//   into four instances would just make an atomic save look like four.
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { InfoTip } from "../../components/InfoTip";
import { SaveButton } from "../../components/SaveButton";
import { StepUpModal } from "../../components/StepUpModal";
import {
  useInfraServices,
  useStartInfraService,
  useStopInfraService,
  useUpdateVoiceSettings,
  useVoiceSettings,
} from "../../lib/queries";
import type { VoiceEngineConfig, VoiceSettings } from "../../lib/types";
import { Field, fieldHelp, fieldOptionLabel } from "../Field";
import { VOICE_FIELDS } from "../fields";
import { useSettingsGroup } from "../useSettingsGroup";
import { VoiceListModal } from "./VoiceListModal";

const F = Object.fromEntries(VOICE_FIELDS.map((f) => [f.id, f]));

// matchLabel is the real backend service name to look up against
// GET /api/v1/infra-services — never translated. Display label is resolved
// separately, inside the component, via t().
const FIXED_SERVICES: { name: "stt" | "embedding" | "aligner"; matchLabel: string }[] = [
  { name: "stt", matchLabel: "STT" },
  { name: "embedding", matchLabel: "Embedding" },
  { name: "aligner", matchLabel: "Aligner" },
];

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

// One compact row per engine — toggle + name + mode inline, unit/url (rarely
// touched, mostly informational) tucked behind a per-row details disclosure.
// Replaces the old stacked 4-field form-grid per engine (16 field-rows total
// for 4 engines), which wasted a lot of vertical space for what's usually
// just "is this on, and in what mode" (operator feedback 2026-08-27).
function EngineRow({
  engineKey,
  label,
  eng,
  disabled,
  onFieldChange,
}: {
  engineKey: keyof VoiceSettings;
  label: string;
  eng: VoiceEngineConfig;
  disabled: boolean;
  onFieldChange: <K extends keyof VoiceEngineConfig>(field: K, value: VoiceEngineConfig[K]) => void;
}) {
  const { t } = useTranslation("settings");
  const [details, setDetails] = useState(false);
  const modeRec = F[`voice.${engineKey}.mode`];

  return (
    <div style={{ padding: "10px 0" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
        <span
          className={`toggle ${disabled ? "disabled" : ""}`}
          onClick={() => !disabled && onFieldChange("enabled", !eng.enabled)}
        >
          <span className={`sw ${eng.enabled ? "on" : ""}`} />
        </span>
        <span style={{ fontWeight: 600, minWidth: 140 }}>{label}</span>
        <select
          value={eng.mode}
          disabled={disabled}
          onChange={(e) => onFieldChange("mode", e.target.value as VoiceEngineConfig["mode"])}
          style={{ width: 110 }}
        >
          {modeRec.options?.map((o) => (
            <option key={o.value} value={o.value}>{fieldOptionLabel(t, modeRec, o)}</option>
          ))}
        </select>
        <InfoTip text={fieldHelp(t, modeRec)} />
        <button type="button" className="tab" style={{ marginLeft: "auto" }} onClick={() => setDetails((v) => !v)}>
          {details ? t("voice.hide_details") : t("voice.show_details")}
        </button>
      </div>
      {details && (
        <div className="form-grid" style={{ marginTop: 10 }}>
          <Field rec={F[`voice.${engineKey}.unit`]} value={eng.unit} disabled={disabled}
            onChange={(v) => onFieldChange("unit", v as string)} />
          <Field rec={F[`voice.${engineKey}.url`]} value={eng.url} disabled={disabled}
            onChange={(v) => onFieldChange("url", v as string)} />
        </div>
      )}
    </div>
  );
}

function VoiceEnginesCard({ canAdmin }: { canAdmin: boolean }) {
  const { t } = useTranslation("settings");
  const cfg = useVoiceSettings();
  const update = useUpdateVoiceSettings();
  const g = useSettingsGroup(cfg.data, update);
  const [showList, setShowList] = useState(false);

  if (cfg.isError) {
    return (
      <>
        <div className="eyebrow">{t("voice.engines_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.role_required", { resource: t("voice.resource_voice_engine") })}</div></div>
      </>
    );
  }
  if (!g.active) {
    return (
      <>
        <div className="eyebrow">{t("voice.engines_title")}</div>
        <div className="card"><div className="empty-note">{t("shared.loading", { resource: t("voice.resource_voice_engine") })}</div></div>
      </>
    );
  }

  const engines: { key: keyof VoiceSettings; label: string }[] = [
    { key: "kokoro", label: t("voice.engine_kokoro") },
    { key: "customvoice", label: t("voice.engine_customvoice") },
    { key: "voicedesign", label: t("voice.engine_voicedesign") },
    { key: "base", label: t("voice.engine_base") },
  ];

  return (
    <>
      <div className="eyebrow" style={{ display: "flex", alignItems: "center", gap: 10 }}>
        <span>{t("voice.engines_title")}</span>
        <button type="button" className="tab" style={{ marginLeft: "auto" }} onClick={() => setShowList(true)}>
          {t("voice.list_all_voices")}
        </button>
      </div>
      <div className="card">
        {g.error && <div className="error-note" style={{ marginBottom: 12 }}>{g.error}</div>}
        {engines.map(({ key, label }, i) => (
          <div key={key} style={i > 0 ? { borderTop: "1px solid var(--border)" } : undefined}>
            <EngineRow engineKey={key} label={label} eng={g.active![key]} disabled={!canAdmin}
              onFieldChange={(field, value) => g.setField(key, { ...g.active![key], [field]: value })} />
          </div>
        ))}
        {canAdmin && g.dirty && (
          <div className="form-actions" style={{ marginTop: 12 }}>
            <button className="btn" onClick={g.reset}>{t("shared.reset")}</button>
            <SaveButton pending={g.pending} isError={g.isError} onClick={g.save} />
          </div>
        )}
      </div>
      <StepUpModal open={g.gate.open} requiredFactor={g.gate.factor} onSuccess={g.gate.onSuccess} onClose={g.gate.onClose} />
      {showList && <VoiceListModal onClose={() => setShowList(false)} />}
    </>
  );
}

export function Voice({ canAdmin }: { canAdmin: boolean }) {
  return (
    <>
      <SpeechServicesCard canAdmin={canAdmin} />
      <VoiceEnginesCard canAdmin={canAdmin} />
    </>
  );
}
