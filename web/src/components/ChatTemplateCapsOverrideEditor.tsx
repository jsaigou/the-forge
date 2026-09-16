import { capLabel, OVERRIDE_ONLY_KEYS } from "../lib/chatTemplateCaps";
import { InfoTip } from "./InfoTip";

// ChatTemplateCapsOverrideEditor (2026-09-14) — the missing control for
// store.Config.ChatTemplateCapsOverride (T1). Before this, the override was
// fully CRUD-capable on the backend (httpapi/catalog_configs.go's
// configBody, store.Config.EffectiveChatTemplateCaps) but had no edit
// surface anywhere in the app — a comment in ConfigEditView.tsx claiming it
// "has its own edit surface elsewhere" was false; grepping the repo found
// none. Without this, router/reasoning.go's `supports_enable_thinking`
// branch is unreachable in practice: nothing can ever set that key, so
// `reasoning_effort: none` silently does nothing on any build without
// native reasoning_effort support (confirmed live 2026-09-13: mainline
// build-vulkan, laguna-rocm715, poolside).
//
// Operates on raw chat_template_caps keys, not lib/chatTemplateCaps.ts's
// dedupedCapEntries() — that helper collapses two build eras' names for the
// same fact (supports_tool_calls/supports_tools) into one display row,
// which is right for a read-only card and wrong here: an override must
// target one real key store-side. Keys offered = every key this config has
// ever probed, union OVERRIDE_ONLY_KEYS (capabilities — currently just
// supports_enable_thinking — no probe could ever populate).
export type CapsOverride = Record<string, boolean> | undefined;

type OverrideState = "auto" | "on" | "off";

function stateOf(override: CapsOverride, key: string): OverrideState {
  if (!override || !(key in override)) return "auto";
  return override[key] ? "on" : "off";
}

export function ChatTemplateCapsOverrideEditor({
  probed,
  override,
  onChange,
}: {
  probed: Record<string, boolean>;
  override: CapsOverride;
  onChange: (next: CapsOverride) => void;
}) {
  const keys = [...new Set([...Object.keys(probed), ...OVERRIDE_ONLY_KEYS])].sort((a, b) => capLabel(a).localeCompare(capLabel(b)));

  function setState(key: string, state: OverrideState) {
    const next = { ...(override ?? {}) };
    if (state === "auto") {
      delete next[key];
    } else {
      next[key] = state === "on";
    }
    onChange(Object.keys(next).length > 0 ? next : undefined);
  }

  return (
    <div className="form-row" style={{ gridColumn: "1 / -1" }}>
      <span style={{ display: "flex", alignItems: "center", gap: 4 }}>
        Chat template overrides
        <InfoTip text="Corrects or supplies a chat-template capability llama.cpp's own /props probe gets wrong, or can't see at all (whether this build honors the enable_thinking kwarg has no live signal — an operator has to say so). Auto uses the live probe from this config's last load; Force on/off always wins over it." />
      </span>
      <div style={{ display: "flex", flexDirection: "column", gap: 6, marginTop: 4 }}>
        {keys.map((key) => {
          const probedVal = probed[key];
          return (
            <div key={key} style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 11.5 }}>
              <span style={{ flex: 1 }}>{capLabel(key)}</span>
              <span className="chip" style={{ fontSize: 10, opacity: probedVal === undefined ? 0.55 : 1 }}>
                probe: {probedVal === undefined ? "—" : probedVal ? "✓" : "✗"}
              </span>
              <select value={stateOf(override, key)} onChange={(e) => setState(key, e.target.value as OverrideState)}>
                <option value="auto">Auto (use probe)</option>
                <option value="on">Force on</option>
                <option value="off">Force off</option>
              </select>
            </div>
          );
        })}
      </div>
    </div>
  );
}
