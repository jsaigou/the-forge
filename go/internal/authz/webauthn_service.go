// SPDX-License-Identifier: Apache-2.0

package authz

// webauthn_service.go — Sprint 0-AUTH Phase B (§7): WebAuthn/passkey service.
//
// Wraps the go-webauthn/webauthn library. Manages:
//   - RP config (RPID, RPDisplayName, RPOrigins) from settings + request origin.
//   - In-memory challenge session store (TTL 5 min) — the begin/finish dance
//     needs to remember the challenge between requests. Keyed by a random
//     token issued to the client via a cookie.
//   - User adapter: bridges store.User + store.WebAuthnCredential to the
//     webauthn.User interface.
//   - Credential conversion: store.WebAuthnCredential ↔ webauthn.Credential.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

// WebAuthnCredentialStore is the credential persistence surface (store.WebAuthnCredentials).
// Declared here so authz doesn't import store (the httpapi layer injects a
// concrete adapter, same pattern as PolicySettings).
type WebAuthnCredentialStore interface {
	Save(ctx context.Context, c WebAuthnCredentialRecord) error
	Get(ctx context.Context, id string) (WebAuthnCredentialRecord, error)
	ListByUser(ctx context.Context, userID int64) ([]WebAuthnCredentialRecord, error)
	Delete(ctx context.Context, id string) error
	UpdateSignCount(ctx context.Context, id string, signCount uint32, at time.Time) error
}

// WebAuthnCredentialRecord is the credential row (mirrors store.WebAuthnCredential).
// Declared here so authz doesn't import store.
type WebAuthnCredentialRecord struct {
	ID         string
	UserID     int64
	PublicKey  []byte
	SignCount  uint32
	Transports string
	Label      string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// WebAuthnUserStore is the user-account lookup surface (subset of store.Users).
type WebAuthnUserStore interface {
	ByUsername(ctx context.Context, username string) (WebAuthnUser, error)
}

// WebAuthnUser is the user-account row (mirrors store.User).
type WebAuthnUser struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	Disabled     bool
}

// WebAuthnService wraps the go-webauthn library with challenge-session
// management and store-backed credential persistence.
type WebAuthnService struct {
	creds WebAuthnCredentialStore
	users WebAuthnUserStore

	// challengeMu guards the in-memory challenge sessions.
	challengeMu   sync.Mutex
	challenges     map[string]*challengeEntry
	challengeTTL   time.Duration
	now            func() time.Time
}

// challengeEntry is one stored challenge session between begin and finish.
type challengeEntry struct {
	session  *webauthn.SessionData
	userID   int64
	expires  time.Time
}

const defaultChallengeTTL = 5 * time.Minute

// NewWebAuthnService returns a WebAuthnService backed by the given stores.
func NewWebAuthnService(creds WebAuthnCredentialStore, users WebAuthnUserStore) *WebAuthnService {
	return &WebAuthnService{
		creds:        creds,
		users:        users,
		challenges:   map[string]*challengeEntry{},
		challengeTTL: defaultChallengeTTL,
		now:          time.Now,
	}
}

// WithClock injects a time source (tests).
func (s *WebAuthnService) WithClock(now func() time.Time) *WebAuthnService {
	s.now = now
	return s
}

// ── RP config ───────────────────────────────────────────────────────────────

// RPConfig holds the Relying Party configuration for WebAuthn.
type RPConfig struct {
	RPID         string
	RPDisplayName string
	RPOrigins    []string
}

// NewWebAuthnInstance creates a go-webauthn WebAuthn instance from RP config.
func NewWebAuthnInstance(cfg RPConfig) (*webauthn.WebAuthn, error) {
	if cfg.RPID == "" {
		return nil, fmt.Errorf("authz: webauthn: RPID is required")
	}
	if len(cfg.RPOrigins) == 0 {
		return nil, fmt.Errorf("authz: webauthn: RPOrigins is required")
	}
	displayName := cfg.RPDisplayName
	if displayName == "" {
		displayName = "Forge"
	}
	return webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: displayName,
		RPOrigins:     cfg.RPOrigins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
		AttestationPreference: protocol.PreferNoAttestation,
	})
}

// ── User adapter ─────────────────────────────────────────────────────────────

// webauthnUser adapts WebAuthnUser + stored credentials to the webauthn.User
// interface.
type webauthnUser struct {
	user  WebAuthnUser
	creds []webauthn.Credential
}

func (u *webauthnUser) WebAuthnID() []byte {
	// Use the int64 user ID as a big-endian byte slice (stable, opaque).
	id := make([]byte, 8)
	for i := 0; i < 8; i++ {
		id[7-i] = byte(u.user.ID >> (i * 8))
	}
	return id
}

func (u *webauthnUser) WebAuthnName() string        { return u.user.Username }
func (u *webauthnUser) WebAuthnDisplayName() string { return u.user.Username }

func (u *webauthnUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// toWebAuthnCredential converts a store credential record to a webauthn.Credential.
func toWebAuthnCredential(c WebAuthnCredentialRecord) webauthn.Credential {
	var transports []protocol.AuthenticatorTransport
	if c.Transports != "" {
		var ts []string
		if json.Unmarshal([]byte(c.Transports), &ts) == nil {
			for _, t := range ts {
				transports = append(transports, protocol.AuthenticatorTransport(t))
			}
		}
	}
	return webauthn.Credential{
		ID:          []byte(c.ID),
		PublicKey:   c.PublicKey,
		AttestationType: "none",
		Transport:   transports,
		Authenticator: webauthn.Authenticator{
			SignCount: c.SignCount,
		},
	}
}

// resolveUser loads a user + their credentials for the WebAuthn library.
func (s *WebAuthnService) resolveUser(ctx context.Context, userID int64) (*webauthnUser, error) {
	// We need the username — the users store has ByUsername but not ByID.
	// The Authorizer has userByID (private); here we use a different approach:
	// the HTTP handler passes us the username from the session identity.
	// For registration, the handler already knows the user (authenticated).
	// This method is called with a pre-resolved user.
	return nil, fmt.Errorf("authz: webauthn: resolveUser not directly callable — use resolveUserByName")
}

// ResolveUserByName loads a user by username + their credentials.
func (s *WebAuthnService) ResolveUserByName(ctx context.Context, username string) (*webauthnUser, error) {
	u, err := s.users.ByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	if u.Disabled {
		return nil, ErrUnauthenticated
	}
	creds, err := s.creds.ListByUser(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: list credentials: %w", err)
	}
	var waCreds []webauthn.Credential
	for _, c := range creds {
		waCreds = append(waCreds, toWebAuthnCredential(c))
	}
	return &webauthnUser{user: u, creds: waCreds}, nil
}

// ── Challenge session store ──────────────────────────────────────────────────

// StoreChallenge stores a WebAuthn session (challenge) and returns a token
// the client uses to reference it in the finish step.
func (s *WebAuthnService) StoreChallenge(session *webauthn.SessionData, userID int64) (string, error) {
	token, err := RandomToken(32)
	if err != nil {
		return "", err
	}
	entry := &challengeEntry{
		session: session,
		userID:  userID,
		expires: s.now().Add(s.challengeTTL),
	}
	s.challengeMu.Lock()
	s.pruneLocked()
	s.challenges[token] = entry
	s.challengeMu.Unlock()
	return token, nil
}

// PopChallenge retrieves and removes a challenge session by token.
// Returns nil if the token is not found or expired.
func (s *WebAuthnService) PopChallenge(token string) (*webauthn.SessionData, int64, error) {
	s.challengeMu.Lock()
	defer s.challengeMu.Unlock()
	s.pruneLocked()
	entry, ok := s.challenges[token]
	if !ok {
		return nil, 0, fmt.Errorf("authz: webauthn: challenge not found or expired")
	}
	delete(s.challenges, token)
	return entry.session, entry.userID, nil
}

// pruneLocked removes expired challenge entries. Caller must hold challengeMu.
func (s *WebAuthnService) pruneLocked() {
	now := s.now()
	for token, entry := range s.challenges {
		if now.After(entry.expires) {
			delete(s.challenges, token)
		}
	}
}

// ── Registration ─────────────────────────────────────────────────────────────

// BeginRegistrationResult is the output of BeginRegistration.
type BeginRegistrationResult struct {
	Creation  *protocol.CredentialCreation
	ChallengeToken string
}

// BeginRegistration starts a registration ceremony for the given user.
func (s *WebAuthnService) BeginRegistration(ctx context.Context, wa *webauthn.WebAuthn, username string) (*BeginRegistrationResult, error) {
	user, err := s.ResolveUserByName(ctx, username)
	if err != nil {
		return nil, err
	}
	creation, session, err := wa.BeginRegistration(user)
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: begin registration: %w", err)
	}
	token, err := s.StoreChallenge(session, user.user.ID)
	if err != nil {
		return nil, err
	}
	return &BeginRegistrationResult{Creation: creation, ChallengeToken: token}, nil
}

// FinishRegistrationResult is the output of FinishRegistration.
type FinishRegistrationResult struct {
	CredentialID string
	PublicKey    []byte
	Transports   []string
	SignCount    uint32
}

// FinishRegistration completes a registration ceremony by verifying the
// client's response and storing the new credential.
func (s *WebAuthnService) FinishRegistration(ctx context.Context, wa *webauthn.WebAuthn, username, challengeToken, label string, body []byte) (*FinishRegistrationResult, error) {
	session, userID, err := s.PopChallenge(challengeToken)
	if err != nil {
		return nil, err
	}
	user, err := s.ResolveUserByName(ctx, username)
	if err != nil {
		return nil, err
	}
	if user.user.ID != userID {
		return nil, fmt.Errorf("authz: webauthn: user mismatch")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(bytesReader(body))
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: parse registration response: %w", err)
	}
	credential, err := wa.CreateCredential(user, *session, parsed)
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: create credential: %w", err)
	}
	// Store the credential.
	transportsJSON, _ := encodeTransportsStr(credential.Transport)
	credID := string(credential.ID)
	err = s.creds.Save(ctx, WebAuthnCredentialRecord{
		ID:         credID,
		UserID:     userID,
		PublicKey:  credential.PublicKey,
		SignCount:  credential.Authenticator.SignCount,
		Transports: transportsJSON,
		Label:      label,
		CreatedAt:  s.now(),
	})
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: save credential: %w", err)
	}
	return &FinishRegistrationResult{
		CredentialID: credID,
		PublicKey:    credential.PublicKey,
		Transports:   transportsFromProtocol(credential.Transport),
		SignCount:    credential.Authenticator.SignCount,
	}, nil
}

// ── Assertion (login / step-up) ─────────────────────────────────────────────

// BeginAssertionResult is the output of BeginAssertion.
type BeginAssertionResult struct {
	Assertion      *protocol.CredentialAssertion
	ChallengeToken string
}

// BeginAssertion starts an assertion ceremony for the given user (step-up).
// The user must be known (authenticated via session) — this is not a
// discoverable login flow (that's Phase C for public deployments).
func (s *WebAuthnService) BeginAssertion(ctx context.Context, wa *webauthn.WebAuthn, username string) (*BeginAssertionResult, error) {
	user, err := s.ResolveUserByName(ctx, username)
	if err != nil {
		return nil, err
	}
	assertion, session, err := wa.BeginLogin(user)
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: begin login: %w", err)
	}
	token, err := s.StoreChallenge(session, user.user.ID)
	if err != nil {
		return nil, err
	}
	return &BeginAssertionResult{Assertion: assertion, ChallengeToken: token}, nil
}

// FinishAssertionResult is the output of FinishAssertion.
type FinishAssertionResult struct {
	CredentialID string
	SignCount    uint32
	UserID       int64
}

// FinishAssertion completes an assertion ceremony by verifying the client's
// response. On success, the credential's sign count is updated in the store.
func (s *WebAuthnService) FinishAssertion(ctx context.Context, wa *webauthn.WebAuthn, username, challengeToken string, body []byte) (*FinishAssertionResult, error) {
	session, userID, err := s.PopChallenge(challengeToken)
	if err != nil {
		return nil, err
	}
	user, err := s.ResolveUserByName(ctx, username)
	if err != nil {
		return nil, err
	}
	if user.user.ID != userID {
		return nil, fmt.Errorf("authz: webauthn: user mismatch")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(bytesReader(body))
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: parse assertion response: %w", err)
	}
	credential, err := wa.ValidateLogin(user, *session, parsed)
	if err != nil {
		return nil, fmt.Errorf("authz: webauthn: validate login: %w", err)
	}
	credID := string(credential.ID)
	// Update the sign count + last_used_at.
	_ = s.creds.UpdateSignCount(ctx, credID, credential.Authenticator.SignCount, s.now())
	return &FinishAssertionResult{
		CredentialID: credID,
		SignCount:    credential.Authenticator.SignCount,
		UserID:       userID,
	}, nil
}

// ── Credential management ───────────────────────────────────────────────────

// ListCredentials returns all WebAuthn credentials for a user.
func (s *WebAuthnService) ListCredentials(ctx context.Context, userID int64) ([]WebAuthnCredentialRecord, error) {
	return s.creds.ListByUser(ctx, userID)
}

// DeleteCredential removes a credential by ID.
func (s *WebAuthnService) DeleteCredential(ctx context.Context, id string) error {
	return s.creds.Delete(ctx, id)
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func encodeTransportsStr(transports []protocol.AuthenticatorTransport) (string, error) {
	if len(transports) == 0 {
		return "", nil
	}
	ts := make([]string, len(transports))
	for i, t := range transports {
		ts[i] = string(t)
	}
	raw, err := json.Marshal(ts)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// bytesReader is a minimal io.Reader over a byte slice (avoids importing bytes).
type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, errEOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

var errEOF = fmt.Errorf("EOF")

func bytesReader(b []byte) *byteReader { return &byteReader{data: b} }

func transportsFromProtocol(ts []protocol.AuthenticatorTransport) []string {
	if len(ts) == 0 {
		return nil
	}
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = string(t)
	}
	return out
}
