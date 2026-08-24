// SPDX-License-Identifier: Apache-2.0

package httpapi

// smith_autonomy_handlers_test.go — GET/PUT /api/v1/smith/autonomy
// (autonomous-remediation Sprint 5, docs/v5-smith.md §13.5).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

func TestHandleSmithAutonomy_GetDefaultsOff(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/autonomy", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp smithAutonomyResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Policy.Enabled {
		t.Error("expected the global kill switch to default false")
	}
	var sawRestartDownUnit bool
	for _, p := range resp.EligibleProcedures {
		if p.ID == "restart_down_unit" {
			sawRestartDownUnit = true
		}
	}
	if !sawRestartDownUnit {
		t.Errorf("eligible_procedures = %+v, want restart_down_unit present", resp.EligibleProcedures)
	}
}

func TestHandleSmithAutonomy_PutAndGetRoundTrip(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	body := `{"enabled":true,"procedures":{"restart_down_unit":{"enabled":true,"cooldown_seconds":300,"max_per_day":2}}}`
	w := do(t, s, authedRequest("PUT", "/api/v1/smith/autonomy", bytes.NewBufferString(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	w = do(t, s, authedRequest("GET", "/api/v1/smith/autonomy", nil))
	var resp smithAutonomyResponse
	decodeJSON(t, w.Body, &resp)
	if !resp.Policy.Enabled {
		t.Fatal("expected the global kill switch to be on after PUT")
	}
	pa, ok := resp.Policy.Procedures["restart_down_unit"]
	if !ok || !pa.Enabled || pa.CooldownSeconds != 300 || pa.MaxPerDay != 2 {
		t.Errorf("procedures[restart_down_unit] = %+v, want enabled with cooldown=300 max=2", pa)
	}
}

func TestHandleSmithAutonomy_RejectsIneligibleProcedure(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	body := `{"enabled":true,"procedures":{"comfyui_prune":{"enabled":true}}}`
	w := do(t, s, authedRequest("PUT", "/api/v1/smith/autonomy", bytes.NewBufferString(body)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT with comfyui_prune (RiskHigh, not eligible) = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

func TestHandleSmithAutonomy_RejectsNegativeCooldown(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	body := `{"enabled":true,"procedures":{"restart_down_unit":{"enabled":true,"cooldown_seconds":-1}}}`
	w := do(t, s, authedRequest("PUT", "/api/v1/smith/autonomy", bytes.NewBufferString(body)))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PUT with negative cooldown = %d, want 422; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleSmithAutonomy_EscalationRequiresStepUp proves the asymmetric
// gate against the REAL authz stack (PolicyStore + session assurance), not
// just the PolicyStore-unwired pass-through serverWithSmith exercises above:
// an L0 (network-only) session can PUT enabled:false freely, but PUT
// enabled:true — an escalation — gets 403 step_up_required for
// action.smith.autonomy.
func TestHandleSmithAutonomy_EscalationRequiresStepUp(t *testing.T) {
	s, auth, db := serverWithSmithActionsAuth(t)

	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleOperator); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	uid := getUserID(t, db, "testuser")
	if err := db.IdentityLinks().Create(context.Background(), store.IdentityLink{
		Provider: "tailscale", Principal: "user@github", UserID: uid, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("IdentityLinks.Create: %v", err)
	}
	s.deps.NetworkIdentity = &authz.TailscaleIdentityProvider{
		Client: &fakeWhoIsClientHTTP{login: "user@github"},
	}

	req := httptest.NewRequest("GET", "/api/v1/session", nil)
	req.RemoteAddr = "100.100.100.100:54321"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie after network bootstrap")
	}
	csrf := getCSRFToken(t, s, cookie)

	// Disabling (the default state is already off, but the request body
	// itself doesn't escalate anything) must succeed at L0.
	req = httptest.NewRequest("PUT", "/api/v1/smith/autonomy", bytes.NewBufferString(`{"enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT enabled:false at L0 = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Escalating (turning the global kill switch on) must be refused at L0.
	req = httptest.NewRequest("PUT", "/api/v1/smith/autonomy", bytes.NewBufferString(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT enabled:true at L0 = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	var errResp map[string]string
	json.NewDecoder(rec.Body).Decode(&errResp)
	if errResp["error"] != "step_up_required" || errResp["resource"] != authz.ResourceActionSmithAutonomy {
		t.Errorf("errResp = %+v, want step_up_required for action.smith.autonomy", errResp)
	}

	// Opting in a specific procedure without the global switch is also an
	// escalation and must be refused too.
	req = httptest.NewRequest("PUT", "/api/v1/smith/autonomy",
		bytes.NewBufferString(`{"enabled":false,"procedures":{"restart_down_unit":{"enabled":true}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", csrf)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PUT opting in a procedure at L0 = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}

	// Verify the policy on disk still reflects only the earlier successful
	// (non-escalating) write, not either refused escalation attempt.
	final := s.deps.Smith.AutonomyPolicy(context.Background())
	if final.Enabled {
		t.Errorf("policy.Enabled = true after refused escalations, want false")
	}
}
