// SPDX-License-Identifier: Apache-2.0

package profile

// "Profile tonight": the operator arms one overnight window (23:00 to 06:00 local time) and Forge profiles every config that has never been
// measured or whose measurement is outdated, for as many as fit.
//
//   - Shortest first, so the window yields as many measurements as it can. A config whose estimate no longer fits before the end is left out.
//   - It waits while a slot is serving or changing, or a reservation is active.
//   - Each run holds a maintenance window, so no other load or eviction can disturb the measurement.
//   - What was loaded before the first eviction is loaded again at the end, into slots that are still empty.
//   - The run at the deadline is cut off, and the whole night survives a daemon restart.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Night window, local time.
const (
	NightStartHour = 23
	NightEndHour   = 6
)

// NightSettingsKey holds the night's state in the settings store.
const NightSettingsKey = "profile.night"

// Night states.
const (
	NightIdle    = "idle"
	NightArmed   = "armed"
	NightRunning = "running"
)

// Outcomes of one config in a night.
const (
	NightProfiled  = "profiled"
	NightFailed    = "failed"
	NightNoTime    = "no_time"
	NightCancelled = "cancelled"
)

// Waiting reasons.
const (
	WaitSlotBusy    = "slot_busy"
	WaitReservation = "reservation"
	WaitMaintenance = "maintenance"
)

var (
	ErrNightActive  = errors.New("profile: an overnight run is already armed or running")
	ErrNightIdle    = errors.New("profile: no overnight run is armed")
	ErrNightNothing = errors.New("profile: nothing to profile")
)

// NightResult is what happened to one config.
type NightResult struct {
	Mode    string `json:"mode"`
	Outcome string `json:"outcome"`
	Message string `json:"message,omitempty"`
	Seconds int64  `json:"seconds,omitempty"`
}

// NightState is the persisted state; after a night ends it keeps that night's results until the next one is armed.
type NightState struct {
	Status   string        `json:"status"`
	StartsAt int64         `json:"starts_at,omitempty"`
	EndsAt   int64         `json:"ends_at,omitempty"`
	ArmedBy  string        `json:"armed_by,omitempty"`
	Current  string        `json:"current,omitempty"`
	Waiting  string        `json:"waiting,omitempty"`
	Finished int64         `json:"finished_at,omitempty"`
	Results  []NightResult `json:"results,omitempty"`
	// Restore is slot -> config loaded before the first eviction; nil until then.
	Restore map[string]string `json:"restore,omitempty"`
	// RestoreFailed lists slots that could not be given their model back.
	RestoreFailed []NightResult `json:"restore_failed,omitempty"`
}

// NightItem is a config the night would measure.
type NightItem struct {
	Mode            string `json:"mode"`
	ConfigID        int64  `json:"config_id"`
	State           string `json:"state"`
	EstimateSeconds int64  `json:"estimate_seconds"`
}

// NightSlot is what the night needs to know about one slot.
type NightSlot struct {
	Mode string
	Busy bool // serving a request, loading or unloading
	// Resting: emptied too recently to take a new load (engine.SlotOrderer). Rank is its place in the load order.
	Resting bool
	Rank    int
}

// NightDeps wires the night runner.
type NightDeps struct {
	Statuses func() []Status
	Run      func(ctx context.Context, mode string) error
	Slots    func() map[string]NightSlot
	Load     func(ctx context.Context, mode, slot string) error
	// Reserved reports whether a reservation is active at t.
	Reserved func(ctx context.Context, t time.Time) (bool, error)
	// Hold opens a maintenance window for at most d and returns a context that may act inside it, and its release.
	Hold func(ctx context.Context, d time.Duration) (context.Context, func(), error)
	// Settings persistence.
	Get func(ctx context.Context) ([]byte, bool, error)
	Set func(ctx context.Context, raw []byte) error

	Now  func() time.Time
	Logf func(format string, args ...any)
	// Poll is how often the loop looks, and how long it waits when something is busy.
	Poll time.Duration
}

// Night runs the overnight profiling.
type Night struct {
	d NightDeps

	mu     sync.Mutex // guards the persisted state's read-modify-write and cancel
	cancel context.CancelFunc
	wake   chan struct{}
}

// NewNight returns a Night; call Loop to drive it.
func NewNight(d NightDeps) *Night {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Poll <= 0 {
		d.Poll = time.Minute
	}
	return &Night{d: d, wake: make(chan struct{}, 1)}
}

// NightWindow is the window "tonight" means at now: the one in progress, else the next one.
func NightWindow(now time.Time) (start, end time.Time) {
	at := func(day time.Time, hour int) time.Time {
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, day.Location())
	}
	switch {
	case now.Hour() < NightEndHour:
		return now, at(now, NightEndHour)
	case now.Hour() >= NightStartHour:
		return now, at(now.AddDate(0, 0, 1), NightEndHour)
	default:
		return at(now, NightStartHour), at(now.AddDate(0, 0, 1), NightEndHour)
	}
}

// Estimate is a deliberately generous guess at one run's length: loading and sampling, then filling the whole context once plus four probes at
// the last measured prefill rate, or a cautious default.
func Estimate(s Status) time.Duration {
	const (
		fixed          = 4 * time.Minute
		defaultPrefill = 150.0 // tokens a second, when never measured
		decode         = 10.0
		loadRate       = 500e6 // bytes a second
	)
	prefill := defaultPrefill
	if s.Result != nil && s.Result.PrefillTPS > 0 {
		// Prefill slows with depth; the last measurement's deepest point is the honest average's lower bound.
		prefill = s.Result.PrefillTPS
		if n := len(s.Result.DepthBenchmarks); n > 0 && s.Result.DepthBenchmarks[n-1].PP2048TPS > 0 {
			prefill = s.Result.DepthBenchmarks[n-1].PP2048TPS
		}
	}
	secs := float64(s.NCtx+4*2048)/prefill + 4*128/decode + float64(s.WeightBytes)/loadRate
	return fixed + time.Duration(secs*float64(time.Second))
}

// Candidates are the configs a night would measure, shortest first, skipping those already attempted.
func Candidates(statuses []Status, attempted map[string]bool) []NightItem {
	out := []NightItem{}
	for _, s := range statuses {
		if (s.State != StateNever && s.State != StateOutdated) || attempted[s.Mode] {
			continue
		}
		out = append(out, NightItem{Mode: s.Mode, ConfigID: s.ConfigID, State: s.State, EstimateSeconds: int64(Estimate(s).Seconds())})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].EstimateSeconds < out[j].EstimateSeconds })
	return out
}

// State returns the persisted state.
func (n *Night) State(ctx context.Context) (NightState, error) {
	raw, ok, err := n.d.Get(ctx)
	if err != nil {
		return NightState{}, err
	}
	if !ok {
		return NightState{Status: NightIdle}, nil
	}
	var s NightState
	if err := json.Unmarshal(raw, &s); err != nil {
		return NightState{}, fmt.Errorf("profile: stored night state: %w", err)
	}
	if s.Status == "" {
		s.Status = NightIdle
	}
	return s, nil
}

func (n *Night) save(ctx context.Context, s NightState) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return n.d.Set(ctx, raw)
}

// update applies fn to the state under the lock and saves it.
func (n *Night) update(ctx context.Context, fn func(*NightState)) (NightState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, err := n.State(ctx)
	if err != nil {
		return s, err
	}
	fn(&s)
	return s, n.save(ctx, s)
}

// Arm schedules tonight's run.
func (n *Night) Arm(ctx context.Context, by string) (NightState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, err := n.State(ctx)
	if err != nil {
		return s, err
	}
	if s.Status != NightIdle {
		return s, ErrNightActive
	}
	if len(Candidates(n.d.Statuses(), nil)) == 0 {
		return s, ErrNightNothing
	}
	start, end := NightWindow(n.d.Now())
	s = NightState{Status: NightArmed, StartsAt: start.Unix(), EndsAt: end.Unix(), ArmedBy: by}
	if err := n.save(ctx, s); err != nil {
		return s, err
	}
	select {
	case n.wake <- struct{}{}:
	default:
	}
	return s, nil
}

// Cancel drops an armed night, or stops a running one (the loop then restores the slots).
func (n *Night) Cancel(ctx context.Context) (NightState, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	s, err := n.State(ctx)
	if err != nil {
		return s, err
	}
	switch s.Status {
	case NightArmed:
		s = NightState{Status: NightIdle}
	case NightRunning:
		// Ending the window now is what the loop checks, including after a restart.
		s.EndsAt = n.d.Now().Unix()
		if n.cancel != nil {
			n.cancel()
		}
	default:
		return s, ErrNightIdle
	}
	return s, n.save(ctx, s)
}

// Loop drives the night until ctx is done.
func (n *Night) Loop(ctx context.Context) {
	ticker := time.NewTicker(n.d.Poll)
	defer ticker.Stop()
	for {
		if err := n.Step(ctx); err != nil && ctx.Err() == nil {
			n.d.Logf("profile: overnight: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.wake:
		}
	}
}

// Step runs the night if one is due. It returns when that night is over (or ctx is done).
func (n *Night) Step(ctx context.Context) error {
	s, err := n.State(ctx)
	if err != nil {
		return err
	}
	now := n.d.Now()
	switch s.Status {
	case NightArmed:
		if now.Unix() < s.StartsAt {
			return nil
		}
		if s, err = n.update(ctx, func(s *NightState) { s.Status = NightRunning }); err != nil {
			return err
		}
	case NightRunning:
		// A restart interrupted a night: the config it was on is not retried.
		if s.Current != "" {
			if s, err = n.update(ctx, func(s *NightState) {
				s.Results = append(s.Results, NightResult{Mode: s.Current, Outcome: NightFailed, Message: "interrupted by a daemon restart"})
				s.Current = ""
			}); err != nil {
				return err
			}
		}
	default:
		return nil
	}
	if err := n.measure(ctx); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err() // shutting down: stay "running" so the next start picks it up
	}
	return n.finish(ctx)
}

// measure profiles candidates until the window closes or none is left that fits.
func (n *Night) measure(ctx context.Context) error {
	for {
		s, err := n.State(ctx)
		if err != nil {
			return err
		}
		left := time.Unix(s.EndsAt, 0).Sub(n.d.Now())
		if ctx.Err() != nil || left <= 0 {
			return nil
		}
		attempted := map[string]bool{}
		for _, r := range s.Results {
			attempted[r.Mode] = true
		}
		items := Candidates(n.d.Statuses(), attempted)
		if len(items) == 0 {
			return nil
		}
		next := items[0] // shortest first: if this one does not fit, none does
		if time.Duration(next.EstimateSeconds)*time.Second > left {
			_, err := n.update(ctx, func(s *NightState) {
				for _, it := range items {
					s.Results = append(s.Results, NightResult{Mode: it.Mode, Outcome: NightNoTime, Seconds: it.EstimateSeconds})
				}
			})
			return err
		}
		reason, err := n.busy(ctx)
		if err != nil {
			return err
		}
		var runCtx context.Context
		release := func() {}
		if reason == "" {
			runCtx, release, err = n.d.Hold(ctx, left)
			if err != nil {
				n.d.Logf("profile: overnight: cannot hold a maintenance window: %v", err)
				reason = WaitMaintenance
			}
		}
		if reason != "" {
			if _, err := n.update(ctx, func(s *NightState) { s.Waiting = reason }); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(n.d.Poll):
			}
			continue
		}
		if err := n.runOne(ctx, runCtx, next.Mode); err != nil {
			release()
			return err
		}
		release()
	}
}

func (n *Night) busy(ctx context.Context) (string, error) {
	for _, slot := range n.d.Slots() {
		if slot.Busy {
			return WaitSlotBusy, nil
		}
	}
	reserved, err := n.d.Reserved(ctx, n.d.Now())
	if err != nil {
		return "", err
	}
	if reserved {
		return WaitReservation, nil
	}
	return "", nil
}

func (n *Night) runOne(ctx, held context.Context, mode string) error {
	s, err := n.update(ctx, func(s *NightState) {
		s.Current, s.Waiting = mode, ""
		if s.Restore == nil {
			s.Restore = map[string]string{}
			for name, slot := range n.d.Slots() {
				if slot.Mode != "" {
					s.Restore[name] = slot.Mode
				}
			}
		}
	})
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithDeadline(held, time.Unix(s.EndsAt, 0))
	defer cancel()
	n.mu.Lock()
	n.cancel = cancel
	n.mu.Unlock()
	started := n.d.Now()
	runErr := n.d.Run(runCtx, mode)
	n.mu.Lock()
	n.cancel = nil
	n.mu.Unlock()
	if ctx.Err() != nil {
		return nil // shutting down; Current stays set and the next start records it
	}
	res := NightResult{Mode: mode, Outcome: NightProfiled, Seconds: int64(n.d.Now().Sub(started).Seconds())}
	if runErr != nil {
		res.Outcome, res.Message = NightFailed, runErr.Error()
		if after, err := n.State(ctx); err == nil && n.d.Now().Unix() >= after.EndsAt {
			res.Outcome = NightCancelled
		}
	}
	_, err = n.update(ctx, func(s *NightState) {
		s.Results = append(s.Results, res)
		s.Current = ""
	})
	return err
}

// restedSlot is the empty, rested slot earliest in the load order that no other restore is owed, or "".
func restedSlot(slots map[string]NightSlot, restore map[string]string) string {
	best := ""
	for name, s := range slots {
		if _, owed := restore[name]; owed || s.Mode != "" || s.Busy || s.Resting {
			continue
		}
		if best == "" || s.Rank < slots[best].Rank {
			best = name
		}
	}
	return best
}

// finish gives the slots their models back and ends the night.
func (n *Night) finish(ctx context.Context) error {
	s, err := n.State(ctx)
	if err != nil {
		return err
	}
	var failed []NightResult
	slots := make([]string, 0, len(s.Restore))
	for slot := range s.Restore {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		mode := s.Restore[slot]
		cur := n.d.Slots()[slot]
		if cur.Mode == mode {
			continue
		}
		if cur.Mode != "" {
			failed = append(failed, NightResult{Mode: mode, Outcome: NightFailed, Message: fmt.Sprintf("slot %s now runs %s", slot, cur.Mode)})
			continue
		}
		// The run may have just emptied this slot: give the model another free one rather than one that cannot start a server yet.
		if cur.Resting {
			if other := restedSlot(n.d.Slots(), s.Restore); other != "" {
				slot = other
			}
		}
		if err := n.d.Load(ctx, mode, slot); err != nil {
			failed = append(failed, NightResult{Mode: mode, Outcome: NightFailed, Message: fmt.Sprintf("slot %s: %v", slot, err)})
		}
	}
	_, err = n.update(ctx, func(s *NightState) {
		s.Status, s.Current, s.Waiting = NightIdle, "", ""
		s.Finished = n.d.Now().Unix()
		s.Restore, s.RestoreFailed = nil, failed
	})
	return err
}
