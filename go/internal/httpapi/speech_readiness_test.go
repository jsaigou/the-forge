// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

func readinessState(t *testing.T, out map[string]any, id string) string {
	t.Helper()
	for _, c := range out["capabilities"].([]any) {
		if m := c.(map[string]any); m["id"] == id {
			return m["state"].(string)
		}
	}
	t.Fatalf("no capability %q in %v", id, out)
	return ""
}

// zonosProgramOnly gives the server a speech root that holds the clone engine's program, which is all a registration without parameters needs.
func zonosProgramOnly(t *testing.T, s *Server) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "zonos3.cpp", "build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "zonos3-server"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	s.deps.SpeechRoot = root
}

func TestSpeechReadiness_EmptyNodeThenAnEngine(t *testing.T) {
	s, p := newSpeechServer(t, authz.RoleOperator)
	zonosProgramOnly(t, s)
	code, out, _ := spCall(t, s, "GET", "/api/v1/speech/readiness", "")
	if code != 200 || out["ready"] != false || out["usable"] != false || readinessState(t, out, "clone") != "not_installed" {
		t.Fatalf("empty: %d %v", code, out)
	}
	if fd := out["front_door"].(map[string]any); fd["state"] != "not_configured" {
		t.Errorf("no tts_url: %v", fd)
	}
	if _, err := s.deps.SpecialistWriter.CreateOccupant(context.Background(), store.Occupant{Name: "z", Kind: specialist.KindZonos3Server,
		Functions: []string{store.FnSpeech}, Policy: store.PolicyOnDemand, Enabled: true, Params: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if _, out, _ = spCall(t, s, "GET", "/api/v1/speech/readiness", ""); readinessState(t, out, "clone") != "ready" || out["usable"] != true || out["ready"] != false {
		t.Errorf("one engine: %v", out)
	}
	if len(p.asked) != 0 {
		t.Errorf("a report must never resolve (and so load) an engine: %+v", p.asked)
	}
}

func TestSpeechReadiness_ADeadFrontDoorIsNotUsable(t *testing.T) {
	s, _ := newSpeechServer(t, authz.RoleOperator)
	zonosProgramOnly(t, s)
	if _, err := s.deps.SpecialistWriter.CreateOccupant(context.Background(), store.Occupant{Name: "z", Kind: specialist.KindZonos3Server,
		Functions: []string{store.FnSpeech}, Policy: store.PolicyOnDemand, Enabled: true, Params: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	var asked string
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked = r.URL.Path }))
	if err := s.deps.Settings.Set(context.Background(), "infra.router", []byte(`{"tts_url":"`+door.URL+`/v1"}`)); err != nil {
		t.Fatal(err)
	}
	_, out, _ := spCall(t, s, "GET", "/api/v1/speech/readiness", "")
	if out["front_door"].(map[string]any)["state"] != "ok" || out["usable"] != true || asked != "/health" {
		t.Fatalf("live door: %v (asked %q)", out, asked)
	}
	door.Close()
	_, out, _ = spCall(t, s, "GET", "/api/v1/speech/readiness", "")
	if out["front_door"].(map[string]any)["state"] != "unreachable" || out["usable"] != false {
		t.Errorf("dead door: %v", out)
	}
}

// forge-tts' own key gets the report without the front door probe: it is the front door, and the probe would call straight back into it.
func TestSpeechReadiness_TheSpeechKeyIsNotProbed(t *testing.T) {
	s, _ := newSpeechServer(t, authz.RoleOperator)
	s.deps.Auth = &stubAuth{identity: authz.Identity{Name: "forge-tts", Role: authz.RoleOperator, KeyID: "k", Scope: authz.ScopeSpeech}}
	code, out, _ := spCall(t, s, "GET", "/api/v1/speech/readiness", "")
	if _, has := out["front_door"]; code != 200 || has {
		t.Errorf("speech key: %d %v", code, out)
	}
}
