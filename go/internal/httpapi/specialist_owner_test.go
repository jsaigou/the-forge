// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
)

// A config has one enabled owner (ADR-0022 decision 3). A disabled occupant holds nothing, so it releases the config; re-enabling it takes the
// same check as creating it, which keeps the pool from ever holding two enabled owners.
func TestSpecialistAPI_DisabledOccupantReleasesItsConfig(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	good := specialist.LaunchSpec{Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/d1.gguf", Alias: "d1-2b", Backend: "vulkan", Ctx: 128000}
	s.deps.SpecialistCatalog = fakeSpecialistCatalog{specs: map[string]specialist.LaunchSpec{"d1-2b": good}}

	create := func(name string, enabled string) (int, map[string]any) {
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants",
			`{"name":"`+name+`","kind":"catalog-config","functions":["decisions"],"policy":"on_demand","enabled":`+enabled+`,"params":{"config":"d1-2b"}}`)
		return code, out
	}
	behavior := func(name, b string) (int, map[string]any) {
		code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/"+name+"/behavior", `{"behavior":"`+b+`"}`)
		return code, out
	}
	paramsMsg := func(out map[string]any) string {
		fields, _ := out["fields"].(map[string]any)
		msg, _ := fields["params"].(string)
		return msg
	}

	if code, out := create("a", "true"); code != 201 {
		t.Fatalf("first owner: %d %v", code, out)
	}
	if code, out := behavior("a", "disabled"); code != 200 {
		t.Fatalf("disable the owner: %d %v", code, out)
	}
	// The disabled owner released the config, so another occupant may take it.
	if code, out := create("b", "true"); code != 201 {
		t.Fatalf("a disabled occupant must release its config: %d %v", code, out)
	}
	// Re-enabling the first occupant while the second owns the config is refused.
	code, out := behavior("a", "on_demand")
	if code != 422 || out["code"] != "validation_failed" || !strings.Contains(paramsMsg(out), "already hosted by b") {
		t.Errorf("re-enable against another owner: %d %v", code, out)
	}
}
