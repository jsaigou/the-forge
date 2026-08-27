// SPDX-License-Identifier: MIT

// Package activity is the per-slot consumer attribution registry: a tiny,
// dependency-free, in-process map of "who last touched this slot" that the
// a0 router (bearer-key identity) and smith (its own reasoning brain)
// write to and the dashboard's /api/v1/status reads back as
// status.slot_consumers. Entries are timestamped; readers apply a freshness
// window so a consumer that stopped mid-generation ages out instead of
// sticking forever.
package activity

import (
	"strings"
	"sync"
	"time"
	"unicode"
)

// ConsumerFreshness is the freshness window the dashboard applies when
// reading labels — long enough to cover a long streaming generation whose
// start-mark is the only write, short enough that a finished request's
// label disappears promptly.
const ConsumerFreshness = 120 * time.Second

// Registry maps slot id ("a1".."a4") → the most recent consumer entry.
// Safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	entries map[string]Entry
}

// Entry is one Mark: who consumed the slot, and when.
type Entry struct {
	Label string
	At    time.Time
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{entries: map[string]Entry{}}
}

// Mark records label as slot's most recent consumer as of now. Called on
// request start and again on completion/refresh by both the router and
// smith; the latest write wins.
func (r *Registry) Mark(slot, label string) {
	if r == nil || slot == "" || label == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]Entry{}
	}
	r.entries[slot] = Entry{Label: label, At: time.Now()}
}

// Label returns slot's consumer label if it was marked within newerThan of
// now; "" if absent or stale.
func (r *Registry) Label(slot string, newerThan time.Duration) string {
	if r == nil || slot == "" {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[slot]
	if !ok || time.Since(e.At) > newerThan {
		return ""
	}
	return e.Label
}

// DeriveLabel builds a human-facing consumer label from information The
// Forge itself has at request time — no hardcoded client roster. This is
// the fallback path only: an operator-set key DisplayName (see
// api_keys.display_name, migration 0068) always wins over derivation when
// present — callers should check that first, same as consumerLabel does.
//
//   - userAgent: the request's User-Agent header. The first product token
//     (before "/" and whitespace) is treated as the app name, kept in
//     whatever casing the client itself sent — "OpenCode/1.2.3" → "OpenCode",
//     "opencode/1.2.3" → "opencode". Empty or junk UAs yield "". Deliberately
//     no capitalization table: a client that sends inconsistent casing is a
//     display_name problem for the operator to set explicitly, not something
//     this function should paper over.
//   - keyName: the operator-chosen bearer-key name. When the app name is
//     also the key name's leading segment ("opencode-examplehost" + UA OpenCode),
//     the remainder is treated as the host: "ExampleHost (OpenCode)". Otherwise
//     a distinct app name is parenthesized after the raw key name.
//
// Examples (all derived, nothing env-specific):
//
//	ua "OpenCode/1.2.3",  key "opencode-examplehost" -> ExampleHost (OpenCode)
//	ua "LibreChat/2.0",   key "librechat"       -> LibreChat
//	ua "myagent/0.9",     key "testuser-laptop"      -> testuser-laptop (myagent)
//	ua "",                key "opencode-core"   -> opencode-core
//
// Both empty → "" (callers fall back to remote address).
func DeriveLabel(keyName, userAgent string) string {
	app := appNameFromUserAgent(userAgent)
	if keyName == "" && app == "" {
		return ""
	}
	if app == "" {
		return keyName
	}
	if keyName != "" {
		lowerKey := strings.ToLower(keyName)
		lowerApp := strings.ToLower(app)
		switch {
		case lowerKey == lowerApp:
			return app
		case strings.HasPrefix(lowerKey, lowerApp+"-"):
			return capitalize(strings.TrimPrefix(lowerKey, lowerApp+"-")) + " (" + app + ")"
		}
		return keyName + " (" + app + ")"
	}
	return app
}

// appNameFromUserAgent extracts the first product token of a User-Agent,
// verbatim casing preserved: "OpenCode/1.2.3 (cli)" → "OpenCode"; "curl/8.4" → "curl";
// "" / garbage → "". Deliberately no app table — whatever the client sends
// is what appears, which stays correct as consumers come and go.
func appNameFromUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if ua == "" {
		return ""
	}
	// First product token: up to first whitespace, then strip version "/x".
	tok := ua
	if i := strings.IndexAny(tok, " \t"); i >= 0 {
		tok = tok[:i]
	}
	if i := strings.IndexByte(tok, '/'); i >= 0 {
		tok = tok[:i]
	}
	tok = strings.Trim(tok, "-_.")
	if tok == "" || !isLetterish(tok) {
		return ""
	}
	// Verbatim client casing — the request is the only source of brand
	// spelling available to us ("OpenCode/1.0" stays OpenCode).
	return tok
}

// isLetterish requires at least one letter so numeric/garbage tokens don't
// become labels.
func isLetterish(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// capitalize uppercases the first rune of s (""-safe).
func capitalize(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}
