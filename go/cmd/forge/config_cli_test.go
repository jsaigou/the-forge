// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func TestConfigCLI_SetThenGet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	if err := runConfigSet([]string{"-db", dbPath, "-force", "test.scratch", `"dark"`}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runConfigGet([]string{"-db", dbPath, "test.scratch"}); err != nil {
			t.Fatalf("runConfigGet: %v", err)
		}
	})
	if strings.TrimSpace(out) != `"dark"` {
		t.Errorf("config get test.scratch = %q, want %q", strings.TrimSpace(out), `"dark"`)
	}

	// Round-trips through the store's real JSON-KV, not just CLI plumbing.
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	raw, err := db.Settings().Get(context.Background(), "test.scratch")
	if err != nil {
		t.Fatalf("Settings.Get: %v", err)
	}
	if string(raw) != `"dark"` {
		t.Errorf("stored value = %q, want %q", raw, `"dark"`)
	}
}

func TestConfigCLI_SetRejectsInvalidJSON(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	if err := runConfigSet([]string{"-db", dbPath, "-force", "test.scratch", "not-json"}); err == nil {
		t.Fatal("expected an error for a non-JSON value")
	}
}

func TestConfigCLI_GetMissingKey(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	db.Close()

	if err := runConfigGet([]string{"-db", dbPath, "does.not.exist"}); err == nil {
		t.Fatal("expected an error for a missing key")
	}
}

func TestConfigCLI_Dump(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	if err := runConfigSet([]string{"-db", dbPath, "infra.server", `{"listen":":5000"}`}); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}

	out := captureStdout(t, func() {
		if err := runConfigDump([]string{"-db", dbPath}); err != nil {
			t.Fatalf("runConfigDump: %v", err)
		}
	})
	if !strings.Contains(out, `"listen": ":5000"`) {
		t.Errorf("dump should contain the infra.server value, got:\n%s", out)
	}
	if !strings.Contains(out, "a0 router config snapshot") {
		t.Errorf("dump should include the router config snapshot, got:\n%s", out)
	}
	if !strings.Contains(out, "NOT read back by forge") {
		t.Errorf("dump should carry the read-only warning, got:\n%s", out)
	}
}

func TestConfigCLI_UnknownSubcommand(t *testing.T) {
	if err := runConfigCLI([]string{"bogus"}); err == nil {
		t.Fatal("expected an error for an unknown config subcommand")
	}
}

// ADR-0020 F7: `config set` shares the settings validation, and leaves an audit record.
func TestConfigCLI_SetValidatesAndAudits(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	ctx := context.Background()
	stored := func(key string) (string, bool) {
		db, err := store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		raw, err := db.Settings().Get(ctx, key)
		return string(raw), err == nil
	}

	// A key nothing reads is refused, and nothing is written.
	if err := runConfigSet([]string{"-db", dbPath, "smith.binaries.trackd", `[]`}); err == nil || !strings.Contains(err.Error(), "unknown settings key") {
		t.Fatalf("typo'd key: err=%v, want an unknown-key refusal", err)
	}
	if _, ok := stored("smith.binaries.trackd"); ok {
		t.Error("a refused write still created the key")
	}
	// A known key with the wrong shape is refused, even with --force, and nothing is written.
	if err := runConfigSet([]string{"-db", dbPath, "-force", "smith.binaries.tracked", `[{"name":"x","kind":"binary","path":"/x"}]`}); err == nil {
		t.Fatal("a mis-shaped smith.binaries.tracked was accepted")
	}
	if _, ok := stored("smith.binaries.tracked"); ok {
		t.Error("a refused write still stored the value")
	}

	// A valid write lands and is audited as the CLI actor, without the value.
	good := `[{"name":"llama.cpp","kind":"llama_build","path":"/opt/x/bin/llama-server"}]`
	if err := runConfigSet([]string{"-db", dbPath, "smith.binaries.tracked", good}); err != nil {
		t.Fatalf("valid write: %v", err)
	}
	if got, _ := stored("smith.binaries.tracked"); got != good {
		t.Errorf("stored %q, want %q", got, good)
	}
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Audit().List(ctx, "settings_set", "smith.binaries.tracked", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit rows = %v (err %v), want exactly one", rows, err)
	}
	if !strings.HasPrefix(rows[0].Actor, "cli:") || strings.Contains(rows[0].Detail, "llama-server") {
		t.Errorf("audit entry = %+v, want a cli:<user> actor and no value in the detail", rows[0])
	}
}
