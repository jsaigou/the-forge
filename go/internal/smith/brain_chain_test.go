// SPDX-License-Identifier: Apache-2.0

package smith

// S5 brain-chain tests: the ordered fallback list (smith.brain_chain) — a
// chain member already loaded AND idle answers with zero load wait, in
// chain-preference order; loaded-but-busy members are never contended with;
// nothing provisioned means behavior is byte-for-byte unchanged.

import (
	"context"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/sched"
)

// chainSched wraps sched.Stub with a configurable Status — the scheduler
// snapshot Brain()'s slot scan reads.
type chainSched struct {
	sched.Stub
	st sched.Status
}

func (c *chainSched) Status() sched.Status { return c.st }

func f64(v float64) *float64 { return &v }

func newChainSmith(t *testing.T, sch sched.Scheduler) *Smith {
	t.Helper()
	db := openDB(t)
	return New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: sch, Logf: t.Logf})
}

func TestBrainChain_LoadedIdleMemberPreferred(t *testing.T) {
	sch := &chainSched{st: sched.Status{
		Slots:       map[string]string{"a2": "gemma4-mtp", "a3": "qwen36-mtp"},
		SlotLabels:  map[string]string{"a2": "A2", "a3": "A3"},
		IdleSeconds: map[string]*float64{"a2": f64(5), "a3": f64(400)},
	}}
	s := newChainSmith(t, sch)
	setSetting(t, s.d.Store, SettingModel, `"ornith-35b"`)
	setSetting(t, s.d.Store, SettingBrainChain, `["gemma4-e4b-qat","gemma4-mtp","qwen36-mtp"]`)

	// gemma4-e4b-qat isn't loaded at all; gemma4-mtp IS loaded but busy
	// (5s idle) — never contend with it. Next member qwen36-mtp is loaded
	// AND idle → answers.
	br := s.Brain(context.Background())
	if br.Resolution != BrainLocalSlot || br.Model != "qwen36-mtp" || br.Slot != "a3" {
		t.Fatalf("Brain = %+v, want qwen36-mtp on a3 (busy member skipped, idle member wins)", br)
	}
	if !strings.Contains(br.Detail, "smith.brain_chain") {
		t.Errorf("detail = %q, want evidence citing the settings key", br.Detail)
	}
}

func TestBrainChain_AllBusyFallsThroughToDefault(t *testing.T) {
	// Both chain members loaded but busy → never contend; falls through to
	// the base resolution for the effective model (chain[0], not loaded).
	sch := &chainSched{st: sched.Status{
		Slots:       map[string]string{"a1": "gemma4-e4b-qat", "a2": "gemma4-mtp"},
		SlotLabels:  map[string]string{"a1": "A1", "a2": "A2"},
		IdleSeconds: map[string]*float64{"a1": f64(0), "a2": f64(2)},
	}}
	s := newChainSmith(t, sch)
	setSetting(t, s.d.Store, SettingModel, `"ornith-35b"`)
	setSetting(t, s.d.Store, SettingBrainChain, `["gemma4-e4b-qat","gemma4-mtp"]`)

	br := s.Brain(context.Background())
	if br.Resolution == BrainLocalSlot && br.Slot != "" {
		t.Fatalf("Brain = %+v, want NO slot handed over while every member is busy", br)
	}
	if br.Model != "gemma4-e4b-qat" {
		t.Errorf("model = %q, want the chain[0] default as the load candidate", br.Model)
	}
}

func TestBrainChain_NilWhenUnsetOrMalformed(t *testing.T) {
	s := newChainSmith(t, &chainSched{})
	ctx := context.Background()
	if got := s.BrainChain(ctx); got != nil {
		t.Errorf("unset = %v, want nil", got)
	}
	setSetting(t, s.d.Store, SettingBrainChain, `{not an array`)
	if got := s.BrainChain(ctx); got != nil {
		t.Errorf("malformed = %v, want nil (fail closed)", got)
	}
	setSetting(t, s.d.Store, SettingBrainChain, `["", "  "]`)
	if got := s.BrainChain(ctx); got != nil {
		t.Errorf("blank entries only = %v, want nil", got)
	}
}

func TestBrainChain_UnprovisionedBehaviorUnchanged(t *testing.T) {
	// No chain setting at all + smith.model resident on a slot → exactly the
	// pre-S5 resolution (the regression guard for the default path).
	sch := &chainSched{st: sched.Status{
		Slots:       map[string]string{"a1": "ornith-35b"},
		SlotLabels:  map[string]string{"a1": "A1"},
		IdleSeconds: map[string]*float64{"a1": f64(10)},
	}}
	db := openDB(t)
	seedBrainCatalog(t, db)
	s := New(Deps{Store: db, Settings: db.Settings(), Catalog: db.Catalog(), Sched: sch, Logf: t.Logf})
	setSetting(t, db, SettingModel, `"ornith-35b"`)

	br := s.Brain(context.Background())
	if br.Resolution != BrainLocalSlot || br.Model != "ornith-35b" || br.Slot != "a1" {
		t.Fatalf("Brain = %+v, want unchanged single-model resolution", br)
	}
}
