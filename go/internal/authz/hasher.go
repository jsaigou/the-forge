// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// Hasher hashes and verifies password/key secrets. The production
// implementation is Argon2id (hard requirement, docs/v5-plan.md design
// decision 4); tests may inject fakes via WithHasher. Implementations never
// include the secret or the hash in error messages.
type Hasher interface {
	Hash(secret string) (string, error)
	// Verify reports whether secret matches the encoded hash. A malformed
	// encoded hash is an error; a clean mismatch is (false, nil).
	Verify(encoded, secret string) (bool, error)
}

// Argon2Params are the Argon2id cost parameters. Defaults match V4's
// argon2-cffi defaults (t=3, m=64 MiB, p=4, 32-byte key, 16-byte salt) —
// also RFC 9106's second recommended option.
type Argon2Params struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
	KeyLen    uint32
	SaltLen   uint32
}

// DefaultArgon2Params returns the frozen default cost parameters.
func DefaultArgon2Params() Argon2Params {
	return Argon2Params{Time: 3, MemoryKiB: 64 * 1024, Threads: 4, KeyLen: 32, SaltLen: 16}
}

// Argon2Hasher is the Argon2id Hasher, encoding hashes in PHC string format
// ($argon2id$v=19$m=...,t=...,p=...$salt$hash, unpadded std base64) —
// interoperable with V4's argon2-cffi output, though all V4 hashes are
// re-created anyway (users via first-run wizard, keys re-minted).
type Argon2Hasher struct {
	Params Argon2Params
}

// NewHasher returns an Argon2id hasher with default parameters.
func NewHasher() *Argon2Hasher {
	return &Argon2Hasher{Params: DefaultArgon2Params()}
}

// HasherAvailable reports whether the Argon2id KDF is compiled in. False
// only in builds without the xcrypto tag (see argon2_stub.go), where every
// Hash/Verify fails closed.
func HasherAvailable() bool { return kdfAvailable }

var b64 = base64.RawStdEncoding

func (h *Argon2Hasher) Hash(secret string) (string, error) {
	p := h.Params
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authz: salt: %w", err)
	}
	key, err := argon2Key([]byte(secret), salt, p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		p.MemoryKiB, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func (h *Argon2Hasher) Verify(encoded, secret string) (bool, error) {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	got, err := argon2Key([]byte(secret), salt, p)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// errBadHash deliberately carries no fragment of the offending string.
var errBadHash = errors.New("authz: malformed argon2id hash")

// parsePHC parses a $argon2id$v=19$m=..,t=..,p=..$salt$hash string.
func parsePHC(encoded string) (Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		return Argon2Params{}, nil, nil, errBadHash
	}
	var m, t uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &threads); err != nil {
		return Argon2Params{}, nil, nil, errBadHash
	}
	if m == 0 || t == 0 || threads == 0 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil || len(key) == 0 {
		return Argon2Params{}, nil, nil, errBadHash
	}
	p := Argon2Params{Time: t, MemoryKiB: m, Threads: threads,
		KeyLen: uint32(len(key)), SaltLen: uint32(len(salt))}
	return p, salt, key, nil
}
