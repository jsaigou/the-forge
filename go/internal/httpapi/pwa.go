// SPDX-License-Identifier: Apache-2.0

package httpapi

// pwa.go — embed.FS serving of the React PWA build (Contract 1 §"Static
// PWA shell routes"). The build output lives in web_dist/ next to this
// file; it is produced by `cd web && npm run build` (vite.config.ts writes
// there) and is not checked in except for a .gitkeep sentinel so the
// directory always exists and `go build` never fails on a fresh clone.
//
// Routes served (frozen behavior, not shapes):
//   GET /                      → web_dist/index.html (no-store)
//   GET /assets/<file>         → web_dist/assets/<file> (1y immutable)
//   GET /manifest.webmanifest  → web_dist/manifest.webmanifest (1h)
//   GET /registerSW.js         → web_dist/registerSW.js (no-cache)
//   GET /sw.js                 → web_dist/sw.js (no-cache)
//   GET /workbox-<hash>.js     → web_dist/workbox-<hash>.js (1y immutable)
//   GET /pwa-192.png           → web_dist/pwa-192.png (1y immutable)
//   GET /pwa-512.png           → web_dist/pwa-512.png (1y immutable)
//   GET /apple-touch-icon.png  → web_dist/apple-touch-icon.png (1y immutable)
//   GET /favicon.svg           → web_dist/favicon.svg (1y immutable)
//   GET /favicon.png           → web_dist/favicon.png (1y immutable)
//
// /login, /setup, and /logout are server-rendered (not part of the PWA
// bundle — Contract 1 §1: the SPA shell is a static build with nowhere to
// hold pre-auth UI state). GET renders the form; POST calls into
// Deps.AuthSetup (authz.LoginService) for account creation / session
// establishment. The V4 Jinja templates go away at cutover.

import (
	"context"
	"embed"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

//go:embed all:web_dist
var webDistFS embed.FS

// webDistRoot is the embed sub-tree containing the built PWA. Tests use
// it directly to verify the shell is served.
var webDistRoot fs.FS = func() fs.FS {
	sub, err := fs.Sub(webDistFS, "web_dist")
	if err != nil {
		// The directive above guarantees the directory exists; reaching
		// here would mean the toolchain is broken, not a runtime issue.
		panic("httpapi: web_dist embed missing: " + err.Error())
	}
	return sub
}()

// cacheImmutable marks long-lived cached content (content-hashed filenames).
const cacheImmutable = "public, max-age=31536000, immutable"

// registerPWARoutes mounts the PWA shell + login/setup HTML stubs on the
// root mux (alongside /api/v1/*). Unauthenticated users hitting / are
// redirected to /login to mirror V4's behavior.
func (s *Server) registerPWARoutes(mux *http.ServeMux) {
	// {$} matches the end-of-path, so "GET /{$}" matches only exactly
	// "/" — not /api/v1/* or /assets/*. This avoids the Go 1.22+ mux
	// precedence conflict between a method-restricted "GET /" (which
	// would also match /api/v1/*) and the "/api/v1/" prefix. The SPA's
	// tab routes are HASH-based (#models, #settings/security — see
	// web/src/App.tsx's useTabRouter), so bare "/" is the only path the
	// shell needs; a catch-all "GET /" here also conflicts with the
	// method-less "/api/v1/" prefix pattern (tried 2026-08-14).
	mux.HandleFunc("GET /{$}", s.handleSPAIndex)
	mux.HandleFunc("GET /assets/", s.handlePWAAsset)
	mux.HandleFunc("GET /manifest.webmanifest", s.handlePWAFile("manifest.webmanifest", "application/manifest+json", "public, max-age=3600"))
	mux.HandleFunc("GET /registerSW.js", s.handlePWAFile("registerSW.js", "application/javascript", "no-cache"))
	mux.HandleFunc("GET /sw.js", s.handlePWAFile("sw.js", "application/javascript", "no-cache"))
	mux.HandleFunc("GET /pwa-192.png", s.handlePWAFile("pwa-192.png", "image/png", cacheImmutable))
	mux.HandleFunc("GET /pwa-512.png", s.handlePWAFile("pwa-512.png", "image/png", cacheImmutable))
	mux.HandleFunc("GET /apple-touch-icon.png", s.handlePWAFile("apple-touch-icon.png", "image/png", cacheImmutable))
	mux.HandleFunc("GET /favicon.svg", s.handlePWAFile("favicon.svg", "image/svg+xml", cacheImmutable))
	mux.HandleFunc("GET /favicon.png", s.handlePWAFile("favicon.png", "image/png", cacheImmutable))

	// Login + setup: server-rendered (not part of the PWA bundle) since the
	// PWA shell is a static build with nowhere to hold pre-auth UI state.
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handlePostLogin)
	mux.HandleFunc("GET /setup", s.handleSetupPage)
	mux.HandleFunc("POST /setup", s.handlePostSetup)
	mux.HandleFunc("POST /logout", s.handlePostLogout)
}

// handleSPAIndex serves web_dist/index.html when present. Falls back to a
// minimal "PWA not built" 503 if the build hasn't been run. Also serves
// /workbox-<hash>.js (vite-plugin-pwa content-hashed filename).
func (s *Server) handleSPAIndex(w http.ResponseWriter, r *http.Request) {
	// Vite-plugin-pwa emits /workbox-<hash>.js with a content hash. Match
	// the prefix and serve from web_dist root.
	if strings.HasPrefix(r.URL.Path, "/workbox-") && strings.HasSuffix(r.URL.Path, ".js") {
		s.handleWorkbox(w, r)
		return
	}
	if r.URL.Path != "/" {
		// Unknown non-API route — let the SPA's own router handle it.
		// Serve index.html so client-side routes (/console, /scheduling,
		// /models, ...) work on a fresh load.
	}

	// V4 redirects unauthenticated users to /login. The PWA itself is
	// served without auth (it bootstraps via GET /api/v1/session, which
	// returns 401 and triggers the PWA's own /login redirect). For the
	// parallel-run we mirror that exactly.
	data, err := fs.ReadFile(webDistRoot, "index.html")
	if err != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8><title>Forge</title>` +
			`<h1>PWA not built</h1><p>Run <code>cd web &amp;&amp; npm run build</code>.</p>`))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handlePWAAsset serves /assets/<file> from web_dist/assets/. The content
// hash in the filename makes these safe to cache immutably.
func (s *Server) handlePWAAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(webDistRoot, "assets/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", cacheImmutable)
	w.Header().Set("Content-Type", contentTypeFor(name))
	_, _ = w.Write(data)
}

// handlePWAFile returns a single named file from web_dist with the given
// content type and cache-control.
func (s *Server) handlePWAFile(name, ctype, cacheControl string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := fs.ReadFile(webDistRoot, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", cacheControl)
		_, _ = w.Write(data)
	}
}

// handleWorkbox serves /workbox-<hash>.js. The hash is per-build (vite-
// plugin-pwa), so we pattern-match the request path.
func (s *Server) handleWorkbox(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(webDistRoot, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript")
	w.Header().Set("Cache-Control", cacheImmutable)
	_, _ = w.Write(data)
}

// loginPageStyle is shared by the login and setup HTML stubs. Input
// font-size must stay ≥ 16px: iOS Safari auto-zooms on focus of smaller
// inputs, which pinned the visual viewport on a blank region (mobile F1).
const loginPageStyle = `<style>body{font:14px system-ui;background:#100e0c;color:#e8e2d8;margin:2rem auto;max-width:24rem}` +
	`h1{font-size:1.25rem;margin:0 0 1rem}input{display:block;width:100%;box-sizing:border-box;margin:.5rem 0;padding:.5rem;font-size:16px}` +
	`button{padding:.5rem 1rem;background:#00d4e8;color:#100e0c;border:0;font-weight:600}` +
	`.note{color:#888;font-size:.85rem;margin-top:1rem}.err{color:#e85d5d;font-size:.85rem}</style>`

// handleLoginPage renders the server-rendered login form (Contract 1 §1:
// the PWA is a static build and cannot host pre-auth UI state itself).
// Submits to POST /login. If setup hasn't run yet, redirects there instead.
func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.setupRequired(r) {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" {
		next = "/"
	}
	errNote := ""
	if r.URL.Query().Get("error") != "" {
		errNote = `<p class=err>Invalid username or password.</p>`
	}
	html := `<!doctype html><meta charset=utf-8>` +
		`<meta name=viewport content="width=device-width, initial-scale=1">` +
		`<meta name=theme-color content=#100e0c>` +
		`<title>Forge — Login</title>` + loginPageStyle +
		`<h1>The Forge</h1>` + errNote +
		`<form method=post action="/login?next=` + next + `">` +
		`<input name=username placeholder=username autofocus>` +
		`<input name=password type=password placeholder=password>` +
		`<button>Sign in</button></form>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// handleSetupPage renders the first-run wizard form (create the initial
// admin account). Once any user exists, redirects to /login — CompleteSetup
// refuses to run twice, so there is no reason to show the form again.
func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request) {
	if !s.setupRequired(r) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	errNote := ""
	if msg := r.URL.Query().Get("error"); msg != "" {
		errNote = `<p class=err>` + htmlEscape(msg) + `</p>`
	}
	html := `<!doctype html><meta charset=utf-8>` +
		`<meta name=viewport content="width=device-width, initial-scale=1">` +
		`<meta name=theme-color content=#100e0c>` +
		`<title>Forge — Setup</title>` + loginPageStyle +
		`<h1>Create the admin account</h1>` + errNote +
		`<form method=post action="/setup">` +
		`<input name=username placeholder=username autofocus>` +
		`<input name=password type=password placeholder="password (min 8 chars)">` +
		`<input name=confirm type=password placeholder="confirm password">` +
		`<button>Create account</button></form>` +
		`<p class=note>This runs once — the first account created here becomes admin.</p>`
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// setupRequired reports whether the first-run wizard still needs to run.
// Fails open to "not required" (routes to /login) if AuthSetup isn't wired
// or the check errors — a broken store must not lock an operator out of a
// page that renders a clear error, but it also must not offer to recreate
// an admin account when we can't actually tell if one exists.
func (s *Server) setupRequired(r *http.Request) bool {
	if s.deps.AuthSetup == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	required, err := s.deps.AuthSetup.SetupRequired(ctx)
	return err == nil && required
}

// handlePostSetup creates the initial admin account, logs them in
// immediately (matching the login-on-first-run UX operators expect from
// V4's wizard), and redirects to /.
func (s *Server) handlePostSetup(w http.ResponseWriter, r *http.Request) {
	if s.deps.AuthSetup == nil {
		writeError(w, http.StatusServiceUnavailable, "setup not available")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/setup?error="+url.QueryEscape("invalid form submission"), http.StatusSeeOther)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if password != r.FormValue("confirm") {
		http.Redirect(w, r, "/setup?error="+url.QueryEscape("passwords do not match"), http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.deps.AuthSetup.CompleteSetup(ctx, username, password); err != nil {
		http.Redirect(w, r, "/setup?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	sess, _, err := s.deps.AuthSetup.Login(ctx, username, password, clientIP(r), r.UserAgent())
	if err != nil {
		// Account was created but the immediate login somehow failed —
		// send them to sign in by hand rather than swallowing the error.
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.setSessionCookie(w, sess)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handlePostLogin verifies the password and establishes a session cookie.
func (s *Server) handlePostLogin(w http.ResponseWriter, r *http.Request) {
	if s.deps.AuthSetup == nil {
		writeError(w, http.StatusServiceUnavailable, "login not available")
		return
	}
	next := r.URL.Query().Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next)+"&error=1", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sess, _, err := s.deps.AuthSetup.Login(ctx, r.FormValue("username"), r.FormValue("password"), clientIP(r), r.UserAgent())
	if err != nil {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(next)+"&error=1", http.StatusSeeOther)
		return
	}
	s.setSessionCookie(w, sess)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// handlePostLogout deletes the session (idempotent) and clears the cookie.
func (s *Server) handlePostLogout(w http.ResponseWriter, r *http.Request) {
	if sid := sessionCookie(r); sid != "" && s.deps.AuthSetup != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		_ = s.deps.AuthSetup.Logout(ctx, sid)
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// setSessionCookie sets the forge_session cookie. Secure=false to match
// V4's Flask cookie config (forge/app.py): tailscale serve terminates
// TLS in front of this process, which itself only ever speaks plain HTTP.
func (s *Server) setSessionCookie(w http.ResponseWriter, sess store.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     "forge_session",
		Value:    sess.ID,
		Path:     "/",
		Expires:  sess.ExpiresAt,
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie expires the forge_session cookie immediately.
func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "forge_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteLaxMode,
	})
}

// clientIP strips the port from r.RemoteAddr for use as a rate-limit key.
// Falls back to the raw value if it isn't in host:port form.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// htmlEscape escapes a string for safe inclusion in the login/setup stub
// pages' error notes (built via string concatenation, not html/template).
func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

// contentTypeFor guesses a Content-Type from a filename extension. Used
// for Vite-emitted assets where the build process already picks the right
// suffix (js, css, woff2, png, svg). Falls back to octet-stream so the
// browser downloads rather than guesses.
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".js"), strings.HasSuffix(name, ".mjs"):
		return "application/javascript"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".woff2"):
		return "font/woff2"
	case strings.HasSuffix(name, ".woff"):
		return "font/woff"
	case strings.HasSuffix(name, ".ico"):
		return "image/x-icon"
	}
	return "application/octet-stream"
}
