// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// slotNameRe bounds what may appear in a slot name (the slot_launches key and
// the systemd unit names): config-supplied slot keys can never turn a unit or
// row name into a traversal. The store repeats the pattern (store.ValidSlotLaunchSlot).
var slotNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// validateSlotService is the check a load makes before anything is written:
// the slot name is legal and the service's engine kind is one this build
// knows (an unknown kind fails closed, ADR-0025). Phase 1b step d2 retired the
// per-slot env/args files this used to be the front door of
// (/etc/sysconfig/forge-<slot>-{env,args}); the slot_launches row is the only
// launch record now.
func validateSlotService(slot string, svc config.Service) error {
	if !slotNameRe.MatchString(slot) {
		return fmt.Errorf("engine: invalid slot name %q", slot)
	}
	if _, err := svc.Engine(); err != nil {
		return fmt.Errorf("engine: slot %s: %w", slot, err)
	}
	return nil
}

// slotRowTimeout bounds the single-row reads below (a local SQLite lookup).
const slotRowTimeout = 2 * time.Second

// slotRow reads a slot's launch row. ok=false when the store is not wired, the
// slot has no row or the read failed (a failure is logged, never swallowed;
// callers treat "unknown" exactly like "no row").
func (m *Manager) slotRow(slot string) (store.SlotLaunch, bool) {
	if m.d.SlotLaunches == nil {
		return store.SlotLaunch{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), slotRowTimeout)
	defer cancel()
	row, err := m.d.SlotLaunches.Get(ctx, slot)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			m.logf("ERROR: reading the launch row of %s: %v", slot, err)
		}
		return store.SlotLaunch{}, false
	}
	return row, true
}

// The collector type-asserts its Slots source to this; a silent miss would zero the weight accounting, so pin it at compile time.
var _ collector.SlotWeightSource = (*Manager)(nil)

// SlotWeightPaths returns the model and mmproj paths of a slot's last launch,
// read from its slot_launches row ("" and "" when there is no row). It
// satisfies collector.SlotWeightSource.
func (m *Manager) SlotWeightPaths(slot string) (modelPath, mmprojPath string) {
	row, ok := m.slotRow(slot)
	if !ok {
		return "", ""
	}
	return row.ModelPath, row.MmprojPath
}

// modeFromRow resolves a launch row to a configured mode: the row's
// config_name, if the catalog still has a config of that name. It replaces the
// old alias/model-path inference over the env file; config_name is the exact
// catalog config the row was built from, so same-weights siblings (the
// 2026-08-25 "-nothink" incident) cannot be confused.
func modeFromRow(cfg *config.Config, row store.SlotLaunch) string {
	if _, ok := cfg.Modes[row.ConfigName]; ok {
		return row.ConfigName
	}
	return ""
}
