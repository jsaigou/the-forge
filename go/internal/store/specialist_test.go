// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
)

func TestSpecialist_OccupantsAndDefaults(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sp := db.Specialist()

	if _, err := sp.CreateOccupant(ctx, Occupant{Name: "kokoro", Kind: "kokoro", Functions: []string{FnSpeech}, Policy: PolicyAlwaysOn, HomeSlot: "s3", Enabled: true,
		Params: map[string]string{"port": "8880"}, MemoryBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	// on_demand never carries a home slot; functions are stored deduplicated in display order.
	if _, err := sp.CreateOccupant(ctx, Occupant{Name: "omni", Kind: "llama-server", Policy: PolicyOnDemand, HomeSlot: "s9", Enabled: true,
		Functions: []string{FnTranscription, FnSpeech, FnSpeech}}); err != nil {
		t.Fatal(err)
	}
	o, err := sp.OccupantByName(ctx, "omni")
	if err != nil || o.HomeSlot != "" || len(o.Functions) != 2 || o.Functions[0] != FnSpeech || o.Functions[1] != FnTranscription {
		t.Fatalf("omni = %+v, %v", o, err)
	}
	k, _ := sp.OccupantByName(ctx, "kokoro")
	if k.Params["port"] != "8880" || k.HomeSlot != "s3" || k.MemoryBytes != 1<<30 || !k.Enabled {
		t.Fatalf("kokoro round trip lost data: %+v", k)
	}

	// Bad input is refused.
	for name, o := range map[string]Occupant{
		"unknown function": {Name: "x", Kind: "k", Functions: []string{"juggling"}},
		"unknown policy":   {Name: "x", Kind: "k", Policy: "sometimes"},
		"no kind":          {Name: "x"},
		"no name":          {Kind: "k"},
	} {
		if _, err := sp.CreateOccupant(ctx, o); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := sp.CreateOccupant(ctx, Occupant{Name: "kokoro", Kind: "kokoro"}); err == nil {
		t.Error("a duplicate name was accepted")
	}

	// Defaults: only a function the occupant serves; deleting the occupant removes its defaults.
	if err := sp.SetDefault(ctx, FnSpeech, "kokoro"); err != nil {
		t.Fatal(err)
	}
	if err := sp.SetDefault(ctx, FnEmbeddings, "kokoro"); err == nil {
		t.Error("a default for a function the occupant does not serve was accepted")
	}
	if err := sp.SetDefault(ctx, FnSpeech, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("default to a missing occupant: %v", err)
	}
	if d, _ := sp.Defaults(ctx); d[FnSpeech] != "kokoro" || len(d) != 1 {
		t.Errorf("defaults = %v", d)
	}
	if err := sp.SetDefault(ctx, FnSpeech, "omni"); err != nil {
		t.Fatal(err)
	}
	if d, _ := sp.Defaults(ctx); d[FnSpeech] != "omni" {
		t.Errorf("default not replaced: %v", d)
	}
	if err := sp.DeleteOccupant(ctx, "omni"); err != nil {
		t.Fatal(err)
	}
	if d, _ := sp.Defaults(ctx); len(d) != 0 {
		t.Errorf("a default survived its occupant: %v", d)
	}
	if err := sp.DeleteOccupant(ctx, "omni"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if err := sp.SetDefault(ctx, FnSpeech, ""); err != nil {
		t.Errorf("clearing a default: %v", err)
	}

	// Update keeps the row and re-validates.
	k.Policy, k.HomeSlot = PolicyOnDemand, "s3"
	if err := sp.UpdateOccupant(ctx, k); err != nil {
		t.Fatal(err)
	}
	if k2, _ := sp.OccupantByName(ctx, "kokoro"); k2.Policy != PolicyOnDemand || k2.HomeSlot != "" {
		t.Errorf("update: %+v", k2)
	}
}

func TestSpecialist_StoppedIsPersistedAndNarrow(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sp := db.Specialist()
	if _, err := sp.CreateOccupant(ctx, Occupant{Name: "a", Kind: "kokoro", Policy: PolicyAlwaysOn, HomeSlot: "s1", Enabled: true, Functions: []string{FnSpeech}}); err != nil {
		t.Fatal(err)
	}
	if o, _ := sp.OccupantByName(ctx, "a"); o.Stopped {
		t.Fatal("a new occupant starts stopped")
	}
	if err := sp.SetStopped(ctx, "a", true); err != nil {
		t.Fatal(err)
	}
	o, _ := sp.OccupantByName(ctx, "a")
	if !o.Stopped || o.HomeSlot != "s1" || !o.Enabled || o.Policy != PolicyAlwaysOn {
		t.Errorf("SetStopped touched more than the flag: %+v", o)
	}
	// An update that carries the flag keeps it.
	o.Label = "A"
	if err := sp.UpdateOccupant(ctx, o); err != nil {
		t.Fatal(err)
	}
	if o2, _ := sp.OccupantByName(ctx, "a"); !o2.Stopped || o2.Label != "A" {
		t.Errorf("update lost the stop: %+v", o2)
	}
	if err := sp.SetStopped(ctx, "a", false); err != nil {
		t.Fatal(err)
	}
	if o3, _ := sp.OccupantByName(ctx, "a"); o3.Stopped {
		t.Error("the stop was not cleared")
	}
	if err := sp.SetStopped(ctx, "ghost", true); err == nil {
		t.Error("stopping a missing occupant succeeded")
	}
}
