// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// Failure errors. Deliberately generic: which of username/password/token
// part failed is never distinguishable to the caller (no enumeration), and
// no secret material ever appears in an error.
var (
	ErrUnauthenticated = errors.New("authz: unauthenticated")
	ErrRateLimited     = errors.New("authz: too many failed attempts")
)

const (
	defaultSessionTTL = 7 * 24 * time.Hour
	defaultTouchEvery = time.Minute
	// maxSecretLen caps password/secret input length before the KDF runs
	// (memory-hard hashing of attacker-sized inputs is a DoS vector).
	maxSecretLen = 1024
)

// Authorizer is the Phase 3 Authenticator implementation: store-backed
// sessions + Argon2id-hashed passwords and bearer-key secrets, RBAC via
// Role.Allows, per-IP rate limiting, CSRF issuance/validation, key minting,
// and first-run wizard state.
//
// The Authenticator interface's VerifyBearerFrom and LoginService's Login
// both take a client-IP rate-limit key and enforce the 10-fails/60s limit
// per Contract 1 §1. The unlimited VerifyBearer (below) is intentionally
// off the interface — it exists only for tests and internal callers that
// already own rate limiting themselves; no production HTTP path should
// reach it (issue #35, 2026-08-25).
type Authorizer struct {
	st         store.Store
	hasher     Hasher
	limiter    *RateLimiter
	sessionTTL time.Duration
	touchEvery time.Duration
	now        func() time.Time

	touchMu   sync.Mutex
	lastTouch map[string]time.Time
}

var _ Authenticator = (*Authorizer)(nil)
var _ LoginService = (*Authorizer)(nil)
var _ StepUpVerifier = (*Authorizer)(nil)
var _ KeyManager = (*Authorizer)(nil)

// Option configures New.
type Option func(*Authorizer)

// WithHasher replaces the Argon2id hasher (tests inject fakes here).
func WithHasher(h Hasher) Option { return func(a *Authorizer) { a.hasher = h } }

// WithSessionTTL sets the fixed session lifetime (default 7 days).
func WithSessionTTL(d time.Duration) Option { return func(a *Authorizer) { a.sessionTTL = d } }

// WithRateLimiter replaces the default 10-fails/60s limiter.
func WithRateLimiter(r *RateLimiter) Option { return func(a *Authorizer) { a.limiter = r } }

// WithClock injects a time source (tests).
func WithClock(now func() time.Time) Option { return func(a *Authorizer) { a.now = now } }

// New returns an Authorizer over st.
func New(st store.Store, opts ...Option) *Authorizer {
	a := &Authorizer{
		st:         st,
		hasher:     NewHasher(),
		limiter:    NewRateLimiter(10, time.Minute),
		sessionTTL: defaultSessionTTL,
		touchEvery: defaultTouchEvery,
		now:        time.Now,
		lastTouch:  make(map[string]time.Time),
	}
	for _, opt := range opts {
		opt(a)
	}
	// An injected clock (tests) also drives the limiter's window, unless
	// the limiter came in with its own clock.
	if a.limiter.now == nil {
		a.limiter.now = a.now
	}
	return a
}

// ── Login / sessions ────────────────────────────────────────────────────────

// Login verifies username/password and creates a session. ip is the client
// IP used for rate limiting (callers resolve it via EffectiveRemoteAddr
// semantics upstream); remoteAddr/userAgent are recorded on the session.
// Returns ErrRateLimited or ErrUnauthenticated on failure.
func (a *Authorizer) Login(ctx context.Context, username, password, ip, userAgent string) (store.Session, Identity, error) {
	if a.limiter.TooMany(ip) {
		return store.Session{}, Identity{}, ErrRateLimited
	}
	if username == "" || password == "" || len(password) > maxSecretLen {
		a.limiter.Fail(ip)
		return store.Session{}, Identity{}, ErrUnauthenticated
	}
	u, err := a.st.Users().ByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		// Burn KDF-comparable time so a missing user is not distinguishable
		// from a wrong password by response timing.
		_, _ = a.hasher.Hash(password)
		a.limiter.Fail(ip)
		return store.Session{}, Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return store.Session{}, Identity{}, err
	}
	ok, err := a.hasher.Verify(u.PasswordHash, password)
	if err != nil || !ok || u.Disabled {
		a.limiter.Fail(ip)
		return store.Session{}, Identity{}, ErrUnauthenticated
	}

	now := a.now()
	sid, err := randToken(32)
	if err != nil {
		return store.Session{}, Identity{}, err
	}
	csrf, err := randToken(32)
	if err != nil {
		return store.Session{}, Identity{}, err
	}
	sess := store.Session{
		ID: sid, UserID: u.ID, CSRFToken: csrf,
		CreatedAt: now, ExpiresAt: now.Add(a.sessionTTL), LastSeenAt: now,
		RemoteAddr: ip, UserAgent: userAgent,
	}
	if err := a.st.Sessions().Create(ctx, sess); err != nil {
		return store.Session{}, Identity{}, err
	}
	return sess, Identity{Name: u.Username, Role: Role(u.Role)}, nil
}

// Logout deletes the session. Idempotent.
func (a *Authorizer) Logout(ctx context.Context, sessionID string) error {
	return a.st.Sessions().Delete(ctx, sessionID)
}

// VerifySession implements Authenticator.
func (a *Authorizer) VerifySession(sessionID string) (Identity, error) {
	id, _, err := a.sessionIdentity(context.Background(), sessionID)
	return id, err
}

// SessionInfo resolves a session to its Identity and CSRF token — what
// GET /api/v1/session needs for the PWA's auth bootstrap (Contract 1 §1:
// the SPA shell is static, so the CSRF token is fetched, not embedded).
func (a *Authorizer) SessionInfo(ctx context.Context, sessionID string) (Identity, string, error) {
	id, sess, err := a.sessionIdentity(ctx, sessionID)
	if err != nil {
		return Identity{}, "", err
	}
	return id, sess.CSRFToken, nil
}

// ValidateCSRF reports whether the given X-CSRF-Token value matches the
// session's token (constant-time). Required on every non-GET API call made
// with a session; bearer-token requests carry no CSRF exposure and skip it.
func (a *Authorizer) ValidateCSRF(ctx context.Context, sessionID, given string) bool {
	if given == "" {
		return false
	}
	_, sess, err := a.sessionIdentity(ctx, sessionID)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(sess.CSRFToken)) == 1
}

func (a *Authorizer) sessionIdentity(ctx context.Context, sessionID string) (Identity, store.Session, error) {
	if sessionID == "" {
		return Identity{}, store.Session{}, ErrUnauthenticated
	}
	sess, err := a.st.Sessions().Get(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, store.Session{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, store.Session{}, err
	}
	now := a.now()
	if !now.Before(sess.ExpiresAt) {
		_ = a.st.Sessions().Delete(ctx, sessionID)
		return Identity{}, store.Session{}, ErrUnauthenticated
	}
	u, err := a.userByID(ctx, sess.UserID)
	if err != nil || u.Disabled {
		return Identity{}, store.Session{}, ErrUnauthenticated
	}
	a.touch("s:"+sessionID, func() {
		_ = a.st.Sessions().Touch(ctx, sessionID, now)
	})
	assurance := Assurance(sess.Assurance)
	if assurance == "" {
		assurance = AssurancePassword
	}
	return Identity{
		Name:             u.Username,
		Role:             Role(u.Role),
		Assurance:        assurance,
		AssuranceAt:      sess.AssuranceAt,
		NetworkPrincipal: sess.NetworkPrincipal,
	}, sess, nil
}

// userByID scans the user list — the frozen Users interface has no ByID,
// and dashboard accounts number in the single digits.
func (a *Authorizer) userByID(ctx context.Context, id int64) (store.User, error) {
	users, err := a.st.Users().List(ctx)
	if err != nil {
		return store.User{}, err
	}
	for _, u := range users {
		if u.ID == id {
			return u, nil
		}
	}
	return store.User{}, store.ErrNotFound
}

// SweepExpiredSessions removes expired sessions; wiring calls it
// periodically (Phase 4/9 owns the ticker).
func (a *Authorizer) SweepExpiredSessions(ctx context.Context) (int64, error) {
	return a.st.Sessions().DeleteExpired(ctx, a.now())
}

// ── Bearer keys ─────────────────────────────────────────────────────────────

// VerifyBearer is full token verification with kind enforcement, one
// Argon2 verify per request (keyid routes to exactly one row), but NOT
// rate-limited. Test/internal use only — deliberately not part of the
// Authenticator interface (see the type doc above); production HTTP
// middleware must use VerifyBearerFrom. Passes ip="" — a key bound to a
// specific IP (#34) never verifies through this path; use VerifyBearerFrom
// with a real ip to exercise binding in tests.
func (a *Authorizer) VerifyBearer(token string, want KeyKind) (Identity, error) {
	return a.verifyBearer(context.Background(), "", token, want)
}

// VerifyBearerFrom is VerifyBearer with the contract's per-IP rate limit
// (failures counted, 10/60s shared with the login path). ip also feeds the
// key's optional host binding (#34): a key with a non-empty BoundIP only
// verifies when ip matches exactly.
func (a *Authorizer) VerifyBearerFrom(ctx context.Context, ip, token string, want KeyKind) (Identity, error) {
	if a.limiter.TooMany(ip) {
		return Identity{}, ErrRateLimited
	}
	id, err := a.verifyBearer(ctx, ip, token, want)
	if errors.Is(err, ErrUnauthenticated) {
		a.limiter.Fail(ip)
	}
	return id, err
}

func (a *Authorizer) verifyBearer(ctx context.Context, ip, token string, want KeyKind) (Identity, error) {
	kind, keyid, secret, err := ParseToken(token)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	if kind != want {
		// A router token must not open the dashboard (Contract 2).
		return Identity{}, ErrUnauthenticated
	}
	k, err := a.st.Keys().Get(ctx, keyid)
	if errors.Is(err, store.ErrNotFound) {
		return Identity{}, ErrUnauthenticated
	}
	if err != nil {
		return Identity{}, err
	}
	if !k.RevokedAt.IsZero() {
		return Identity{}, ErrUnauthenticated
	}
	if !k.ExpiresAt.IsZero() && !a.now().Before(k.ExpiresAt) {
		return Identity{}, ErrUnauthenticated
	}
	if k.BoundIP != "" && k.BoundIP != ip {
		return Identity{}, ErrUnauthenticated
	}
	ok, err := a.hasher.Verify(k.SecretHash, secret)
	if err != nil || !ok {
		return Identity{}, ErrUnauthenticated
	}
	a.touch("k:"+keyid, func() {
		_ = a.st.Keys().TouchUsed(ctx, keyid, a.now())
	})
	role := Role("")
	if kind == KindForge {
		role = Role(k.Role)
	}
	return Identity{Name: k.Name, Role: role, KeyID: keyid, Kind: kind, DisplayName: k.DisplayName}, nil
}

// MintKey creates a bearer key and returns the full token — shown exactly
// once, never stored or logged in plaintext. role is required for forge
// keys and must be empty otherwise. Any existing active key of the same
// kind+name is revoked first (V4 mint semantics: one key per consumer name).
// boundIP == "" mints an unbound key (verifies from any IP — the default for
// router/MCP keys); expiresAt.IsZero() mints a key that never expires
// (security sprint 3, #34/#36). Callers choose the defaults for their own
// surface — MintKey itself applies none.
func (a *Authorizer) MintKey(ctx context.Context, kind KeyKind, name, displayName string, role Role, boundIP string, expiresAt time.Time) (string, error) {
	switch kind {
	case KindForge:
		if !validRole(role) {
			return "", fmt.Errorf("authz: mint: forge key requires a valid role")
		}
	case KindRouter, KindMCP:
		if role != "" {
			return "", fmt.Errorf("authz: mint: %s keys carry no role", kind)
		}
	default:
		return "", fmt.Errorf("authz: mint: unknown key kind")
	}
	if name == "" {
		return "", fmt.Errorf("authz: mint: key name required")
	}

	existing, err := a.st.Keys().List(ctx, string(kind))
	if err != nil {
		return "", err
	}
	for _, k := range existing {
		if k.Name == name && k.RevokedAt.IsZero() {
			if err := a.st.Keys().Revoke(ctx, k.KeyID); err != nil {
				return "", err
			}
		}
	}

	keyidBytes := make([]byte, 6)
	if _, err := rand.Read(keyidBytes); err != nil {
		return "", fmt.Errorf("authz: mint: %w", err)
	}
	keyid := hex.EncodeToString(keyidBytes) // 12 lowercase hex — the frozen grammar
	secret, err := randToken(32)
	if err != nil {
		return "", err
	}
	hash, err := a.hasher.Hash(secret)
	if err != nil {
		return "", err
	}
	if err := a.st.Keys().Create(ctx, store.APIKey{
		KeyID: keyid, Kind: string(kind), Name: name, SecretHash: hash,
		Role: string(role), DisplayName: displayName, BoundIP: boundIP,
		CreatedAt: a.now(), ExpiresAt: expiresAt,
	}); err != nil {
		return "", err
	}
	return FormatToken(kind, keyid, secret), nil
}

// RevokeKey soft-revokes a key by keyid.
func (a *Authorizer) RevokeKey(ctx context.Context, keyid string) error {
	return a.st.Keys().Revoke(ctx, keyid)
}

// ── First-run wizard ────────────────────────────────────────────────────────

// wizardCompletedKey is the Contract 3 settings key.
const wizardCompletedKey = "wizard.completed"

// SetupRequired reports whether the first-run wizard must run: no users
// exist (V4 semantics — V4 users are deliberately not migrated).
func (a *Authorizer) SetupRequired(ctx context.Context) (bool, error) {
	users, err := a.st.Users().List(ctx)
	if err != nil {
		return false, err
	}
	return len(users) == 0, nil
}

// CompleteSetup creates the initial admin account and marks the wizard
// completed. Refuses to run once any user exists.
func (a *Authorizer) CompleteSetup(ctx context.Context, username, password string) error {
	required, err := a.SetupRequired(ctx)
	if err != nil {
		return err
	}
	if !required {
		return fmt.Errorf("authz: setup already completed")
	}
	if err := a.CreateUser(ctx, username, password, RoleAdmin); err != nil {
		return err
	}
	return a.st.Settings().Set(ctx, wizardCompletedKey, []byte("true"))
}

// CreateUser hashes the password and stores the account.
func (a *Authorizer) CreateUser(ctx context.Context, username, password string, role Role) error {
	if username == "" {
		return fmt.Errorf("authz: create user: username required")
	}
	if !validRole(role) {
		return fmt.Errorf("authz: create user: invalid role")
	}
	if len(password) < 8 || len(password) > maxSecretLen {
		return fmt.Errorf("authz: create user: password must be 8-%d characters", maxSecretLen)
	}
	hash, err := a.hasher.Hash(password)
	if err != nil {
		return err
	}
	_, err = a.st.Users().Create(ctx, store.User{
		Username: username, PasswordHash: hash, Role: string(role), CreatedAt: a.now(),
	})
	return err
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func validRole(r Role) bool {
	return r == RoleViewer || r == RoleOperator || r == RoleAdmin
}

// touch runs fn at most once per touchEvery per key — keeps last_seen /
// last_used fresh without a DB write on every request (the a0 hot path
// carries real OpenCode/LibreChat traffic).
func (a *Authorizer) touch(key string, fn func()) {
	now := a.now()
	a.touchMu.Lock()
	last, ok := a.lastTouch[key]
	if ok && now.Sub(last) < a.touchEvery {
		a.touchMu.Unlock()
		return
	}
	a.lastTouch[key] = now
	if len(a.lastTouch) > 4096 {
		for k, t := range a.lastTouch {
			if now.Sub(t) >= a.touchEvery {
				delete(a.lastTouch, k)
			}
		}
	}
	a.touchMu.Unlock()
	fn()
}

// randToken returns n random bytes as unpadded url-safe base64 (matches the
// token-secret charset in the frozen sk-* grammar; 32 bytes → 43 chars).
func randToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("authz: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
