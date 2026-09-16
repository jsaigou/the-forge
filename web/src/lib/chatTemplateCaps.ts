// Curated display labels for llama.cpp /props' chat_template_caps object
// (T1, per-request thinking control, 2026-09-14). The field set is
// build-dependent — no fixed schema anywhere in this pipeline (see
// store.Config.ChatTemplateCaps's doc comment) — so this is a label lookup
// with a readable fallback, not an exhaustive enum: an unrecognized key
// still renders, title-cased from its raw name.
const LABELS: Record<string, string> = {
  supports_reasoning_effort: "Reasoning effort",
  supports_preserve_reasoning: "Preserve reasoning",
  supports_tool_calls: "Tool calls",
  supports_tools: "Tool calls",
  supports_parallel_tool_calls: "Parallel tool calls",
  supports_object_arguments: "Object tool arguments",
  supports_string_content: "String content",
  supports_system_role: "System role",
  supports_typed_content: "Typed content",
  // Override-only (router/reasoning.go) — there is deliberately no live
  // probe for this one: chat_template_kwargs is a raw Jinja passthrough,
  // and llama.cpp's own chat_template_caps never reports which kwargs a
  // given template understands. An operator has to say so.
  supports_enable_thinking: "Honors enable_thinking kwarg",
};

// OVERRIDE_ONLY_KEYS never appear in a live probe (LABELS' comment above) —
// the caps editor must offer them even for a config that has never loaded,
// since dedupedCapEntries()/the probed map alone would never surface them.
export const OVERRIDE_ONLY_KEYS: string[] = ["supports_enable_thinking"];

export function capLabel(key: string): string {
  if (LABELS[key]) return LABELS[key];
  const stripped = key.startsWith("supports_") ? key.slice("supports_".length) : key;
  return stripped
    .split("_")
    .filter(Boolean)
    .map((w) => w[0].toUpperCase() + w.slice(1))
    .join(" ");
}

// dedupedCapEntries collapses supports_tool_calls/supports_tools — two
// build eras' names for the same fact (see catalog.go's slotProps) — into
// one entry (true if either is true) so the card doesn't show "Tool calls"
// twice. Sorted by label for a stable render order.
export function dedupedCapEntries(caps: Record<string, boolean>): { key: string; label: string; value: boolean }[] {
  const merged = new Map<string, boolean>();
  for (const [key, value] of Object.entries(caps)) {
    const label = capLabel(key);
    merged.set(label, (merged.get(label) ?? false) || value);
  }
  return [...merged.entries()]
    .map(([label, value]) => ({ key: label, label, value }))
    .sort((a, b) => a.label.localeCompare(b.label));
}
