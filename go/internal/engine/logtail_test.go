// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRedactLogTail(t *testing.T) {
	raw := strings.Join([]string{
		"terminate called after throwing an instance of 'std::invalid_argument'",
		"",
		"  what():  entropy acceptance requires unconstrained greedy host sampling   ",
		"loaded with --api-key sk-forge-abcdef1234567890 on port 8080",
		"Authorization: Bearer abcdefghijklmnop.qrstuv",
		"HF_TOKEN=hf_abcdefghijklmnop done",
		"password: hunter2",
	}, "\n")
	got := redactLogTail(raw)
	for _, secret := range []string{"sk-forge-abcdef1234567890", "abcdefghijklmnop", "hf_abcdefghijklmnop", "hunter2"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted tail still contains %q:\n%s", secret, got)
		}
	}
	if !strings.Contains(got, "what():  entropy acceptance requires unconstrained greedy host sampling") {
		t.Errorf("the useful error line was lost:\n%s", got)
	}
	if strings.Contains(got, "\n\n") || strings.HasSuffix(got, " ") {
		t.Errorf("blank lines / trailing space were kept:\n%q", got)
	}
}

func TestRedactLogTailKeepsTheMostRecentOutput(t *testing.T) {
	old := strings.Repeat("old line that should be cut\n", 400)
	got := redactLogTail(old + "FINAL: the actual failure")
	if !strings.HasSuffix(got, "FINAL: the actual failure") {
		t.Errorf("the tail was cut from the wrong end: ...%q", got[len(got)-40:])
	}
	if n := len([]rune(got)); n > startupDetailMaxRunes+1 {
		t.Errorf("tail is %d runes, want at most %d", n, startupDetailMaxRunes+1)
	}
}

// A unit that comes up and then crashes before /health answers: the load must fail with the reason, carry the log
// tail the engine was given (after redaction), and ask for the logs since the load began.
func TestLoadFailureCarriesReasonAndLogTail(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	// reload path: occupied -> gone -> running -> then the unit cycles into auto-restart (a crash loop)
	sys.setSeq("forge-a1",
		st("active", "running"),
		st("inactive", "dead"),
		st("active", "running"),
		st("activating", "auto-restart"),
	)
	m, _, _ := newTestManager(t, cfg, sys, nil) // no llama stub: /health never answers
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")
	m.setSlotMode("a1", "gemma")

	var gotUnit string
	var gotSince time.Time
	m.d.LogTail = func(_ context.Context, unit string, since time.Time, maxLines int) string {
		gotUnit, gotSince = unit, since
		return "what():  something bad\napi_key=supersecretvalue1"
	}
	before := time.Now()
	res := m.Load(context.Background(), "gemma", "a1")

	if res.Success {
		t.Fatal("a crash during startup must fail the load")
	}
	if !strings.Contains(res.Message, "did not reach running state") || !strings.Contains(res.Message, "crashed during startup (SubState=auto-restart)") {
		t.Errorf("message does not say why: %q", res.Message)
	}
	if !strings.Contains(res.Detail, "what():  something bad") {
		t.Errorf("detail lacks the log tail: %q", res.Detail)
	}
	if strings.Contains(res.Detail, "supersecretvalue1") {
		t.Errorf("detail leaked a credential: %q", res.Detail)
	}
	if gotUnit != "forge-a1" {
		t.Errorf("LogTail asked for unit %q, want forge-a1", gotUnit)
	}
	if gotSince.IsZero() || gotSince.After(before.Add(time.Minute)) {
		t.Errorf("LogTail since = %v, want about when the load began", gotSince)
	}
}

func TestLoadFailureWithoutLogTailStillSaysWhy(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"), st("inactive", "dead"), st("active", "running"), st("activating", "auto-restart"))
	m, _, _ := newTestManager(t, cfg, sys, nil)
	writeSlotEnv(t, cfg, "a1", "gemma", "gemma.gguf")
	m.setSlotMode("a1", "gemma")

	res := m.Load(context.Background(), "gemma", "a1")
	if res.Success || !strings.Contains(res.Message, "crashed during startup") {
		t.Fatalf("want a failed load that says why, got success=%v message=%q", res.Success, res.Message)
	}
	if res.Detail != "" {
		t.Errorf("no LogTail was configured but Detail = %q", res.Detail)
	}
}

func TestLoadStartErrorIsIncludedInTheMessage(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("inactive", "dead"))
	sys.startErr = context.DeadlineExceeded
	m, _, _ := newTestManager(t, cfg, sys, nil)
	res := m.Load(context.Background(), "gemma", "a1")
	if res.Success || !strings.Contains(res.Message, "Failed to start forge-a1") || !strings.Contains(res.Message, "deadline exceeded") {
		t.Errorf("message = %q, want the start error included", res.Message)
	}
}
