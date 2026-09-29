import { useTranslation } from "react-i18next";
import { useVoiceList } from "../../lib/queries";
import type { VoiceListEntry } from "../../lib/types";

// VoiceListModal — Sprint 1 UI papercuts (2026-08-27). Operator request: "a
// button that lists all the available voices by voice engine." Groups
// forge-tts's flat voice registry (GET /api/v1/voice/list) by the `engine`
// field the backend computes from each voice's Type, in the same order the
// Voice engines card above lists them.
const ENGINE_ORDER_KEYS = ["kokoro", "customvoice", "voicedesign", "base"] as const;

function groupByEngine(voices: VoiceListEntry[]): Map<string, VoiceListEntry[]> {
  const groups = new Map<string, VoiceListEntry[]>();
  for (const v of voices) {
    const list = groups.get(v.engine) ?? [];
    list.push(v);
    groups.set(v.engine, list);
  }
  return groups;
}

export function VoiceListModal({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation("settings");
  const engineOrder = ENGINE_ORDER_KEYS.map((key) => ({ key, label: t(`voice.engine_${key}`) }));
  const list = useVoiceList(true);
  const groups = list.data ? groupByEngine(list.data.voices) : null;
  const knownKeys = new Set(ENGINE_ORDER_KEYS as readonly string[]);
  const otherKeys = groups ? [...groups.keys()].filter((k) => !knownKeys.has(k)) : [];

  return (
    <div className="modal-backdrop" onClick={(e) => { e.stopPropagation(); onClose(); }}>
      <div className="modal wide" onClick={(e) => e.stopPropagation()}>
        <h3>{t("voice_list_modal.title")}</h3>
        {list.isLoading && <div className="empty-note">{t("voice_list_modal.loading")}</div>}
        {list.isError && <div className="error-note">{t("voice_list_modal.error")}</div>}
        {groups && (
          <>
            {[...engineOrder, ...otherKeys.map((k) => ({ key: k, label: k }))].map(({ key, label }) => {
              const voices = groups.get(key) ?? [];
              return (
                <div key={key} style={{ marginBottom: 16 }}>
                  <div style={{ fontWeight: 600, marginBottom: 6 }}>
                    {label} <span style={{ color: "var(--text-mute)", fontWeight: 400 }}>({voices.length})</span>
                  </div>
                  {voices.length === 0 ? (
                    <div className="empty-note">{t("voice_list_modal.no_voices")}</div>
                  ) : (
                    <div className="form-grid">
                      {voices.map((v) => (
                        <div key={v.id} className="form-row" style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
                          <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
                            {v.name}
                            <span style={{ color: "var(--text-mute)", fontFamily: "var(--mono)", fontSize: 11 }}>{v.id}</span>
                          </span>
                          <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
                            {v.builtin && <span className="chip apply-live">{t("voice_list_modal.builtin")}</span>}
                            <span style={{ color: "var(--text-mute)" }}>{v.language}</span>
                          </span>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
          </>
        )}
        <div className="form-actions">
          <button className="btn" onClick={onClose}>{t("voice_list_modal.close")}</button>
        </div>
      </div>
    </div>
  );
}
