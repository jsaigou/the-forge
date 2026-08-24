// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSQLCache_NilDB(t *testing.T) {
	c := NewSQLCache(nil)
	if _, ok := c.Get(context.Background(), "fetch", "x"); ok {
		t.Fatal("nil db Get should always miss")
	}
	if err := c.Put(context.Background(), CacheEntry{Kind: "fetch", Key: "x"}); err != nil {
		t.Fatalf("nil db Put should no-op, got %v", err)
	}
}

func TestSQLCache_RoundTrip(t *testing.T) {
	db := newTestDB(t)
	c := NewSQLCache(db)
	now := time.Now().Truncate(time.Second)
	entry := CacheEntry{
		Kind: "fetch", Key: "https://example.com", Provider: "direct",
		Title: "Example", ContentType: "text/html", StatusCode: 200,
		Body: "hello world", BodySHA256: "abc123", Truncated: false, Bytes: 11,
		FetchedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	if err := c.Put(context.Background(), entry); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok := c.Get(context.Background(), "fetch", "https://example.com")
	if !ok {
		t.Fatal("expected a hit after Put")
	}
	if got.Provider != "direct" || got.Body != "hello world" || got.Title != "Example" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.FetchedAt.Equal(now) {
		t.Fatalf("FetchedAt = %v, want %v", got.FetchedAt, now)
	}
}

func TestSQLCache_Upsert(t *testing.T) {
	db := newTestDB(t)
	c := NewSQLCache(db)
	now := time.Now().Truncate(time.Second)
	key := "https://example.com/q"
	_ = c.Put(context.Background(), CacheEntry{Kind: "search", Key: key, Body: "v1", FetchedAt: now, ExpiresAt: now.Add(time.Hour)})
	_ = c.Put(context.Background(), CacheEntry{Kind: "search", Key: key, Body: "v2", FetchedAt: now.Add(time.Minute), ExpiresAt: now.Add(2 * time.Hour)})

	got, ok := c.Get(context.Background(), "search", key)
	if !ok || got.Body != "v2" {
		t.Fatalf("expected upsert to overwrite, got %+v ok=%v", got, ok)
	}
	var count int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM smith_web_cache WHERE kind='search' AND cache_key=?`, key).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one row after upsert, got %d", count)
	}
}

func TestSQLCache_KindIsolation(t *testing.T) {
	// The same key under "search" and "fetch" must not collide — the
	// unique index is (kind, cache_key), not cache_key alone.
	db := newTestDB(t)
	c := NewSQLCache(db)
	now := time.Now()
	key := "same-key"
	_ = c.Put(context.Background(), CacheEntry{Kind: "search", Key: key, Body: "search-body", FetchedAt: now, ExpiresAt: now.Add(time.Hour)})
	_ = c.Put(context.Background(), CacheEntry{Kind: "fetch", Key: key, Body: "fetch-body", FetchedAt: now, ExpiresAt: now.Add(time.Hour)})

	s, ok := c.Get(context.Background(), "search", key)
	if !ok || s.Body != "search-body" {
		t.Fatalf("search entry corrupted: %+v ok=%v", s, ok)
	}
	f, ok := c.Get(context.Background(), "fetch", key)
	if !ok || f.Body != "fetch-body" {
		t.Fatalf("fetch entry corrupted: %+v ok=%v", f, ok)
	}
}

// TestSQLCache_PutNoLongerPrunes is a regression test for the P7 retention
// sprint (docs/v5-smith.md §9): Put used to opportunistically delete
// expired rows on every write ("the interim retention mechanism until P7's
// real pruning sprint"); that responsibility moved to a real scheduled
// pruner (smith/retention.go's pruneOnce, covered there against a real
// store — TestPruneOnce_WebCacheAgeAndRowCap). Put itself must now leave
// other rows alone — a Put for one key is not license to delete another.
func TestSQLCache_PutNoLongerPrunes(t *testing.T) {
	db := newTestDB(t)
	c := NewSQLCache(db)
	past := time.Now().Add(-2 * time.Hour)
	_ = c.Put(context.Background(), CacheEntry{Kind: "fetch", Key: "stale", Body: "old", FetchedAt: past, ExpiresAt: past.Add(time.Minute)})

	now := time.Now()
	_ = c.Put(context.Background(), CacheEntry{Kind: "fetch", Key: "fresh", Body: "new", FetchedAt: now, ExpiresAt: now.Add(time.Hour)})

	var count int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM smith_web_cache WHERE cache_key='stale'`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("Put must no longer prune other rows — expected the expired row to survive, got %d rows", count)
	}
}

func TestSQLCache_MissReturnsZeroValue(t *testing.T) {
	db := newTestDB(t)
	c := NewSQLCache(db)
	if _, ok := c.Get(context.Background(), "fetch", "never-fetched"); ok {
		t.Fatal("expected a miss for an unknown key")
	}
}
