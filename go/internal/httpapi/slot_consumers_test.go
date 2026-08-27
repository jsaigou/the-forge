// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
	"github.com/jsaigou/the-forge/internal/collector"
)

// TestStatusResponseSlotConsumers: buildStatusResponse surfaces the shared
// activity registry's fresh, non-empty labels as slot_consumers (additive,
// omitempty — absent entirely when the registry is unwired or everything is
// stale), keyed by slot id like Slots/SlotActivity.
func TestStatusResponseSlotConsumers(t *testing.T) {
	s := newTestServer(t)
	s.deps.Activity = activity.New()

	snap := &collector.Snapshot{
		TakenAt:  time.Now(),
		Hostname: "test-host",
		Units:    map[string]collector.UnitState{},
		Slots: map[string]collector.SlotState{
			"a1": {Slot: "a1", Label: "A1", Mode: "qwen3"},
			"a2": {Slot: "a2", Label: "A2", Mode: "swallow"},
		},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}
	s.deps.Snapshots = collector.NewStatic(snap)

	resp := s.buildStatusResponse()
	if resp.SlotConsumers != nil {
		t.Errorf("unmarked slots → SlotConsumers should be nil/absent, got %+v", resp.SlotConsumers)
	}

	s.deps.Activity.Mark("a1", "Examplehost (OpenCode)")
	s.deps.Activity.Mark("a2", "SMITH")
	resp = s.buildStatusResponse()
	if got := resp.SlotConsumers["a1"]; got != "Examplehost (OpenCode)" {
		t.Errorf("slot_consumers[a1] = %q, want Examplehost (OpenCode)", got)
	}
	if got := resp.SlotConsumers["a2"]; got != "SMITH" {
		t.Errorf("slot_consumers[a2] = %q, want SMITH", got)
	}
}
