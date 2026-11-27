// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"testing"
)

// httpapi treats an unwired Sessions or PolicyStore as "no gate" (many test servers rely on that), so production must wire them: a daemon that
// silently started without them would serve every step-up-protected route to any session. This pins the wiring in main.go.
func TestMainWiresTheAuthDependenciesTheGatesFailOpenWithout(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, field := range []string{"Sessions", "PolicyStore", "StepUpVerifier", "Auth"} {
		wired := regexp.MustCompile(`(?m)^\s*` + field + `:\s+\S+,`)
		unwired := regexp.MustCompile(`(?m)^\s*` + field + `:\s+nil,`)
		if !wired.MatchString(s) {
			t.Errorf("main.go does not set httpapi.Deps.%s", field)
		}
		if unwired.MatchString(s) {
			t.Errorf("main.go sets httpapi.Deps.%s to nil", field)
		}
	}
}
