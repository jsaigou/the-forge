// SPDX-License-Identifier: Apache-2.0

package authz

// assurance.go — Sprint 0-AUTH §3.1: assurance levels.
//
// A session carries the max factor it has satisfied. Policy expresses "resource
// R needs at least level L, satisfied by any factor at/above L." The ordering
// is: network (L0) < password/totp (L1) < passkey (L2). Password and TOTP are
// both L1 alternatives; passkey is the L2 strong option (Phase B).

// Assurance is the authentication strength a session has proven.
type Assurance string

const (
	// AssuranceNetwork is L0: the request arrived as a trusted, identified
	// network principal (tailnet WhoIs, forward-auth header). A
	// network-bootstrapped session starts here.
	AssuranceNetwork Assurance = "network"
	// AssurancePassword is L1: a knowledge factor (Argon2id password).
	AssurancePassword Assurance = "password"
	// AssuranceTOTP is L1: an authenticator-app 2FA code (RFC 6238).
	// Same tier as password, different factor.
	AssuranceTOTP Assurance = "totp"
	// AssurancePasskey is L2: phishing-resistant WebAuthn (Phase B).
	AssurancePasskey Assurance = "passkey"
)

// assuranceRank orders levels for policy evaluation. Password and TOTP are
// both rank 1 (L1 alternatives); a policy may require specifically `passkey`
// (L2). A session that has proven password also satisfies a TOTP requirement
// and vice versa, since both are L1.
var assuranceRank = map[Assurance]int{
	AssuranceNetwork: 0,
	AssurancePassword: 1,
	AssuranceTOTP:     1,
	AssurancePasskey:  2,
}

// AtLeast reports whether the session's assurance satisfies the policy's
// minimum factor. Both password and TOTP satisfy any L1 requirement; only
// passkey satisfies an L2 requirement.
func (a Assurance) AtLeast(min Assurance) bool {
	got, ok1 := assuranceRank[a]
	want, ok2 := assuranceRank[min]
	if !ok1 || !ok2 {
		return false
	}
	return got >= want
}

// ValidAssurance reports whether s is a known assurance level.
func ValidAssurance(s string) bool {
	_, ok := assuranceRank[Assurance(s)]
	return ok
}
