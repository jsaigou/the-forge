// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// CacheEntry is one smith_web_cache row.
type CacheEntry struct {
	Kind        string // "search" | "fetch"
	Key         string
	Provider    string
	Title       string
	ContentType string
	StatusCode  int
	Body        string
	BodySHA256  string
	Truncated   bool
	Bytes       int
	FetchedAt   time.Time
	ExpiresAt   time.Time
}

// NewSQLCache backs Cache with smith_web_cache (migration 0037), using raw
// SQL via db.SQL() — smith's house style (conversations.go, kb.go), not an
// ORM. nil db is tolerated: every method becomes a no-op, matching
// conversations.go's `s.d.Store == nil` convention.
func NewSQLCache(db *store.DB) Cache {
	return &sqlCache{db: db}
}

type sqlCache struct {
	db *store.DB
}

func (c *sqlCache) Get(ctx context.Context, kind, key string) (CacheEntry, bool) {
	if c.db == nil {
		return CacheEntry{}, false
	}
	row := c.db.SQL().QueryRowContext(ctx, `
		SELECT provider, title, content_type, status_code, body, body_sha256,
		       truncated, bytes, fetched_at, expires_at
		FROM smith_web_cache WHERE kind = ? AND cache_key = ?`, kind, key)
	var e CacheEntry
	var fetchedAt, expiresAt int64
	var truncated int
	if err := row.Scan(&e.Provider, &e.Title, &e.ContentType, &e.StatusCode, &e.Body,
		&e.BodySHA256, &truncated, &e.Bytes, &fetchedAt, &expiresAt); err != nil {
		return CacheEntry{}, false
	}
	e.Kind, e.Key = kind, key
	e.Truncated = truncated != 0
	e.FetchedAt = time.Unix(fetchedAt, 0).UTC()
	e.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return e, true
}

// Put upserts the row. Expired-row pruning used to happen opportunistically
// here on every write ("the interim retention mechanism until P7's real
// pruning sprint" — docs/v5-smith.md §9); P7 replaced it with a real
// scheduled pruner (smith/retention.go's pruneOnce, run from scheduleLoop
// every retentionInterval) that also handles the findings tables this file
// has no reach into, so this method no longer prunes anything itself. A row
// past its TTL is never served regardless — cachedSearch/cachedDocument
// already compare FetchedAt/ExpiresAt against the TTL at read time.
func (c *sqlCache) Put(ctx context.Context, e CacheEntry) error {
	if c.db == nil {
		return nil
	}
	truncated := 0
	if e.Truncated {
		truncated = 1
	}
	_, err := c.db.SQL().ExecContext(ctx, `
		INSERT INTO smith_web_cache
			(kind, cache_key, provider, title, content_type, status_code, body,
			 body_sha256, truncated, bytes, fetched_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(kind, cache_key) DO UPDATE SET
			provider = excluded.provider, title = excluded.title,
			content_type = excluded.content_type, status_code = excluded.status_code,
			body = excluded.body, body_sha256 = excluded.body_sha256,
			truncated = excluded.truncated, bytes = excluded.bytes,
			fetched_at = excluded.fetched_at, expires_at = excluded.expires_at`,
		e.Kind, e.Key, e.Provider, e.Title, e.ContentType, e.StatusCode, e.Body,
		e.BodySHA256, truncated, e.Bytes, e.FetchedAt.Unix(), e.ExpiresAt.Unix())
	return err
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
