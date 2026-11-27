// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func postStepUp(t *testing.T, s *Server, cookieValue, csrf, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/step-up", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookieValue})
	req.Header.Set("X-CSRF-Token", csrf)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// A stolen session must not be able to guess factors without limit: after five failures the user is locked out for the window, correct
// credentials included, and the response says so.
func TestStepUpIsThrottledPerUser(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	csrf := getCSRFToken(t, s, cookie)
	for i := 0; i < 5; i++ {
		if rec := postStepUp(t, s, cookie.Value, csrf, `{"factor":"password","password":"wrong-password"}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, rec.Code)
		}
	}
	rec := postStepUp(t, s, cookie.Value, csrf, `{"factor":"password","password":"hunter2hunter2"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after 5 failures a correct password = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if e.Code != "too_many_attempts" {
		t.Errorf("code = %q, want too_many_attempts (body %s)", e.Code, rec.Body.String())
	}
}

// The tight limit is per session: one session burning its attempts must not lock the same user's OTHER session out, and a different user is
// never affected. The user-wide cap still catches guessing spread across sessions.
func TestStepUpThrottleIsPerSessionWithAUserWideCap(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	a := loginAndCookie(t, s, auth, "alice", "alice-password-1")
	b := loginAndCookie(t, s, auth, "bob", "bob-password-123")
	csrfA, csrfB := getCSRFToken(t, s, a), getCSRFToken(t, s, b)
	for i := 0; i < 6; i++ {
		postStepUp(t, s, a.Value, csrfA, `{"factor":"password","password":"nope"}`)
	}
	if rec := postStepUp(t, s, a.Value, csrfA, `{"factor":"password","password":"alice-password-1"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the abused session should be locked, got %d", rec.Code)
	}
	if rec := postStepUp(t, s, b.Value, csrfB, `{"factor":"password","password":"bob-password-123"}`); rec.Code != http.StatusOK {
		t.Fatalf("bob's step-up = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// alice's second login (a different session) is not locked by the first session's failures.
	rec := doForm(t, s.Handler(), "POST", "/login", url.Values{"username": {"alice"}, "password": {"alice-password-1"}})
	var second *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			second = c
		}
	}
	if second == nil {
		t.Fatal("no second session")
	}
	if rec := postStepUp(t, s, second.Value, getCSRFToken(t, s, second), `{"factor":"password","password":"alice-password-1"}`); rec.Code != http.StatusOK {
		t.Fatalf("alice's other session = %d, want 200 (a thief on one session must not lock the owner out): %s", rec.Code, rec.Body.String())
	}
}

// A successful step-up forgets earlier failures.
func TestStepUpSuccessResetsTheFailureCount(t *testing.T) {
	s, auth, _ := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	csrf := getCSRFToken(t, s, cookie)
	for i := 0; i < 4; i++ {
		postStepUp(t, s, cookie.Value, csrf, `{"factor":"password","password":"nope"}`)
	}
	rec := postStepUp(t, s, cookie.Value, csrf, `{"factor":"password","password":"hunter2hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("4 failures then the right password = %d, want 200", rec.Code)
	}
	var rotated string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			rotated = c.Value
		}
	}
	// 4 more failures on the rotated session still leave attempts: the user-wide count was reset too.
	csrf2 := getCSRFToken(t, s, &http.Cookie{Name: "forge_session", Value: rotated})
	for i := 0; i < 4; i++ {
		postStepUp(t, s, rotated, csrf2, `{"factor":"password","password":"nope"}`)
	}
	if rec := postStepUp(t, s, rotated, csrf2, `{"factor":"password","password":"hunter2hunter2"}`); rec.Code != http.StatusOK {
		t.Fatalf("after a success the count restarts; got %d", rec.Code)
	}
}

// A password step-up must not downgrade a session that is still validly elevated to passkey, nor refresh its timestamp; once the
// passkey elevation has expired the fresh password factor applies.
func TestStepUpDoesNotDowngradeALivePasskeySession(t *testing.T) {
	s, auth, db := newAuthTestServer(t)
	cookie := loginAndCookie(t, s, auth, "testuser", "hunter2hunter2")
	passkeyAt := time.Now().Add(-2 * time.Minute)
	if err := db.Sessions().ElevateSession(t.Context(), cookie.Value, "sid-passkey", "csrf-passkey", "passkey", passkeyAt); err != nil {
		t.Fatal(err)
	}

	rec := postStepUp(t, s, "sid-passkey", "csrf-passkey", `{"factor":"password","password":"hunter2hunter2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("step-up = %d: %s", rec.Code, rec.Body.String())
	}
	var resp stepUpResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Assurance != "passkey" {
		t.Errorf("a live passkey session was downgraded to %q", resp.Assurance)
	}
	if resp.AssuranceAt == nil || int64(*resp.AssuranceAt) != passkeyAt.Unix() {
		t.Errorf("a weaker factor refreshed the passkey timestamp: got %v, want %d", resp.AssuranceAt, passkeyAt.Unix())
	}

	// Expired passkey elevation: the password factor now stands.
	var newSID string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			newSID = c.Value
		}
	}
	if err := db.Sessions().ElevateSession(t.Context(), newSID, "sid-old", "csrf-old", "passkey", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rec = postStepUp(t, s, "sid-old", "csrf-old", `{"factor":"password","password":"hunter2hunter2"}`)
	resp = stepUpResponse{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Assurance != "password" {
		t.Errorf("after the passkey elevation expired, password should apply: %d %q", rec.Code, resp.Assurance)
	}
}
