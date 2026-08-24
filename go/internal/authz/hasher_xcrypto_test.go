// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"strings"
	"testing"
)

// Round-trip tests for the real Argon2id KDF. Run with:
//
//	go test -tags xcrypto ./internal/authz/
//
// (requires golang.org/x/crypto in go.mod — integrator-owned; verified
// against a dependency-added module copy until then.)

func TestArgon2RoundTrip(t *testing.T) {
	h := NewHasher()
	encoded, err := h.Hash("correct horse battery staple")
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Errorf("unexpected PHC prefix: %s", encoded[:32])
	}
	ok, err := h.Verify(encoded, "correct horse battery staple")
	if err != nil || !ok {
		t.Fatalf("Verify match: ok=%v err=%v", ok, err)
	}
	ok, err = h.Verify(encoded, "wrong password")
	if err != nil || ok {
		t.Fatalf("Verify mismatch: ok=%v err=%v", ok, err)
	}
}

func TestArgon2SaltsDiffer(t *testing.T) {
	h := NewHasher()
	a, err := h.Hash("same secret")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Hash("same secret")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("two hashes of the same secret must differ (random salt)")
	}
}

// TestArgon2VerifyUsesEncodedParams: verification derives with the params
// stored in the hash string, so cost upgrades don't invalidate old hashes.
func TestArgon2VerifyUsesEncodedParams(t *testing.T) {
	weak := &Argon2Hasher{Params: Argon2Params{Time: 1, MemoryKiB: 8, Threads: 1, KeyLen: 32, SaltLen: 16}}
	encoded, err := weak.Hash("s")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := NewHasher().Verify(encoded, "s")
	if err != nil || !ok {
		t.Fatalf("default hasher must verify weak-param hash: ok=%v err=%v", ok, err)
	}
}
