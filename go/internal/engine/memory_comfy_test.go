// SPDX-License-Identifier: Apache-2.0

package engine

// FitPlan tests for the ComfyUI eviction candidate (S1, feedback F3):
// when the loaded slots alone cannot free enough memory, the plan offers
// stopping ComfyUI as additional recovery; when even that is not enough,
// the refusal says so in plain VRAM words.

import (
	"strings"
	"testing"
)

// comfyManager builds a manager over an empty-slot box whose GTT holds
// usedGiB already (as if ComfyUI owned it), with the comfy footprint
// closure returning comfyGiB (0 = feature off).
func comfyManager(t *testing.T, usedGiB, totalGiB, comfyGiB int64) *Manager {
	t.Helper()
	// siblingConfig carries a config ID so the harness's
	// WeightEstimateBytes stub (the 50 GiB need figure) engages.
	cfg := siblingConfig(t)
	sys := newFakeSys()
	gpu := incidentGPU(t, usedGiB*incidentGiB, totalGiB*incidentGiB)
	procRoot := incidentProcRoot(t, -1, 0, "")
	m, _, _ := newIncidentManager(t, cfg, sys, gpu, procRoot, nil)
	if comfyGiB > 0 {
		m.d.ComfyUIFootprintBytes = func() int64 { return comfyGiB * incidentGiB }
	}
	return m
}

func TestFitPlanProposesComfyUIStopWhenSlotsInsufficient(t *testing.T) {
	// 10 GiB free vs a 50 GiB need: no slots are loaded, so slot eviction
	// recovers nothing; ComfyUI's 45 GiB closes the gap.
	m := comfyManager(t, 80, 90, 45)

	plan, err := m.FitPlan("gemma")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.EvictComfyUI {
		t.Fatalf("plan = %+v, want EvictComfyUI proposed", plan)
	}
	if plan.ComfyUIBytes != float64(45*incidentGiB) {
		t.Errorf("ComfyUIBytes = %v, want %d", plan.ComfyUIBytes, 45*incidentGiB)
	}
	if !plan.Fits || plan.Slot == "" {
		t.Errorf("plan = %+v, want Fits with a placement once ComfyUI stops", plan)
	}
}

func TestFitPlanLeavesComfyUIAloneWhenSlotsSuffice(t *testing.T) {
	// 60 GiB free vs a 50 GiB need: fits without touching ComfyUI even
	// though the probe is wired.
	m := comfyManager(t, 30, 90, 45)

	plan, err := m.FitPlan("gemma")
	if err != nil {
		t.Fatal(err)
	}
	if plan.EvictComfyUI || plan.ComfyUIBytes != 0 {
		t.Fatalf("plan = %+v, want ComfyUI untouched", plan)
	}
	if !plan.Fits {
		t.Fatalf("plan = %+v, want Fits", plan)
	}
}

func TestFitPlanTerminalWhenEvenComfyUIInsufficient(t *testing.T) {
	// 10 GiB free + 5 GiB from ComfyUI is still far short of 50 GiB: the
	// refusal must be terminal AND say "not enough VRAM" in plain words —
	// the operator-facing wording this sprint exists for.
	m := comfyManager(t, 80, 90, 5)

	plan, err := m.FitPlan("gemma")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fits || plan.Slot != "" {
		t.Fatalf("plan = %+v, want refusal", plan)
	}
	if !strings.Contains(plan.Message, "not enough VRAM") {
		t.Errorf("message = %q, want explicit not-enough-VRAM wording", plan.Message)
	}
	if !strings.Contains(plan.Message, "ComfyUI") {
		t.Errorf("message = %q, want it to mention ComfyUI was counted", plan.Message)
	}
}

func TestFitPlanTerminalWordingWithoutComfyFeature(t *testing.T) {
	// Feature off (nil closure): same terminal refusal, generic tail.
	m := comfyManager(t, 80, 90, 0)

	plan, err := m.FitPlan("gemma")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fits || plan.EvictComfyUI {
		t.Fatalf("plan = %+v, want plain refusal", plan)
	}
	if !strings.Contains(plan.Message, "not enough VRAM") {
		t.Errorf("message = %q, want explicit not-enough-VRAM wording", plan.Message)
	}
}
