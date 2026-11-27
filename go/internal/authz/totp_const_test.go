// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"testing"
	"time"
)

func TestValidateTOTPAcceptsCurrentAndRejectsLengthMismatch(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	code := ComputeTOTPCode(secret, now)
	if !ValidateTOTP(secret, code, now) {
		t.Fatal("the current code must validate")
	}
	for _, bad := range []string{code[:len(code)-1], code + "0", "000000x", " " + code} {
		if ValidateTOTP(secret, bad, now) {
			t.Errorf("%q must not validate", bad)
		}
	}
}
