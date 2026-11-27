// SPDX-License-Identifier: Apache-2.0

package speechsetup

// Register and activate (docs/speech-setup-plan.md section 2.3): turn files that are already on the host into a working engine. One implementation for
// the web page, the command line and Smith. It adds only what is missing, so running it again changes nothing, and it never overrides a choice the
// operator made (a disabled engine stays disabled, an existing default stays the default).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// DefaultFrontDoorURL is where a forge-tts on this host answers.
const DefaultFrontDoorURL = "http://127.0.0.1:8082/v1"

// Recipe states on this host.
const (
	RecipeRegistered = "registered" // an engine of this sort is registered
	RecipePresent    = "present"    // the files are here and can be registered
	RecipeAbsent     = "absent"     // some file is missing
)

// RecipeStatus is a recipe as this host has it.
type RecipeStatus struct {
	ID            string        `json:"id"`
	Label         string        `json:"label"`
	Mode          string        `json:"mode"`
	State         string        `json:"state"`
	Engine        string        `json:"engine,omitempty"` // the registered engine, when there is one
	MemoryBytes   int64         `json:"memory_bytes"`
	DownloadBytes int64         `json:"download_bytes"`
	License       string        `json:"license"`
	Problems      []FileProblem `json:"problems,omitempty"`
	// Installable: `forge speech install` can build this engine.
	Installable bool `json:"installable"`
	// InstallCommand is what to run on the server for an installable engine that is absent; only the daemon knows it.
	InstallCommand string `json:"install_command,omitempty"`
}

func sortProblems(p []FileProblem) {
	sort.Slice(p, func(i, j int) bool { return p[i].Path < p[j].Path })
}

// registeredEngine picks the engine that already is r's: the one under the recipe's own name, else the default for its mode, else the first by name.
func registeredEngine(r Recipe, engines []store.Occupant, defaults map[string]string) (store.Occupant, bool) {
	var matches []store.Occupant
	for _, o := range engines {
		if r.Matches(o) {
			matches = append(matches, o)
		}
	}
	if len(matches) == 0 {
		return store.Occupant{}, false
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
	for _, want := range []string{r.Engine, defaults[r.Mode]} {
		for _, o := range matches {
			if o.Name == want {
				return o, true
			}
		}
	}
	return matches[0], true
}

// Statuses reports every recipe on a host whose engines live under root.
func Statuses(root string, engines []store.Occupant, defaults map[string]string) []RecipeStatus {
	out := make([]RecipeStatus, 0, len(recipes))
	for _, r := range recipes {
		s := RecipeStatus{ID: r.ID, Label: r.Label, Mode: r.Mode, MemoryBytes: r.MemoryBytes, DownloadBytes: r.DownloadBytes(), License: r.License,
			Installable: r.Setup != nil}
		if o, ok := registeredEngine(r, engines, defaults); ok {
			s.State, s.Engine = RecipeRegistered, o.Name
		} else if s.Problems = r.Check(root); len(s.Problems) == 0 {
			s.State, s.Problems = RecipePresent, nil
		} else {
			s.State = RecipeAbsent
		}
		out = append(out, s)
	}
	return out
}

// Host is what registration changes. The daemon implements it over the same rules its API uses.
type Host interface {
	Engines(ctx context.Context) ([]store.Occupant, error)
	// SpeechDefaults maps a kind of speech to the engine that renders it by default.
	SpeechDefaults(ctx context.Context) (map[string]string, error)
	SetSpeechDefault(ctx context.Context, mode, engine string) error
	// Slots lists the pool's slots in order.
	Slots(ctx context.Context) ([]specialist.SlotState, error)
	AddSlot(ctx context.Context) (string, error)
	Create(ctx context.Context, o store.Occupant) error
	// SetHome gives an always-on engine a new home slot.
	SetHome(ctx context.Context, engine, slot string) error
	// EnsureFrontDoorURL records where forge-tts answers, if nothing is recorded yet.
	EnsureFrontDoorURL(ctx context.Context, url string) (changed bool, err error)
	// FreeBytes is the memory an engine could be loaded into now; 0 when that cannot be read (the implementation says why in its log), which makes a
	// new engine on demand.
	FreeBytes() int64
}

// ErrUnknownRecipe: no recipe has that id.
var ErrUnknownRecipe = errors.New("no such speech engine recipe")

// FilesMissingError: the engine cannot be registered because its files are not all here.
type FilesMissingError struct {
	Recipe   string
	Problems []FileProblem
}

func (e *FilesMissingError) Error() string {
	names := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		names = append(names, p.Path+" ("+p.Problem+")")
	}
	return fmt.Sprintf("the files of %s are incomplete: %s", e.Recipe, strings.Join(names, ", "))
}

// NameTakenError: another sort of engine is registered under the name the recipe uses.
type NameTakenError struct{ Engine string }

func (e *NameTakenError) Error() string {
	return fmt.Sprintf("a different engine is already registered as %s; rename or delete it first", e.Engine)
}

// Result says what a registration did.
type Result struct {
	Recipe  string `json:"recipe"`
	Engine  string `json:"engine"`
	Created bool   `json:"created"` // false: the engine was registered already
	Policy  string `json:"policy"`
	Enabled bool   `json:"enabled"`
	// SlotAdded names the pool slot that had to be added.
	SlotAdded    string `json:"slot_added,omitempty"`
	DefaultSet   bool   `json:"default_set"`
	FrontDoorSet bool   `json:"front_door_set"`
}

// Register makes recipe id's engine usable on a host whose engines live under root.
func Register(ctx context.Context, h Host, root, id string) (Result, error) {
	r, ok := RecipeByID(id)
	if !ok {
		return Result{}, ErrUnknownRecipe
	}
	engines, err := h.Engines(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list engines: %w", err)
	}
	defaults, err := h.SpeechDefaults(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("read speech defaults: %w", err)
	}
	slots, err := h.Slots(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("list pool slots: %w", err)
	}
	res := Result{Recipe: r.ID}

	o, exists := registeredEngine(r, engines, defaults)
	if !exists {
		if problems := r.Check(root); len(problems) > 0 {
			return Result{}, &FilesMissingError{Recipe: r.ID, Problems: problems}
		}
		for _, other := range engines {
			if other.Name == r.Engine {
				return Result{}, &NameTakenError{Engine: r.Engine}
			}
		}
		o = r.Occupant(root)
		if r.Keep == KeepAlways || (r.Keep == KeepWhenMemoryAllows && h.FreeBytes() >= r.MemoryBytes) {
			o.Policy = store.PolicyAlwaysOn
		}
	}

	// A slot to run in. An always-on engine owns one; the on demand engines share those nobody owns. A disabled engine needs none.
	reserved := map[string]bool{}
	for _, other := range engines {
		if other.Name != o.Name && other.Enabled && other.Policy == store.PolicyAlwaysOn {
			reserved[other.HomeSlot] = true
		}
	}
	usable := func(slot string) bool { return slot != "" && !reserved[slot] && hasSlot(slots, slot) }
	need := o.Enabled && (o.Policy != store.PolicyAlwaysOn || !usable(o.HomeSlot))
	if need {
		pick := ""
		for _, s := range slots {
			// An always-on engine takes only a slot that holds nothing; an on demand one can share.
			if !reserved[s.Slot] && (o.Policy != store.PolicyAlwaysOn || s.Occupant == "" || s.Occupant == o.Name) {
				pick = s.Slot
				break
			}
		}
		if pick == "" {
			if pick, err = h.AddSlot(ctx); err != nil {
				return Result{}, fmt.Errorf("add a pool slot: %w", err)
			}
			res.SlotAdded = pick
		}
		if o.Policy == store.PolicyAlwaysOn {
			o.HomeSlot = pick
			if exists {
				if err := h.SetHome(ctx, o.Name, pick); err != nil {
					return res, fmt.Errorf("give %s a home slot: %w", o.Name, err)
				}
			}
		}
	}

	if !exists {
		if err := h.Create(ctx, o); err != nil {
			return res, fmt.Errorf("register %s: %w", o.Name, err)
		}
		res.Created = true
		engines = append(engines, o)
	}
	res.Engine, res.Policy, res.Enabled = o.Name, o.Policy, o.Enabled

	// The default for its kind of speech, only if that kind has none that exists.
	if o.Enabled && !validDefault(engines, r.Mode, defaults[r.Mode]) {
		if err := h.SetSpeechDefault(ctx, r.Mode, o.Name); err != nil {
			return res, fmt.Errorf("make %s the default: %w", o.Name, err)
		}
		res.DefaultSet = true
	}
	if res.FrontDoorSet, err = h.EnsureFrontDoorURL(ctx, DefaultFrontDoorURL); err != nil {
		return res, fmt.Errorf("record the speech front door's address: %w", err)
	}
	return res, nil
}

func validDefault(engines []store.Occupant, mode, name string) bool {
	for _, o := range engines {
		if got, ok := specialist.SpeechModeOf(o); ok && got == mode && o.Name == name {
			return true
		}
	}
	return false
}
