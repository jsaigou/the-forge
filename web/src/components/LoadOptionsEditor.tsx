import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  canonicalFlag,
  LLAMA_FLAGS,
  parseLoadOptions,
  serializeLoadOptions,
  VLLM_FLAGS,
  type FlagRef,
  type LoadOption,
} from "../lib/llamaFlags";
import { translatedFlagLabel, translatedFlagWhy } from "../lib/flagI18n";
import { translatedHazardsFor } from "../lib/hazardLabels";
import { InfoTip } from "./InfoTip";

// LoadOptionsEditor (Sprint C) — tap-to-select flag picker replacing the
// freeform extra-args textarea in ConfigForm/the premium config editor.
// Reuses lib/llamaFlags.ts's curated table verbatim, as that file was
// written to be (docs/v5-prerelease-readiness.md's Sprint B/C entries).
//
// The hard requirement is lossless round-tripping: the source of truth is
// always the flat token array (LoadOption rows carrying their ORIGINAL
// flag spelling — short or long, e.g. laguna's -np vs qwen25coder's
// --parallel), never a regeneration from the curated table. An untouched
// row is serialized back byte-identical (serializeLoadOptions in
// llamaFlags.ts); only rows the operator actually adds/edits/removes
// change. The one deliberate exception is a malformed row (BUG-2,
// docs/v5-prerelease-readiness.md) — those are re-split into two
// well-formed tokens on save rather than preserved broken.
export function LoadOptionsEditor({
  value,
  onChange,
  backend,
  nCtx,
}: {
  value: string[];
  onChange: (next: string[]) => void;
  backend: string;
  nCtx: number;
}) {
  const { t } = useTranslation("common");
  const isVllm = backend === "vllm";
  const flagTable = isVllm ? VLLM_FLAGS : LLAMA_FLAGS;
  const nextId = useRef(0);
  const [rows, setRows] = useState<(LoadOption & { id: number })[]>(() =>
    parseLoadOptions(value, flagTable).map((o) => ({ ...o, id: nextId.current++ })),
  );
  const [rawMode, setRawMode] = useState(false);
  const [rawText, setRawText] = useState("");

  // Fires on every row mutation (add/remove/edit) — the parent (ConfigForm
  // or the premium editor) holds extra_args in its own draft state and
  // only actually PUTs it on Save, so pushing on every change here is
  // safe: it just keeps that draft in sync, the same way typing into a
  // plain <input> would.
  useEffect(() => {
    onChange(serializeLoadOptions(rows));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rows]);

  const hazards = translatedHazardsFor(t, { extra_args: serializeLoadOptions(rows), n_ctx: nCtx });
  const hazardByFlag = new Map(hazards.map((h) => [h.flag, h.translated]));

  const booleanFlags = Object.entries(flagTable).filter(([, ref]) => ref.arg === "none" && !ref.managed);
  const valueRows = rows.filter((r) => r.ref != null && r.ref.arg !== "none" && !r.malformed);
  const customRows = rows.filter((r) => r.ref == null || r.malformed);
  const addableCurated = Object.entries(flagTable).filter(
    ([key, ref]) => ref.arg !== "none" && !ref.managed && !rows.some((r) => canonicalFlag(r.flag) === key),
  );

  function toggleBoolean(key: string, ref: FlagRef) {
    const present = rows.find((r) => canonicalFlag(r.flag) === key);
    if (present) {
      setRows(rows.filter((r) => r.id !== present.id));
    } else {
      setRows([...rows, { id: nextId.current++, flag: key, value: null, ref }]);
    }
  }

  function addCurated(key: string) {
    const ref = flagTable[key];
    if (!ref) return;
    const initial = ref.presets?.[0] ?? ref.placeholder ?? (ref.arg === "number" ? "1" : "");
    setRows([...rows, { id: nextId.current++, flag: key, value: initial, ref }]);
  }

  function addCustom() {
    setRows([...rows, { id: nextId.current++, flag: "", value: "", ref: null }]);
  }

  function updateValue(id: number, v: string) {
    setRows(rows.map((r) => (r.id === id ? { ...r, value: v } : r)));
  }

  function updateFlag(id: number, f: string) {
    setRows(rows.map((r) => (r.id === id ? { ...r, flag: f } : r)));
  }

  function removeRow(id: number) {
    setRows(rows.filter((r) => r.id !== id));
  }

  function enterRawMode() {
    setRawText(serializeLoadOptions(rows).join("\n"));
    setRawMode(true);
  }

  function exitRawMode() {
    const args = rawText.split("\n").map((s) => s.trim()).filter((s) => s.length > 0);
    setRows(parseLoadOptions(args, flagTable).map((o) => ({ ...o, id: nextId.current++ })));
    setRawMode(false);
  }

  if (rawMode) {
    return (
      <div className="load-opt-editor">
        <textarea
          value={rawText}
          rows={8}
          style={{ background: "var(--bg-2)", border: "1px solid var(--border)", borderRadius: 8, color: "var(--text)", padding: "8px 10px", fontFamily: "var(--mono)", fontSize: 12, resize: "vertical", width: "100%" }}
          onChange={(e) => setRawText(e.target.value)}
        />
        <div style={{ fontSize: 11, color: "var(--text-mute)", marginTop: 4 }}>
          {t("catalog_form.load_options.raw_mode_hint")}
        </div>
        <button type="button" className="btn" style={{ fontSize: 11, padding: "4px 8px", marginTop: 8 }} onClick={exitRawMode}>
          {t("catalog_form.load_options.back_to_picker")}
        </button>
      </div>
    );
  }

  return (
    <div className="load-opt-editor">
      {booleanFlags.length > 0 && (
        <div className="flag-chips">
          {booleanFlags.map(([key, ref]) => {
            const present = rows.find((r) => canonicalFlag(r.flag) === key);
            return (
              <button
                key={key}
                type="button"
                className={`flag-chip ${present ? "on" : ""}`}
                onClick={() => toggleBoolean(key, ref)}
              >
                {translatedFlagLabel(t, isVllm, key, ref.label)}
                <InfoTip text={translatedFlagWhy(t, isVllm, key, ref.why)} />
              </button>
            );
          })}
        </div>
      )}

      <div className="load-opts" style={{ marginTop: 10 }}>
        {valueRows.map((r) => {
          const hazard = hazardByFlag.get(r.flag);
          return (
            <div className={`load-opt editable ${hazard ? "hazard" : ""}`} key={r.id}>
              <span className="flag">{r.flag}</span>
              <input
                className="val-input"
                type={r.ref?.arg === "number" ? "number" : "text"}
                value={r.value ?? ""}
                placeholder={r.ref?.placeholder}
                onChange={(e) => updateValue(r.id, e.target.value)}
              />
              {r.ref?.presets && (
                <div className="flag-presets">
                  {r.ref.presets.map((p) => (
                    <button
                      key={p}
                      type="button"
                      className={`preset ${r.value === p ? "active" : ""}`}
                      onClick={() => updateValue(r.id, p)}
                    >
                      {p}
                    </button>
                  ))}
                </div>
              )}
              {r.ref && <InfoTip text={translatedFlagWhy(t, isVllm, canonicalFlag(r.flag), r.ref.why)} />}
              {hazard && <InfoTip text={hazard} className="hazard-tip" />}
              <button type="button" className="row-remove" onClick={() => removeRow(r.id)} aria-label={t("catalog_form.load_options.remove_flag_aria", { flag: r.flag })}>
                ×
              </button>
            </div>
          );
        })}
      </div>

      {addableCurated.length > 0 && (
        <select
          className="add-option"
          value=""
          onChange={(e) => {
            if (e.target.value) addCurated(e.target.value);
          }}
        >
          <option value="">{t("catalog_form.load_options.add_option_ellipsis")}</option>
          {addableCurated.map(([key, ref]) => (
            <option key={key} value={key}>
              {translatedFlagLabel(t, isVllm, key, ref.label)} ({key})
            </option>
          ))}
        </select>
      )}

      <div className="eyebrow" style={{ fontSize: 10.5 }}>
        {t("catalog_form.load_options.advanced_title")}
        <InfoTip text={t("catalog_form.load_options.advanced_hint")} />
      </div>
      <div className="load-opts">
        {customRows.map((r) => (
          <div className={`load-opt editable ${r.malformed ? "hazard" : ""}`} key={r.id}>
            <input className="flag-input" value={r.flag} placeholder={t("catalog_form.load_options.flag_placeholder")} onChange={(e) => updateFlag(r.id, e.target.value)} />
            <input
              className="val-input"
              value={r.value ?? ""}
              placeholder={t("catalog_form.load_options.value_optional_placeholder")}
              onChange={(e) => updateValue(r.id, e.target.value)}
            />
            {r.malformed && (
              <InfoTip
                text={t("catalog_form.load_options.malformed_hint")}
                className="hazard-tip"
              />
            )}
            <button type="button" className="row-remove" onClick={() => removeRow(r.id)} aria-label={t("catalog_form.load_options.remove_flag_aria", { flag: r.flag || "flag" })}>
              ×
            </button>
          </div>
        ))}
      </div>
      <div style={{ display: "flex", gap: 8, marginTop: 8 }}>
        <button type="button" className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={addCustom}>
          {t("catalog_form.load_options.add_custom_flag")}
        </button>
        <button type="button" className="btn" style={{ fontSize: 11, padding: "4px 8px" }} onClick={enterRawMode}>
          {t("catalog_form.load_options.edit_as_raw_text")}
        </button>
      </div>
    </div>
  );
}
