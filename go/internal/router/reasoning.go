// SPDX-License-Identifier: Apache-2.0

package router

// reasoning.go — per-request thinking control (Sprint T2, 2026-09-14, Part 2
// of ~/.claude/plans/squishy-snuggling-acorn.md). a0 accepts one standard
// knob, reasoning_effort: none|low|medium|high, on any local
// (foundry_slot) chat-completion request and translates it per-config into
// whatever the resolved config's own build actually understands:
//
//  1. Native reasoning_effort support (chat_template_caps.supports_reasoning_effort,
//     probed live per config at load time — T1) ⇒ set it directly. Covers
//     every kintsugi build.
//  2. Else, an operator-curated "supports_enable_thinking" key in
//     ChatTemplateCapsOverride ⇒ inject chat_template_kwargs.enable_thinking
//     for "none" only, omit otherwise. There is deliberately no live probe
//     for this: chat_template_kwargs is a raw passthrough to the model's
//     Jinja template, and llama.cpp's own chat_template_caps never reports
//     which kwargs a given template understands — an operator has to say so
//     (T1's ChatTemplateCapsOverride column, generalized from "correcting a
//     wrong probe" to "supplying a fact the probe structurally can't see").
//  3. Neither ⇒ the resolved level can't be honored on this build (true
//     today for mainline llama.cpp/build-vulkan, llama.cpp-laguna-rocm715,
//     and llama.cpp-poolside — confirmed live, 2026-09-13) — leave the body
//     untouched rather than send a parameter the server will silently
//     ignore, or worse, choke on.
//
// reasoning_budget_tokens mapping (the plan's step 3, explicitly "optional")
// is deliberately not implemented here: no curated signal exists yet for
// which templates are "budget-capable", and building one without a real use
// case would be guessing. Add it in a follow-up once that signal exists.

// validReasoningEfforts is the standard vocabulary a0 accepts on the wire,
// independent of what any particular build understands.
var validReasoningEfforts = map[string]bool{"none": true, "low": true, "medium": true, "high": true}

// applyReasoningEffort resolves the effective reasoning_effort level for a
// local request — the client's own body value if valid, else b's
// ReasoningEffortDefault (copied from the resolved store.Config at
// catalogChain-build time, so this needs no catalog read of its own — see
// Backend.ReasoningEffortDefault's doc comment) — and translates it via
// normalizeReasoningEffort.
func applyReasoningEffort(b *Backend, body map[string]any) map[string]any {
	level, _ := body["reasoning_effort"].(string)
	if !validReasoningEfforts[level] {
		level = b.ReasoningEffortDefault
		if !validReasoningEfforts[level] {
			return body // no client-sent level, no config default — nothing to do
		}
	}
	// A client that did send its own level still needs the same per-config
	// translation — it can't be expected to know which knob this build's
	// template honors.
	return normalizeReasoningEffort(body, b.ChatTemplateCaps, level)
}

// normalizeReasoningEffort translates one resolved reasoning_effort level
// into whatever knob caps says this config's build actually honors. See
// this file's package-level doc comment for the full precedence rationale.
func normalizeReasoningEffort(body map[string]any, caps map[string]bool, level string) map[string]any {
	switch {
	case caps["supports_reasoning_effort"]:
		out := shallowCopyBody(body)
		out["reasoning_effort"] = level
		return out
	case caps["supports_enable_thinking"] && level == "none":
		out := shallowCopyBody(body)
		kwargs := map[string]any{}
		if existing, ok := out["chat_template_kwargs"].(map[string]any); ok {
			for k, v := range existing {
				kwargs[k] = v
			}
		}
		kwargs["enable_thinking"] = false
		out["chat_template_kwargs"] = kwargs
		return out
	default:
		return body
	}
}

func shallowCopyBody(body map[string]any) map[string]any {
	out := make(map[string]any, len(body)+1)
	for k, v := range body {
		out[k] = v
	}
	return out
}
