// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// ── Fakes ───────────────────────────────────────────────────────────────────

type fakeCredStore struct {
	creds map[string]WebAuthnCredentialRecord
}

func newFakeCredStore() *fakeCredStore {
	return &fakeCredStore{creds: map[string]WebAuthnCredentialRecord{}}
}

func (f *fakeCredStore) Save(_ context.Context, c WebAuthnCredentialRecord) error {
	f.creds[c.ID] = c
	return nil
}
func (f *fakeCredStore) Get(_ context.Context, id string) (WebAuthnCredentialRecord, error) {
	c, ok := f.creds[id]
	if !ok {
		return WebAuthnCredentialRecord{}, errNotFoundFake
	}
	return c, nil
}
func (f *fakeCredStore) ListByUser(_ context.Context, userID int64) ([]WebAuthnCredentialRecord, error) {
	var out []WebAuthnCredentialRecord
	for _, c := range f.creds {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeCredStore) Delete(_ context.Context, id string) error {
	if _, ok := f.creds[id]; !ok {
		return errNotFoundFake
	}
	delete(f.creds, id)
	return nil
}
func (f *fakeCredStore) UpdateSignCount(_ context.Context, id string, signCount uint32, at time.Time) error {
	c, ok := f.creds[id]
	if !ok {
		return errNotFoundFake
	}
	c.SignCount = signCount
	c.LastUsedAt = at
	f.creds[id] = c
	return nil
}

type fakeUserStore struct {
	users map[string]WebAuthnUser
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{users: map[string]WebAuthnUser{}}
}

func (f *fakeUserStore) ByUsername(_ context.Context, username string) (WebAuthnUser, error) {
	u, ok := f.users[username]
	if !ok {
		return WebAuthnUser{}, errNotFoundFake
	}
	return u, nil
}

// ── Tests ───────────────────────────────────────────────────────────────────

func TestWebAuthnChallengeStore(t *testing.T) {
	svc := NewWebAuthnService(newFakeCredStore(), newFakeUserStore())

	// Store a challenge (use a non-nil session).
	session := &webauthn.SessionData{Challenge: "test-challenge"}
	token, err := svc.StoreChallenge(session, 42)
	if err != nil {
		t.Fatalf("StoreChallenge: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}

	// Pop it.
	gotSession, userID, err := svc.PopChallenge(token)
	if err != nil {
		t.Fatalf("PopChallenge: %v", err)
	}
	if gotSession == nil {
		t.Fatal("session is nil")
	}
	if gotSession.Challenge != "test-challenge" {
		t.Errorf("challenge = %q, want test-challenge", gotSession.Challenge)
	}
	if userID != 42 {
		t.Errorf("userID = %d, want 42", userID)
	}

	// Popping again should fail (consumed).
	_, _, err = svc.PopChallenge(token)
	if err == nil {
		t.Error("PopChallenge on consumed token should fail")
	}
}

func TestWebAuthnChallengeExpiry(t *testing.T) {
	svc := NewWebAuthnService(newFakeCredStore(), newFakeUserStore())
	// Inject a past time for storage, then a future time for pop.
	now := time.Now()
	svc.now = func() time.Time { return now }

	token, _ := svc.StoreChallenge(nil, 1)

	// Advance time past the TTL.
	svc.now = func() time.Time { return now.Add(10 * time.Minute) }

	_, _, err := svc.PopChallenge(token)
	if err == nil {
		t.Error("PopChallenge on expired token should fail")
	}
}

func TestNewWebAuthnInstance(t *testing.T) {
	// Valid config.
	wa, err := NewWebAuthnInstance(RPConfig{
		RPID:       "example-tailnet.ts.net",
		RPOrigins:  []string{"https://ops.example.ts.net"},
	})
	if err != nil {
		t.Fatalf("NewWebAuthnInstance: %v", err)
	}
	if wa == nil {
		t.Fatal("WebAuthn instance is nil")
	}

	// Missing RPID.
	_, err = NewWebAuthnInstance(RPConfig{
		RPOrigins: []string{"https://ops.example.ts.net"},
	})
	if err == nil {
		t.Error("NewWebAuthnInstance without RPID should fail")
	}

	// Missing RPOrigins.
	_, err = NewWebAuthnInstance(RPConfig{
		RPID: "example-tailnet.ts.net",
	})
	if err == nil {
		t.Error("NewWebAuthnInstance without RPOrigins should fail")
	}

	// Default display name.
	wa, _ = NewWebAuthnInstance(RPConfig{
		RPID:      "example.com",
		RPOrigins: []string{"https://example.com"},
	})
	if wa.Config.RPDisplayName != "Forge" {
		t.Errorf("RPDisplayName = %q, want Forge (default)", wa.Config.RPDisplayName)
	}
}

func TestWebAuthnResolveUserByName(t *testing.T) {
	credStore := newFakeCredStore()
	userStore := newFakeUserStore()
	userStore.users["testuser"] = WebAuthnUser{
		ID: 1, Username: "testuser", Role: "admin", CreatedAt: time.Now(),
	}
	credStore.creds["cred-1"] = WebAuthnCredentialRecord{
		ID: "cred-1", UserID: 1, PublicKey: []byte{0x04}, SignCount: 5,
		Transports: `["usb"]`,
	}
	svc := NewWebAuthnService(credStore, userStore)

	user, err := svc.ResolveUserByName(context.Background(), "testuser")
	if err != nil {
		t.Fatalf("ResolveUserByName: %v", err)
	}
	if user.user.ID != 1 {
		t.Errorf("user ID = %d, want 1", user.user.ID)
	}
	if len(user.creds) != 1 {
		t.Fatalf("creds = %d, want 1", len(user.creds))
	}
	if user.creds[0].Authenticator.SignCount != 5 {
		t.Errorf("sign count = %d, want 5", user.creds[0].Authenticator.SignCount)
	}
}

func TestWebAuthnResolveUserByNameNotFound(t *testing.T) {
	svc := NewWebAuthnService(newFakeCredStore(), newFakeUserStore())
	_, err := svc.ResolveUserByName(context.Background(), "nobody")
	if err == nil {
		t.Error("ResolveUserByName on nonexistent user should fail")
	}
}

func TestWebAuthnResolveUserDisabled(t *testing.T) {
	userStore := newFakeUserStore()
	userStore.users["disabled"] = WebAuthnUser{
		ID: 1, Username: "disabled", Disabled: true, CreatedAt: time.Now(),
	}
	svc := NewWebAuthnService(newFakeCredStore(), userStore)
	_, err := svc.ResolveUserByName(context.Background(), "disabled")
	if err != ErrUnauthenticated {
		t.Errorf("ResolveUserByName on disabled user = %v, want ErrUnauthenticated", err)
	}
}

func TestWebAuthnListCredentials(t *testing.T) {
	credStore := newFakeCredStore()
	credStore.creds["c1"] = WebAuthnCredentialRecord{ID: "c1", UserID: 1, Label: "Key1"}
	credStore.creds["c2"] = WebAuthnCredentialRecord{ID: "c2", UserID: 1, Label: "Key2"}
	credStore.creds["c3"] = WebAuthnCredentialRecord{ID: "c3", UserID: 2, Label: "Other"}

	svc := NewWebAuthnService(credStore, newFakeUserStore())
	creds, err := svc.ListCredentials(context.Background(), 1)
	if err != nil {
		t.Fatalf("ListCredentials: %v", err)
	}
	if len(creds) != 2 {
		t.Errorf("ListCredentials = %d, want 2", len(creds))
	}
}

func TestWebAuthnDeleteCredential(t *testing.T) {
	credStore := newFakeCredStore()
	credStore.creds["c1"] = WebAuthnCredentialRecord{ID: "c1", UserID: 1}

	svc := NewWebAuthnService(credStore, newFakeUserStore())
	if err := svc.DeleteCredential(context.Background(), "c1"); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if err := svc.DeleteCredential(context.Background(), "c1"); err == nil {
		t.Error("DeleteCredential on nonexistent should fail")
	}
}

// ── User adapter ─────────────────────────────────────────────────────────────

func TestWebAuthnUserIDEncoding(t *testing.T) {
	u := &webauthnUser{user: WebAuthnUser{ID: 1}}
	id := u.WebAuthnID()
	if len(id) != 8 {
		t.Fatalf("WebAuthnID len = %d, want 8", len(id))
	}
	// ID 1 → big-endian → last byte is 1.
	if id[7] != 1 {
		t.Errorf("WebAuthnID[7] = %d, want 1", id[7])
	}

	// Round-trip a larger ID.
	u2 := &webauthnUser{user: WebAuthnUser{ID: 0x0102030405060708}}
	id2 := u2.WebAuthnID()
	var decoded int64
	for i := 0; i < 8; i++ {
		decoded = (decoded << 8) | int64(id2[i])
	}
	if decoded != 0x0102030405060708 {
		t.Errorf("round-trip = 0x%x, want 0x0102030405060708", decoded)
	}
}

func TestToWebAuthnCredential(t *testing.T) {
	c := WebAuthnCredentialRecord{
		ID:         "cred-id",
		PublicKey:  []byte{0x04, 0x05},
		SignCount:  10,
		Transports: `["usb","nfc"]`,
	}
	wa := toWebAuthnCredential(c)
	if string(wa.ID) != "cred-id" {
		t.Errorf("ID = %q, want cred-id", wa.ID)
	}
	if wa.Authenticator.SignCount != 10 {
		t.Errorf("SignCount = %d, want 10", wa.Authenticator.SignCount)
	}
	if len(wa.Transport) != 2 {
		t.Errorf("Transport len = %d, want 2", len(wa.Transport))
	}
}
