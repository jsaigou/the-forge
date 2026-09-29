// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// secretHashMarker is deliberately distinctive so the leak assertion can
// search the CLI output for it verbatim.
const secretHashMarker = "$argon2id$v=19$SECRET-DO-NOT-PRINT"

// seedKeys inserts key rows straight through the store seam so the tests
// control every column (including a recognizable secret_hash).
func seedKeys(t *testing.T, dbPath string, keys ...store.APIKey) {
	t.Helper()
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	for _, k := range keys {
		k.SecretHash = secretHashMarker
		if err := db.Keys().Create(context.Background(), k); err != nil {
			t.Fatalf("Keys.Create(%s): %v", k.KeyID, err)
		}
	}
}

func threeKeys() []store.APIKey {
	created := time.Date(2026, 7, 24, 9, 30, 0, 0, time.UTC)
	lastUsed := time.Date(2026, 8, 6, 14, 5, 7, 0, time.UTC)
	revoked := time.Date(2026, 7, 28, 18, 0, 0, 0, time.UTC)
	return []store.APIKey{
		{KeyID: "a1b2c3d4e5f6", Kind: "forge", Name: "ops-dashboard", Role: "admin",
			CreatedAt: created, LastUsedAt: lastUsed},
		{KeyID: "0123456789ab", Kind: "router", Name: "opencode-examplehost",
			CreatedAt: created}, // never used
		{KeyID: "ffffffffffff", Kind: "mcp", Name: "laguna-verify",
			CreatedAt: created, LastUsedAt: lastUsed, RevokedAt: revoked},
	}
}

func TestKeysList_PrintsOnlyNonSensitiveColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedKeys(t, dbPath, threeKeys()...)

	out := captureStdout(t, func() {
		if err := runKeysList([]string{"-db", dbPath}); err != nil {
			t.Fatalf("keys list: %v", err)
		}
	})

	// Every non-sensitive column of every key is present.
	for _, want := range []string{
		"KIND", "NAME", "KEYID", "ROLE", "CREATED_AT", "LAST_USED_AT", "REVOKED",
		"forge", "ops-dashboard", "a1b2c3d4e5f6", "admin",
		"router", "opencode-examplehost", "0123456789ab",
		"mcp", "laguna-verify", "ffffffffffff",
		"2026-07-24 09:30:00 UTC", // created_at, human-readable UTC
		"2026-08-06 14:05:07 UTC", // last_used_at
		"2026-07-28 18:00:00 UTC", // revoked_at of the mcp key
		"active",                  // unrevoked status
		"never",                   // router key was never used
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// The secret hash must never appear, verbatim or in part.
	for _, leak := range []string{secretHashMarker, "argon", "SECRET", "secret_hash"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks secret material %q:\n%s", leak, out)
		}
	}
}

func TestKeysList_KindFilter(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedKeys(t, dbPath, threeKeys()...)

	out := captureStdout(t, func() {
		if err := runKeysList([]string{"-db", dbPath, "-kind", "mcp"}); err != nil {
			t.Fatalf("keys list -kind mcp: %v", err)
		}
	})
	if !strings.Contains(out, "laguna-verify") {
		t.Errorf("-kind mcp should list the mcp key, got:\n%s", out)
	}
	for _, other := range []string{"ops-dashboard", "opencode-examplehost", "forge", "router"} {
		if strings.Contains(out, other) {
			t.Errorf("-kind mcp leaked non-mcp row data %q:\n%s", other, out)
		}
	}
}

func TestKeysList_RendersUTC(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	// Seed with a +09:00 timestamp: the CLI must render the UTC equivalent.
	jst := time.FixedZone("UTC+9", 9*3600)
	seedKeys(t, dbPath, store.APIKey{
		KeyID: "abcdef123456", Kind: "router", Name: "kakehashi",
		CreatedAt: time.Date(2026, 8, 6, 9, 0, 0, 0, jst), // 00:00:00 UTC
	})

	out := captureStdout(t, func() {
		if err := runKeysList([]string{"-db", dbPath}); err != nil {
			t.Fatalf("keys list: %v", err)
		}
	})
	if !strings.Contains(out, "2026-08-06 00:00:00 UTC") {
		t.Errorf("expected UTC-rendered created_at, got:\n%s", out)
	}
}

func TestKeysList_RejectsUnknownKind(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedKeys(t, dbPath, threeKeys()...)
	err := runKeysList([]string{"-db", dbPath, "-kind", "bogus"})
	if err == nil || !strings.Contains(err.Error(), "-kind must be one of") {
		t.Fatalf("want a -kind validation error, got %v", err)
	}
}

func TestKeysList_DBErrorExitsNonZero(t *testing.T) {
	// A directory cannot be opened as a SQLite database. runKeysList must
	// fail; main() turns that into log.Fatalf (non-zero exit).
	err := runKeysList([]string{"-db", t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "open state db") {
		t.Fatalf("want an open-state-db error, got %v", err)
	}
}

func TestKeysCLI_SubcommandDispatch(t *testing.T) {
	if err := runKeysCLI(nil); err == nil {
		t.Fatal("expected a usage error with no subcommand")
	}
	if err := runKeysCLI([]string{"bogus"}); err == nil ||
		!strings.Contains(err.Error(), "unknown keys subcommand") {
		t.Fatalf("want an unknown-subcommand error, got %v", err)
	}
}

// TestKeysRevoke_MarksRevoked is the regression coverage for the CLI
// revoke path added alongside security sprint 3 (#37: a bearer key can no
// longer DELETE /api/v1/keys under any circumstance, so this host-side
// command is now the only non-interactive revoke path).
func TestKeysRevoke_MarksRevoked(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedKeys(t, dbPath, threeKeys()...)

	if err := runKeysRevoke([]string{"-db", dbPath, "-keyid", "a1b2c3d4e5f6"}); err != nil {
		t.Fatalf("keys revoke: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runKeysList([]string{"-db", dbPath, "-kind", "forge"}); err != nil {
			t.Fatalf("keys list: %v", err)
		}
	})
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "a1b2c3d4e5f6") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("revoked key should still be listed (soft revoke):\n%s", out)
	}
	if strings.Contains(row, "active") {
		t.Errorf("revoked key's row should no longer show active: %q", row)
	}
}

func TestKeysRevoke_RequiresKeyID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedKeys(t, dbPath, threeKeys()...)
	err := runKeysRevoke([]string{"-db", dbPath})
	if err == nil || !strings.Contains(err.Error(), "-keyid is required") {
		t.Fatalf("want a -keyid validation error, got %v", err)
	}
}

func TestKeysRevoke_UnknownKeyIDErrors(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seedKeys(t, dbPath, threeKeys()...)
	err := runKeysRevoke([]string{"-db", dbPath, "-keyid", "does-not-exist"})
	if err == nil || !strings.Contains(err.Error(), "revoke key") {
		t.Fatalf("want a revoke error for an unknown keyid, got %v", err)
	}
}
