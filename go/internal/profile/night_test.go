// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

type nightFake struct {
	mu       sync.Mutex
	now      time.Time
	raw      []byte
	statuses []Status
	slots    map[string]NightSlot
	ran      []string
	loaded   []string
	fail     map[string]error
	takes    time.Duration
	held     int
	reserved bool
}

func (f *nightFake) night() *Night {
	return NewNight(NightDeps{
		Statuses: func() []Status { f.mu.Lock(); defer f.mu.Unlock(); return append([]Status(nil), f.statuses...) },
		Run: func(_ context.Context, mode string) error {
			f.ran = append(f.ran, mode)
			f.now = f.now.Add(f.takes)
			// A run evicts the slots and leaves them empty.
			for name := range f.slots {
				f.slots[name] = NightSlot{}
			}
			return f.fail[mode]
		},
		Slots: func() map[string]NightSlot {
			out := map[string]NightSlot{}
			for k, v := range f.slots {
				out[k] = v
			}
			return out
		},
		Load: func(_ context.Context, mode, slot string) error {
			f.loaded = append(f.loaded, slot+"="+mode)
			f.slots[slot] = NightSlot{Mode: mode}
			return nil
		},
		Reserved: func(context.Context, time.Time) (bool, error) { return f.reserved, nil },
		Hold: func(ctx context.Context, _ time.Duration) (context.Context, func(), error) {
			f.held++
			return ctx, func() { f.held-- }, nil
		},
		Get:  func(context.Context) ([]byte, bool, error) { return f.raw, f.raw != nil, nil },
		Set:  func(_ context.Context, raw []byte) error { f.raw = raw; return nil },
		Now:  func() time.Time { return f.now },
		Poll: time.Millisecond,
	})
}

func newNightFake(hour int) *nightFake {
	return &nightFake{
		now:   time.Date(2026, 11, 27, hour, 0, 0, 0, time.Local),
		slots: map[string]NightSlot{"a1": {Mode: "flagship"}, "a2": {}},
		fail:  map[string]error{},
		takes: 30 * time.Minute,
		statuses: []Status{
			{Mode: "big", State: StateNever, NCtx: 262144},
			{Mode: "small", State: StateOutdated, NCtx: 8192},
			{Mode: "fine", State: StateCurrent, NCtx: 8192},
			{Mode: "radiance", State: StateUnsupported, NCtx: 8192},
		},
	}
}

func TestNightWindow(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, time.Local) }
	for _, c := range []struct{ now, start, end time.Time }{
		{day(27, 9), day(27, 23), day(28, 6)},
		{day(27, 23), day(27, 23), day(28, 6)},
		{day(28, 2), day(28, 2), day(28, 6)},
		{day(28, 6), day(28, 23), day(29, 6)},
	} {
		start, end := NightWindow(c.now)
		if !start.Equal(c.start) || !end.Equal(c.end) {
			t.Errorf("NightWindow(%v) = %v, %v; want %v, %v", c.now, start, end, c.start, c.end)
		}
	}
}

func TestNightProfilesShortestFirstThenRestores(t *testing.T) {
	f := newNightFake(9)
	n := f.night()
	ctx := context.Background()
	st, err := n.Arm(ctx, "me")
	if err != nil || st.Status != NightArmed {
		t.Fatalf("arm: %+v %v", st, err)
	}
	if _, err := n.Arm(ctx, "me"); !errors.Is(err, ErrNightActive) {
		t.Fatalf("second arm: %v", err)
	}
	if err := n.Step(ctx); err != nil || len(f.ran) != 0 {
		t.Fatalf("ran before the window: %v %v", f.ran, err)
	}
	f.now = time.Date(2026, 11, 27, 23, 0, 0, 0, time.Local)
	f.fail["big"] = errors.New("does not fit")
	if err := n.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 2 || f.ran[0] != "small" || f.ran[1] != "big" {
		t.Fatalf("ran = %v, want shortest first and only never/outdated", f.ran)
	}
	if len(f.loaded) != 1 || f.loaded[0] != "a1=flagship" {
		t.Fatalf("restored = %v", f.loaded)
	}
	if f.held != 0 {
		t.Fatalf("maintenance windows left open: %d", f.held)
	}
	st, err = n.State(ctx)
	if err != nil || st.Status != NightIdle || len(st.Results) != 2 || st.Results[0].Outcome != NightProfiled || st.Results[1].Outcome != NightFailed {
		t.Fatalf("state = %+v %v", st, err)
	}
}

func TestNightLeavesOutWhatDoesNotFit(t *testing.T) {
	f := newNightFake(5) // one hour left
	n := f.night()
	ctx := context.Background()
	f.statuses = []Status{{Mode: "huge", State: StateNever, NCtx: 1048576}, {Mode: "small", State: StateNever, NCtx: 8192}}
	if _, err := n.Arm(ctx, "me"); err != nil {
		t.Fatal(err)
	}
	if err := n.Step(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := n.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 1 || f.ran[0] != "small" || len(st.Results) != 2 || st.Results[1].Outcome != NightNoTime {
		t.Fatalf("ran = %v, results = %+v", f.ran, st.Results)
	}
}

func TestNightWaitsWhileBusyAndTouchesNothing(t *testing.T) {
	f := newNightFake(23)
	f.slots["a1"] = NightSlot{Mode: "flagship", Busy: true}
	n := f.night()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := n.Arm(ctx, "me"); err != nil {
		t.Fatal(err)
	}
	_ = n.Step(ctx) // returns when ctx expires
	st, err := n.State(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 0 || len(f.loaded) != 0 || st.Status != NightRunning || st.Waiting != WaitSlotBusy {
		t.Fatalf("ran=%v loaded=%v state=%+v", f.ran, f.loaded, st)
	}
}

func TestNightResumesAfterRestart(t *testing.T) {
	f := newNightFake(2)
	f.slots = map[string]NightSlot{"a1": {}}
	f.raw = []byte(`{"status":"running","ends_at":` + itoaUnix(f.now.Add(4*time.Hour)) + `,"current":"big","restore":{"a1":"flagship"}}`)
	n := f.night()
	if err := n.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.ran) != 1 || f.ran[0] != "small" {
		t.Fatalf("ran = %v: the interrupted config must not be retried", f.ran)
	}
	if len(f.loaded) != 1 || f.loaded[0] != "a1=flagship" {
		t.Fatalf("restored = %v", f.loaded)
	}
}

func TestMeasurementFlagsIgnoreRequestDefaults(t *testing.T) {
	base := []string{"--parallel", "1", "--cache-type-k", "q8_0", "--temp", "0.7", "--jinja", "--threads", "16"}
	retuned := []string{"--temp", "1.0", "--threads", "16", "--cache-type-k", "q8_0", "--parallel", "1", "--top-p", "0.9", "--presence-penalty", "-1"}
	swapped := []string{"--parallel", "16", "--cache-type-k", "q8_0", "--threads", "1"}
	join := func(a []string) string { return hashParts(measurementFlags(a)) }
	if join(base) != join(retuned) {
		t.Errorf("request defaults changed the measurement flags: %v vs %v", measurementFlags(base), measurementFlags(retuned))
	}
	if join(base) == join(swapped) {
		t.Errorf("swapping two flags' values went unnoticed: %v", measurementFlags(swapped))
	}
}

func itoaUnix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// A run leaves its slot freshly emptied; the model that was there before is given another one.
func TestNightRestoresElsewhereWhenItsSlotIsResting(t *testing.T) {
	f := newNightFake(23)
	f.slots = map[string]NightSlot{"a1": {Mode: "flagship"}, "a2": {}, "a3": {}}
	f.statuses = []Status{{Mode: "small", State: StateNever, NCtx: 8192}}
	n := f.night()
	run := n.d.Run
	n.d.Run = func(ctx context.Context, mode string) error {
		err := run(ctx, mode)
		f.slots = map[string]NightSlot{"a1": {Resting: true, Rank: 2}, "a2": {Rank: 0}, "a3": {Rank: 1}}
		return err
	}
	ctx := context.Background()
	if _, err := n.Arm(ctx, "me"); err != nil {
		t.Fatal(err)
	}
	if err := n.Step(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.loaded) != 1 || f.loaded[0] != "a2=flagship" {
		t.Fatalf("restored = %v, want a2", f.loaded)
	}
}
