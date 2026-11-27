// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// P5 (docs/speech-routing-plan.md decision 7): a0's viaPool and pooled catalog configs take a lease through Resolve and ResolveConfig, exactly as speech
// does, so an in-flight request of ANY function keeps its on-demand member from being evicted.

// A request resolved for a non-speech function (transcription) holds its member: the pool will not evict it for a newcomer.
func TestResolveLease_ProtectsANonSpeechMemberFromEviction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Hour
	for _, n := range []string{"asr-a", "asr-b", "asr-c"} {
		e.add(t, n, store.PolicyOnDemand, "", 1<<30)
	}
	e.speech(t, "design-d", "voicedesign", true)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	var targets []Target
	for _, n := range []string{"asr-a", "asr-b", "asr-c"} {
		tg, err := e.m.Resolve(ctx, store.FnTranscription, n)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Release == nil || tg.Renew == nil {
			t.Fatalf("Resolve(%s) gave a target without a lease", n)
		}
		targets = append(targets, tg)
		e.advance(time.Second)
	}
	e.advance(10 * time.Minute) // all idle for long, asr-a the least recently used; the pool is full
	targets[1].Finish()
	targets[2].Finish()
	e.advance(181 * time.Second) // a release restarts the idle clock

	got, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign})
	if err != nil {
		t.Fatalf("placing a fourth engine: %v", err)
	}
	if got.Slot == targets[0].Slot {
		t.Fatalf("asr-a, with a request in flight, was evicted for %s", got.Engine)
	}
	if st := state(t, e, targets[0].Slot); st.Occupant != "asr-a" || st.State != StateRunning {
		t.Errorf("the leased member's slot is %+v, want asr-a running", st)
	}
}

// Without leases an in-flight request was invisible: this is the gap the lease closes. A pool full of leased members refuses with PoolFullError.
func TestResolveLease_FullPoolWhenEveryMemberIsInUse(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Hour
	for _, n := range []string{"asr-a", "asr-b", "asr-c"} {
		e.add(t, n, store.PolicyOnDemand, "", 1<<30)
	}
	e.speech(t, "design-d", "voicedesign", true)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	var targets []Target
	for _, n := range []string{"asr-a", "asr-b", "asr-c"} {
		tg, err := e.m.Resolve(ctx, store.FnTranscription, n)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, tg)
	}
	e.advance(10 * time.Minute)
	var full *PoolFullError
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign}); !errors.As(err, &full) {
		t.Fatalf("every member in use: %v, want PoolFullError", err)
	}
	targets[0].Finish()
	e.advance(181 * time.Second)
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign}); err != nil {
		t.Fatalf("after one request finished: %v", err)
	}
}

func TestResolveLease_ReleaseIsIdempotentAndRestartsTheIdleClock(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "asr-a", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	a, err := e.m.Resolve(ctx, store.FnTranscription, "asr-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.m.Resolve(ctx, store.FnTranscription, "asr-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := state(t, e, "s1").Leases; got != 2 {
		t.Fatalf("leases = %d, want 2", got)
	}
	e.advance(90 * time.Second)
	a.Release()
	a.Release() // harmless the second time: it must not drop b's lease
	if got := state(t, e, "s1"); got.Leases != 1 || !got.LastUse.Equal(e.clock()) {
		t.Errorf("after one release: leases=%d lastUse=%v, want 1 and now", got.Leases, got.LastUse)
	}
	b.Finish()
	if got := state(t, e, "s1").Leases; got != 0 {
		t.Errorf("leases = %d after both finished, want 0", got)
	}
}

// Finish on a Target that was not made by a Manager (a test fake, or a pool-less path) falls back to the idle-clock touch.
func TestTarget_FinishWithoutALeaseTouches(t *testing.T) {
	touched := 0
	Target{Touch: func() { touched++ }}.Finish()
	if touched != 1 {
		t.Errorf("touched %d times, want 1", touched)
	}
	Target{}.Finish() // nothing to do, must not panic
}

// A lease belongs to one run of a member: after a stop and start the old Release is harmless and the old lease does not pin the new run.
func TestResolveLease_DoesNotSurviveAReloadOfTheMember(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.add(t, "asr-a", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	old, err := e.m.Resolve(ctx, store.FnTranscription, "asr-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.Unload(ctx, old.Slot); err != nil {
		t.Fatal(err)
	}
	e.advance(5 * time.Second)
	again, err := e.m.Resolve(ctx, store.FnTranscription, "asr-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := state(t, e, "s1").Leases; got != 1 {
		t.Errorf("leases after a reload = %d, want only the new run's 1", got)
	}
	old.Release() // the previous run's lease: must neither error nor drop the new run's lease
	if got := state(t, e, "s1").Leases; got != 1 {
		t.Errorf("releasing the old run's lease changed the new run's leases to %d", got)
	}
	if old.Renew() {
		t.Error("a lease from the previous run could still be renewed")
	}
	again.Release()
}

func TestResolveLease_RenewKeepsALongRequestProtectedUntilItIsReleased(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Minute
	e.add(t, "asr-a", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	tg, err := e.m.Resolve(ctx, store.FnTranscription, "asr-a")
	if err != nil {
		t.Fatal(err)
	}
	e.advance(50 * time.Second)
	if !tg.Renew() {
		t.Fatal("renewing a live lease reported false")
	}
	e.advance(50 * time.Second) // 100 s in: past the original minute, inside the renewed one
	if got := state(t, e, "s1").Leases; got != 1 {
		t.Errorf("a renewed lease stopped protecting the member: leases = %d", got)
	}
	e.advance(61 * time.Second) // not renewed again: it lapses on its own
	if got := state(t, e, "s1").Leases; got != 0 {
		t.Errorf("an expired lease still counts: leases = %d", got)
	}
	if tg.Renew() {
		t.Error("renewing an expired lease reported true")
	}
}

// A pooled catalog config is leased too (the router's chain finishes it when the proxied request ends).
func TestResolveConfig_TakesAndReleasesALease(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.cat.specs["d1-2b"] = d1Spec()
	e.cat.est["d1-2b"] = 7 << 30
	e.addCatalog(t, "d1-2b", "d1-2b", store.PolicyOnDemand, "")
	e.health(8100, true)
	tg, err := e.m.ResolveConfig(ctx, "d1-2b")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Release == nil || state(t, e, tg.Slot).Leases != 1 {
		t.Fatalf("pooled config target %+v: leases = %d, want a lease", tg, state(t, e, tg.Slot).Leases)
	}
	tg.Finish()
	if got := state(t, e, tg.Slot).Leases; got != 0 {
		t.Errorf("leases after Finish = %d, want 0", got)
	}
}

// With the lease table full a non-speech request is turned away BEFORE it starts a member.
func TestResolveLease_TableFullRefusesBeforeStartingAMember(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Hour
	e.add(t, "asr-a", store.PolicyOnDemand, "", 1<<30)
	e.add(t, "asr-b", store.PolicyOnDemand, "", 1<<30)
	e.health(8100, true)
	e.health(8101, true)
	for i := 0; i < maxLeases; i++ {
		if _, err := e.m.Resolve(ctx, store.FnTranscription, "asr-a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.m.Resolve(ctx, store.FnTranscription, "asr-b"); !errors.Is(err, ErrTooManyLeases) {
		t.Fatalf("with the table full: %v, want ErrTooManyLeases", err)
	}
	for _, s := range e.m.Snapshot(ctx) {
		if s.Occupant == "asr-b" {
			t.Errorf("the refused request started asr-b in %s", s.Slot)
		}
	}
}
