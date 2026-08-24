// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"
	"time"
)

// TestGPUDeviceLost_PassesWindow asserts the --since window fed to both
// journal seams matches env.Now() minus the configured
// DeviceLostWindowMinutes — the bug found live 2026-08-18 was that no window
// was passed at all, so a resolved incident's journal lines read as a fresh
// crit indefinitely.
func TestGPUDeviceLost_PassesWindow(t *testing.T) {
	fixedNow := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var gotKernelSince, gotUnitSince time.Time
	env := &CheckEnv{
		Now:        func() time.Time { return fixedNow },
		Thresholds: Thresholds{DeviceLostWindowMinutes: 20},
		KernelJournal: func(_ context.Context, n int, since time.Time) ([]string, error) {
			gotKernelSince = since
			return nil, nil
		},
		JournalErrors: func(_ context.Context, n int, since time.Time) ([]string, error) {
			gotUnitSince = since
			return nil, nil
		},
	}
	runGPUDeviceLost(context.Background(), env)

	want := fixedNow.Add(-20 * time.Minute)
	if !gotKernelSince.Equal(want) {
		t.Errorf("kernel journal since = %v, want %v", gotKernelSince, want)
	}
	if !gotUnitSince.Equal(want) {
		t.Errorf("unit journal since = %v, want %v", gotUnitSince, want)
	}
}

// TestGPUDeviceLost_DefaultWindow asserts an unset/invalid threshold falls
// back to DefaultThresholds' window rather than an unbounded read.
func TestGPUDeviceLost_DefaultWindow(t *testing.T) {
	fixedNow := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	var gotSince time.Time
	env := &CheckEnv{
		Now: func() time.Time { return fixedNow },
		KernelJournal: func(_ context.Context, n int, since time.Time) ([]string, error) {
			gotSince = since
			return nil, nil
		},
	}
	runGPUDeviceLost(context.Background(), env)

	want := fixedNow.Add(-time.Duration(DefaultThresholds().DeviceLostWindowMinutes) * time.Minute)
	if !gotSince.Equal(want) {
		t.Errorf("since = %v, want %v (default window)", gotSince, want)
	}
}

// TestGPUDeviceLost_MatchWithinWindow confirms a windowed read that still
// finds a signature reports crit — the window narrows the read, it doesn't
// disable detection.
func TestGPUDeviceLost_MatchWithinWindow(t *testing.T) {
	env := &CheckEnv{
		Now: time.Now,
		KernelJournal: func(_ context.Context, n int, since time.Time) ([]string, error) {
			return []string{"amdgpu: ring comp_1.2.0 timeout, signaled seq=1, emitted seq=4"}, nil
		},
	}
	f := runGPUDeviceLost(context.Background(), env)
	if f.Severity != SeverityCrit {
		t.Errorf("severity = %s, want crit", f.Severity)
	}
}

// TestGPUDeviceLost_NoMatchIsOK confirms journalctl having already filtered
// out an old signature via --since (simulated here by the fake returning no
// lines) reads as a clean OK, not a stale crit.
func TestGPUDeviceLost_NoMatchIsOK(t *testing.T) {
	env := &CheckEnv{
		Now: time.Now,
		KernelJournal: func(_ context.Context, n int, since time.Time) ([]string, error) {
			return nil, nil // journalctl --since already excluded the old line
		},
		JournalErrors: func(_ context.Context, n int, since time.Time) ([]string, error) {
			return nil, nil
		},
	}
	f := runGPUDeviceLost(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok", f.Severity)
	}
}
