// SPDX-License-Identifier: Apache-2.0

package main

// Wiring for the overnight profiling run (profile.Night).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/profile"
	"github.com/jsaigou/the-forge/internal/store"
)

func newProfileNight(db *store.DB, runner *profile.Runner, eng engine.Engine, coll collector.Source, gate *maintenance.Gate) *profile.Night {
	return profile.NewNight(profile.NightDeps{
		Statuses: runner.Statuses,
		Run: func(ctx context.Context, mode string) error {
			_, err := runner.Run(ctx, profile.RunRequest{Mode: mode})
			return err
		},
		Slots: func() map[string]profile.NightSlot {
			out := map[string]profile.NightSlot{}
			snap := coll.Current()
			// A run started by hand counts as every slot being busy, as does not yet knowing what the slots are doing.
			manual := runner.IsRunning()
			if snap == nil {
				return map[string]profile.NightSlot{"unknown": {Busy: true}}
			}
			rank := map[string]int{}
			for i, name := range engine.LoadOrderOf(eng, eng.Slots()) {
				rank[name] = i
			}
			orderer, _ := eng.(engine.SlotOrderer)
			for name, s := range snap.Slots {
				out[name] = profile.NightSlot{Mode: s.Mode,
					Busy:    manual || s.Loading != nil || s.Unloading != nil || snap.Inference[name].RequestsProcessing > 0,
					Resting: orderer != nil && orderer.RecentlyFreed(name), Rank: rank[name]}
			}
			return out
		},
		Load: func(ctx context.Context, mode, slot string) error {
			if res := eng.Load(ctx, mode, slot); !res.Success {
				return errors.New(res.Message)
			}
			return nil
		},
		Reserved: func(ctx context.Context, t time.Time) (bool, error) {
			rows, err := db.Sched().Reservations(ctx)
			if err != nil {
				return false, err
			}
			for _, r := range rows {
				if !t.Before(r.Start) && t.Before(r.End) {
					return true, nil
				}
			}
			return false, nil
		},
		Hold: func(ctx context.Context, d time.Duration) (context.Context, func(), error) {
			st, err := gate.Enter(maintenance.EnterRequest{Reason: "overnight profiling", EnteredBy: "profile-night", Duration: d})
			if err != nil {
				return nil, nil, fmt.Errorf("enter maintenance: %w", err)
			}
			release := func() {
				if _, err := gate.Exit(st.LeaseID, false); err != nil && !errors.Is(err, maintenance.ErrNotActive) {
					log.Printf("forge: overnight profiling: leave maintenance: %v", err)
				}
			}
			return maintenance.WithLease(ctx, st.LeaseID), release, nil
		},
		Get: func(ctx context.Context) ([]byte, bool, error) {
			raw, err := db.Settings().Get(ctx, profile.NightSettingsKey)
			if errors.Is(err, store.ErrNotFound) {
				return nil, false, nil
			}
			return raw, err == nil, err
		},
		Set: func(ctx context.Context, raw []byte) error {
			return db.Settings().Set(ctx, profile.NightSettingsKey, raw)
		},
		Logf: log.Printf,
	})
}
