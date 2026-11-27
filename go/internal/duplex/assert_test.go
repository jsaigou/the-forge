// SPDX-License-Identifier: Apache-2.0

package duplex

import (
	"errors"
	"math"
	"testing"
)

// requireCode asserts err is a typed *Error with the documented code and no
// panic or bare string error slipped through.
func requireCode(t *testing.T, err error, want ErrorCode) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %s", want)
	}
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("error %v (%T) is not a *duplex.Error, want %s", err, err, want)
	}
	if de.Code != want {
		t.Fatalf("error code %q, want %q (%s)", de.Code, want, de.Message)
	}
	return de
}

// rampFrame returns a frame whose samples are all distinct, so a shifted or
// partially copied frame cannot compare equal.
func rampFrame(base float32) Frame {
	f := make(Frame, FrameSamples)
	for i := range f {
		f[i] = base + float32(i)/float32(FrameSamples)
	}
	return f
}

// requireSameBits compares frames as float32 bit patterns, not with ==, so a
// NaN payload or the sign of zero cannot pass a "bit-exact" test by accident.
func requireSameBits(t *testing.T, got, want Frame) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("frame length %d, want %d", len(got), len(want))
	}
	for i := range want {
		if gb, wb := math.Float32bits(got[i]), math.Float32bits(want[i]); gb != wb {
			t.Fatalf("sample %d: got %#08x (%v), want %#08x (%v)", i, gb, got[i], wb, want[i])
		}
	}
}
