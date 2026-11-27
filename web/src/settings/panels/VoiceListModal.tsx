import { useTranslation } from "react-i18next";
import { Modal } from "../../components/Modal";
import { useVoiceList } from "../../lib/queries";
import { SPEECH_KINDS } from "../../lib/speechModel";
import type { VoiceListEntry } from "../../lib/types";

// VoiceListModal — every voice forge-tts serves, grouped by the kind of voice (Kokoro, Custom, Designed, Cloned) with the engine each one is bound to
// (GET /api/v1/voice/list: `kind` and `engine`). A voice that names no engine shows the word "default": the default engine for its kind makes it.
// On the shared Modal shell (ADR-0023 contract b) — the .modal.wide chrome it always used, now with dialog semantics.
function groupByKind(voices: VoiceListEntry[]): Map<string, VoiceListEntry[]> {
  const groups = new Map<string, VoiceListEntry[]>();
  for (const v of voices) {
    const list = groups.get(v.kind) ?? [];
    list.push(v);
    groups.set(v.kind, list);
  }
  return groups;
}

export function VoiceListModal({ onClose }: { onClose: () => void }) {
  const { t } = useTranslation("settings");
  const kindOrder = SPEECH_KINDS.map((key) => ({ key: key as string, label: t(`voice.kind_${key}`) }));
  const list = useVoiceList(true);
  const groups = list.data ? groupByKind(list.data.voices) : null;
  const known = new Set<string>(SPEECH_KINDS);
  const otherKeys = groups ? [...groups.keys()].filter((k) => !known.has(k)) : [];

  return (
    <Modal title={t("voice_list_modal.title")} onClose={onClose} wide>
      {list.isLoading && <div className="empty-note">{t("voice_list_modal.loading")}</div>}
      {list.isError && <div className="error-note">{t("voice_list_modal.error")}</div>}
      {groups && (
        <>
          {[...kindOrder, ...otherKeys.map((k) => ({ key: k, label: k }))].map(({ key, label }) => {
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
                      <div key={v.id} className="form-row" style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
                        <span style={{ display: "flex", alignItems: "center", gap: 8, minWidth: 0 }}>
                          {v.name}
                          <span style={{ color: "var(--text-mute)", fontFamily: "var(--mono)", fontSize: 11 }}>{v.id}</span>
                        </span>
                        <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
                          {v.builtin && <span className="chip apply-live">{t("voice_list_modal.builtin")}</span>}
                          <span style={{ color: "var(--text-mute)", fontFamily: "var(--mono)", fontSize: 11 }}>
                            {v.engine || t("voice_list_modal.engine_default")}
                          </span>
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
    </Modal>
  );
}
