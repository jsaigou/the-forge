// SPDX-License-Identifier: Apache-2.0

package router

import "testing"

func kintsugiCaps() map[string]bool {
	// The real live shape confirmed against qwen38-flash-next on ForgeHost,
	// 2026-09-13 (see progress.md's Part 1/Part 2 investigation entry).
	return map[string]bool{
		"supports_object_arguments":    true,
		"supports_parallel_tool_calls": true,
		"supports_preserve_reasoning":  true,
		"supports_reasoning_effort":    true,
		"supports_tool_calls":          true,
	}
}

func gemmaCapsWithOverride() map[string]bool {
	// The real live shape confirmed against gemma4-e4b-qat on ForgeHost,
	// 2026-09-14 (T1) — supports_reasoning_effort false — plus the curated
	// override T2 depends on since no live probe covers enable_thinking.
	return map[string]bool{
		"supports_object_arguments":   true,
		"supports_preserve_reasoning": false,
		"supports_reasoning_effort":   false,
		"supports_tool_calls":         true,
		"supports_enable_thinking":    true, // curated override, not probed
	}
}

func mainlineCaps() map[string]bool {
	// llama.cpp/build-vulkan — reasoning_effort genuinely absent, per the
	// live investigation this session's plan cites — and no curated
	// enable_thinking override set either (the common case for most
	// configs, at least until an operator curates one).
	return map[string]bool{
		"supports_tool_calls": true,
	}
}

func TestNormalizeReasoningEffort_NativeSupport(t *testing.T) {
	body := map[string]any{"messages": []any{"hi"}}
	out := normalizeReasoningEffort(body, kintsugiCaps(), "low")
	if out["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low", out["reasoning_effort"])
	}
	if out["messages"] == nil {
		t.Error("unrelated fields must survive the translation")
	}
	// The original body must not be mutated (shallow copy).
	if _, ok := body["reasoning_effort"]; ok {
		t.Error("input body was mutated — normalizeReasoningEffort must copy, not mutate")
	}
}

func TestNormalizeReasoningEffort_EnableThinkingFallback(t *testing.T) {
	t.Run("none injects enable_thinking:false", func(t *testing.T) {
		out := normalizeReasoningEffort(map[string]any{}, gemmaCapsWithOverride(), "none")
		kwargs, ok := out["chat_template_kwargs"].(map[string]any)
		if !ok {
			t.Fatalf("chat_template_kwargs missing or wrong type: %#v", out["chat_template_kwargs"])
		}
		if kwargs["enable_thinking"] != false {
			t.Errorf("enable_thinking = %v, want false", kwargs["enable_thinking"])
		}
		// reasoning_effort itself must not be injected onto a build that
		// doesn't understand it.
		if _, present := out["reasoning_effort"]; present {
			t.Error("reasoning_effort must not be set for a non-native build")
		}
	})

	t.Run("non-none level omits the kwarg entirely", func(t *testing.T) {
		for _, level := range []string{"low", "medium", "high"} {
			out := normalizeReasoningEffort(map[string]any{}, gemmaCapsWithOverride(), level)
			if _, present := out["chat_template_kwargs"]; present {
				t.Errorf("level %q: chat_template_kwargs = %v, want absent (plan: omit for anything but none)", level, out["chat_template_kwargs"])
			}
		}
	})

	t.Run("preserves an existing chat_template_kwargs sibling field", func(t *testing.T) {
		body := map[string]any{"chat_template_kwargs": map[string]any{"preserve_thinking": true}}
		out := normalizeReasoningEffort(body, gemmaCapsWithOverride(), "none")
		kwargs := out["chat_template_kwargs"].(map[string]any)
		if kwargs["preserve_thinking"] != true {
			t.Errorf("preserve_thinking was dropped: %+v", kwargs)
		}
		if kwargs["enable_thinking"] != false {
			t.Errorf("enable_thinking = %v, want false", kwargs["enable_thinking"])
		}
		// The caller's own map must not be mutated in place.
		if orig := body["chat_template_kwargs"].(map[string]any); orig["enable_thinking"] != nil {
			t.Error("input chat_template_kwargs map was mutated in place")
		}
	})
}

func TestNormalizeReasoningEffort_NoKnownLever(t *testing.T) {
	body := map[string]any{"messages": []any{"hi"}}
	out := normalizeReasoningEffort(body, mainlineCaps(), "none")
	// Nothing this router can do for this build — must be a true no-op,
	// not even a defensive copy (same map identity is fine to assert via
	// length/content since maps aren't comparable, so check no injected keys).
	if _, present := out["reasoning_effort"]; present {
		t.Error("must not inject reasoning_effort onto a build with no known lever")
	}
	if _, present := out["chat_template_kwargs"]; present {
		t.Error("must not inject chat_template_kwargs onto a build with no known lever")
	}
}

func TestApplyReasoningEffort_ClientSentLevelWins(t *testing.T) {
	b := &Backend{ReasoningEffortDefault: "high", ChatTemplateCaps: kintsugiCaps()}
	out := applyReasoningEffort(b, map[string]any{"reasoning_effort": "low"})
	if out["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want low (client's own value must win over the config default)", out["reasoning_effort"])
	}
}

func TestApplyReasoningEffort_FallsBackToConfigDefault(t *testing.T) {
	b := &Backend{ReasoningEffortDefault: "none", ChatTemplateCaps: gemmaCapsWithOverride()}
	out := applyReasoningEffort(b, map[string]any{})
	kwargs, ok := out["chat_template_kwargs"].(map[string]any)
	if !ok || kwargs["enable_thinking"] != false {
		t.Errorf("expected the config default (none) to apply, got %+v", out)
	}
}

func TestApplyReasoningEffort_InvalidClientValueFallsBackToDefault(t *testing.T) {
	b := &Backend{ReasoningEffortDefault: "low", ChatTemplateCaps: kintsugiCaps()}
	out := applyReasoningEffort(b, map[string]any{"reasoning_effort": "extreme"})
	if out["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort = %v, want the config default low (client sent an invalid level)", out["reasoning_effort"])
	}
}

func TestApplyReasoningEffort_NoLevelNoDefaultIsNoOp(t *testing.T) {
	b := &Backend{ChatTemplateCaps: kintsugiCaps()} // ReasoningEffortDefault zero value ""
	body := map[string]any{"messages": []any{"hi"}}
	out := applyReasoningEffort(b, body)
	if len(out) != len(body) {
		t.Errorf("out = %+v, want body unchanged", out)
	}
}
