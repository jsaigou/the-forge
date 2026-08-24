// SPDX-License-Identifier: Apache-2.0

package sched

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

func resCore(t *testing.T, db *store.DB) *Core {
	t.Helper()
	eng := newFakeEngine()
	return newTestCore(t, eng, staticSource(time.Now(), eng.Slots(), nil), func(d *Deps) {
		if db != nil {
			d.Sched = db.Sched()
			d.Settings = db.Settings()
		}
	})
}

func window(startIn, length time.Duration) (time.Time, time.Time) {
	start := time.Now().Add(startIn).Truncate(time.Second)
	return start, start.Add(length)
}

func TestReservationCRUDRoundtrip(t *testing.T) {
	db := openTestStore(t)
	c := resCore(t, db)
	ctx := context.Background()

	start, end := window(time.Hour, 2*time.Hour)
	r := mustReservation("nightly", "nemotron", "bay", "a1", HumanIdentity, start, end)
	if err := c.CreateReservation(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	comfy := mustReservation("art-run", "comfyui", "comfyui", "", "agent-x", start, end)
	comfy.AllowAgentReschedule = true
	comfy.AllowAgentCancellation = true
	if err := c.CreateReservation(ctx, comfy); err != nil {
		t.Fatalf("create comfyui-scope: %v", err)
	}

	got := c.Reservations()
	if len(got) != 2 {
		t.Fatalf("reservations = %v, want 2", got)
	}

	// Update (human identity can always modify).
	upd := r
	upd.CreatedBy = HumanIdentity // requester identity channel
	upd.End = end.Add(time.Hour)
	if err := c.UpdateReservation(ctx, "nightly", upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	var found *Reservation
	for _, x := range c.Reservations() {
		if x.Label == "nightly" {
			x := x
			found = &x
		}
	}
	if found == nil || !found.End.Equal(end.Add(time.Hour)) {
		t.Fatalf("updated reservation = %+v", found)
	}

	// Cancel.
	if err := c.CancelReservation(ctx, "nightly", HumanIdentity); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got := c.Reservations(); len(got) != 1 || got[0].Label != "art-run" {
		t.Fatalf("reservations after cancel = %v", got)
	}
	rows, err := db.Sched().Reservations(ctx)
	if err != nil {
		t.Fatalf("store reservations: %v", err)
	}
	if len(rows) != 1 || rows[0].Label != "art-run" {
		t.Fatalf("persisted reservations = %v", rows)
	}
}

func TestCreateReservationDuplicateLabelConflicts(t *testing.T) {
	c := resCore(t, nil)
	ctx := context.Background()
	start, end := window(time.Hour, time.Hour)
	r := mustReservation("dup", "llama", "whole_box", "", HumanIdentity, start, end)
	if err := c.CreateReservation(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}
	err := c.CreateReservation(ctx, r)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	// httpapi's isConflict matches on the message text.
	if !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("message %q must contain \"conflict\"", err.Error())
	}
}

func TestReservationValidation(t *testing.T) {
	c := resCore(t, nil)
	ctx := context.Background()
	start, end := window(time.Hour, time.Hour)
	base := mustReservation("ok", "llama", "whole_box", "", HumanIdentity, start, end)

	cases := []struct {
		name   string
		mutate func(*Reservation)
	}{
		{"missing label", func(r *Reservation) { r.Label = "" }},
		{"missing model", func(r *Reservation) { r.Model = "" }},
		{"bad scope", func(r *Reservation) { r.Scope = "gpu" }},
		{"bay scope without bay", func(r *Reservation) { r.Scope = "bay" }},
		{"whole_box with bay", func(r *Reservation) { r.Bay = "a1" }},
		{"comfyui with bay", func(r *Reservation) { r.Scope = "comfyui"; r.Bay = "a1" }},
		{"end before start", func(r *Reservation) { r.End = r.Start.Add(-time.Minute) }},
		{"end equals start", func(r *Reservation) { r.End = r.Start }},
		{"missing created_by", func(r *Reservation) { r.CreatedBy = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.mutate(&r)
			if err := c.CreateReservation(ctx, r); err == nil {
				t.Fatalf("invalid reservation accepted: %+v", r)
			}
		})
	}
	if err := c.CreateReservation(ctx, base); err != nil {
		t.Fatalf("valid reservation rejected: %v", err)
	}
}

func TestReservationPermissions(t *testing.T) {
	start, end := window(time.Hour, time.Hour)
	locked := mustReservation("locked", "llama", "whole_box", "", HumanIdentity, start, end)
	open := mustReservation("open", "llama", "whole_box", "", "agent-a", start, end)
	open.AllowAgentReschedule = true
	open.AllowAgentCancellation = true
	cancelOnly := mustReservation("cancel-only", "llama", "whole_box", "", "agent-a", start, end)
	cancelOnly.AllowAgentCancellation = true

	cases := []struct {
		name        string
		res         Reservation
		requestedBy string
		action      string
		wantDenied  bool
	}{
		{"human can always reschedule", locked, HumanIdentity, "reschedule", false},
		{"human can always cancel", locked, HumanIdentity, "cancel", false},
		{"agent blocked on locked human reservation", locked, "agent-b", "reschedule", true},
		{"agent blocked on locked human cancel", locked, "agent-b", "cancel", true},
		{"creator agent may touch its own", open, "agent-a", "reschedule", false},
		{"other agent allowed by flag", open, "agent-b", "reschedule", false},
		{"other agent allowed cancel by flag", open, "agent-b", "cancel", false},
		{"reschedule denied when only cancel is open", cancelOnly, "agent-b", "reschedule", true},
		{"cancel allowed when cancel flag open", cancelOnly, "agent-b", "cancel", false},
		{"empty identity never owns anything", locked, "", "cancel", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReservationPermission(tc.res, tc.requestedBy, tc.action)
			if tc.wantDenied {
				if !errors.Is(err, ErrPermissionDenied) {
					t.Fatalf("err = %v, want ErrPermissionDenied", err)
				}
				// httpapi's isPermissionDenied matches message text.
				if !strings.Contains(err.Error(), "permission") && !strings.Contains(err.Error(), "denied") {
					t.Fatalf("message %q must contain permission/denied", err.Error())
				}
			} else if err != nil {
				t.Fatalf("err = %v, want allowed", err)
			}
		})
	}
}

func TestUpdateReservationEnforcesOwnershipAndPreservesCreator(t *testing.T) {
	c := resCore(t, nil)
	ctx := context.Background()
	start, end := window(time.Hour, time.Hour)
	r := mustReservation("mine", "llama", "whole_box", "", "agent-a", start, end)
	if err := c.CreateReservation(ctx, r); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Another agent, no reschedule flag: denied.
	hostile := r
	hostile.CreatedBy = "agent-b"
	hostile.End = end.Add(time.Hour)
	if err := c.UpdateReservation(ctx, "mine", hostile); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrPermissionDenied", err)
	}

	// The creator updates its own; the stored creator survives even
	// though the body's CreatedBy is the requester channel.
	own := r
	own.CreatedBy = "agent-a"
	own.End = end.Add(2 * time.Hour)
	if err := c.UpdateReservation(ctx, "mine", own); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := c.Reservations()
	if len(got) != 1 || got[0].CreatedBy != "agent-a" || !got[0].End.Equal(end.Add(2*time.Hour)) {
		t.Fatalf("reservation = %+v", got)
	}

	// Unknown label.
	if err := c.UpdateReservation(ctx, "ghost", own); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := c.CancelReservation(ctx, "ghost", HumanIdentity); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancel err = %v, want ErrNotFound", err)
	}
}

func TestResolveAgentFlags(t *testing.T) {
	tr, fa := true, false
	cases := []struct {
		createdBy    string
		resched      *bool
		cancel       *bool
		wantR, wantC bool
	}{
		{HumanIdentity, nil, nil, false, false}, // human default: locked
		{"agent-a", nil, nil, true, true},       // agent default: open
		{HumanIdentity, &tr, nil, true, false},  // explicit overrides default
		{"agent-a", &fa, &fa, false, false},
		{"agent-a", nil, &fa, true, false},
	}
	for i, tc := range cases {
		r, cl := ResolveAgentFlags(tc.createdBy, tc.resched, tc.cancel)
		if r != tc.wantR || cl != tc.wantC {
			t.Errorf("case %d: got (%v,%v), want (%v,%v)", i, r, cl, tc.wantR, tc.wantC)
		}
	}
}

// TestRestartRecovery: reservations and config survive a restart via the
// store; stale queue tickets (dead blocked callers) are purged.
func TestRestartRecovery(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	c1 := resCore(t, db)
	start, end := window(time.Hour, time.Hour)
	if err := c1.CreateReservation(ctx, mustReservation("r1", "llama", "whole_box", "", HumanIdentity, start, end)); err != nil {
		t.Fatalf("create r1: %v", err)
	}
	if err := c1.CreateReservation(ctx, mustReservation("r2", "gemma", "bay", "a3", "agent-a", start, end)); err != nil {
		t.Fatalf("create r2: %v", err)
	}
	cfg := Config{IdleUnloadS: 600, SmallJobTokenThreshold: 500, PriorityJumpCap: 3, ReservationSoonMin: 20}
	if err := c1.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	// Simulate a ticket that was mid-flight when the process died.
	if err := db.Sched().SaveTicket(ctx, store.QueueRow{
		TicketID: "dead", Model: "llama", RequestedBy: "a0", Status: StatusQueued,
		EnqueuedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}

	// "Restart": a fresh Core over the same store.
	c2 := resCore(t, db)
	if got := c2.Config(); got != cfg {
		t.Fatalf("recovered config = %+v, want %+v", got, cfg)
	}
	rs := c2.Reservations()
	if len(rs) != 2 {
		t.Fatalf("recovered reservations = %v, want 2", rs)
	}
	byLabel := map[string]Reservation{}
	for _, r := range rs {
		byLabel[r.Label] = r
	}
	r2 := byLabel["r2"]
	if r2.Scope != "bay" || r2.Bay != "a3" || r2.CreatedBy != "agent-a" ||
		!r2.Start.Equal(start) || !r2.End.Equal(end) {
		t.Fatalf("recovered r2 = %+v", r2)
	}
	if q := c2.Status().Queue; len(q) != 0 {
		t.Fatalf("in-memory queue after restart = %v, want empty", q)
	}
	rows, err := db.Sched().Queue(ctx)
	if err != nil {
		t.Fatalf("store queue: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("persisted queue after restart = %v, want purged", rows)
	}

	// Recovered reservations drive eviction decisions: r1 (llama) is
	// active-in-window?? No — it starts in an hour. Make a third core
	// with an active window to close the loop end-to-end.
	if err := c2.CreateReservation(ctx, mustReservation("active", "m2", "whole_box", "",
		HumanIdentity, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("create active: %v", err)
	}
	c3 := resCore(t, db)
	eng := c3.d.Engine.(*fakeEngine)
	occupyAll(eng, map[string]string{"a1": "m1", "a2": "m2", "a3": "m3", "a4": "m4"})
	src := staticSource(time.Now(), eng.Slots(), map[string]time.Duration{
		"a1": time.Second, "a2": time.Hour, "a3": time.Second, "a4": time.Second,
	})
	c3.d.Source = src
	pl := c3.place(context.Background(), "llama", "", c3.Config(), c3.Reservations(), 30*time.Second)
	if pl.slot != "" {
		t.Fatalf("place = %+v, want blocked (only idle slot holds the recovered active reservation's model)", pl)
	}
}
