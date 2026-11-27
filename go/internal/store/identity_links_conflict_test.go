// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"testing"
)

func TestIdentityLinkCreateDoesNotRepointAnExistingPrincipal(t *testing.T) {
	db := openTestDB(t)
	ctx := t.Context()
	links := db.IdentityLinks()
	newUser := func(name string) int64 {
		uid, err := db.Users().Create(ctx, User{Username: name, PasswordHash: "x", Role: "viewer"})
		if err != nil {
			t.Fatalf("create user %s: %v", name, err)
		}
		return uid
	}
	u1, u2 := newUser("alice"), newUser("bob")
	mk := func(uid int64) IdentityLink { return IdentityLink{Provider: "tailscale", Principal: "alice@example.com", UserID: uid} }
	if err := links.Create(ctx, mk(u1)); err != nil {
		t.Fatal(err)
	}
	if err := links.Create(ctx, mk(u1)); err != nil {
		t.Errorf("re-creating the same link must be a no-op, got %v", err)
	}
	if err := links.Create(ctx, mk(u2)); !errors.Is(err, ErrConflict) {
		t.Fatalf("pointing an existing principal at another user = %v, want ErrConflict", err)
	}
	if uid, err := links.Lookup(ctx, "tailscale", "alice@example.com"); err != nil || uid != u1 {
		t.Errorf("link now resolves to user %d (err %v), want %d", uid, err, u1)
	}
	if err := links.Delete(ctx, "tailscale", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := links.Create(ctx, mk(u2)); err != nil {
		t.Errorf("after an explicit delete the principal can be re-linked: %v", err)
	}
}
