// SPDX-License-Identifier: Apache-2.0

package httpapi

// Audit reproductions (concurrency / failure handling). Tests only; they demonstrate bugs and are expected to FAIL until the production code is fixed.

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// Regression (was a bug, fixed): always_on while the member's slot is `stopping` (a disable's background stop still running) saved the behavior
// and then failed with a 503 because the manager refuses to load into a slot being unloaded. The handler now skips the load and the supervisor
// starts the member once the stop ends, so the user gets a success, not an error.
func TestAudit_BehaviorAlwaysOnOnAStoppingSlotSavesButFailsAndIsNeverApplied(t *testing.T) {
	s, pool := newSpecialistServer(t, authz.RoleAdmin)
	ctx := context.Background()
	if code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants", asrBody); code != 201 {
		t.Fatalf("create: %d %v", code, out)
	}
	pool.slots[0] = specialist.SlotState{Slot: "s1", Occupant: "asr", State: specialist.StateStopping, Policy: store.PolicyOnDemand}
	pool.loadErr = &specialist.UnavailableError{Occupant: "asr", Reason: "s1 is still being unloaded"}

	code, out, _ := spCall(t, s, "POST", "/api/v1/specialists/occupants/asr/behavior", `{"behavior":"always_on"}`)
	o, _ := s.deps.SpecialistStore.OccupantByName(ctx, "asr")
	if o.Policy == store.PolicyAlwaysOn && o.Enabled && code >= 400 {
		t.Errorf("behavior saved as always_on (enabled, home %s) yet the user got %d %v; the load is not retried by anything", o.HomeSlot, code, out)
	}
}
