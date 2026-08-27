// SPDX-License-Identifier: Apache-2.0

package smith

import "testing"

func TestRedactValue_DropsSensitiveKeys(t *testing.T) {
	in := map[string]any{
		"url":              "http://127.0.0.1:8085/healthz",
		"status":           200,
		"api_key":          "sk-router-abc123-def456ghijklmnop",
		"providers.token":  "sk-deepseek-xyz",
		"nested": map[string]any{
			"active_token": "sk-router-nested-secret1234",
			"latency_ms":   12.5,
		},
		"list": []any{
			map[string]any{"password": "hunter2", "ok": true},
		},
	}
	out := redactValue(in).(map[string]any)

	if out["api_key"] != redactedPlaceholder {
		t.Errorf("api_key = %v, want redacted", out["api_key"])
	}
	if out["providers.token"] != redactedPlaceholder {
		t.Errorf("providers.token = %v, want redacted", out["providers.token"])
	}
	if out["url"] != "http://127.0.0.1:8085/healthz" {
		t.Errorf("url should survive unredacted, got %v", out["url"])
	}
	if out["status"] != 200 {
		t.Errorf("status should survive unredacted, got %v", out["status"])
	}

	nested := out["nested"].(map[string]any)
	if nested["active_token"] != redactedPlaceholder {
		t.Errorf("nested.active_token = %v, want redacted", nested["active_token"])
	}
	if nested["latency_ms"] != 12.5 {
		t.Errorf("nested.latency_ms should survive unredacted, got %v", nested["latency_ms"])
	}

	list := out["list"].([]any)
	entry := list[0].(map[string]any)
	if entry["password"] != redactedPlaceholder {
		t.Errorf("list[0].password = %v, want redacted", entry["password"])
	}
	if entry["ok"] != true {
		t.Errorf("list[0].ok should survive unredacted, got %v", entry["ok"])
	}

	// Input must not be mutated — Evidence is also read by the FE's
	// expandable-detail view with real values.
	if in["api_key"] != "sk-router-abc123-def456ghijklmnop" {
		t.Error("redactValue mutated its input")
	}
}

func TestScrubSecretPatterns(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"router key", "loaded token sk-router-abc123-def456ghijklmnop into memory", "loaded token [redacted] into memory"},
		{"bearer header", "Authorization: Bearer abcDEF123456789012345", "Authorization: [redacted]"},
		{"hf token", "request failed with hf_AbCdEfGhIjKlMnOpQrStUvWx in the URL", "request failed with [redacted] in the URL"},
		{"clean text", "a0 unreachable on port 8085: connection refused", "a0 unreachable on port 8085: connection refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrubSecretPatterns(tc.in); got != tc.want {
				t.Errorf("scrubSecretPatterns(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}


