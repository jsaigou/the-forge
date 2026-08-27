// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// newLoginTestServer builds a Server with a real authz.Authorizer backed by
// an in-memory store, so the first-run wizard + login flow exercises the
// actual SQL/Argon2id paths rather than a fake.
func newLoginTestServer(t *testing.T) (*Server, *authz.Authorizer) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	auth := authz.New(db)
	events := bus.New()
	cfg, err := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}

	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      auth,
		AuthSetup: auth,
		Events:    events,
		Publish:   events,
		Config: func() *config.Config { return cfg },
		Hostname:  "test-host",
		Sessions:  db.Sessions(),
	})
	t.Cleanup(func() { s.Close() })
	return s, auth
}

func doForm(t *testing.T, h http.Handler, method, path string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "100.100.100.100:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestLoginSetupPagesMobileMeta guards the F1 fix: phones got a black
// screen because the stubs had no viewport meta (iOS fell back to the
// 980px layout viewport) and the 14px autofocused input triggered iOS
// focus-zoom. Both stubs must carry the viewport + theme-color metas and
// the ≥16px input rule.
func TestLoginSetupPagesMobileMeta(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()

	const wantMetaViewport = `<meta name=viewport content="width=device-width, initial-scale=1">`
	const wantMetaThemeColor = `<meta name=theme-color content=#100e0c>`
	const wantInputFont = `font-size:16px`
	check := func(page, body string) {
		t.Helper()
		for _, want := range []string{wantMetaViewport, wantMetaThemeColor, wantInputFont} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s missing %q", page, want)
			}
		}
	}

	// /setup renders while no user exists.
	req := httptest.NewRequest("GET", "/setup", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup: got %d, want 200", rec.Code)
	}
	check("/setup", rec.Body.String())

	// /login renders once a user exists.
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	req = httptest.NewRequest("GET", "/login", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d, want 200", rec.Code)
	}
	check("/login", rec.Body.String())
}

func TestSetupRedirectsToLoginOnceUserExists(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()

	req := httptest.NewRequest("GET", "/setup", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /setup before any user: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Create the admin account") {
		t.Fatalf("GET /setup body missing form: %s", rec.Body.String())
	}

	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	req = httptest.NewRequest("GET", "/setup", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /setup after user exists: got %d Location=%q, want 303 /login", rec.Code, rec.Header().Get("Location"))
	}
}

func TestPostSetupCreatesAdminAndLogsIn(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()

	rec := doForm(t, h, "POST", "/setup", url.Values{
		"username": {"testuser"},
		"password": {"hunter2hunter2"},
		"confirm":  {"hunter2hunter2"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("POST /setup: got %d Location=%q, want 303 /", rec.Code, rec.Header().Get("Location"))
	}
	var sessionCookieVal string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "forge_session" {
			sessionCookieVal = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
				t.Fatalf("session cookie flags: HttpOnly=%v SameSite=%v", c.HttpOnly, c.SameSite)
			}
		}
	}
	if sessionCookieVal == "" {
		t.Fatal("POST /setup did not set forge_session cookie")
	}
	if _, err := auth.VerifySession(sessionCookieVal); err != nil {
		t.Fatalf("session from setup does not verify: %v", err)
	}

	// A second setup attempt must fail — CompleteSetup refuses once a user
	// exists, and no second cookie should be issued.
	rec2 := doForm(t, h, "POST", "/setup", url.Values{
		"username": {"someone-else"},
		"password": {"anotherpassword1"},
		"confirm":  {"anotherpassword1"},
	})
	if rec2.Code != http.StatusSeeOther || !strings.HasPrefix(rec2.Header().Get("Location"), "/setup?error=") {
		t.Fatalf("second POST /setup: got %d Location=%q, want redirect back to /setup with error", rec2.Code, rec2.Header().Get("Location"))
	}
}

func TestPostSetupPasswordMismatch(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()

	rec := doForm(t, h, "POST", "/setup", url.Values{
		"username": {"testuser"},
		"password": {"hunter2hunter2"},
		"confirm":  {"different-password"},
	})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/setup?error=") {
		t.Fatalf("mismatched POST /setup: got %d Location=%q", rec.Code, rec.Header().Get("Location"))
	}
	required, err := auth.SetupRequired(t.Context())
	if err != nil {
		t.Fatalf("SetupRequired: %v", err)
	}
	if !required {
		t.Fatal("a user was created despite the password mismatch")
	}
}

func TestPostLoginSuccessAndFailure(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	// Wrong password: redirect back to /login with error=1, no cookie.
	rec := doForm(t, h, "POST", "/login?next=%2Fconsole", url.Values{
		"username": {"testuser"},
		"password": {"wrong-password"},
	})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bad login: got %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/login") || !strings.Contains(loc, "error=1") {
		t.Fatalf("bad login Location=%q, want /login...error=1", loc)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Fatalf("bad login set a cookie: %v", rec.Result().Cookies())
	}

	// Correct password: redirect to next, cookie set, session verifies
	// with the right identity.
	rec = doForm(t, h, "POST", "/login?next=%2Fconsole", url.Values{
		"username": {"testuser"},
		"password": {"hunter2hunter2"},
	})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/console" {
		t.Fatalf("good login: got %d Location=%q, want 303 /console", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != "forge_session" {
		t.Fatalf("good login cookies: %v", cookies)
	}
	ident, err := auth.VerifySession(cookies[0].Value)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if ident.Name != "testuser" || ident.Role != authz.RoleAdmin {
		t.Fatalf("identity from session: %+v", ident)
	}
}

func TestLoginNextMustBeSameOriginPath(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rec := doForm(t, h, "POST", "/login?next=https%3A%2F%2Fevil.example%2Fx", url.Values{
		"username": {"testuser"},
		"password": {"hunter2hunter2"},
	})
	if rec.Header().Get("Location") != "/" {
		t.Fatalf("open-redirect guard failed: Location=%q, want /", rec.Header().Get("Location"))
	}
}

// TestLoginPageEscapesNextParam guards a real reflected-XSS bug: GET
// /login used to splice ?next= straight into the rendered form's `action=`
// attribute with no escaping at all, so a crafted link could break out of
// the attribute and inject arbitrary markup into the login page itself —
// the single worst place for that, since it lets an attacker rewrite the
// credential form a victim is about to submit to. Fixed via
// sanitizeNextPath (same-origin-path rule, shared with the POST handler)
// plus url.QueryEscape + html.EscapeString before interpolation.
func TestLoginPageEscapesNextParam(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	payload := `"><script>alert(1)</script>`
	req := httptest.NewRequest("GET", "/login?next="+url.QueryEscape(payload), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatalf("payload reflected unescaped into the login page: %s", body)
	}
	if strings.Contains(body, `action="/login?next="><script>`) {
		t.Fatalf("payload broke out of the form action attribute: %s", body)
	}
}

func TestPostLogout(t *testing.T) {
	s, auth := newLoginTestServer(t)
	h := s.Handler()
	if err := auth.CreateUser(t.Context(), "testuser", "hunter2hunter2", authz.RoleAdmin); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	rec := doForm(t, h, "POST", "/login", url.Values{"username": {"testuser"}, "password": {"hunter2hunter2"}})
	sid := rec.Result().Cookies()[0].Value

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: sid})
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusSeeOther || rec2.Header().Get("Location") != "/login" {
		t.Fatalf("POST /logout: got %d Location=%q", rec2.Code, rec2.Header().Get("Location"))
	}
	if _, err := auth.VerifySession(sid); err == nil {
		t.Fatal("session still verifies after logout")
	}
}

func TestLoginSetupUnavailableWhenAuthSetupNil(t *testing.T) {
	s := newTestServer(t) // Deps.AuthSetup is nil
	h := s.Handler()

	rec := doForm(t, h, "POST", "/login", url.Values{"username": {"a"}, "password": {"b"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /login with nil AuthSetup: got %d, want 503", rec.Code)
	}
	rec = doForm(t, h, "POST", "/setup", url.Values{"username": {"a"}, "password": {"b"}, "confirm": {"b"}})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST /setup with nil AuthSetup: got %d, want 503", rec.Code)
	}
}
