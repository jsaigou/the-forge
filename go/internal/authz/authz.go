// SPDX-License-Identifier: Apache-2.0

// Package authz is V5's "boring auth" (design decision 4, docs/v5-plan.md):
// sessions + Argon2id, bearer tokens in the V4 formats, RBAC, rate limiting.
// Owned by track B (Phase 3). No WebAuthn in core.
//
// The interfaces and the token/tailnet helpers here are Contract 2. The
// helpers are implemented in Phase 1 (they are contract-critical for
// track D's tailnet-conditional auth and small enough to freeze as code).
package authz

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Role is the RBAC level. Ordering: viewer < operator < admin.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Allows reports whether a holder of r may act at level need.
func (r Role) Allows(need Role) bool {
	rank := map[Role]int{RoleViewer: 0, RoleOperator: 1, RoleAdmin: 2}
	got, ok1 := rank[r]
	want, ok2 := rank[need]
	return ok1 && ok2 && got >= want
}

// KeyKind is the bearer-token family.
type KeyKind string

const (
	KindForge   KeyKind = "forge"   // dashboard API
	KindRouter  KeyKind = "router"  // a0
	KindMCP     KeyKind = "mcp"     // MCP server
)

// Identity is the authenticated caller, produced by session-cookie or
// bearer-token verification.
type Identity struct {
	// Name is the username (sessions) or key name (bearer) — the identity
	// used as requested_by in scheduler calls.
	Name string
	Role Role
	// KeyID is set for bearer identities, "" for sessions.
	KeyID string
	Kind  KeyKind // "" for sessions
	// DisplayName is the operator's preferred consumer label (api_keys
	// display_name), set on bearer identities when the operator chose one.
	// a0's slot-consumer attribution uses it verbatim; "" = derive at
	// request time from key name + User-Agent.
	DisplayName string
	// Sprint 0-AUTH (§3.1): assurance level the session has proven. Set
	// for session-based identities (from the session row); zero for bearer
	// identities — bearer-key paths skip the policy matrix (§5).
	Assurance        Assurance
	AssuranceAt      time.Time
	NetworkPrincipal string
}

// ErrBadToken is returned for tokens that do not match the sk-* grammar.
// Callers must not echo the token back in errors or logs.
var ErrBadToken = errors.New("authz: malformed bearer token")

// tokenRE encodes the frozen token grammar:
// sk-<kind>-<keyid:12 lowercase hex>-<secret>.
//
// Rebrand tolerance (2026-08): the dashboard kind was renamed
// "foundry" → "forge", so both prefixes are accepted at parse time and
// ParseToken normalizes legacy "foundry" to KindForge. Pre-cutover minted
// sk-foundry-* keys keep working until re-minted.
var tokenRE = regexp.MustCompile(`^sk-(foundry|forge|router|mcp)-([0-9a-f]{12})-([A-Za-z0-9_\-]{16,128})$`)

// ParseToken splits a bearer token into kind, keyid, and secret. The keyid
// routes to exactly one api_keys row so exactly one Argon2 verify runs per
// request. Legacy dashboard tokens (sk-foundry-*) normalize to KindForge.
func ParseToken(token string) (kind KeyKind, keyid, secret string, err error) {
	m := tokenRE.FindStringSubmatch(strings.TrimSpace(token))
	if m == nil {
		return "", "", "", ErrBadToken
	}
	k := KeyKind(m[1])
	if k == "foundry" {
		k = KindForge
	}
	return k, m[2], m[3], nil
}

// FormatToken assembles a token in the frozen format (used by key minting).
func FormatToken(kind KeyKind, keyid, secret string) string {
	return fmt.Sprintf("sk-%s-%s-%s", kind, keyid, secret)
}

// tailnetCGNAT is the Tailscale CGNAT range; requests sourced from it skip
// a0's bearer check (docs/scheduler.md "A0's Tailnet-Conditional Auth").
var tailnetCGNAT = netip.MustParsePrefix("100.64.0.0/10")

// IsTailnetAddr reports whether addr is in the Tailscale CGNAT range.
func IsTailnetAddr(addr netip.Addr) bool {
	return addr.Is4() && tailnetCGNAT.Contains(addr)
}

// EffectiveRemoteAddr resolves the client address for the tailnet check:
// X-Forwarded-For is trusted ONLY when remoteAddr is loopback (the
// `tailscale serve` HTTPS path terminates on loopback and sets XFF
// trustworthily); on the direct HTTP path remoteAddr is the real tailnet IP
// and XFF must be ignored. Frozen semantics — see the crown-jewels list.
func EffectiveRemoteAddr(remoteAddr netip.Addr, xff string) netip.Addr {
	if !remoteAddr.IsLoopback() || xff == "" {
		return remoteAddr
	}
	// First hop in the XFF chain is the original client.
	first := strings.TrimSpace(strings.Split(xff, ",")[0])
	parsed, err := netip.ParseAddr(first)
	if err != nil {
		return remoteAddr
	}
	return parsed
}

// Authenticator is what httpapi / router / mcp middleware consume (Contract
// 2). Phase 3 implements it against the store with Argon2id verification and
// per-IP rate limiting (10 failed attempts / 60s, both paths).
type Authenticator interface {
	// VerifySession resolves a session cookie value to an Identity.
	VerifySession(sessionID string) (Identity, error)
	// VerifyBearerFrom resolves a full sk-* token to an Identity, enforcing
	// the expected kind (a router token must not open the dashboard) and
	// the per-IP rate limit shared with the login path (10 failed
	// attempts / 60s). ip is an opaque rate-limit key (client address,
	// port stripped) — every production caller must go through this, not
	// the unlimited VerifyBearer, so bearer-key brute-forcing is bounded
	// the same way password brute-forcing already is.
	VerifyBearerFrom(ctx context.Context, ip, token string, want KeyKind) (Identity, error)
}

// LoginService is the account-creation/session-establishment surface for
// the server-rendered POST /login and POST /setup handlers (httpapi). Kept
// separate from Authenticator because router/mcp only ever verify an
// existing session/bearer — they never create accounts or sessions, so
// their fakes shouldn't need to grow these methods too.
type LoginService interface {
	// SetupRequired reports whether the first-run wizard must still run
	// (no users exist yet).
	SetupRequired(ctx context.Context) (bool, error)
	// CompleteSetup creates the initial admin account. Refuses to run once
	// any user exists.
	CompleteSetup(ctx context.Context, username, password string) error
	// Login verifies a password and creates a session. ip is an opaque
	// rate-limit key (client address, port stripped).
	Login(ctx context.Context, username, password, ip, userAgent string) (store.Session, Identity, error)
	// Logout deletes a session. Idempotent.
	Logout(ctx context.Context, sessionID string) error
}

// StubAuthenticator accepts everything as the given identity — for wiring
// tracks C/D before Phase 3 lands. Never ships enabled.
type StubAuthenticator struct{ Identity Identity }

var _ Authenticator = (*StubAuthenticator)(nil)

func (s *StubAuthenticator) VerifySession(string) (Identity, error) { return s.Identity, nil }

// VerifyBearerFrom ignores ip and the rate limit entirely — this stub is
// never wired in production (cmd/forge always wires the real *Authorizer).
func (s *StubAuthenticator) VerifyBearerFrom(_ context.Context, _, token string, want KeyKind) (Identity, error) {
	if _, _, _, err := ParseToken(token); err != nil {
		return Identity{}, err
	}
	return s.Identity, nil
}
