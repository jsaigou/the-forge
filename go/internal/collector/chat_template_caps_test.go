// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestChatTemplateCapsParsesLiveShape uses the exact /props chat_template_caps
// object confirmed live against ForgeHost (T1, per-request thinking control,
// 2026-09-14) — including supports_typed_content:false, which a naive "only
// scan truthy fields" parse would silently drop.
func TestChatTemplateCapsParsesLiveShape(t *testing.T) {
	body := `{
		"chat_template_caps": {
			"supports_object_arguments": true,
			"supports_parallel_tool_calls": true,
			"supports_preserve_reasoning": true,
			"supports_reasoning_effort": true,
			"supports_string_content": true,
			"supports_system_role": true,
			"supports_tool_calls": true,
			"supports_tools": true,
			"supports_typed_content": false
		}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	caps, err := NewLlamaClient(func(port int) string { return srv.URL }).ChatTemplateCaps(context.Background(), 8080)
	if err != nil {
		t.Fatalf("ChatTemplateCaps: %v", err)
	}
	want := map[string]bool{
		"supports_object_arguments":    true,
		"supports_parallel_tool_calls": true,
		"supports_preserve_reasoning":  true,
		"supports_reasoning_effort":    true,
		"supports_string_content":      true,
		"supports_system_role":         true,
		"supports_tool_calls":          true,
		"supports_tools":               true,
		"supports_typed_content":       false,
	}
	if len(caps) != len(want) {
		t.Fatalf("ChatTemplateCaps = %+v, want %+v", caps, want)
	}
	for k, v := range want {
		if caps[k] != v {
			t.Errorf("ChatTemplateCaps[%q] = %v, want %v", k, caps[k], v)
		}
	}
}

// TestChatTemplateCapsDropsNonBoolFields proves one unexpected field shape
// can't blind every other capability — a build shipping a stray non-bool
// value must not fail the whole probe.
func TestChatTemplateCapsDropsNonBoolFields(t *testing.T) {
	body := `{"chat_template_caps": {"supports_tool_calls": true, "weird_field": "not a bool", "another": 5}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	caps, err := NewLlamaClient(func(port int) string { return srv.URL }).ChatTemplateCaps(context.Background(), 8080)
	if err != nil {
		t.Fatalf("ChatTemplateCaps: %v", err)
	}
	if len(caps) != 1 || caps["supports_tool_calls"] != true {
		t.Errorf("ChatTemplateCaps = %+v, want only supports_tool_calls:true", caps)
	}
}

// TestChatTemplateCapsMissingObject covers a build with no chat_template_caps
// at all (mainline llama.cpp/build-vulkan, per the Part 2 investigation) —
// must return an empty, non-error map, not fail the load's probe step.
func TestChatTemplateCapsMissingObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"n_ctx": 32768}`)
	}))
	defer srv.Close()

	caps, err := NewLlamaClient(func(port int) string { return srv.URL }).ChatTemplateCaps(context.Background(), 8080)
	if err != nil {
		t.Fatalf("ChatTemplateCaps: %v", err)
	}
	if len(caps) != 0 {
		t.Errorf("ChatTemplateCaps = %+v, want empty", caps)
	}
}
