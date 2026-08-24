// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T, opts ...Option) *DB {
	t.Helper()
	db, err := Open(":memory:", opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func ts(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func TestUsersRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	id, err := db.Users().Create(ctx, User{
		Username: "testuser", PasswordHash: "$argon2id$...", Role: "admin",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	u, err := db.Users().ByUsername(ctx, "testuser")
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	if u.ID != id || u.Role != "admin" || u.Disabled || u.CreatedAt.IsZero() {
		t.Errorf("round trip mismatch: %+v", u)
	}

	if _, err := db.Users().ByUsername(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing user: got %v, want ErrNotFound", err)
	}

	// Duplicate username violates UNIQUE.
	if _, err := db.Users().Create(ctx, User{Username: "testuser", PasswordHash: "h", Role: "viewer"}); err == nil {
		t.Error("duplicate username should fail")
	}
	// Bad role violates CHECK.
	if _, err := db.Users().Create(ctx, User{Username: "x", PasswordHash: "h", Role: "root"}); err == nil {
		t.Error("invalid role should fail")
	}

	users, err := db.Users().List(ctx)
	if err != nil || len(users) != 1 {
		t.Fatalf("List: %v, n=%d", err, len(users))
	}

	if err := db.Users().Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := db.Users().Delete(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: got %v, want ErrNotFound", err)
	}
}

func TestSessionsRoundTripAndSweep(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	uid, err := db.Users().Create(ctx, User{Username: "testuser", PasswordHash: "h", Role: "admin"})
	if err != nil {
		t.Fatalf("user: %v", err)
	}

	s := Session{
		ID: "sess-1", UserID: uid, CSRFToken: "csrf-1",
		CreatedAt: ts(1000), ExpiresAt: ts(2000),
		RemoteAddr: "100.100.100.100", UserAgent: "test",
	}
	if err := db.Sessions().Create(ctx, s); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := db.Sessions().Get(ctx, "sess-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CSRFToken != "csrf-1" || !got.ExpiresAt.Equal(ts(2000)) || !got.LastSeenAt.IsZero() {
		t.Errorf("round trip mismatch: %+v", got)
	}

	if err := db.Sessions().Touch(ctx, "sess-1", ts(1500)); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	got, _ = db.Sessions().Get(ctx, "sess-1")
	if !got.LastSeenAt.Equal(ts(1500)) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, ts(1500))
	}
	if err := db.Sessions().Touch(ctx, "missing", ts(1)); !errors.Is(err, ErrNotFound) {
		t.Errorf("touch missing: got %v", err)
	}

	// Second session that expires later; sweep at t=2000 removes only #1.
	if err := db.Sessions().Create(ctx, Session{
		ID: "sess-2", UserID: uid, CSRFToken: "c2", CreatedAt: ts(1000), ExpiresAt: ts(9000),
	}); err != nil {
		t.Fatalf("Create 2: %v", err)
	}
	n, err := db.Sessions().DeleteExpired(ctx, ts(2000))
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpired: n=%d err=%v, want 1", n, err)
	}
	if _, err := db.Sessions().Get(ctx, "sess-1"); !errors.Is(err, ErrNotFound) {
		t.Error("sess-1 should be swept")
	}
	if _, err := db.Sessions().Get(ctx, "sess-2"); err != nil {
		t.Errorf("sess-2 should survive: %v", err)
	}

	// Idempotent delete + FK cascade.
	if err := db.Sessions().Delete(ctx, "gone"); err != nil {
		t.Errorf("idempotent delete: %v", err)
	}
	if err := db.Users().Delete(ctx, uid); err != nil {
		t.Fatalf("user delete: %v", err)
	}
	if _, err := db.Sessions().Get(ctx, "sess-2"); !errors.Is(err, ErrNotFound) {
		t.Error("sessions should cascade on user delete")
	}
}

func TestKeysRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	k := APIKey{KeyID: "a6a0da5609b8", Kind: "forge", Name: "opencode", SecretHash: "$argon2id$h", Role: "operator"}
	if err := db.Keys().Create(ctx, k); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Non-forge kinds have no role — must store NULL, not '' (CHECK).
	if err := db.Keys().Create(ctx, APIKey{KeyID: "0123456789ab", Kind: "mcp", Name: "agent", SecretHash: "h"}); err != nil {
		t.Fatalf("Create mcp: %v", err)
	}
	if err := db.Keys().Create(ctx, APIKey{KeyID: "ffffffffffff", Kind: "beans", Name: "x", SecretHash: "h"}); err == nil {
		t.Error("invalid kind should fail CHECK")
	}

	got, err := db.Keys().Get(ctx, "a6a0da5609b8")
	if err != nil || got.Role != "operator" || !got.RevokedAt.IsZero() || !got.LastUsedAt.IsZero() {
		t.Fatalf("Get: %+v err=%v", got, err)
	}
	mcp, _ := db.Keys().Get(ctx, "0123456789ab")
	if mcp.Role != "" {
		t.Errorf("mcp role = %q, want empty", mcp.Role)
	}

	if err := db.Keys().TouchUsed(ctx, "a6a0da5609b8", ts(5000)); err != nil {
		t.Fatalf("TouchUsed: %v", err)
	}
	got, _ = db.Keys().Get(ctx, "a6a0da5609b8")
	if !got.LastUsedAt.Equal(ts(5000)) {
		t.Errorf("LastUsedAt = %v", got.LastUsedAt)
	}

	all, err := db.Keys().List(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("List all: n=%d err=%v", len(all), err)
	}
	onlyMCP, err := db.Keys().List(ctx, "mcp")
	if err != nil || len(onlyMCP) != 1 || onlyMCP[0].Kind != "mcp" {
		t.Fatalf("List mcp: %+v err=%v", onlyMCP, err)
	}

	if err := db.Keys().Revoke(ctx, "a6a0da5609b8"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ = db.Keys().Get(ctx, "a6a0da5609b8")
	if got.RevokedAt.IsZero() {
		t.Error("RevokedAt should be set")
	}
	if err := db.Keys().Revoke(ctx, "a6a0da5609b8"); err != nil {
		t.Errorf("re-revoke should be idempotent: %v", err)
	}
	if err := db.Keys().Revoke(ctx, "000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoke missing: got %v", err)
	}
}

func TestSchedRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.Sched().SaveSlot(ctx, "a1", "gemma", ts(100)); err != nil {
		t.Fatalf("SaveSlot: %v", err)
	}
	if err := db.Sched().SaveSlot(ctx, "a3", "", time.Time{}); err != nil {
		t.Fatalf("SaveSlot empty: %v", err)
	}
	// Upsert overwrites.
	if err := db.Sched().SaveSlot(ctx, "a1", "nemotron", ts(200)); err != nil {
		t.Fatalf("SaveSlot upsert: %v", err)
	}
	slots, err := db.Sched().Slots(ctx)
	if err != nil {
		t.Fatalf("Slots: %v", err)
	}
	if slots["a1"] != "nemotron" || slots["a3"] != "" || len(slots) != 2 {
		t.Errorf("Slots = %v", slots)
	}

	q := QueueRow{TicketID: "t1", Model: "gemma", RequestedBy: "opencode", Status: "queued", Priority: 1, SmallJob: true, EnqueuedAt: ts(10), UpdatedAt: ts(10)}
	if err := db.Sched().SaveTicket(ctx, q); err != nil {
		t.Fatalf("SaveTicket: %v", err)
	}
	q.Status = "loading"
	q.TargetSlot = "a4"
	if err := db.Sched().SaveTicket(ctx, q); err != nil {
		t.Fatalf("SaveTicket upsert: %v", err)
	}
	queue, err := db.Sched().Queue(ctx)
	if err != nil || len(queue) != 1 {
		t.Fatalf("Queue: n=%d err=%v", len(queue), err)
	}
	if queue[0].Status != "loading" || queue[0].TargetSlot != "a4" || !queue[0].SmallJob {
		t.Errorf("ticket = %+v", queue[0])
	}
	if err := db.Sched().DeleteTicket(ctx, "t1"); err != nil {
		t.Fatalf("DeleteTicket: %v", err)
	}
	if err := db.Sched().DeleteTicket(ctx, "t1"); err != nil {
		t.Errorf("DeleteTicket idempotent: %v", err)
	}

	r := ReservationRow{
		Label: "genomics-run", Model: "carbon8b", Start: ts(1000), End: ts(2000),
		Scope: "bay", Bay: "a3", CreatedBy: "testuser", AllowAgentReschedule: true,
	}
	if err := db.Sched().SaveReservation(ctx, r); err != nil {
		t.Fatalf("SaveReservation: %v", err)
	}
	// CHECK: whole_box must have NULL bay — Bay "" maps to NULL, so this works.
	if err := db.Sched().SaveReservation(ctx, ReservationRow{
		Label: "all-night", Model: "m", Start: ts(1), End: ts(2), Scope: "whole_box", CreatedBy: "testuser",
	}); err != nil {
		t.Fatalf("whole_box reservation: %v", err)
	}
	// CHECK violations surface as errors.
	if err := db.Sched().SaveReservation(ctx, ReservationRow{
		Label: "bad", Model: "m", Start: ts(2), End: ts(1), Scope: "comfyui", CreatedBy: "testuser",
	}); err == nil {
		t.Error("end <= start should violate CHECK")
	}
	if err := db.Sched().SaveReservation(ctx, ReservationRow{
		Label: "bad2", Model: "m", Start: ts(1), End: ts(2), Scope: "bay", CreatedBy: "testuser",
	}); err == nil {
		t.Error("scope=bay without bay should violate CHECK")
	}

	rs, err := db.Sched().Reservations(ctx)
	if err != nil || len(rs) != 2 {
		t.Fatalf("Reservations: n=%d err=%v", len(rs), err)
	}
	// Ordered by start_ts: all-night (1) first.
	if rs[0].Label != "all-night" || rs[1].Bay != "a3" || !rs[1].AllowAgentReschedule {
		t.Errorf("Reservations = %+v", rs)
	}

	if err := db.Sched().DeleteReservation(ctx, "genomics-run"); err != nil {
		t.Fatalf("DeleteReservation: %v", err)
	}
	if err := db.Sched().DeleteReservation(ctx, "genomics-run"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing reservation: got %v", err)
	}
}

func TestUsageRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	// usage_events.provider_id is a real FK since 0042 — seed a provider so
	// the external_request event below has a real parent to point at.
	if err := db.Routing().SaveProvider(ctx, ProviderRow{Name: "deepseek", APIKey: "sk-x", CreatedAt: ts(1)}); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	deepseekID := testProviderID(t, db, "deepseek")

	events := []UsageEvent{
		{TS: ts(100), Kind: "load_ok", Model: "gemma", Slot: "a1"},
		{TS: ts(200), Kind: "inference", Model: "gemma", Slot: "a1", PromptTokens: 500, CompletionTokens: 100},
		{TS: ts(300), Kind: "external_request", ProviderID: &deepseekID, CostUSD: 0.0123},
	}
	for _, e := range events {
		if err := db.Usage().Record(ctx, e); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, err := db.Usage().Events(ctx, ts(150), 0)
	if err != nil || len(got) != 2 {
		t.Fatalf("Events: n=%d err=%v, want 2", len(got), err)
	}
	if got[0].Kind != "external_request" || got[0].CostUSD != 0.0123 || got[0].ProviderName != "deepseek" {
		t.Errorf("newest first mismatch: %+v", got[0])
	}
	if got[1].PromptTokens != 500 || got[1].Model != "gemma" {
		t.Errorf("event round trip: %+v", got[1])
	}
	limited, _ := db.Usage().Events(ctx, ts(0), 1)
	if len(limited) != 1 {
		t.Errorf("limit: n=%d, want 1", len(limited))
	}

	h := ModeHistoryEntry{Mode: "qwen36", TS: ts(50), TrainedCtx: 262144, ConfiguredCtx: 131072, ActualCtx: 65536, LoadTimeS: 42.5, Result: "ok"}
	if err := db.Usage().RecordHistory(ctx, h); err != nil {
		t.Fatalf("RecordHistory: %v", err)
	}
	if err := db.Usage().RecordHistory(ctx, ModeHistoryEntry{Mode: "gemma", TS: ts(60), Result: "failed"}); err != nil {
		t.Fatalf("RecordHistory 2: %v", err)
	}
	hs, err := db.Usage().History(ctx, "qwen36", 0)
	if err != nil || len(hs) != 1 {
		t.Fatalf("History: n=%d err=%v", len(hs), err)
	}
	// The crown-jewels record: trained/configured/actual ctx survive intact.
	if hs[0].TrainedCtx != 262144 || hs[0].ConfiguredCtx != 131072 || hs[0].ActualCtx != 65536 {
		t.Errorf("ctx triple mismatch: %+v", hs[0])
	}
	all, _ := db.Usage().History(ctx, "", 0)
	if len(all) != 2 {
		t.Errorf("History all: n=%d, want 2", len(all))
	}
}

func TestCompressorRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	p := ProxyRow{Service: "a1", Label: "A1", Port: 8788, TargetURL: "http://localhost:8080/v1", Unit: "headroom-a1", Token: "tok-secret", Passthrough: true}
	if err := db.Routing().SaveProxy(ctx, p); err != nil {
		t.Fatalf("SaveProxy: %v", err)
	}
	p.Port = 8798
	p.OrphanedAt = ts(700)
	if err := db.Routing().SaveProxy(ctx, p); err != nil {
		t.Fatalf("SaveProxy upsert: %v", err)
	}
	ps, err := db.Routing().Proxies(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("Proxies: n=%d err=%v", len(ps), err)
	}
	if ps[0].Port != 8798 || !ps[0].Passthrough || ps[0].Token != "tok-secret" || !ps[0].OrphanedAt.Equal(ts(700)) {
		t.Errorf("proxy round trip: %+v", ps[0])
	}
	if err := db.Routing().DeleteProxy(ctx, "a1"); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	if err := db.Routing().DeleteProxy(ctx, "a1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing proxy: got %v", err)
	}

	pr := ProviderRow{Name: "deepseek", APIKey: "sk-secret", TargetURL: "https://api.deepseek.com/v1", Model: "deepseek-chat"}
	if err := db.Routing().SaveProvider(ctx, pr); err != nil {
		t.Fatalf("SaveProvider: %v", err)
	}
	pr.Model2 = "deepseek-reasoner"
	if err := db.Routing().SaveProvider(ctx, pr); err != nil {
		t.Fatalf("SaveProvider upsert: %v", err)
	}
	prs, err := db.Routing().Providers(ctx)
	if err != nil || len(prs) != 1 || prs[0].Model2 != "deepseek-reasoner" {
		t.Fatalf("Providers: %+v err=%v", prs, err)
	}
	if err := db.Routing().DeleteProvider(ctx, testProviderID(t, db, "deepseek")); err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}

	// Savings: delta samples summed per proxy within the window. "a1" was
	// hard-deleted above (DeleteProxy) — re-create it, plus "a2" — so
	// RecordSavings has real compressor_proxies rows to point its FK at
	// (0042 — compressor_savings_totals.proxy_id).
	var a1ID, a2ID int64
	for _, svc := range []string{"a1", "a2"} {
		if err := db.Routing().SaveProxy(ctx, ProxyRow{Service: svc, Port: 9000, TargetURL: "http://x", Unit: "headroom@" + svc}); err != nil {
			t.Fatalf("seed proxy %s: %v", svc, err)
		}
	}
	seededProxies, err := db.Routing().Proxies(ctx)
	if err != nil {
		t.Fatalf("Proxies: %v", err)
	}
	for _, p := range seededProxies {
		switch p.Service {
		case "a1":
			a1ID = p.ID
		case "a2":
			a2ID = p.ID
		}
	}
	samples := []struct {
		proxyID  int64
		at       time.Time
		in, save int64
	}{
		{a1ID, ts(100), 1000, 400},
		{a1ID, ts(200), 2000, 900},
		{a2ID, ts(200), 500, 100},
		{a1ID, ts(50), 9999, 9999}, // outside window
	}
	for _, s := range samples {
		if err := db.Routing().RecordSavings(ctx, s.proxyID, s.at, s.in, s.save); err != nil {
			t.Fatalf("RecordSavings: %v", err)
		}
	}
	totals, err := db.Routing().Savings(ctx, ts(100))
	if err != nil {
		t.Fatalf("Savings: %v", err)
	}
	if totals["a1"] != (SavingsTotal{TokensIn: 3000, Saved: 1300}) {
		t.Errorf("a1 totals = %+v", totals["a1"])
	}
	if totals["a2"] != (SavingsTotal{TokensIn: 500, Saved: 100}) {
		t.Errorf("a2 totals = %+v", totals["a2"])
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if _, err := db.Settings().Get(ctx, "test.scratch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unset key: got %v, want ErrNotFound", err)
	}
	if err := db.Settings().Set(ctx, "test.scratch", []byte(`"aurora"`)); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := db.Settings().Set(ctx, "test.scratch", []byte(`"fire"`)); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	v, err := db.Settings().Get(ctx, "test.scratch")
	if err != nil || string(v) != `"fire"` {
		t.Errorf("Get = %q err=%v", v, err)
	}
}

func TestAuditWriteAndMirror(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "audit", "audit.jsonl")
	db := openTest(t, WithAuditMirror(mirror))
	ctx := context.Background()

	e := AuditEntry{TS: ts(1700000000), Actor: "testuser", Action: "switch_mode", Target: "gemma", Detail: `{"slot":"a1"}`, RemoteAddr: "100.100.100.100"}
	if err := db.Audit().Write(ctx, e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := db.Audit().Write(ctx, AuditEntry{Actor: "agent", Action: "unload"}); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	var n int
	if err := db.SQL().QueryRow(`SELECT COUNT(*) FROM audit_log`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit rows = %d err=%v", n, err)
	}

	f, err := os.Open(mirror)
	if err != nil {
		t.Fatalf("mirror missing: %v", err)
	}
	defer f.Close()
	var lines []map[string]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]string
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("mirror line not JSON: %v", err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 || lines[0]["actor"] != "testuser" || lines[0]["action"] != "switch_mode" {
		t.Errorf("mirror lines = %+v", lines)
	}
}

// TestNoMirrorByDefault: the mirror knob off means no file writes at all.
func TestNoMirrorByDefault(t *testing.T) {
	db := openTest(t)
	if err := db.Audit().Write(context.Background(), AuditEntry{Actor: "a", Action: "x"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
}
