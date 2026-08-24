// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"
)

// TestPromptStaysInSyncWithSmith fails when cmd/braineval's hand copy of
// smith's real system prompt drifts from the source of truth
// (internal/smith/prompt.md). braineval's evaluation is only meaningful if
// it feeds the model the SAME prompt smith actually runs in production.
func TestPromptStaysInSyncWithSmith(t *testing.T) {
	assertCopyInSync(t, "../../internal/smith/prompt.md", "prompt.md")
}

// TestAuditPromptStaysInSyncWithSmith is prompt.md's sibling for the
// auditor role — cmd/braineval/audit.md must stay byte-identical to
// internal/smith/audit.md, the prompt tool_loop.go swaps in for the verify
// round (tool_loop.go:200-210), or the auditor scenarios (Sprint 6) score a
// model against a prompt smith would never actually run.
func TestAuditPromptStaysInSyncWithSmith(t *testing.T) {
	assertCopyInSync(t, "../../internal/smith/audit.md", "audit.md")
}

func assertCopyInSync(t *testing.T, sourcePath, copyPath string) {
	t.Helper()
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatalf("could not read source %s: %v", sourcePath, err)
	}
	copy, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("could not read braineval's %s: %v", copyPath, err)
	}
	if string(source) != string(copy) {
		t.Fatalf("go/cmd/braineval/%s has drifted from go/%s — copy the source file verbatim: cp %s cmd/braineval/%s",
			copyPath, sourcePath, sourcePath, copyPath)
	}
}
