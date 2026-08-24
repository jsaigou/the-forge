// SPDX-License-Identifier: Apache-2.0

package authz

// totp.go — Sprint 0-AUTH §6: TOTP (RFC 6238) secret generation, code
// validation, and otpauth:// URI construction.
//
// Self-contained (no external dependency): HMAC-SHA1 from the standard
// library. The secret is stored as base32 (no padding) per the otpauth URI
// convention. §9 Q6: rely on the 0600 DB file for at-rest security (same
// posture as session IDs and provider API keys today).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

const (
	// totpPeriod is the TOTP time step in seconds (RFC 6238 §5.2, Google
	// Authenticator default).
	totpPeriod = 30
	// totpDigits is the code length (6 digits, RFC 4226 §5.3).
	totpDigits = 6
	// totpSecretBytes is the raw secret length (20 bytes = 160 bits, the
	// SHA-1 output size — RFC 4226 §4, recommended key length).
	totpSecretBytes = 20
)

// totpBase32 is the unpadded base32 encoding used for TOTP secrets and
// otpauth URIs (the standard Authenticator convention).
var totpBase32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTOTPSecret generates a random 20-byte secret as unpadded base32.
func GenerateTOTPSecret() (string, error) {
	raw := make([]byte, totpSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authz: totp: rand: %w", err)
	}
	return totpBase32.EncodeToString(raw), nil
}

// ValidateTOTP reports whether code is a valid TOTP for secret at time t,
// with a ±1 period window (to accommodate clock drift between the
// authenticator app and the server).
func ValidateTOTP(secret, code string, t time.Time) bool {
	if secret == "" || code == "" {
		return false
	}
	counter := uint64(t.Unix() / totpPeriod)
	// Check the current period and ±1 (±30s window).
	for _, delta := range []int64{-1, 0, 1} {
		c := counter
		if delta < 0 {
			c -= uint64(-delta)
		} else {
			c += uint64(delta)
		}
		if computeTOTP(secret, c) == code {
			return true
		}
	}
	return false
}

// ComputeTOTPCode returns the TOTP code for the given secret + time. Exported
// so tests (httpapi) can generate valid codes without duplicating the HOTP
// algorithm. Not used by production code (production only validates).
func ComputeTOTPCode(secret string, t time.Time) string {
	counter := uint64(t.Unix() / totpPeriod)
	return computeTOTP(secret, counter)
}

// computeTOTP returns the 6-digit code for the given secret + counter.
func computeTOTP(secret string, counter uint64) string {
	key, err := totpBase32.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	code := bin % mod
	return fmt.Sprintf("%0*d", totpDigits, code)
}

// TOTPOtpAuthURI builds the otpauth:// URI for QR-code enrollment (RFC 6238
// §6, used by Google Authenticator / Authy / etc.).
//
// otpauth://totp/<label>?secret=<secret>&issuer=<issuer>&digits=6&period=30
func TOTPOtpAuthURI(issuer, accountName, secret string) string {
	label := fmt.Sprintf("%s:%s", escapeOTPAuth(issuer), escapeOTPAuth(accountName))
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=%s&digits=%d&period=%d",
		label, secret, escapeOTPAuth(issuer), totpDigits, totpPeriod)
}

// escapeOTPAuth percent-encodes characters that are not allowed in a URI
// component (RFC 3986). For otpauth labels, colons and spaces are common.
func escapeOTPAuth(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == ':' || r == ' ' || r == '?' || r == '&' || r == '=' || r == '/' || r == '#' {
			b.WriteString(fmt.Sprintf("%%%02X", r))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
