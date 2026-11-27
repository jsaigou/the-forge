// SPDX-License-Identifier: Apache-2.0

package speechsetup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

type fakeHost struct {
	engines   []store.Occupant
	defaults  map[string]string
	slots     []specialist.SlotState
	frontDoor string
	free      int64
	did       []string // every change, in order
}

func (h *fakeHost) Engines(context.Context) ([]store.Occupant, error) {
	return slices.Clone(h.engines), nil
}
func (h *fakeHost) SpeechDefaults(context.Context) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range h.defaults {
		out[k] = v
	}
	return out, nil
}
func (h *fakeHost) SetSpeechDefault(_ context.Context, mode, engine string) error {
	if h.defaults == nil {
		h.defaults = map[string]string{}
	}
	h.defaults[mode] = engine
	h.did = append(h.did, "default "+mode+"="+engine)
	return nil
}
func (h *fakeHost) Slots(context.Context) ([]specialist.SlotState, error) {
	return slices.Clone(h.slots), nil
}
func (h *fakeHost) AddSlot(context.Context) (string, error) {
	name := fmt.Sprintf("s%d", len(h.slots)+1)
	h.slots = append(h.slots, specialist.SlotState{Slot: name, State: specialist.StateEmpty})
	h.did = append(h.did, "slot "+name)
	return name, nil
}
func (h *fakeHost) Create(_ context.Context, o store.Occupant) error {
	h.engines = append(h.engines, o)
	h.did = append(h.did, "create "+o.Name+" "+o.Policy+" "+o.HomeSlot)
	return nil
}
func (h *fakeHost) SetHome(_ context.Context, engine, slot string) error {
	for i := range h.engines {
		if h.engines[i].Name == engine {
			h.engines[i].HomeSlot = slot
		}
	}
	h.did = append(h.did, "home "+engine+"="+slot)
	return nil
}
func (h *fakeHost) EnsureFrontDoorURL(_ context.Context, url string) (bool, error) {
	if h.frontDoor != "" {
		return false, nil
	}
	h.frontDoor = url
	h.did = append(h.did, "front door")
	return true, nil
}
func (h *fakeHost) FreeBytes() int64 { return h.free }

func register(t *testing.T, h *fakeHost, root, id string) Result {
	t.Helper()
	h.did = nil
	res, err := Register(context.Background(), h, root, id)
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	return res
}

func wantDid(t *testing.T, h *fakeHost, want ...string) {
	t.Helper()
	if !slices.Equal(h.did, want) {
		t.Fatalf("changes:\n got %q\nwant %q", h.did, want)
	}
}

func TestRegister_FromNothingToWorkingAndAgainChangesNothing(t *testing.T) {
	root := t.TempDir()
	install(t, root, RecipeKokoro)
	install(t, root, RecipeZonos)
	h := &fakeHost{free: 64 * gib}

	res := register(t, h, root, RecipeKokoro)
	wantDid(t, h, "slot s1", "create kokoro always_on s1", "default kokoro=kokoro", "front door")
	if !res.Created || res.SlotAdded != "s1" || !res.DefaultSet || !res.FrontDoorSet || res.Policy != store.PolicyAlwaysOn {
		t.Fatalf("result: %+v", res)
	}
	if h.frontDoor != DefaultFrontDoorURL {
		t.Fatalf("front door: %q", h.frontDoor)
	}
	// A second always-on engine does not take the first one's slot.
	register(t, h, root, RecipeZonos)
	wantDid(t, h, "slot s2", "create zonos3-mini-clone always_on s2", "default base=zonos3-mini-clone")

	for _, id := range []string{RecipeKokoro, RecipeZonos} {
		res := register(t, h, root, id)
		wantDid(t, h)
		if res.Created || res.SlotAdded != "" || res.DefaultSet || res.FrontDoorSet {
			t.Fatalf("%s again: %+v", id, res)
		}
	}
}

func TestRegister_LittleMemoryMeansOnDemandAndSharedSlots(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{RecipeZonos, RecipeQwenVoiceDesign, RecipeQwenCustomVoice} {
		install(t, root, id)
	}
	h := &fakeHost{free: 1 * gib, frontDoor: "http://elsewhere:8082/v1"}
	register(t, h, root, RecipeZonos)
	wantDid(t, h, "slot s1", "create zonos3-mini-clone on_demand ", "default base=zonos3-mini-clone")
	register(t, h, root, RecipeQwenVoiceDesign)
	wantDid(t, h, "create qwen3-tts-voicedesign on_demand ", "default voicedesign=qwen3-tts-voicedesign")
	h.free = 64 * gib // an optional engine stays on demand however much memory there is
	register(t, h, root, RecipeQwenCustomVoice)
	wantDid(t, h, "create qwen3-tts-customvoice on_demand ", "default customvoice=qwen3-tts-customvoice")
}

func TestRegister_AnOnDemandEngineNeedsASlotNobodyOwns(t *testing.T) {
	root := t.TempDir()
	install(t, root, RecipeQwenBase)
	owner := engine("kokoro", "kokoro", "", store.PolicyAlwaysOn, "s1")
	h := &fakeHost{engines: []store.Occupant{owner}, slots: []specialist.SlotState{{Slot: "s1", Occupant: "kokoro", State: specialist.StateRunning}}, frontDoor: "x"}
	register(t, h, root, RecipeQwenBase)
	wantDid(t, h, "slot s2", "create qwen3-tts-base on_demand ", "default base=qwen3-tts-base")
}

func TestRegister_RespectsWhatTheOperatorChose(t *testing.T) {
	root := t.TempDir() // empty: an engine that is registered needs no recipe files
	off := engine("qwen3-tts-base", "qwen-tts-server", "base", store.PolicyAlwaysOn, "s9")
	off.Enabled = false
	h := &fakeHost{engines: []store.Occupant{off}, frontDoor: "x"}
	res := register(t, h, root, RecipeQwenBase)
	wantDid(t, h)
	if res.Enabled || res.Created {
		t.Fatalf("a disabled engine stays as it is: %+v", res)
	}

	// Another default of the same kind stays.
	zonos := engine("zonos3-mini-clone", specialist.KindZonos3Server, "", store.PolicyOnDemand, "")
	qb := engine("qwen3-tts-base", "qwen-tts-server", "base", store.PolicyOnDemand, "")
	h = &fakeHost{engines: []store.Occupant{zonos, qb}, defaults: map[string]string{"base": "zonos3-mini-clone"},
		slots: []specialist.SlotState{{Slot: "s1", State: specialist.StateEmpty}}, frontDoor: "x"}
	register(t, h, root, RecipeQwenBase)
	wantDid(t, h)
	// A default that names nothing is replaced.
	h.defaults["base"] = "deleted-long-ago"
	register(t, h, root, RecipeQwenBase)
	wantDid(t, h, "default base=qwen3-tts-base")
}

func TestRegister_RepairsAnEngineWithNowhereToRun(t *testing.T) {
	k := engine("kokoro", "kokoro", "", store.PolicyAlwaysOn, "s7")
	h := &fakeHost{engines: []store.Occupant{k}, defaults: map[string]string{"kokoro": "kokoro"}, frontDoor: "x"}
	res := register(t, h, t.TempDir(), RecipeKokoro)
	wantDid(t, h, "slot s1", "home kokoro=s1")
	if res.SlotAdded != "s1" || res.Created {
		t.Fatalf("result: %+v", res)
	}
}

func TestRegister_Refusals(t *testing.T) {
	root := t.TempDir()
	h := &fakeHost{free: 64 * gib}
	if _, err := Register(context.Background(), h, root, "nope"); !errors.Is(err, ErrUnknownRecipe) {
		t.Errorf("unknown: %v", err)
	}
	var missing *FilesMissingError
	if _, err := Register(context.Background(), h, root, RecipeKokoro); !errors.As(err, &missing) || len(missing.Problems) != 4 {
		t.Errorf("missing files: %v", err)
	}
	install(t, root, RecipeKokoro)
	h.engines = []store.Occupant{{Name: "kokoro", Kind: "llama-server", Enabled: true}}
	var taken *NameTakenError
	if _, err := Register(context.Background(), h, root, RecipeKokoro); !errors.As(err, &taken) {
		t.Errorf("name taken: %v", err)
	}
	wantDid(t, h)
}
