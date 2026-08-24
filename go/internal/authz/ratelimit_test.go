// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"testing"
	"time"
)

func TestRateLimiter(t *testing.T) {
	now := time.Unix(1000, 0)
	r := NewRateLimiter(10, time.Minute)
	r.now = func() time.Time { return now }

	if r.TooMany("1.2.3.4") {
		t.Fatal("fresh IP must not be limited")
	}
	for i := 0; i < 9; i++ {
		r.Fail("1.2.3.4")
	}
	if r.TooMany("1.2.3.4") {
		t.Fatal("9 failures must not trip the 10-limit")
	}
	r.Fail("1.2.3.4")
	if !r.TooMany("1.2.3.4") {
		t.Fatal("10 failures must trip the limit")
	}
	// Other IPs are independent.
	if r.TooMany("5.6.7.8") {
		t.Fatal("other IP must be unaffected")
	}
	// Window expiry resets the count.
	now = now.Add(61 * time.Second)
	if r.TooMany("1.2.3.4") {
		t.Fatal("expired window must reset the limit")
	}
	r.Fail("1.2.3.4")
	if r.TooMany("1.2.3.4") {
		t.Fatal("one failure in the new window must not trip")
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	r := NewRateLimiter(10, time.Minute)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 100; j++ {
				r.Fail("ip")
				r.TooMany("ip")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	if !r.TooMany("ip") {
		t.Error("800 failures must trip the limit")
	}
}
