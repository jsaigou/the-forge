// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
	"time"
)

// TestMigration0105UpgradeKeepsOldRows seeds usage events at schema 104, applies 0105 and checks the old rows are untouched
// (new columns NULL, nothing backfilled, existing columns identical).
func TestMigration0105UpgradeKeepsOldRows(t *testing.T) {
	sqlDB := openThrough(t, 104)
	defer sqlDB.Close()

	if _, err := sqlDB.Exec(
		`INSERT INTO usage_events (ts, kind, model, prompt_tokens, completion_tokens, cost_usd)
		 VALUES (1700000000, 'external_request', 'old-model', 1000, 500, 0.001)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	body, err := migrationsFS.ReadFile("migrations/0105_usage_request_timing.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(string(body)); err != nil {
		t.Fatalf("apply 0105: %v", err)
	}

	var ts, prompt, completion int64
	var model string
	var started, dur, wait any
	var outcome, consumer any
	if err := sqlDB.QueryRow(
		`SELECT ts, model, prompt_tokens, completion_tokens, started_ms, duration_ms, outcome, consumer, wait_ms
		 FROM usage_events`).Scan(&ts, &model, &prompt, &completion, &started, &dur, &outcome, &consumer, &wait); err != nil {
		t.Fatalf("read after 0105: %v", err)
	}
	if ts != 1700000000 || model != "old-model" || prompt != 1000 || completion != 500 {
		t.Errorf("existing columns changed: ts=%d model=%q tokens=%d/%d", ts, model, prompt, completion)
	}
	for name, v := range map[string]any{"started_ms": started, "duration_ms": dur, "outcome": outcome, "consumer": consumer, "wait_ms": wait} {
		if v != nil {
			t.Errorf("%s = %v, want NULL on a pre-0105 row (no backfill)", name, v)
		}
	}
}

// TestMigration0105FreshDB: a database built from nothing has the columns and the index, and accepts a timed row.
func TestMigration0105FreshDB(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, col := range []string{"started_ms", "duration_ms", "outcome", "consumer", "wait_ms"} {
		var n int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('usage_events') WHERE name = ?`, col).Scan(&n); err != nil || n != 1 {
			t.Errorf("column %s: count=%d err=%v, want 1", col, n, err)
		}
	}
	var idx int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_usage_events_started_ms'`).Scan(&idx); err != nil || idx != 1 {
		t.Errorf("index idx_usage_events_started_ms: count=%d err=%v, want 1", idx, err)
	}
}

func TestUsageRecordTimingRoundTripAndIntervals(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	u := db.Usage()
	base := time.UnixMilli(1_700_000_000_123)

	// A pre-0105 style row (no Timing) in the window, one timed ok row with tokens, one timed aborted row without usage,
	// and one timed row for another model.
	if err := u.Record(ctx, UsageEvent{TS: base, Kind: "external_request", Model: "m", PromptTokens: 5, CompletionTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if err := u.Record(ctx, UsageEvent{TS: base, Kind: "a0_request", Model: "m", PromptTokens: 100, CompletionTokens: 20,
		Timing: &RequestTiming{Start: base, Duration: 1500 * time.Millisecond, Wait: 200 * time.Millisecond, Outcome: OutcomeOK, Consumer: "opencode"}}); err != nil {
		t.Fatal(err)
	}
	if err := u.Record(ctx, UsageEvent{TS: base, Kind: "a0_request", Model: "m", Unmetered: true,
		Timing: &RequestTiming{Start: base.Add(time.Second), Duration: 2 * time.Second, Wait: -1, Outcome: OutcomeAborted, Consumer: "librechat"}}); err != nil {
		t.Fatal(err)
	}
	if err := u.Record(ctx, UsageEvent{TS: base, Kind: "a0_request", Model: "other",
		Timing: &RequestTiming{Start: base, Duration: time.Second, Wait: -1, Outcome: OutcomeError, Consumer: "opencode"}}); err != nil {
		t.Fatal(err)
	}

	var started, dur, wait int64
	var outcome, consumer string
	if err := db.sql.QueryRow(`SELECT started_ms, duration_ms, wait_ms, outcome, consumer FROM usage_events WHERE consumer='opencode' AND model='m'`).
		Scan(&started, &dur, &wait, &outcome, &consumer); err != nil {
		t.Fatal(err)
	}
	if started != 1_700_000_000_123 || dur != 1500 || wait != 200 || outcome != "ok" {
		t.Errorf("stored timing = %d/%d/%d/%s, want 1700000000123/1500/200/ok (millisecond resolution)", started, dur, wait, outcome)
	}
	var waitNull any
	if err := db.sql.QueryRow(`SELECT wait_ms FROM usage_events WHERE consumer='librechat'`).Scan(&waitNull); err != nil || waitNull != nil {
		t.Errorf("never-dispatched wait_ms = %v err=%v, want NULL", waitNull, err)
	}

	set, err := u.Intervals(ctx, IntervalQuery{Model: "m", Since: base.Add(-time.Minute), Until: base.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Intervals) != 2 || set.ExcludedUntimed != 1 {
		t.Fatalf("intervals=%d excluded=%d, want 2 and 1", len(set.Intervals), set.ExcludedUntimed)
	}
	first, second := set.Intervals[0], set.Intervals[1]
	if first.EndMS-first.StartMS != 1500 || first.PromptTokens == nil || *first.PromptTokens != 100 || first.Outcome != OutcomeOK {
		t.Errorf("first interval = %+v", first)
	}
	if second.PromptTokens != nil || second.Outcome != OutcomeAborted {
		t.Errorf("unmetered interval must carry no tokens: %+v", second)
	}

	byConsumer, err := u.Intervals(ctx, IntervalQuery{Consumer: "opencode", Since: base.Add(-time.Minute)})
	if err != nil || len(byConsumer.Intervals) != 2 {
		t.Fatalf("consumer filter: n=%d err=%v, want 2 (both models)", len(byConsumer.Intervals), err)
	}

	empty, err := u.Intervals(ctx, IntervalQuery{Since: base.Add(time.Hour), Until: base.Add(2 * time.Hour)})
	if err != nil || len(empty.Intervals) != 0 || empty.ExcludedUntimed != 0 {
		t.Fatalf("empty window: %+v err=%v", empty, err)
	}
}
