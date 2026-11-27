// SPDX-License-Identifier: Apache-2.0

package launchers

import (
	"strings"
	"testing"
)

// Since ADR-0026 step (e) each embedded start-aN.sh is the generic shim, byte-identical to what is installed on ForgeHost. This is the text, with the
// slot name as the only variable. Smith restores a missing launcher from these bytes, so they must stay exactly what the unit can run.
func wantShim(slot string) string {
	return "#!/bin/bash\n" +
		"# Generic slot launcher (ADR-0026): all engine knowledge lives in the database row; this only hands over to forge.\n" +
		"exec /opt/forge/forge slot-exec " + slot + "\n"
}

func TestEmbeddedSlotLaunchersAreTheShimForTheirOwnSlot(t *testing.T) {
	for _, slot := range []string{"a1", "a2", "a3", "a4"} {
		b, ok := Content("start-" + slot + ".sh")
		if !ok {
			t.Fatalf("start-%s.sh is not embedded", slot)
		}
		if string(b) != wantShim(slot) {
			t.Errorf("start-%s.sh = %q, want the shim %q", slot, b, wantShim(slot))
		}
		// Names its own slot and no other slot (a copy-paste error would start the wrong engine).
		for _, other := range []string{"a1", "a2", "a3", "a4"} {
			if other != slot && strings.Contains(string(b), other) {
				t.Errorf("start-%s.sh mentions %s", slot, other)
			}
		}
	}
}

// The legacy launchers' llama-server fallbacks, argument parsing and port numbers are gone from the embedded copies for good; a regression to the
// old script would bring back a second source of truth for the command line.
func TestEmbeddedSlotLaunchersCarryNoEngineKnowledge(t *testing.T) {
	for _, slot := range []string{"a1", "a2", "a3", "a4"} {
		b, _ := Content("start-" + slot + ".sh")
		for _, banned := range []string{"llama-server", "LLAMA_BIN", "ARGS_FILE", "mapfile", "--port"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("start-%s.sh contains %q: engine knowledge belongs in the database row, not the launcher", slot, banned)
			}
		}
	}
}

// start-comfyui.sh is not a slot launcher and is untouched by the shim change.
func TestComfyuiLauncherStillEmbedded(t *testing.T) {
	b, ok := Content("start-comfyui.sh")
	if !ok || len(b) == 0 || strings.Contains(string(b), "slot-exec") {
		t.Errorf("start-comfyui.sh: ok=%v len=%d", ok, len(b))
	}
}

func TestUnknownLauncherHasNoCanonicalCopy(t *testing.T) {
	if _, ok := Content("start-a9.sh"); ok {
		t.Error("start-a9.sh unexpectedly embedded")
	}
}
