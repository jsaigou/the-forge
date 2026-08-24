// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"strings"
	"testing"
)

func TestParsePHC(t *testing.T) {
	// Structurally valid argon2id PHC string (values from the argon2-cffi
	// output shape V4 produced).
	good := "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHRzb21lc2FsdA$RdescudvJCsgt3ub+b+dWRWJTmaaJObG"
	p, salt, key, err := parsePHC(good)
	if err != nil {
		t.Fatalf("parsePHC: %v", err)
	}
	if p.MemoryKiB != 65536 || p.Time != 3 || p.Threads != 4 {
		t.Errorf("params = %+v", p)
	}
	if len(salt) != 16 || int(p.SaltLen) != 16 {
		t.Errorf("salt len = %d", len(salt))
	}
	if int(p.KeyLen) != len(key) {
		t.Errorf("keylen %d != %d", p.KeyLen, len(key))
	}

	bad := []string{
		"",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA",  // wrong variant
		"$argon2id$v=18$m=65536,t=3,p=4$c2FsdA$aGFzaA", // wrong version
		"$argon2id$v=19$m=0,t=3,p=4$c2FsdA$aGFzaA",     // zero cost
		"$argon2id$v=19$m=65536,t=3,p=4$!!!$aGFzaA",    // bad salt b64
		"$argon2id$v=19$m=65536,t=3,p=4$c2FsdA",        // missing hash
		"plaintext",
	}
	for _, s := range bad {
		if _, _, _, err := parsePHC(s); err == nil {
			t.Errorf("parsePHC(%q) should fail", s)
		}
	}
}

// TestVerifyFailsClosedWithoutKDF: in default (no-xcrypto) builds the
// hasher must error — never silently pass — and the error must steer the
// integrator. In xcrypto builds this test is a no-op.
func TestVerifyFailsClosedWithoutKDF(t *testing.T) {
	if HasherAvailable() {
		t.Skip("real KDF compiled in")
	}
	h := NewHasher()
	if _, err := h.Hash("secret"); err == nil {
		t.Fatal("Hash must fail without the KDF")
	}
	ok, err := h.Verify("$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHRzb21lc2FsdA$RdescudvJCsgt3ub+b+dWRWJTmaaJObG", "secret")
	if err == nil || ok {
		t.Fatal("Verify must fail closed without the KDF")
	}
	if !strings.Contains(err.Error(), "golang.org/x/crypto") {
		t.Errorf("error should tell the integrator what to do: %v", err)
	}
}
