// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Runbook commands are pasted into a root-capable shell by an operator; a hostile unit/mode/slot name must stay one literal word.
func TestOperationCommandNeutralisesHostileValues(t *testing.T) {
	mk := func(kind string, d map[string]string) *Action {
		raw, _ := json.Marshal(d)
		return &Action{Kind: kind, Detail: raw}
	}
	hostile := "x'; touch /tmp/pwned; echo '"
	for _, a := range []*Action{
		mk(KindRestartForgeUnit, map[string]string{"unit": hostile}),
		mk(KindLoadConfig, map[string]string{"mode": hostile, "slot": "a1"}),
		mk(KindUnloadSlot, map[string]string{"slot": hostile}),
	} {
		cmd := operationCommand(a)
		// Let a real shell split the argument text and print each word; a value that escaped its quotes would show up as separate words.
		out, err := exec.Command("sh", "-c", "for w in "+wordsAfterProgram(cmd)+"; do printf '[%s]' \"$w\"; done").CombinedOutput()
		if err != nil {
			t.Fatalf("%s: shell rejected %q: %v %s", a.Kind, cmd, err, out)
		}
		if strings.Contains(string(out), "[touch]") {
			t.Errorf("%s: hostile value escaped its quotes in %q -> %s", a.Kind, cmd, out)
		}
	}
}

// wordsAfterProgram returns the command's argument text (everything after the leading program words) so a test shell can enumerate the words.
func wordsAfterProgram(cmd string) string {
	for _, p := range []string{"sudo systemctl restart ", "curl -X POST localhost:5000/api/v1/load -d ", "curl -X POST localhost:5000/api/v1/unload -d "} {
		if strings.HasPrefix(cmd, p) {
			return strings.TrimPrefix(cmd, p)
		}
	}
	return cmd
}

func TestCurlPostBodyIsOneQuotedJSONWord(t *testing.T) {
	cmd := curlPost("/api/v1/load", map[string]string{"mode": "a'b\"c$(id)", "slot": "a1"})
	out, err := exec.Command("sh", "-c", "printf %s "+strings.TrimPrefix(cmd, "curl -X POST localhost:5000/api/v1/load -d ")).Output()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]string
	if err := json.Unmarshal(out, &back); err != nil || back["mode"] != "a'b\"c$(id)" {
		t.Errorf("body did not round-trip: %q err=%v", out, err)
	}
}
