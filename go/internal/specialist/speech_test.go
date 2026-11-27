// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// speech registers a speech engine of the given Qwen mode (alias) as an on-demand member.
func (e *env) speech(t *testing.T, name, alias string, enabled bool) {
	t.Helper()
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: name, Kind: "qwen-tts-server", Policy: store.PolicyOnDemand, Enabled: enabled,
		Functions: []string{store.FnSpeech}, MemoryBytes: 1 << 30,
		Params: map[string]string{"model": "/opt/forge/models/" + name + ".gguf", "codec": "/opt/forge/models/codec.gguf", "alias": alias}}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) defaults(m map[string]string) {
	e.m.d.SpeechDefaults = func(context.Context) (map[string]string, error) { return m, nil }
}

func TestSpeechModeOf(t *testing.T) {
	for name, tc := range map[string]struct {
		o    store.Occupant
		mode string
		ok   bool
	}{
		"qwen base":        {store.Occupant{Kind: "qwen-tts-server", Functions: []string{"speech"}, Params: map[string]string{"alias": "base"}}, SpeechBase, true},
		"qwen custom":      {store.Occupant{Kind: "qwen-tts-server", Functions: []string{"speech"}, Params: map[string]string{"alias": "customvoice"}}, SpeechCustomVoice, true},
		"qwen design":      {store.Occupant{Kind: "qwen-tts-server", Functions: []string{"speech"}, Params: map[string]string{"alias": "voicedesign"}}, SpeechVoiceDesign, true},
		"kokoro":           {store.Occupant{Kind: "kokoro", Functions: []string{"speech"}}, SpeechKokoro, true},
		"zonos clone":      {store.Occupant{Kind: KindZonos3Server, Functions: []string{"speech"}}, SpeechBase, true}, // no alias: the model only clones
		"zonos not speech": {store.Occupant{Kind: KindZonos3Server, Functions: []string{"transcription"}}, "", false},
		"alias unknown":    {store.Occupant{Kind: "qwen-tts-server", Functions: []string{"speech"}, Params: map[string]string{"alias": "other"}}, "", false},
		"no alias":         {store.Occupant{Kind: "qwen-tts-server", Functions: []string{"speech"}}, "", false},
		"not speech":       {store.Occupant{Kind: "qwen-tts-server", Functions: []string{"transcription"}, Params: map[string]string{"alias": "base"}}, "", false},
		"other kind":       {store.Occupant{Kind: "llama-server", Functions: []string{"speech"}, Params: map[string]string{"alias": "base"}}, "", false},
	} {
		if mode, ok := SpeechModeOf(tc.o); mode != tc.mode || ok != tc.ok {
			t.Errorf("%s: SpeechModeOf = %q, %v; want %q, %v", name, mode, ok, tc.mode, tc.ok)
		}
	}
}

// The order of the plan (4.2): a named engine is used as it is and never swapped; otherwise the kind's default; otherwise any enabled engine of the kind.
func TestResolveSpeech_Order(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.speech(t, "base-b", "base", true)
	e.speech(t, "base-off", "base", false)
	e.speech(t, "design-a", "voicedesign", true)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	e.defaults(map[string]string{"base": "base-b"})

	get := func(req SpeechRequest) (string, error) {
		l, err := e.m.ResolveSpeech(ctx, req)
		if err == nil {
			e.m.ReleaseSpeech(l.ID)
		}
		return l.Engine, err
	}
	if got, err := get(SpeechRequest{Mode: SpeechBase, Engine: "base-a"}); err != nil || got != "base-a" {
		t.Errorf("a named engine: %q, %v, want base-a", got, err)
	}
	if got, err := get(SpeechRequest{Mode: SpeechBase}); err != nil || got != "base-b" {
		t.Errorf("the default for the kind: %q, %v, want base-b", got, err)
	}
	// The default is disabled: another enabled engine of the kind, in name order.
	e.defaults(map[string]string{"base": "base-off"})
	if got, err := get(SpeechRequest{Mode: SpeechBase}); err != nil || got != "base-a" {
		t.Errorf("default disabled: %q, %v, want base-a (first enabled by name)", got, err)
	}
	// No default set at all: the same.
	e.defaults(nil)
	if got, err := get(SpeechRequest{Mode: SpeechBase}); err != nil || got != "base-a" {
		t.Errorf("no default: %q, %v, want base-a", got, err)
	}

	// A named engine that is disabled is NOT swapped for another: another engine would render a different voice.
	var un *UnavailableError
	if _, err := get(SpeechRequest{Mode: SpeechBase, Engine: "base-off"}); !errors.As(err, &un) || un.Reason != ReasonDisabled || un.Occupant != "base-off" {
		t.Errorf("a named disabled engine: %v, want UnavailableError(disabled) naming base-off", err)
	}
	// Hard answers.
	var no *NoSpeechEngineError
	for name, req := range map[string]SpeechRequest{
		"unknown engine":    {Mode: SpeechBase, Engine: "ghost"},
		"wrong kind":        {Mode: SpeechBase, Engine: "design-a"},
		"no engine of kind": {Mode: SpeechCustomVoice},
		"unknown kind":      {Mode: "opera"},
	} {
		if _, err := get(req); !errors.As(err, &no) {
			t.Errorf("%s: %v, want NoSpeechEngineError", name, err)
		}
	}
	// Every engine of the kind is Disabled: that is the operator's choice, said by name.
	e.speech(t, "custom-off", "customvoice", false)
	if _, err := get(SpeechRequest{Mode: SpeechCustomVoice}); !errors.As(err, &un) || un.Reason != ReasonDisabled {
		t.Errorf("all engines of a kind disabled: %v, want UnavailableError(disabled)", err)
	}
}

func TestResolveSpeech_LoadsLeasesAndReleases(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.health(8100, true)

	l1, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase})
	if err != nil || !l1.Loaded || l1.Engine != "base-a" || l1.BaseURL != "http://127.0.0.1:8100" || l1.ID == "" {
		t.Fatalf("first resolve = %+v, %v; want loaded base-a on 8100 with a lease", l1, err)
	}
	l2, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase})
	if err != nil || l2.Loaded || l2.ID == l1.ID {
		t.Fatalf("second resolve = %+v, %v; want not loaded, a different lease", l2, err)
	}
	if got := state(t, e, "s1").Leases; got != 2 {
		t.Errorf("leases on s1 = %d, want 2", got)
	}
	e.advance(90 * time.Second)
	if !e.m.ReleaseSpeech(l1.ID) || e.m.ReleaseSpeech(l1.ID) {
		t.Error("releasing a lease must succeed once and be harmless the second time")
	}
	if e.m.ReleaseSpeech("no-such-lease") {
		t.Error("an unknown lease reported as released")
	}
	// Release marks the engine just used: the idle clock starts from the END of the request.
	if got := state(t, e, "s1"); got.Leases != 1 || !got.LastUse.Equal(e.clock()) {
		t.Errorf("after one release: leases=%d lastUse=%v, want 1 and now (%v)", got.Leases, got.LastUse, e.clock())
	}
}

func TestResolveSpeech_KokoroUsesItsFixedPort(t *testing.T) {
	e := newEnv(t)
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: "kokoro", Kind: "kokoro", Policy: store.PolicyOnDemand, Enabled: true,
		Functions: []string{store.FnSpeech}, MemoryBytes: 1 << 30, Params: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	e.health(8880, true)
	l, err := e.m.ResolveSpeech(context.Background(), SpeechRequest{Mode: SpeechKokoro})
	if err != nil || l.Port != 8880 || l.BaseURL != "http://127.0.0.1:8880" {
		t.Fatalf("kokoro = %+v, %v; want port 8880", l, err)
	}
}

// A clone engine of the Zonos kind is chosen for base speech like any other base engine, and its lease says which kind it is, because forge-tts picks
// the wire protocol from the kind (Qwen's /v1/audio/speech against Zonos's /v2/generate).
func TestResolveSpeech_ZonosLeaseCarriesItsKind(t *testing.T) {
	e := newEnv(t)
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: "zonos-clone", Kind: KindZonos3Server, Policy: store.PolicyAlwaysOn, Enabled: true,
		Functions: []string{store.FnSpeech}, MemoryBytes: 2 << 30,
		Params: map[string]string{"model": "/opt/forge/models/zonos3-mini/backbone-ptq.gguf", "zac": "/opt/forge/models/zonos3-mini/zac-keep2.gguf",
			"spk_encoder": "/opt/forge/models/zonos3-mini/spk-encoder.gguf"}}); err != nil {
		t.Fatal(err)
	}
	e.speech(t, "design-a", "voicedesign", true)
	e.health(8100, true)
	e.health(8101, true)
	e.defaults(map[string]string{"base": "zonos-clone"})
	l, err := e.m.ResolveSpeech(context.Background(), SpeechRequest{Mode: SpeechBase})
	if err != nil || l.Engine != "zonos-clone" || l.Kind != KindZonos3Server {
		t.Fatalf("base speech = %+v, %v; want zonos-clone of kind %s", l, err, KindZonos3Server)
	}
	d, err := e.m.ResolveSpeech(context.Background(), SpeechRequest{Mode: SpeechVoiceDesign})
	if err != nil || d.Engine != "design-a" || d.Kind != "qwen-tts-server" {
		t.Fatalf("voice design = %+v, %v; want design-a of kind qwen-tts-server", d, err)
	}
}

// The daemon cannot see a speech request, so a lease is what stops an idle-looking engine being evicted while one is running on it.
func TestLease_ProtectsAnEngineFromEviction(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Hour // long enough that expiry is not what this test is about
	for _, n := range []string{"base-a", "base-b", "base-c"} {
		e.speech(t, n, "base", true)
	}
	e.speech(t, "design-d", "voicedesign", true)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	var leases []SpeechLease
	for _, n := range []string{"base-a", "base-b", "base-c"} {
		l, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase, Engine: n})
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		e.advance(time.Second)
	}
	e.advance(10 * time.Minute) // all three are now long idle, base-a the least recently used; the pool is full

	// Only base-b and base-c are released: base-a (the natural victim) is still in use.
	e.m.ReleaseSpeech(leases[1].ID)
	e.m.ReleaseSpeech(leases[2].ID)
	e.advance(181 * time.Second) // a release restarts the idle clock: wait until base-b and base-c have been idle again
	got, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign})
	if err != nil {
		t.Fatalf("placing a fourth engine: %v", err)
	}
	if got.Slot == leases[0].Slot {
		t.Fatalf("the engine with a request in flight (%s) was evicted for %s", leases[0].Engine, got.Engine)
	}
	if st := state(t, e, leases[0].Slot); st.Occupant != "base-a" || st.State != StateRunning {
		t.Errorf("the leased engine's slot is %+v, want base-a running", st)
	}

	e.m.ReleaseSpeech(got.ID)
}

func TestLease_FullPoolWhenEverythingIsLeased(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Hour
	for _, n := range []string{"base-a", "base-b", "base-c"} {
		e.speech(t, n, "base", true)
	}
	e.speech(t, "design-d", "voicedesign", true)
	for _, p := range []int{8100, 8101, 8102} {
		e.health(p, true)
	}
	var ids []string
	for _, n := range []string{"base-a", "base-b", "base-c"} {
		l, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase, Engine: n})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, l.ID)
	}
	e.advance(10 * time.Minute) // idle for a long time, but each lease is still inside its hour
	var full *PoolFullError
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign}); !errors.As(err, &full) {
		t.Fatalf("every engine leased: %v, want PoolFullError", err)
	}
	// Once one is released AND has been idle again (a release restarts its idle clock) the pool can place the new engine.
	e.m.ReleaseSpeech(ids[0])
	e.advance(181 * time.Second)
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign}); err != nil {
		t.Fatalf("after a release: %v", err)
	}
}

// A holder that never releases (a crashed caller) cannot pin an engine for ever: the lease expires.
func TestLease_ExpiresOnItsOwn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Minute
	e.speech(t, "base-a", "base", true)
	e.health(8100, true)
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase}); err != nil {
		t.Fatal(err)
	}
	if got := state(t, e, "s1").Leases; got != 1 {
		t.Fatalf("leases = %d, want 1", got)
	}
	e.advance(61 * time.Second)
	if got := state(t, e, "s1").Leases; got != 0 {
		t.Errorf("an expired lease still counts: leases = %d", got)
	}
}

// A lease belongs to one run of an engine. If the engine was stopped and started again, the old lease protects nothing and must not pin the new run.
func TestLease_DoesNotSurviveARestartOfTheEngine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.health(8100, true)
	old, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.m.Unload(ctx, old.Slot); err != nil {
		t.Fatal(err)
	}
	e.advance(5 * time.Second)
	again, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase})
	if err != nil || !again.Loaded {
		t.Fatalf("second run: %+v, %v", again, err)
	}
	if got := state(t, e, "s1").Leases; got != 1 {
		t.Errorf("leases after a restart = %d, want only the new run's 1", got)
	}
	if e.m.ReleaseSpeech(old.ID) {
		t.Error("a lease from the previous run was still live")
	}
}

func TestLease_TableIsBounded(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.health(8100, true)
	for i := 0; i < maxLeases; i++ {
		if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase}); err != nil {
			t.Fatalf("lease %d: %v", i, err)
		}
	}
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase}); !errors.Is(err, ErrTooManyLeases) {
		t.Errorf("one lease past the cap: %v, want ErrTooManyLeases", err)
	}
	e.advance(11 * time.Minute) // everything has expired: the table frees itself
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase}); err != nil {
		t.Errorf("after expiry: %v", err)
	}
}

// Many requests at once across two engines, resolving and releasing: no race, no lost or duplicate lease, and the counts add up.
func TestResolveSpeech_Concurrent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.speech(t, "custom-a", "customvoice", true)
	e.health(8100, true)
	e.health(8101, true)
	var wg sync.WaitGroup
	seen := sync.Map{}
	for i := 0; i < 48; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mode := SpeechBase
			if i%2 == 1 {
				mode = SpeechCustomVoice
			}
			l, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: mode})
			if err != nil {
				t.Errorf("request %d: %v", i, err)
				return
			}
			if _, dup := seen.LoadOrStore(l.ID, true); dup {
				t.Errorf("duplicate lease id %s", l.ID)
			}
			_ = e.m.Snapshot(ctx)
			if !e.m.ReleaseSpeech(l.ID) {
				t.Errorf("lease %s could not be released", l.ID)
			}
		}(i)
	}
	wg.Wait()
	for _, s := range e.m.Snapshot(ctx) {
		if s.Leases != 0 {
			t.Errorf("%s still holds %d leases after every request finished", s.Slot, s.Leases)
		}
	}
}

// An always-on engine the operator stopped is not chosen for a voice that names none (the operator said it should not run), and a voice bound to it
// gets a clear answer naming the stop, not a generic failure.
func TestResolveSpeech_AnOperatorStoppedEngineIsNotChosenAndSaysSo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.speech(t, "base-a", "base", true)
	e.speech(t, "base-b", "base", true)
	e.health(8100, true)
	e.health(8101, true)
	if err := e.sp.SetStopped(ctx, "base-a", true); err != nil {
		t.Fatal(err)
	}
	e.defaults(map[string]string{"base": "base-a"}) // the default is the stopped one
	l, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase})
	if err != nil || l.Engine != "base-b" {
		t.Fatalf("default stopped: %q, %v, want base-b", l.Engine, err)
	}
	var un *UnavailableError
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase, Engine: "base-a"}); !errors.As(err, &un) || un.Reason != ReasonStopped {
		t.Errorf("a voice bound to the stopped engine: %v, want UnavailableError(stopped)", err)
	}
	// Every engine of the kind stopped: said as a stop, not as a disable.
	if err := e.sp.SetStopped(ctx, "base-b", true); err != nil {
		t.Fatal(err)
	}
	e.m.ReleaseSpeech(l.ID)
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase}); !errors.As(err, &un) || un.Reason != ReasonStopped {
		t.Errorf("all stopped: %v, want UnavailableError(stopped)", err)
	}
}

// A request that will be turned away because the lease table is full must not start an engine first.
func TestLease_TableFullRefusesBeforeStartingAnEngine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.LeaseTTL = time.Hour
	e.speech(t, "base-a", "base", true)
	e.speech(t, "design-a", "voicedesign", true)
	e.health(8100, true)
	e.health(8101, true)
	for i := 0; i < maxLeases; i++ {
		if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechBase}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.m.ResolveSpeech(ctx, SpeechRequest{Mode: SpeechVoiceDesign}); !errors.Is(err, ErrTooManyLeases) {
		t.Fatalf("with the table full: %v, want ErrTooManyLeases", err)
	}
	for _, s := range e.m.Snapshot(ctx) {
		if s.Occupant == "design-a" {
			t.Errorf("the refused request started design-a in %s", s.Slot)
		}
	}
}

func TestParseSpeechDefaults(t *testing.T) {
	got, err := ParseSpeechDefaults([]byte(`{"base":"a","voicedesign":"d","future_kind":{"engine":"x"},"kokoro":"","customvoice":7}`))
	if err != nil || len(got) != 2 || got["base"] != "a" || got["voicedesign"] != "d" {
		t.Errorf("ParseSpeechDefaults = %v, %v; want base and voicedesign only (unknown kinds, non-string and empty values left out)", got, err)
	}
	if got, err := ParseSpeechDefaults(nil); err != nil || len(got) != 0 {
		t.Errorf("empty: %v, %v", got, err)
	}
	if _, err := ParseSpeechDefaults([]byte(`[1,2]`)); err == nil {
		t.Error("not an object was accepted")
	}
}
