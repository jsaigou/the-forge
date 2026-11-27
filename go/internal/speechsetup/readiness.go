// SPDX-License-Identifier: Apache-2.0

// Package speechsetup answers "can this node speak, and if not, what is missing?" (docs/speech-setup-plan.md section 2.2).
//
// The report is per capability, because they fail independently: preset voices, cloning and voice design each need an engine of their own kind. It is
// built from what the daemon already knows (registered engines, the defaults, the pool's slots) and from whether the engine's files are there. Building
// it never loads, starts or probes an engine and reads no file contents.
//
// The package also holds the engine recipes (recipes.go) and the one way to register an engine from files already on the host (register.go).
package speechsetup

import (
	"errors"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// Capability ids. They are part of the API: voice-studio and the CLI key their wording on them.
const (
	CapPreset = "preset" // built-in voices, nothing to record
	CapClone  = "clone"  // a voice from a reference recording
	CapDesign = "design" // a voice described in words
)

// States of a capability.
const (
	StateReady         = "ready"
	StateStarting      = "starting"       // usable: a request waits for the engine
	StateNotInstalled  = "not_installed"  // no engine of the kind exists and its files are not here
	StateFilesMissing  = "files_missing"  // an engine is registered but some of its files are gone
	StateNotRegistered = "not_registered" // no engine of the kind exists
	StateNoSlot        = "no_slot"        // an engine exists but the pool has nowhere to run it
	StateDisabled      = "disabled"
	StateStopped       = "stopped"
	StateFailed        = "failed"
	StateUnknown       = "unknown" // the reporter could not find out (used by forge-tts when the daemon does not answer)
)

// Stable codes for a capability that is not simply ready, and the one next action for each.
const (
	CodeNotInstalled  = "speech_not_installed"
	CodeFilesMissing  = "speech_files_missing"
	CodeNotRegistered = "speech_not_registered"
	CodeNoSlot        = "speech_no_slot"
	CodeDisabled      = "speech_engine_disabled"
	CodeStopped       = "speech_engine_stopped"
	CodeFailed        = "speech_engine_failed"
	CodeStarting      = "speech_engine_starting"

	ActionInstall  = "install_engine"
	ActionRestore  = "restore_files"
	ActionRegister = "register_engine"
	ActionAddSlot  = "add_slot"
	ActionEnable   = "enable_engine"
	ActionStart    = "start_engine"
	ActionCheckLog = "check_engine_log"
	ActionWait     = "wait"
)

// capabilities pairs each capability with the kind of speech engine that provides it, in display order.
var capabilities = []struct{ id, mode string }{
	{CapPreset, specialist.SpeechKokoro},
	{CapClone, specialist.SpeechBase},
	{CapDesign, specialist.SpeechVoiceDesign},
}

// Capability is one thing a person can do with speech, and whether it works now.
type Capability struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	State string `json:"state"`
	// Usable is whether a request for this capability can succeed now. A starting engine is usable: the request waits for it.
	Usable bool   `json:"usable"`
	Engine string `json:"engine,omitempty"`
	// Recipe names the recipe to register or install when the kind has no engine.
	Recipe string `json:"recipe,omitempty"`
	Code   string `json:"code,omitempty"`
	Action string `json:"action,omitempty"`
	// Detail is the engine's last start error or the missing files. It names files on the host, so it is only given to signed-in operators.
	Detail string `json:"detail,omitempty"`
}

// Report is the node's speech readiness.
type Report struct {
	// Ready is every capability usable; Usable is at least one.
	Ready        bool         `json:"ready"`
	Usable       bool         `json:"usable"`
	Capabilities []Capability `json:"capabilities"`
}

// Input is what the daemon knows.
type Input struct {
	Engines  []store.Occupant       // every registered occupant, speech or not (the others matter: they can hold the slots)
	Defaults map[string]string      // speech kind -> engine name
	Slots    []specialist.SlotState // the pool's slots
	// Recipes is what the host has of each recipe (Statuses); nil when the caller did not look, and then files are not part of the answer.
	Recipes []RecipeStatus
	// Missing lists, by engine name, the files a registered engine needs and does not have (EngineFiles).
	Missing map[string][]FileProblem
}

// Inspect looks at a host whose engines live under root and builds the report: which recipes' files are there, which registered speech engines have
// lost files, and then Evaluate.
func Inspect(root string, engines []store.Occupant, defaults map[string]string, slots []specialist.SlotState) Report {
	missing := map[string][]FileProblem{}
	for _, o := range engines {
		if _, speech := specialist.SpeechModeOf(o); !speech {
			continue
		}
		if problems := EngineFiles(root, o); len(problems) > 0 {
			missing[o.Name] = problems
		}
	}
	return Evaluate(Input{Engines: engines, Defaults: defaults, Slots: slots, Recipes: Statuses(root, engines, defaults), Missing: missing})
}

// Evaluate builds the report.
func Evaluate(in Input) Report {
	r := Report{Ready: true, Capabilities: make([]Capability, 0, len(capabilities))}
	for _, c := range capabilities {
		got := evaluateOne(in, c.id, c.mode)
		r.Ready = r.Ready && got.Usable
		r.Usable = r.Usable || got.Usable
		r.Capabilities = append(r.Capabilities, got)
	}
	return r
}

func evaluateOne(in Input, id, mode string) Capability {
	c := Capability{ID: id, Mode: mode}
	set := func(state, code, action string, usable bool) Capability {
		c.State, c.Code, c.Action, c.Usable = state, code, action, usable
		return c
	}
	o, err := specialist.DefaultSpeechEngine(in.Engines, in.Defaults, mode)
	if err != nil {
		var unavail *specialist.UnavailableError
		if errors.As(err, &unavail) {
			c.Engine = unavail.Occupant
			if unavail.Reason == specialist.ReasonDisabled {
				return set(StateDisabled, CodeDisabled, ActionEnable, false)
			}
			return set(StateStopped, CodeStopped, ActionStart, false)
		}
		return withoutEngine(in, mode, set)
	}
	c.Engine = o.Name
	if missing := in.Missing[o.Name]; len(missing) > 0 && !isUp(in.Slots, o.Name) {
		// A running engine has what it needs already; any other will fail to start.
		c.Detail = (&FilesMissingError{Recipe: o.Name, Problems: missing}).Error()
		return set(StateFilesMissing, CodeFilesMissing, ActionRestore, false)
	}
	for _, s := range in.Slots {
		if s.Occupant != o.Name {
			continue
		}
		switch s.State {
		case specialist.StateRunning:
			return set(StateReady, "", "", true)
		case specialist.StateStarting:
			return set(StateStarting, CodeStarting, ActionWait, true)
		case specialist.StateFailed:
			c.Detail = s.Err
			return set(StateFailed, CodeFailed, ActionCheckLog, false)
		case specialist.StateStopped:
			return set(StateStopped, CodeStopped, ActionStart, false)
		}
	}
	// Not loaded. An always-on engine lives in its home slot, which the daemon fills by itself; an on demand engine needs a slot nothing has reserved.
	if o.Policy == store.PolicyAlwaysOn {
		if !hasSlot(in.Slots, o.HomeSlot) {
			return set(StateNoSlot, CodeNoSlot, ActionAddSlot, false)
		}
		return set(StateStarting, CodeStarting, ActionWait, true)
	}
	if !hasUnreservedSlot(in) {
		return set(StateNoSlot, CodeNoSlot, ActionAddSlot, false)
	}
	return set(StateReady, "", "", true)
}

// withoutEngine says whether a kind of speech with no engine only needs registering (some recipe's files are here) or has to be installed first.
func withoutEngine(in Input, mode string, set func(state, code, action string, usable bool) Capability) Capability {
	first := ""
	for _, r := range in.Recipes {
		if r.Mode != mode {
			continue
		}
		if r.State == RecipePresent {
			c := set(StateNotRegistered, CodeNotRegistered, ActionRegister, false)
			c.Recipe = r.ID
			return c
		}
		if first == "" {
			first = r.ID
		}
	}
	if first == "" {
		return set(StateNotRegistered, CodeNotRegistered, ActionRegister, false)
	}
	c := set(StateNotInstalled, CodeNotInstalled, ActionInstall, false)
	c.Recipe = first
	return c
}

func isUp(slots []specialist.SlotState, engine string) bool {
	for _, s := range slots {
		if s.Occupant == engine && (s.State == specialist.StateRunning || s.State == specialist.StateStarting) {
			return true
		}
	}
	return false
}

func hasSlot(slots []specialist.SlotState, name string) bool {
	for _, s := range slots {
		if s.Slot == name {
			return true
		}
	}
	return false
}

// hasUnreservedSlot reports whether some slot is not the home of an enabled always-on member. Such a slot can take an on demand engine, by evicting an
// idle one if it must.
func hasUnreservedSlot(in Input) bool {
	reserved := map[string]bool{}
	for _, o := range in.Engines {
		if o.Enabled && o.Policy == store.PolicyAlwaysOn && o.HomeSlot != "" {
			reserved[o.HomeSlot] = true
		}
	}
	for _, s := range in.Slots {
		if !reserved[s.Slot] {
			return true
		}
	}
	return false
}
