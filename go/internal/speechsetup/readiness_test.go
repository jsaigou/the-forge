// SPDX-License-Identifier: Apache-2.0

package speechsetup

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

func engine(name, kind, alias, policy, home string) store.Occupant {
	o := store.Occupant{Name: name, Kind: kind, Functions: []string{store.FnSpeech}, Policy: policy, HomeSlot: home, Enabled: true, Params: map[string]string{}}
	if alias != "" {
		o.Params["alias"] = alias
	}
	return o
}

func capByID(t *testing.T, r Report, id string) Capability {
	t.Helper()
	for _, c := range r.Capabilities {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no capability %q in %+v", id, r)
	return Capability{}
}

func TestEvaluate_EmptyNodeCanDoNothingAndSaysWhatToDo(t *testing.T) {
	r := Evaluate(Input{})
	if r.Ready || r.Usable || len(r.Capabilities) != 3 {
		t.Fatalf("empty node: %+v", r)
	}
	for _, c := range r.Capabilities {
		if c.State != StateNotRegistered || c.Code != CodeNotRegistered || c.Action != ActionRegister || c.Usable {
			t.Errorf("%s: %+v", c.ID, c)
		}
	}
}

func TestEvaluate_CapabilitiesFailIndependently(t *testing.T) {
	off := engine("design-a", "qwen-tts-server", "voicedesign", store.PolicyOnDemand, "")
	off.Enabled = false
	r := Evaluate(Input{
		Engines: []store.Occupant{engine("kokoro", "kokoro", "", store.PolicyAlwaysOn, "s1"), off},
		Slots:   []specialist.SlotState{{Slot: "s1", Occupant: "kokoro", State: specialist.StateRunning}},
	})
	if r.Ready || !r.Usable {
		t.Fatalf("want usable but not ready: %+v", r)
	}
	if c := capByID(t, r, CapPreset); c.State != StateReady || !c.Usable || c.Engine != "kokoro" || c.Code != "" {
		t.Errorf("preset: %+v", c)
	}
	if c := capByID(t, r, CapClone); c.State != StateNotRegistered {
		t.Errorf("clone: %+v", c)
	}
	if c := capByID(t, r, CapDesign); c.State != StateDisabled || c.Action != ActionEnable || c.Engine != "design-a" {
		t.Errorf("design: %+v", c)
	}
}

func TestEvaluate_SlotStates(t *testing.T) {
	zonos := engine("z", specialist.KindZonos3Server, "", store.PolicyAlwaysOn, "s1")
	for _, tc := range []struct {
		slot   specialist.SlotState
		state  string
		usable bool
	}{
		{specialist.SlotState{Slot: "s1", Occupant: "z", State: specialist.StateRunning}, StateReady, true},
		{specialist.SlotState{Slot: "s1", Occupant: "z", State: specialist.StateStarting}, StateStarting, true},
		{specialist.SlotState{Slot: "s1", Occupant: "z", State: specialist.StateFailed, Err: "boom"}, StateFailed, false},
		{specialist.SlotState{Slot: "s1", Occupant: "z", State: specialist.StateStopped}, StateStopped, false},
		{specialist.SlotState{Slot: "s1", State: specialist.StateEmpty}, StateStarting, true}, // the daemon has not filled it yet
	} {
		c := capByID(t, Evaluate(Input{Engines: []store.Occupant{zonos}, Slots: []specialist.SlotState{tc.slot}}), CapClone)
		if c.State != tc.state || c.Usable != tc.usable {
			t.Errorf("slot %+v: got %s usable=%v, want %s usable=%v", tc.slot, c.State, c.Usable, tc.state, tc.usable)
		}
		if tc.state == StateFailed && (c.Detail != "boom" || c.Action != ActionCheckLog) {
			t.Errorf("failed: %+v", c)
		}
	}
}

func TestEvaluate_NowhereToRunIsNotReady(t *testing.T) {
	onDemand := engine("d", "qwen-tts-server", "voicedesign", store.PolicyOnDemand, "")
	if c := capByID(t, Evaluate(Input{Engines: []store.Occupant{onDemand}}), CapDesign); c.State != StateNoSlot || c.Action != ActionAddSlot || c.Usable {
		t.Errorf("no slots at all: %+v", c)
	}
	// The only slot is reserved by an always-on member that is not a speech engine.
	other := store.Occupant{Name: "emb", Kind: "llama-server", Functions: []string{"embeddings"}, Policy: store.PolicyAlwaysOn, HomeSlot: "s1", Enabled: true}
	slots := []specialist.SlotState{{Slot: "s1", Occupant: "emb", State: specialist.StateRunning}}
	if c := capByID(t, Evaluate(Input{Engines: []store.Occupant{onDemand, other}, Slots: slots}), CapDesign); c.State != StateNoSlot {
		t.Errorf("every slot reserved: %+v", c)
	}
	slots = append(slots, specialist.SlotState{Slot: "s2", State: specialist.StateEmpty})
	if c := capByID(t, Evaluate(Input{Engines: []store.Occupant{onDemand, other}, Slots: slots}), CapDesign); c.State != StateReady || !c.Usable {
		t.Errorf("a free slot: %+v", c)
	}
	// An always-on engine whose home slot does not exist.
	home := engine("z", specialist.KindZonos3Server, "", store.PolicyAlwaysOn, "s9")
	if c := capByID(t, Evaluate(Input{Engines: []store.Occupant{home}, Slots: slots}), CapClone); c.State != StateNoSlot {
		t.Errorf("missing home slot: %+v", c)
	}
}

func TestEvaluate_FollowsTheDefaultTheRequestWouldUse(t *testing.T) {
	a, b := engine("a", "qwen-tts-server", "base", store.PolicyOnDemand, ""), engine("b", specialist.KindZonos3Server, "", store.PolicyOnDemand, "")
	slots := []specialist.SlotState{{Slot: "s1", Occupant: "b", State: specialist.StateFailed, Err: "x"}, {Slot: "s2", State: specialist.StateEmpty}}
	if c := capByID(t, Evaluate(Input{Engines: []store.Occupant{a, b}, Defaults: map[string]string{"base": "b"}, Slots: slots}), CapClone); c.Engine != "b" || c.State != StateFailed {
		t.Errorf("the default decides, even when it is broken: %+v", c)
	}
	if c := capByID(t, Evaluate(Input{Engines: []store.Occupant{a, b}, Slots: slots}), CapClone); c.Engine != "a" || !c.Usable {
		t.Errorf("no default: the first usable engine by name: %+v", c)
	}
}
