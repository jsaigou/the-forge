// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/web"
)

func TestBlockedWorkRecheck_RegisteredDeepOnly(t *testing.T) {
	c := findCheck(t, "blocked_work_recheck")
	if c.Fast {
		t.Error("blocked_work_recheck must be deep-sweep only (Fast=false) — it makes real network calls")
	}
}

func TestBlockedWorkRecheckItems_NilWeb(t *testing.T) {
	env := &CheckEnv{}
	f := runBlockedWorkRecheckItems(context.Background(), env, []BlockedItem{
		{Number: 1, Title: "x", Status: "open", URLs: []string{"https://example.com"}},
	})
	if f.Severity != SeverityInfo || f.Evidence["skipped"] == nil {
		t.Errorf("f = %+v, want a skip finding", f)
	}
}

func TestBlockedWorkRecheckItems_NoOpenCandidates(t *testing.T) {
	env := &CheckEnv{Web: &fakeWebService{}}
	items := []BlockedItem{
		{Number: 1, Title: "closed one", Status: "closed", URLs: []string{"https://example.com"}},
		{Number: 2, Title: "no url", Status: "open", URLs: nil},
	}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok (nothing to check)", f.Severity)
	}
}

func TestBlockedWorkRecheckItems_CappedAtThreeNetworkFetches(t *testing.T) {
	items := make([]BlockedItem, 5)
	docs := map[string]*web.Document{}
	for i := range items {
		url := "https://example.com/" + string(rune('a'+i))
		items[i] = BlockedItem{Number: i + 1, Title: "item", Status: "open", URLs: []string{url}}
		docs[url] = &web.Document{Text: `{"state":"open","merged":false}`} // non-GitHub, no signal
	}
	env := &CheckEnv{Web: &fakeWebService{fetchDocs: docs}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)

	ev, ok := f.Evidence["network_fetches"].(int)
	if !ok || ev != blockedRecheckMaxNetworkFetches {
		t.Fatalf("network_fetches = %v, want %d", f.Evidence["network_fetches"], blockedRecheckMaxNetworkFetches)
	}
	checked, _ := f.Evidence["checked_items"].(int)
	if checked != blockedRecheckMaxNetworkFetches {
		t.Errorf("checked_items = %d, want %d (every check here is a real fetch)", checked, blockedRecheckMaxNetworkFetches)
	}
}

func TestBlockedWorkRecheckItems_CachedDoesNotCountAgainstBudget(t *testing.T) {
	items := make([]BlockedItem, 5)
	docs := map[string]*web.Document{}
	for i := range items {
		url := "https://example.com/" + string(rune('a'+i))
		items[i] = BlockedItem{Number: i + 1, Title: "item", Status: "open", URLs: []string{url}}
		docs[url] = &web.Document{Text: "x", Cached: true}
	}
	env := &CheckEnv{Web: &fakeWebService{fetchDocs: docs}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)

	networkFetches, _ := f.Evidence["network_fetches"].(int)
	if networkFetches != 0 {
		t.Errorf("network_fetches = %d, want 0 (every hit was cached)", networkFetches)
	}
	checked, _ := f.Evidence["checked_items"].(int)
	if checked != 5 {
		t.Errorf("checked_items = %d, want 5 (cache hits don't count against the budget, so all 5 items get checked)", checked)
	}
}

func TestBlockedWorkRecheckItems_MergedPRProducesSignal(t *testing.T) {
	items := []BlockedItem{
		{Number: 8, Title: "some upstream fix", Status: "open", URLs: []string{"https://github.com/ggml-org/llama.cpp/pull/22105"}},
	}
	env := &CheckEnv{Web: &fakeWebService{fetchDocs: map[string]*web.Document{
		"https://api.github.com/repos/ggml-org/llama.cpp/pulls/22105": {Text: `{"state":"closed","merged":true}`},
	}}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)

	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (a positive signal is good news, not a warn-class alert)", f.Severity)
	}
	if !strings.Contains(f.Summary, "may have unblocked") {
		t.Errorf("summary = %q, want it to mention a possible unblock", f.Summary)
	}
	signals, ok := f.Evidence["signals"].([]blockedRecheckSignal)
	if !ok || len(signals) != 1 || signals[0].ItemNumber != 8 {
		t.Fatalf("signals = %+v, want one entry for item 8", f.Evidence["signals"])
	}
}

// TestBlockedWorkRecheckItems_MultiURLItemSummaryNeverExceedsItemCount is a
// regression test for a real bug found live 2026-08-11: one item with two
// blocking URLs that both produce a signal made the summary read "5 of 4
// open blocked item(s) may have unblocked" — len(signals) (per-URL) can
// exceed len(candidates) (per-item), which reads as nonsensical. The
// summary must count distinct items, not raw signal entries.
func TestBlockedWorkRecheckItems_MultiURLItemSummaryNeverExceedsItemCount(t *testing.T) {
	items := []BlockedItem{
		{Number: 2, Title: "two blocking URLs", Status: "open", URLs: []string{
			"https://github.com/ggml-org/llama.cpp/pull/23660",
			"https://github.com/ggml-org/llama.cpp/pull/22833",
		}},
	}
	env := &CheckEnv{Web: &fakeWebService{fetchDocs: map[string]*web.Document{
		"https://api.github.com/repos/ggml-org/llama.cpp/pulls/23660": {Text: `{"state":"closed","merged":true}`},
		"https://api.github.com/repos/ggml-org/llama.cpp/pulls/22833": {Text: `{"state":"closed","merged":false}`},
	}}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)

	signals, _ := f.Evidence["signals"].([]blockedRecheckSignal)
	if len(signals) != 2 {
		t.Fatalf("expected both URLs to produce a signal, got %+v", signals)
	}
	if !strings.Contains(f.Summary, "1 of 1 open blocked item(s)") {
		t.Errorf("summary = %q, want it to count the one distinct item, not the two raw signals (the '5 of 4' bug)", f.Summary)
	}
}

func TestBlockedWorkRecheckItems_NoSignalsReportsCounts(t *testing.T) {
	items := []BlockedItem{
		{Number: 1, Title: "still open", Status: "open", URLs: []string{"https://github.com/ggml-org/llama.cpp/pull/1"}},
	}
	env := &CheckEnv{Web: &fakeWebService{fetchDocs: map[string]*web.Document{
		"https://api.github.com/repos/ggml-org/llama.cpp/pulls/1": {Text: `{"state":"open","merged":false}`},
	}}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)
	if !strings.Contains(f.Summary, "no change detected") {
		t.Errorf("summary = %q, want it to report no change", f.Summary)
	}
}

func TestBlockedWorkRecheckItems_DisabledStopsAndSkips(t *testing.T) {
	items := []BlockedItem{
		{Number: 1, Title: "x", Status: "open", URLs: []string{"https://example.com/a"}},
	}
	env := &CheckEnv{Web: &fakeWebService{fetchErr: map[string]error{"https://example.com/a": web.ErrDisabled}}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)
	if f.Severity != SeverityInfo || f.Evidence["skipped"] == nil {
		t.Errorf("f = %+v, want a skip finding when web research is disabled", f)
	}
}

func TestBlockedWorkRecheckItems_OneBadURLDoesNotAbortSweep(t *testing.T) {
	items := []BlockedItem{
		{Number: 1, Title: "bad", Status: "open", URLs: []string{"https://example.com/bad"}},
		{Number: 2, Title: "good", Status: "open", URLs: []string{"https://github.com/ggml-org/llama.cpp/pull/2"}},
	}
	env := &CheckEnv{Web: &fakeWebService{
		fetchErr: map[string]error{"https://example.com/bad": errors.New("dns failure")},
		fetchDocs: map[string]*web.Document{
			"https://api.github.com/repos/ggml-org/llama.cpp/pulls/2": {Text: `{"state":"closed","merged":true}`},
		},
	}}
	f := runBlockedWorkRecheckItems(context.Background(), env, items)
	checked, _ := f.Evidence["checked_items"].(int)
	if checked != 1 {
		t.Fatalf("checked_items = %d, want 1 (the bad URL is skipped, not fatal)", checked)
	}
	signals, _ := f.Evidence["signals"].([]blockedRecheckSignal)
	if len(signals) != 1 {
		t.Fatalf("signals = %+v, want the good item's signal to still land", f.Evidence["signals"])
	}
}

func TestBlockedWorkRecheckItems_HashesCarryForwardAcrossSweeps(t *testing.T) {
	db := openDB(t)
	s := New(Deps{Store: db, Now: func() time.Time { return time.Unix(1000, 0) }})
	ctx := context.Background()
	items := []BlockedItem{
		{Number: 1, Title: "watched page", Status: "open", URLs: []string{"https://example.com/status"}},
	}

	// First sweep: content "v1", nothing to compare against yet.
	env1 := &CheckEnv{Store: db, Web: &fakeWebService{fetchDocs: map[string]*web.Document{
		"https://example.com/status": {Text: "v1"},
	}}}
	f1 := runBlockedWorkRecheckItems(ctx, env1, items)
	signals1, _ := f1.Evidence["signals"].([]blockedRecheckSignal)
	if len(signals1) != 0 {
		t.Fatalf("first-ever check should never claim a change, got %+v", signals1)
	}
	if _, err := s.persistFindings(ctx, []Finding{f1}, SweepManual, time.Unix(1000, 0), nil); err != nil {
		t.Fatalf("persistFindings: %v", err)
	}

	// Second sweep: content changed to "v2" — must diff against the hash
	// persisted in the first sweep's finding row, not start from scratch.
	env2 := &CheckEnv{Store: db, Web: &fakeWebService{fetchDocs: map[string]*web.Document{
		"https://example.com/status": {Text: "v2"},
	}}}
	f2 := runBlockedWorkRecheckItems(ctx, env2, items)
	signals2, _ := f2.Evidence["signals"].([]blockedRecheckSignal)
	if len(signals2) != 1 {
		t.Fatalf("second sweep should detect the content change via the carried-forward hash, got %+v", signals2)
	}
}
