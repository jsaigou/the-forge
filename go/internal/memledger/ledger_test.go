// SPDX-License-Identifier: Apache-2.0

package memledger

import "testing"

const gib = float64(1 << 30)

var forgehost = Policy{OSFloorBytes: 2 << 30, GrowthMarginBytes: 4 << 30, HostFloorBytes: 8 << 30}

func g(n float64) float64 { return n * gib }

// The measured 2026-11-17 state: Flash-Next size container on a1, five specialists declared at 13 GiB.
func TestLoadedFlashNextLeavesRealHeadroom(t *testing.T) {
	l := Compute(forgehost, Inputs{
		HostTotal: g(123.5), HostAvail: g(22.6),
		GTTTotal: int64(g(120)), GTTUsed: int64(g(91.6)),
		SlotRSS: g(4.4), SpecialistGPU: g(3.1), SpecialistDeclared: int64(g(13)),
	})
	if want := int64(g(123.5 - 2 - 13 - 4)); l.ModelBudget != want {
		t.Fatalf("ModelBudget = %d, want %d", l.ModelBudget, want)
	}
	if got := float64(l.ModelUsed) / gib; got < 92 || got > 93.5 {
		t.Fatalf("ModelUsed = %.1f GiB, want ~92.9 (GTT less specialists, plus slot RSS)", got)
	}
	// 104.5 - 92.9 = 11.6 by partition; 22.6 - 8 = 14.6 by host; 120 - 96 = 24 by GTT.
	if l.Binding != BindPartition {
		t.Fatalf("Binding = %s, want partition", l.Binding)
	}
	if got := float64(l.Free) / gib; got < 11 || got > 12 {
		t.Fatalf("Free = %.1f GiB, want ~11.6", got)
	}
}

// The incident this package exists for: the old gate saw 24 GiB free (GTT ceiling) while the host had ~4 GiB of real headroom.
func TestHostFloorBindsWhenSomethingOutsideTheLedgerEatsMemory(t *testing.T) {
	l := Compute(forgehost, Inputs{
		HostTotal: g(123.5), HostAvail: g(12.3), // tmpfs scratch + zram were holding ~10 GiB
		GTTTotal: int64(g(120)), GTTUsed: int64(g(91.6)),
		SlotRSS: g(4.4), SpecialistGPU: g(3.1), SpecialistDeclared: int64(g(13)),
	})
	if l.Binding != BindHost {
		t.Fatalf("Binding = %s, want host", l.Binding)
	}
	if got := float64(l.Free) / gib; got < 4 || got > 4.6 {
		t.Fatalf("Free = %.1f GiB, want ~4.3", got)
	}
}

func TestBelowHostFloorIsZeroNotUnknown(t *testing.T) {
	l := Compute(forgehost, Inputs{HostTotal: g(123.5), HostAvail: g(5), GTTTotal: int64(g(120)), GTTUsed: int64(g(3))})
	if l.Free != 0 || l.Binding != BindHost {
		t.Fatalf("Free=%d Binding=%s, want 0 / host", l.Free, l.Binding)
	}
}

// A stopped specialist's declared memory stays reserved: that is the point of declaring it.
func TestDeclaredSpecialistsAreReservedWhetherOrNotRunning(t *testing.T) {
	idle := Inputs{HostTotal: g(123.5), HostAvail: g(104), GTTTotal: int64(g(120)), GTTUsed: int64(g(3.2)), SpecialistGPU: g(3.2)}
	none := Compute(forgehost, idle)
	idle.SpecialistDeclared = int64(g(13))
	declared := Compute(forgehost, idle)
	if d := none.ModelBudget - declared.ModelBudget; d != int64(g(13)) {
		t.Fatalf("declaring 13 GiB shrank the budget by %.1f GiB, want 13", float64(d)/gib)
	}
}

// The size container (about 93 GiB) must fit an otherwise idle ForgeHost, and a second 20 GiB model must not.
func TestSizeContainerFitsIdleBoxButNotAHeavySecondModel(t *testing.T) {
	idle := Compute(forgehost, Inputs{
		HostTotal: g(123.5), HostAvail: g(104.9),
		GTTTotal: int64(g(120)), GTTUsed: int64(g(3.2)), SpecialistGPU: g(3.2), SpecialistDeclared: int64(g(13)),
	})
	if need := int64(g(93)); idle.Free < need {
		t.Fatalf("idle Free %.1f GiB cannot take a 93 GiB container", float64(idle.Free)/gib)
	}
	loaded := Compute(forgehost, Inputs{
		HostTotal: g(123.5), HostAvail: g(22.6),
		GTTTotal: int64(g(120)), GTTUsed: int64(g(91.6)), SlotRSS: g(4.4), SpecialistGPU: g(3.1), SpecialistDeclared: int64(g(13)),
	})
	if loaded.Free >= int64(g(20)) {
		t.Fatalf("loaded Free %.1f GiB would admit a 20 GiB second model", float64(loaded.Free)/gib)
	}
}

func TestUnknownHostFallsBackToGTT(t *testing.T) {
	l := Compute(forgehost, Inputs{GTTTotal: int64(g(120)), GTTUsed: int64(g(40)), SpecialistDeclared: int64(g(10))})
	if l.ModelBudget != int64(g(110)) {
		t.Fatalf("ModelBudget = %.1f GiB, want 110", float64(l.ModelBudget)/gib)
	}
	if l.FreeByHost != -1 {
		t.Fatalf("FreeByHost = %d, want -1 (unknown)", l.FreeByHost)
	}
}

func TestSpecialistGPUAboveCounterClamps(t *testing.T) {
	l := Compute(forgehost, Inputs{HostTotal: g(100), HostAvail: g(90), GTTTotal: int64(g(120)), GTTUsed: int64(g(1)), SpecialistGPU: g(5)})
	if l.ModelUsed != 0 {
		t.Fatalf("ModelUsed = %d, want 0", l.ModelUsed)
	}
}
