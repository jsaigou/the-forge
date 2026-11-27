// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
)

// Link-only Console entries (infra.external_tools): stored as data, validated field by field, and served as rows of kind "external" that carry a
// link and no state.

func putTools(t *testing.T, s *Server, body string) (int, map[string]any) {
	t.Helper()
	code, out, _ := spCall(t, s, "PUT", "/api/v1/external-tools", body)
	return code, out
}

func TestExternalTools_StoredAsDataAndServedAsLinkOnlyRows(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	if code, out, _ := spCall(t, s, "GET", "/api/v1/external-tools", ""); code != 200 || len(out["tools"].([]any)) != 0 {
		t.Fatalf("empty by default: %d %v", code, out)
	}
	code, out := putTools(t, s, `{"tools":[
	  {"name":" voice-studio ","url":"https://voice-studio.example.net/","icon":"qwen","description":"Voices"},
	  {"name":"talk-studio","url":"https://talk-studio.example.net/"}]}`)
	if code != 200 || len(out["tools"].([]any)) != 2 {
		t.Fatalf("put: %d %v", code, out)
	}
	if first := out["tools"].([]any)[0].(map[string]any); first["name"] != "voice-studio" {
		t.Errorf("the name was not trimmed: %v", first)
	}
	_, got, _ := spCall(t, s, "GET", "/api/v1/external-tools", "")
	if len(got["tools"].([]any)) != 2 {
		t.Fatalf("stored list: %v", got)
	}
	// They appear among the services as link-only rows: a kind of their own, the link, no unit, no port, never active.
	_, svc, _ := spCall(t, s, "GET", "/api/v1/infra-services", "")
	var rows []map[string]any
	for _, r := range svc["services"].([]any) {
		if m := r.(map[string]any); m["kind"] == "external" {
			rows = append(rows, m)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("external rows: %v", svc["services"])
	}
	v := rows[0]
	if v["name"] != "voice-studio" || v["url"] != "https://voice-studio.example.net/" || v["logo"] != "qwen" || v["detail"] != "Voices" || v["unit"] != nil || v["port"] != nil || v["active"] != false || v["mode_key"] != nil {
		t.Errorf("voice-studio row: %v", v)
	}
	if rows[1]["logo"] != nil || rows[1]["detail"] != nil {
		t.Errorf("an entry with no icon or description must carry none: %v", rows[1])
	}
	// Clearing the list removes the rows.
	if code, _ := putTools(t, s, `{"tools":[]}`); code != 200 {
		t.Fatal("clear")
	}
	_, svc, _ = spCall(t, s, "GET", "/api/v1/infra-services", "")
	for _, r := range svc["services"].([]any) {
		if r.(map[string]any)["kind"] == "external" {
			t.Fatalf("a cleared entry is still served: %v", r)
		}
	}
}

func TestExternalTools_Validation(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	long := strings.Repeat("a", 70)
	for name, body := range map[string]string{
		"missing list":     `{}`,
		"no name":          `{"tools":[{"name":"","url":"https://a.example/"}]}`,
		"long name":        `{"tools":[{"name":"` + long + `","url":"https://a.example/"}]}`,
		"duplicate (case)": `{"tools":[{"name":"A","url":"https://a.example/"},{"name":"a","url":"https://b.example/"}]}`,
		"no scheme":        `{"tools":[{"name":"a","url":"a.example"}]}`,
		"javascript link":  `{"tools":[{"name":"a","url":"javascript:alert(1)"}]}`,
		"ftp":              `{"tools":[{"name":"a","url":"ftp://a.example/"}]}`,
		"no host":          `{"tools":[{"name":"a","url":"https://"}]}`,
		"bad icon":         `{"tools":[{"name":"a","url":"https://a.example/","icon":"Not An Icon"}]}`,
		"long description": `{"tools":[{"name":"a","url":"https://a.example/","description":"` + strings.Repeat("d", 201) + `"}]}`,
		"not json":         `{`,
	} {
		code, out := putTools(t, s, body)
		if code < 400 || code >= 500 {
			t.Errorf("%s: %d %v, want a 4xx", name, code, out)
		}
	}
	var many []string
	for i := 0; i < 25; i++ {
		many = append(many, `{"name":"t`+strings.Repeat("x", i)+`","url":"https://a.example/"}`)
	}
	if code, _ := putTools(t, s, `{"tools":[`+strings.Join(many, ",")+`]}`); code != 422 {
		t.Errorf("25 entries: %d, want 422", code)
	}
	// A refused write changed nothing.
	if _, got, _ := spCall(t, s, "GET", "/api/v1/external-tools", ""); len(got["tools"].([]any)) != 0 {
		t.Errorf("a refused write stored something: %v", got)
	}
}

func TestExternalTools_FieldCodesAreStable(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	code, out := putTools(t, s, `{"tools":[{"name":"a","url":"nope"}]}`)
	if code != 422 {
		t.Fatalf("%d %v", code, out)
	}
	codes, _ := out["field_codes"].(map[string]any)
	if codes["tools[0].url"] != "invalid_url" {
		t.Errorf("codes = %v, want tools[0].url: invalid_url", out)
	}
}

func TestExternalTools_ReadersCanListButOnlyAdminsCanChange(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleOperator)
	if code, _, _ := spCall(t, s, "GET", "/api/v1/external-tools", ""); code != 200 {
		t.Errorf("an operator must be able to read: %d", code)
	}
	if code, _ := putTools(t, s, `{"tools":[]}`); code != 403 {
		t.Errorf("an operator must not change the list: %d, want 403", code)
	}
}
