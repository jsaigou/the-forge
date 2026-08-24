// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

func TestUsageRecordAndEventsRoundTripNewFields(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	u := db.Usage()

	if err := db.Routing().SaveProvider(ctx, ProviderRow{Name: "deepseek", APIKey: "sk-x", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekID := testProviderID(t, db, "deepseek")

	cost := 1.2345
	cached := int64(300)
	now := time.Now()
	if err := u.Record(ctx, UsageEvent{
		TS: now, Kind: "external_request", Model: "wire-m", ProviderID: &deepseekID,
		PromptTokens: 1000, CompletionTokens: 200,
		CostNative: &cost, CostCurrency: "USD", CachedPromptTokens: &cached,
	}); err != nil {
		t.Fatalf("Record metered: %v", err)
	}
	if err := u.Record(ctx, UsageEvent{
		TS: now.Add(time.Second), Kind: "external_request", Model: "wire-m", ProviderID: &deepseekID,
		Unmetered: true,
	}); err != nil {
		t.Fatalf("Record unmetered: %v", err)
	}
	// A plain "inference" row (the pre-existing write path) must round-trip
	// with all the new fields at their zero values, not error or panic.
	if err := u.Record(ctx, UsageEvent{
		TS: now.Add(2 * time.Second), Kind: "inference", Model: "gemma4-e2b", Slot: "a1",
		PromptTokens: 50, CompletionTokens: 10,
	}); err != nil {
		t.Fatalf("Record inference: %v", err)
	}

	events, err := u.Events(ctx, now.Add(-time.Minute), 10)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	// Events are newest-first.
	inference := events[0]
	if inference.Kind != "inference" || inference.Unmetered {
		t.Errorf("inference row wrong: %+v", inference)
	}
	if inference.CostNative != nil {
		t.Errorf("inference row CostNative = %v, want nil", *inference.CostNative)
	}
	if inference.CachedPromptTokens != nil {
		t.Errorf("inference row CachedPromptTokens = %v, want nil", *inference.CachedPromptTokens)
	}

	unmetered := events[1]
	if !unmetered.Unmetered {
		t.Error("unmetered row: Unmetered = false, want true")
	}
	if unmetered.CostNative != nil {
		t.Errorf("unmetered row CostNative = %v, want nil", *unmetered.CostNative)
	}
	if unmetered.PromptTokens != 0 || unmetered.CompletionTokens != 0 {
		t.Errorf("unmetered row tokens = (%d,%d), want (0,0)", unmetered.PromptTokens, unmetered.CompletionTokens)
	}

	metered := events[2]
	if metered.CostNative == nil || *metered.CostNative != cost {
		t.Errorf("metered row CostNative = %v, want %v", metered.CostNative, cost)
	}
	if metered.CostCurrency != "USD" {
		t.Errorf("metered row CostCurrency = %q, want USD", metered.CostCurrency)
	}
	if metered.CachedPromptTokens == nil || *metered.CachedPromptTokens != cached {
		t.Errorf("metered row CachedPromptTokens = %v, want %v", metered.CachedPromptTokens, cached)
	}
	if metered.Unmetered {
		t.Error("metered row: Unmetered = true, want false")
	}
}

func TestTokenActivityFiltersKindsAndWindow(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	u := db.Usage()
	now := time.Now()

	if err := db.Routing().SaveProvider(ctx, ProviderRow{Name: "deepseek", APIKey: "sk-x", CreatedAt: now}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekID := testProviderID(t, db, "deepseek")

	rows := []UsageEvent{
		{TS: now.Add(-time.Hour), Kind: "inference", Model: "gemma4-e2b", PromptTokens: 100, CompletionTokens: 50},
		{TS: now.Add(-30 * time.Minute), Kind: "external_request", Model: "deepseek-v4-flash", ProviderID: &deepseekID, PromptTokens: 200, CompletionTokens: 20},
		// Non-token-bearing lifecycle kinds must not contribute, even though
		// they carry a (zero) prompt/completion value.
		{TS: now.Add(-20 * time.Minute), Kind: "load_ok", Model: "gemma4-e2b"},
		{TS: now.Add(-10 * time.Minute), Kind: "unload", Model: "gemma4-e2b"},
		// Outside the query window — must not appear.
		{TS: now.Add(-2 * time.Hour), Kind: "inference", Model: "gemma4-e2b", PromptTokens: 999, CompletionTokens: 999},
	}
	for _, e := range rows {
		if err := u.Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	activity, err := u.TokenActivity(ctx, now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("TokenActivity: %v", err)
	}
	if len(activity) != 2 {
		t.Fatalf("activity = %d rows, want 2 (got %+v)", len(activity), activity)
	}
	// Oldest first.
	if activity[0].Tokens != 150 {
		t.Errorf("activity[0].Tokens = %d, want 150", activity[0].Tokens)
	}
	if activity[1].Tokens != 220 {
		t.Errorf("activity[1].Tokens = %d, want 220", activity[1].Tokens)
	}
	if !activity[0].TS.Before(activity[1].TS) {
		t.Errorf("activity not ordered oldest-first: %+v", activity)
	}
}
