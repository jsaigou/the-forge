// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/engine"
)

// The daemon runs enforcing: the slot shim launches from the row, so a row that cannot be built or written must fail the load.
func TestSlotLaunchWiringEnforces(t *testing.T) {
	var st engine.SlotLaunchStore // nil is fine: the wiring must pass the store through untouched
	gotStore, enforce := slotLaunchWiring(st)
	if !enforce {
		t.Fatal("production wiring must set SlotLaunchEnforce=true")
	}
	if gotStore != st {
		t.Error("store not passed through")
	}
}

// main() must take both values from slotLaunchWiring rather than a literal, so the test above is what decides production behaviour.
func TestMainUsesSlotLaunchWiring(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"slotLaunchWiring(db.SlotLaunches())", "SlotLaunchEnforce: slotLaunchEnforce,", "SlotLaunches: slotLaunchStore,"} {
		if !strings.Contains(s, want) {
			t.Errorf("main.go lacks %q", want)
		}
	}
	if strings.Contains(s, "SlotLaunchEnforce: false") {
		t.Error("main.go sets SlotLaunchEnforce: false")
	}
}
