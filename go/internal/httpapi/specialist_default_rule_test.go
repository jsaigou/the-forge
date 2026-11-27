// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
)

const asrAlwaysOnBody = `{"name":"asr","kind":"qwen3-asr-server","functions":["transcription"],"policy":"always_on","home_slot":"s1","enabled":true,
 "memory_bytes":1073741824,"params":{"model":"/opt/forge/models/qwen3-asr/q8.gguf","threads":"4"}}`

// A function's default must be an enabled always-on member: a default that is on demand or disabled would be answered by a cold start or not at
// all. The rule holds both ways: only such a member can be made the default, and the default can't be taken out of that state without first
// choosing another default.
func TestSpecialistAPI_DefaultMustBeAlwaysOn(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	setDefault := func(occupant string) (int, map[string]any) {
		code, out, _ := spCall(t, s, "PUT", "/api/v1/specialists/defaults/transcription", `{"occupant":"`+occupant+`"}`)
		return code, out
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}

	// On demand: refused, and no default is recorded.
	code, out := setDefault("asr")
	fields, _ := out["fields"].(map[string]any)
	fieldCodes, _ := out["field_codes"].(map[string]any)
	if msg, _ := fields["occupant"].(string); code != 422 || !strings.Contains(msg, "always-on") || fieldCodes["occupant"] != "engine_must_be_always_on" {
		t.Fatalf("on-demand default: %d %v, want 422 with field code engine_must_be_always_on", code, out)
	}
	if _, got, _ := spCall(t, s, "GET", "/api/v1/specialists", ""); len(got["defaults"].(map[string]any)) != 0 {
		t.Fatalf("a refused default was recorded: %v", got["defaults"])
	}

	// Always on: accepted.
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/policy", `{"policy":"always_on","home_slot":"s1"}`); code != 200 {
		t.Fatalf("make always-on: %d %v", code, out)
	}
	if code, out := setDefault("asr"); code != 200 {
		t.Fatalf("always-on default: %d %v", code, out)
	}

	// While it is the default it can't be taken out of "enabled always-on" by any route.
	for name, call := range map[string]func() (int, map[string]any){
		"policy on_demand": func() (int, map[string]any) {
			c, o, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/policy", `{"policy":"on_demand"}`)
			return c, o
		},
		"behavior on_demand": func() (int, map[string]any) {
			c, o, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"on_demand"}`)
			return c, o
		},
		"behavior disabled": func() (int, map[string]any) {
			c, o, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"disabled"}`)
			return c, o
		},
		"full update to on demand": func() (int, map[string]any) {
			c, o, _ := spCall(t, s, "PUT", "/api/v1/specialists/occupants/asr", asrBody)
			return c, o
		},
	} {
		code, out := call()
		params, _ := out["params"].(map[string]any)
		if code != 409 || out["code"] != "default_engine_in_use" || params["functions"] != "transcription" {
			t.Errorf("%s while default: %d %v, want 409 default_engine_in_use with params.functions=transcription", name, code, out)
		}
	}
	// Nothing changed behind the refusals.
	_, list, _ := spCall(t, s, "GET", "/api/v1/specialists", "")
	occ, _ := list["occupants"].([]any)
	if len(occ) != 1 || occ[0].(map[string]any)["policy"] != "always_on" || occ[0].(map[string]any)["enabled"] != true {
		t.Errorf("a refused change altered the member: %v", occ)
	}
	// An edit that keeps it enabled and always-on is fine.
	if code, out, _ := spCall(t, s, "PUT", "/api/v1/specialists/occupants/asr", asrAlwaysOnBody); code != 200 {
		t.Errorf("edit keeping always-on: %d %v", code, out)
	}

	// Clear the default, then the member can move freely.
	if code, out := setDefault(""); code != 200 {
		t.Fatalf("clear default: %d %v", code, out)
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"disabled"}`); code != 200 {
		t.Errorf("disable once it is not a default: %d %v", code, out)
	}
}
