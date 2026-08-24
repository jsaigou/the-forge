// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// reconcileOrphanedSlotProcedure wraps the existing atomic unload_slot
// operation (smith's dispatchUnloadSlot) as a procedure — the runner
// equivalent of Sprint 3's other two atomic-to-procedure wraps
// (docs/v5-smith.md §13). No maintenance window: unloading one orphaned
// slot is exactly as disruptive as it was as an atomic action.
var reconcileOrphanedSlotProcedure = Procedure{
	ID:    "reconcile_orphaned_slot",
	Title: "Reconcile an orphaned slot",
	Impact: Impact{
		NeedsMaintenance: false,
		EstDuration:      30 * time.Second,
	},
	Params: []Param{{Name: "slot", Allowed: shallowTokenAllowed}},
	Steps: []Step{
		{
			Title:          "Unload the orphaned slot",
			Why:            "the slot's real engine/scheduler state disagrees with what's actually loaded — an unload lets the next placement start clean.",
			Op:             "unload_slot",
			Timeout:        60 * time.Second,
			VerifyCheckIDs: []string{"slot_agreement"},
			OnFail:         FailAbort,
		},
	},
}
