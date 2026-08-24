// SPDX-License-Identifier: Apache-2.0

package router

import (
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// TestGroupOfferingsByModel_TieBreak covers the tie-break settled here
// (priority, then provider name, then offering id) — the one behavior
// change from the pre-extraction code, which relied on ListOfferings' own
// SQL order (priority, provider name, wire_model) instead. Two offerings
// tying on both priority and provider name must resolve by id.
func TestGroupOfferingsByModel_TieBreak(t *testing.T) {
	offerings := []store.Offering{
		{ID: 5, ModelID: 1, ProviderID: 1, ProviderName: "qwen", Priority: 100, WireModel: "z-model"},
		{ID: 2, ModelID: 1, ProviderID: 1, ProviderName: "qwen", Priority: 100, WireModel: "a-model"},
		{ID: 9, ModelID: 1, ProviderID: 2, ProviderName: "aiand", Priority: 10, WireModel: "aiand-model"},
	}
	groups := GroupOfferingsByModel(offerings)
	group := groups[1]
	if len(group) != 3 {
		t.Fatalf("group size = %d, want 3", len(group))
	}
	// aiand (priority 10) always wins regardless of wire_model/id.
	if group[0].ProviderName != "aiand" {
		t.Errorf("group[0].ProviderName = %q, want aiand (lowest priority)", group[0].ProviderName)
	}
	// The two qwen offerings tie on (priority, provider name) — id breaks
	// the tie (2 before 5), NOT wire_model (which would put a-model first).
	if group[1].ID != 2 || group[2].ID != 5 {
		t.Errorf("qwen tie-break order = [%d, %d], want [2, 5] (offering id, not wire_model)", group[1].ID, group[2].ID)
	}
}

// TestGroupOfferingsByModel_SeparatesModels confirms offerings for
// different catalog models never bleed into each other's group.
func TestGroupOfferingsByModel_SeparatesModels(t *testing.T) {
	offerings := []store.Offering{
		{ID: 1, ModelID: 1, ProviderName: "a"},
		{ID: 2, ModelID: 2, ProviderName: "b"},
	}
	groups := GroupOfferingsByModel(offerings)
	if len(groups) != 2 || len(groups[1]) != 1 || len(groups[2]) != 1 {
		t.Fatalf("groups = %+v, want two singleton groups", groups)
	}
}

func TestSelectOfferingChain_NoFailoverReturnsOnlyPrimary(t *testing.T) {
	group := []store.Offering{
		{ID: 1, ProviderID: 1, Priority: 10, Enabled: true},
		{ID: 2, ProviderID: 2, Priority: 20, Enabled: true},
	}
	enabled := map[int64]bool{1: true, 2: true}
	chain := SelectOfferingChain(group, enabled, false)
	if len(chain) != 1 || chain[0].ID != 1 {
		t.Fatalf("chain = %+v, want just offering 1 (failover off)", chain)
	}
}

func TestSelectOfferingChain_FailoverReturnsWholeRoutableChain(t *testing.T) {
	group := []store.Offering{
		{ID: 1, ProviderID: 1, Priority: 10, Enabled: true},
		{ID: 2, ProviderID: 2, Priority: 20, Enabled: true},
	}
	enabled := map[int64]bool{1: true, 2: true}
	chain := SelectOfferingChain(group, enabled, true)
	if len(chain) != 2 || chain[0].ID != 1 || chain[1].ID != 2 {
		t.Fatalf("chain = %+v, want [1, 2] in priority order", chain)
	}
}

// TestSelectOfferingChain_SkipsDisabledOfferingAndProvider covers both
// exclusion reasons independently: an offering can be disabled while its
// provider is enabled, or vice versa — either alone must drop the entry.
func TestSelectOfferingChain_SkipsDisabledOfferingAndProvider(t *testing.T) {
	group := []store.Offering{
		{ID: 1, ProviderID: 1, Priority: 10, Enabled: false}, // offering disabled
		{ID: 2, ProviderID: 2, Priority: 20, Enabled: true},  // provider disabled (not in enabled map)
		{ID: 3, ProviderID: 3, Priority: 30, Enabled: true},  // routable
	}
	enabled := map[int64]bool{1: true, 3: true}
	chain := SelectOfferingChain(group, enabled, true)
	if len(chain) != 1 || chain[0].ID != 3 {
		t.Fatalf("chain = %+v, want only offering 3", chain)
	}
}

func TestSelectOfferingChain_NothingRoutableReturnsNil(t *testing.T) {
	group := []store.Offering{{ID: 1, ProviderID: 1, Enabled: false}}
	if chain := SelectOfferingChain(group, map[int64]bool{1: true}, true); chain != nil {
		t.Errorf("chain = %+v, want nil", chain)
	}
}
