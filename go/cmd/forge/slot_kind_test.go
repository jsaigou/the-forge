// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type fakeLaunches struct {
	rows  []store.SlotLaunch
	err   error
	calls int
}

func (f *fakeLaunches) List(context.Context) ([]store.SlotLaunch, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func TestSlotKindLookup(t *testing.T) {
	f := &fakeLaunches{rows: []store.SlotLaunch{
		{Slot: "a1", Port: 8080, EngineKind: "radiance"},
		{Slot: "a2", Port: 8081, EngineKind: "llamacpp"},
	}}
	clock := time.Unix(1_800_000_000, 0)
	look := slotKindLookup(f, func() time.Time { return clock })

	if got := look(8080); got != "radiance" {
		t.Errorf("8080 = %q", got)
	}
	if got := look(8081); got != "llamacpp" {
		t.Errorf("8081 = %q", got)
	}
	if got := look(9999); got != "" {
		t.Errorf("unknown port = %q, want empty (llama.cpp default)", got)
	}
	if f.calls != 1 {
		t.Errorf("store read %d times inside one window, want 1", f.calls)
	}

	// A reload of the slot with another kind shows up after the window.
	f.rows[0].EngineKind = "llamacpp"
	clock = clock.Add(slotKindTTL + time.Second)
	if got := look(8080); got != "llamacpp" {
		t.Errorf("after refresh 8080 = %q", got)
	}

	// A read failure degrades to the default and does not wedge the lookup.
	f.err = errors.New("db locked")
	clock = clock.Add(slotKindTTL + time.Second)
	if got := look(8080); got != "" {
		t.Errorf("on error 8080 = %q, want empty", got)
	}
	f.err = nil
	clock = clock.Add(slotKindTTL + time.Second)
	if got := look(8080); got != "llamacpp" {
		t.Errorf("after recovery 8080 = %q", got)
	}
}
