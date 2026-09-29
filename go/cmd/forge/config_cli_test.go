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

	if err := runConfigSet([]string{"-db", dbPath, "test.scratch", `"dark"`}); err != nil {
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
	if err := runConfigSet([]string{"-db", dbPath, "test.scratch", "not-json"}); err == nil {
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
