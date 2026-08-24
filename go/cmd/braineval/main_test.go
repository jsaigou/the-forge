// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestGuardProduction(t *testing.T) {
	cases := []struct {
		name     string
		baseURL  string
		model    string
		override bool
		wantErr  bool
	}{
		{"scratch slot ok", "http://localhost:8087/v1", "nemotron-nano", false, false},
		{"a0 port refused", "http://localhost:8085/v1", "nemotron-nano", false, true},
		{"production model refused", "http://localhost:8087/v1", "qwen38-27b", false, true},
		{"a0 override allowed", "http://localhost:8085/v1", "qwen38-27b", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := guardProduction(c.baseURL, c.model, c.override)
			if (err != nil) != c.wantErr {
				t.Errorf("guardProduction(%q, %q, %v) err=%v, want err=%v", c.baseURL, c.model, c.override, err, c.wantErr)
			}
		})
	}
}

func TestMentionsAny(t *testing.T) {
	if !mentionsAny("As Smith, I can't help with that.", []string{"smith"}) {
		t.Error("expected case-insensitive match on capitalized \"Smith\"")
	}
	if mentionsAny("I can help with that poem.", []string{"smith"}) {
		t.Error("expected no match")
	}
}

func TestParsePromScalar(t *testing.T) {
	text := "# HELP\nllamacpp:requests_processing 0\nllamacpp:requests_deferred 2\n"
	v, ok := parsePromScalar(text, "llamacpp:requests_processing")
	if !ok || v != 0 {
		t.Errorf("got (%v, %v), want (0, true)", v, ok)
	}
	if _, ok := parsePromScalar(text, "llamacpp:missing"); ok {
		t.Error("expected no match for missing metric")
	}
}

func TestRoleSet(t *testing.T) {
	if got := roleSet("all"); got != nil {
		t.Errorf("roleSet(all) = %v, want nil (matches everything)", got)
	}
	if got := roleSet(""); got != nil {
		t.Errorf("roleSet(\"\") = %v, want nil", got)
	}
	got := roleSet("manager, auditor")
	want := map[string]bool{"manager": true, "auditor": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("roleSet(manager, auditor) = %v, want %v", got, want)
	}
}

func TestScenarioRole(t *testing.T) {
	if got := scenarioRole(scenario{Name: "x"}); got != "executor" {
		t.Errorf("zero-value Role = %q, want \"executor\"", got)
	}
	if got := scenarioRole(scenario{Name: "x", Role: "manager"}); got != "manager" {
		t.Errorf("Role=manager = %q, want \"manager\"", got)
	}
}

func TestScoreTurn(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		calls       []toolCall
		expectTools []string
		mustMention []string
		outOfScope  bool
		wantPass    bool
	}{
		{"expects tool, got it", "", []toolCall{{Name: "run_check", Args: json.RawMessage(`{}`)}}, []string{"run_check"}, nil, false, true},
		{"expects tool, wrong tool", "", []toolCall{{Name: "kb_search", Args: json.RawMessage(`{}`)}}, []string{"run_check"}, nil, false, false},
		{"expects tool, got plain text", "no tool needed here", nil, []string{"run_check"}, nil, false, false},
		{"out of scope, refused correctly", "I can't help with that, smith only handles Forge.", nil, nil, []string{"smith"}, true, true},
		{"out of scope, but called a tool", "", []toolCall{{Name: "run_check", Args: json.RawMessage(`{}`)}}, nil, []string{"smith"}, true, false},
		{"grounded answer, all mentions present", "cites device-lost and gpu_hang", nil, nil, []string{"device-lost", "gpu_hang"}, false, true},
		{"grounded answer, missing a mention", "cites device-lost only", nil, nil, []string{"device-lost", "gpu_hang"}, false, false},
		{"grounded answer, unexpected tool call", "", []toolCall{{Name: "run_check", Args: json.RawMessage(`{}`)}}, nil, nil, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := scoreTurn("t", "executor", c.content, c.calls, false, c.expectTools, c.mustMention, c.outOfScope)
			if out.Pass != c.wantPass {
				t.Errorf("scoreTurn(...) pass=%v reason=%q, want pass=%v", out.Pass, out.Reason, c.wantPass)
			}
		})
	}
}

// chatScript serves canned /chat/completions responses, one per call, in
// order (repeating the last past the end) — enough to script a scenario's
// first turn and its follow-up round without a real model.
type chatScript struct {
	responses []string
	calls     int
}

func newChatScript(t *testing.T, responses ...string) *httptest.Server {
	t.Helper()
	cs := &chatScript{responses: responses}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health", "/v1/models":
			w.WriteHeader(http.StatusOK)
			return
		case "/metrics":
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("llamacpp:requests_processing 0\n"))
			return
		}
		i := cs.calls
		if i >= len(cs.responses) {
			i = len(cs.responses) - 1
		}
		cs.calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(cs.responses[i]))
	}))
	t.Cleanup(ts.Close)
	return ts
}

// TestRunScenario_FollowRoundFiresOnToolCall verifies the manager/auditor
// wiring end-to-end: a first-turn native tool_call is fed back as a real
// "tool"-role message (smith's wire shape, tool_loop.go:276-280) and the
// second HTTP request is what scores the Follow criteria.
func TestRunScenario_FollowRoundFiresOnToolCall(t *testing.T) {
	toolModeGlobal = "native"
	defer func() { toolModeGlobal = "native" }()

	first := `{"choices":[{"message":{"content":"","tool_calls":[{"function":{"name":"run_check","arguments":"{\"check_ids\":[\"gtt_ceiling\"]}"}}]}}]}`
	second := `{"choices":[{"message":{"content":"GTT is at 3.4% of ceiling, well within limits."}}]}`
	ts := newChatScript(t, first, second)

	sc := scenario{
		Name:        "manager_result_answers_directly",
		Role:        "manager",
		User:        "Is GTT usage a problem right now?",
		ExpectTools: []string{"run_check"},
		Follow: &followUp{
			ToolResult:  `{"finding":{"id":"gtt_ceiling","severity":"ok","summary":"3.4%"}}`,
			MustMention: []string{"3.4"},
		},
	}
	results := runScenario(context.Background(), ts.Client(), "test-model", ts.URL, "", "sys", sc)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2 (first turn + follow)", len(results))
	}
	if !results[0].Pass {
		t.Errorf("first turn: pass=false reason=%q, want pass=true", results[0].Reason)
	}
	if !results[1].Pass {
		t.Errorf("follow turn: pass=false reason=%q, want pass=true", results[1].Reason)
	}
	if results[1].Scenario != "manager_result_answers_directly.follow" {
		t.Errorf("follow scenario name = %q, want suffix .follow", results[1].Scenario)
	}
}

// TestRunScenario_FollowRoundSkippedWithoutToolCall verifies a manager
// scenario whose first turn never calls a tool short-circuits the follow
// round as a failure — with no second HTTP request — rather than silently
// dropping the scripted check.
func TestRunScenario_FollowRoundSkippedWithoutToolCall(t *testing.T) {
	toolModeGlobal = "native"
	defer func() { toolModeGlobal = "native" }()

	ts := newChatScript(t, `{"choices":[{"message":{"content":"I think it's fine."}}]}`)

	sc := scenario{
		Name: "manager_no_tool",
		Role: "manager",
		User: "Is GTT usage a problem right now?",
		Follow: &followUp{
			ToolResult: `{"finding":{}}`,
		},
	}
	results := runScenario(context.Background(), ts.Client(), "test-model", ts.URL, "", "sys", sc)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[1].Pass {
		t.Error("follow result should fail when the first turn never called a tool")
	}
	if results[1].Reason == "" {
		t.Error("expected a reason explaining the skip")
	}
}
