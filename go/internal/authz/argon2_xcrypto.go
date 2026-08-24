// SPDX-License-Identifier: Apache-2.0

// The real Argon2id KDF (golang.org/x/crypto/argon2). The former xcrypto
// build-tag gate and its fail-closed stub were removed when the integrator
// added the dependency to go.mod (docs/v5-go-contracts.md: a stub must
// never ship enabled).

package authz

import "golang.org/x/crypto/argon2"

const kdfAvailable = true

func argon2Key(secret, salt []byte, p Argon2Params) ([]byte, error) {
	return argon2.IDKey(secret, salt, p.Time, p.MemoryKiB, p.Threads, p.KeyLen), nil
}
