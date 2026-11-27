// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"testing"
	"time"
)

// Regression (was a bug, fixed): a stored tts.defaults whose value for a (known or unknown) kind is not a string makes every default-resolution fail
// (ParseSpeechDefaults returns an error for the whole object), although the doc comment promises an older daemon still serves the kinds it knows.
func TestAudit_DefaultsWithNonStringValueBreakDefaultResolution(t *testing.T) {
	if _, err := ParseSpeechDefaults([]byte(`{"base":"base-a","future_kind":{"engine":"x"}}`)); err != nil {
		t.Errorf("a newer file with a structured value for an unknown kind fails the whole read: %v", err)
	}
}

// An operator Unload of a leased engine must work, and the lease must not pin the next occupant of the slot.
func TestAudit_UnloadOfLeasedEngineWorksAndLeaseDoesNotPinNext(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.speech(t, "base-b", "base", true)
	e.health(8100, true)
	e.health(8101, true)
	l, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase, Engine: "base-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.Unload(ctx, l.Slot); err != nil {
		t.Fatalf("operator unload of a leased engine: %v", err)
	}
	if got := state(t, e, l.Slot); got.Leases != 0 || got.Occupant != "" {
		t.Errorf("after unload: %+v", got)
	}
	l2, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase, Engine: "base-b"})
	if err != nil {
		t.Fatal(err)
	}
	if l2.Slot == l.Slot {
		if got := state(t, e, l2.Slot).Leases; got != 1 {
			t.Errorf("leases on the reused slot = %d, want 1", got)
		}
	}
	e.advance(time.Second)
}

// A disabled-but-running leased engine: resolve for it again must fail as Disabled while the old lease is still releasable.
func TestAudit_ReleaseOfExpiredLease(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.health(8100, true)
	l, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase})
	if err != nil {
		t.Fatal(err)
	}
	e.advance(11 * time.Minute)
	before := state(t, e, l.Slot).LastUse
	got := e.m.ReleaseSpeech(l.ID)
	t.Logf("release of an expired-but-unreaped lease returned %v; lastUse moved %v", got, !state(t, e, l.Slot).LastUse.Equal(before))
}
