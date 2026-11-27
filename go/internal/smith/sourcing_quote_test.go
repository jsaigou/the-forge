// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"os/exec"
	"strings"
	"testing"
)

// The quoted form must survive a real shell as exactly one literal argument, whatever the file name holds.
func TestShellQuoteRoundTripsThroughARealShell(t *testing.T) {
	for _, in := range []string{"plain.gguf", "with space.gguf", "a;touch pwned", "$(id)", "`id`", "it's", `a"b`, "a\\b", "line1\nline2", "-rf", "*", "a|b&c>d<e", ""} {
		out, err := exec.Command("sh", "-c", "printf %s "+shellQuote(in)).Output()
		if err != nil {
			t.Fatalf("sh failed for %q: %v", in, err)
		}
		if string(out) != in {
			t.Errorf("shellQuote(%q) round-tripped to %q", in, out)
		}
	}
}

func TestDownloadStepsQuoteTheRepoAndTheHFSuppliedFilename(t *testing.T) {
	steps := downloadSteps("org/repo", "dir/we ird;$(id).gguf")
	if len(steps) != 1 {
		t.Fatalf("steps = %d", len(steps))
	}
	if !strings.Contains(steps[0].Command, "'dir/we ird;$(id).gguf'") || !strings.Contains(steps[0].Command, "'org/repo'") {
		t.Errorf("command not quoted: %s", steps[0].Command)
	}
	if !strings.Contains(steps[0].VerifyCommand, "'/opt/forge/models/dir/we ird;$(id).gguf'") {
		t.Errorf("verify command not quoted: %s", steps[0].VerifyCommand)
	}
}
