// SPDX-License-Identifier: Apache-2.0

// Package memledger is the one place that turns host memory facts into "how much can a model still claim".
//
// On Strix Halo the GPU's GTT pages are ordinary system RAM, so the binding limit is the host, not the GTT ceiling. Two independent guards bound a
// new load, and the smaller wins:
//
//  1. The declared partition. Host RAM is split ahead of time: OS floor + the declared peak of every always-on specialist + a growth margin are
//     reserved, and what is left is the model budget. It is deterministic: it does not depend on what happens to be resident (tmpfs scratch, page
//     cache, a stopped specialist's memory) at the moment of the check.
//  2. The dynamic floor. MemAvailable must stay above a floor. This is the safety net for anything the partition does not know about (an agent
//     filling /tmp, a process started outside Forge, a specialist that outgrew its declaration).
//
// The legacy GTT ceiling (GTTTotal - GTTUsed - slot RSS) stays as a third bound. Everything here is pure arithmetic over its inputs so the
// engine's live probe and the collector's snapshot read produce the same numbers.
package memledger

// Policy is the operator-tunable part (infra.memory).
type Policy struct {
	OSFloorBytes      int64 // kernel, slab, sshd, tailscale, the forge daemon
	GrowthMarginBytes int64 // specialist growth, load-time spikes, page-cache churn
	HostFloorBytes    int64 // MemAvailable the dynamic guard refuses to plan below
}

// Inputs are the measured facts. Zero means "not measured" for every field except the byte counters that are legitimately zero.
type Inputs struct {
	HostTotal float64 // /proc/meminfo MemTotal
	HostAvail float64 // /proc/meminfo MemAvailable
	SwapTotal float64
	SwapUsed  float64

	GTTTotal int64 // amdgpu mem_info_gtt_total (0 = unknown)
	GTTUsed  int64 // amdgpu mem_info_gtt_used (whole-GPU counter, includes specialists)

	SlotRSS       float64 // resident set of the inference slots' processes
	SpecialistGPU float64 // measured GPU bytes of the specialist pool's processes (inside GTTUsed)

	SpecialistDeclared int64 // sum of declared peaks of enabled, always-on specialists, running or not
}

// Ledger is the computed result. Every field is bytes except Binding.
type Ledger struct {
	HostTotal int64
	HostAvail int64
	SwapTotal int64
	SwapUsed  int64

	OSFloor            int64
	SpecialistDeclared int64
	GrowthMargin       int64
	ModelBudget        int64 // HostTotal - OSFloor - SpecialistDeclared - GrowthMargin (GTT total when HostTotal is unknown)
	ModelUsed          int64 // GTT outside the specialists + slot RSS: what the models (and any non-specialist GPU client) hold

	FreeByPartition int64 // ModelBudget - ModelUsed
	FreeByHost      int64 // HostAvail - HostFloor; -1 when MemAvailable is unknown
	FreeByGTT       int64 // GTTTotal - GTTUsed - SlotRSS; -1 when GTT is unknown
	Free            int64 // the smallest applicable bound, never negative

	// Binding names which bound set Free: "partition", "host" or "gtt".
	Binding string
}

// Binding values.
const (
	BindPartition = "partition"
	BindHost      = "host"
	BindGTT       = "gtt"
)

// Compute applies the policy to the measurements.
func Compute(p Policy, in Inputs) Ledger {
	l := Ledger{
		HostTotal:          int64(in.HostTotal),
		HostAvail:          int64(in.HostAvail),
		SwapTotal:          int64(in.SwapTotal),
		SwapUsed:           int64(in.SwapUsed),
		OSFloor:            p.OSFloorBytes,
		SpecialistDeclared: in.SpecialistDeclared,
		GrowthMargin:       p.GrowthMarginBytes,
		FreeByHost:         -1,
		FreeByGTT:          -1,
	}

	// Models hold the whole GTT counter except what the specialists measurably hold, plus the slots' resident memory (the counter is blind to
	// anonymous and unified pages: docs/pitfalls.md). A specialist figure larger than the counter would make the difference negative; clamp it.
	nonSpecialistGTT := float64(in.GTTUsed) - in.SpecialistGPU
	if nonSpecialistGTT < 0 {
		nonSpecialistGTT = 0
	}
	l.ModelUsed = int64(nonSpecialistGTT + in.SlotRSS)

	switch {
	case in.HostTotal > 0:
		l.ModelBudget = l.HostTotal - l.OSFloor - l.SpecialistDeclared - l.GrowthMargin
	case in.GTTTotal > 0:
		// No host figure: fall back to the GTT ceiling less the declared specialists, the closest honest bound.
		l.ModelBudget = in.GTTTotal - l.SpecialistDeclared
	}
	if l.ModelBudget < 0 {
		l.ModelBudget = 0
	}
	l.FreeByPartition = l.ModelBudget - l.ModelUsed

	if in.HostAvail > 0 {
		l.FreeByHost = l.HostAvail - p.HostFloorBytes
	}
	if in.GTTTotal > 0 {
		l.FreeByGTT = in.GTTTotal - int64(float64(in.GTTUsed)+in.SlotRSS)
	}

	l.Free, l.Binding = l.FreeByPartition, BindPartition
	if l.FreeByHost >= 0 && l.FreeByHost < l.Free {
		l.Free, l.Binding = l.FreeByHost, BindHost
	}
	// A negative FreeByHost means "below the floor", which is a real (zero) bound, not "unknown".
	if in.HostAvail > 0 && l.FreeByHost < 0 {
		l.Free, l.Binding = 0, BindHost
	}
	if in.GTTTotal > 0 && l.FreeByGTT < l.Free {
		l.Free, l.Binding = l.FreeByGTT, BindGTT
	}
	if l.Free < 0 {
		l.Free = 0
	}
	return l
}
