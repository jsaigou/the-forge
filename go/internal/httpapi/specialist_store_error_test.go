// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// failingListSpecialist is the real store with ListOccupants switched to fail on demand.
type failingListSpecialist struct {
	store.Specialist
	fail atomic.Bool
}

func (f *failingListSpecialist) ListOccupants(ctx context.Context) ([]store.Occupant, error) {
	if f.fail.Load() {
		return nil, errors.New("disk I/O error")
	}
	return f.Specialist.ListOccupants(ctx)
}

// The one-owner and home-slot rules read the other occupants. When that read fails the write must be refused: a failed check is not a passed check.
func TestSpecialistAPI_StoreReadFailureRefusesTheWrite(t *testing.T) {
	s, _ := newSpecialistServer(t, authz.RoleAdmin)
	real := s.deps.SpecialistStore.(store.Specialist)
	wrapped := &failingListSpecialist{Specialist: real}
	s.deps.SpecialistStore = wrapped
	good := specialist.LaunchSpec{Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/d1.gguf", Alias: "d1-2b", Backend: "vulkan", Ctx: 128000}
	s.deps.SpecialistCatalog = fakeSpecialistCatalog{specs: map[string]specialist.LaunchSpec{"d1-2b": good}}

	catalogBody := func(name string) string {
		return `{"name":"` + name + `","kind":"catalog-config","functions":["decisions"],"policy":"on_demand","enabled":true,"params":{"config":"d1-2b"}}`
	}
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", catalogBody("d1-member")); code != 201 {
		t.Fatalf("first owner: %d %v", code, out)
	}

	wrapped.fail.Store(true)

	// One-owner rule: a second occupant for the same config must not slip through while the owners cannot be listed.
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", catalogBody("d1-again")); code != 500 || out["code"] != "internal" {
		t.Errorf("one-owner check with a failing store: %d %v, want 500 internal", code, out)
	}
	// Home-slot rule: same for an always-on member claiming a home.
	always := `{"name":"tts","kind":"kokoro","functions":["speech"],"policy":"always_on","home_slot":"s1","enabled":true}`
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", always); code != 500 || out["code"] != "internal" {
		t.Errorf("home-slot check with a failing store: %d %v, want 500 internal", code, out)
	}

	wrapped.fail.Store(false)
	list, err := real.ListOccupants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "d1-member" {
		t.Errorf("a refused write must leave no occupant behind: %v", list)
	}
}
