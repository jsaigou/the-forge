// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/store"
)

// writeSeedFile writes raw to a temp seed file and returns its path.
func writeSeedFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	return path
}

func TestSmithCLI_ImportLocal(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed := writeSeedFile(t, `{
		"mesh_services": [{"name":"svc-one","aliases":["svc-one"],"address":"svc-one.example.ts.net:1000"}],
		"build_refresh_forks": [{"source_ref":"/opt/test/tree","remote":"origin","upstream_ref":"origin/main",
			"backends":{"vulkan":{"backend":"vulkan","configure_flags":["-DGGML_VULKAN=ON"]}},
			"representative_config":{"vulkan":"tiny-model"}}]
	}`)

	out := captureStdout(t, func() {
		if err := runSmithImportLocal([]string{"-db", dbPath, seed}); err != nil {
			t.Fatalf("runSmithImportLocal: %v", err)
		}
	})
	if !strings.Contains(out, "smith.mesh.services: imported 1 entries") {
		t.Errorf("summary missing mesh count: %q", out)
	}
	if !strings.Contains(out, "smith.build_refresh.forks: imported 1 entries") {
		t.Errorf("summary missing fork count: %q", out)
	}
	if !strings.Contains(out, "smith.binaries.tracked: not in file") {
		t.Errorf("summary must report absent sections untouched: %q", out)
	}

	// Round-trips through the store's real JSON-KV, read by the live
	// decoders — the CLI is thin plumbing over smith.ImportLocalSeed.
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	s := smith.New(smith.Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	if got := len(s.MeshServices(context.Background())); got != 1 {
		t.Errorf("MeshServices = %d entries, want 1", got)
	}
	if got := len(s.BuildRefreshForks(context.Background())); got != 1 {
		t.Errorf("BuildRefreshForks = %d entries, want 1", got)
	}
}

func TestSmithCLI_ImportLocalRejectsMalformed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	seed := writeSeedFile(t, `{"mesh_servicez": []}`) // typo'd section name
	if err := runSmithImportLocal([]string{"-db", dbPath, seed}); err == nil {
		t.Fatal("expected an error for an unknown section name (typo guard)")
	}
}

func TestSmithCLI_ImportLocalMissingFile(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	if err := runSmithImportLocal([]string{"-db", dbPath, filepath.Join(t.TempDir(), "nope.json")}); err == nil {
		t.Fatal("expected an error for a missing seed file")
	}
}

func TestSmithCLI_UnknownSubcommand(t *testing.T) {
	if err := runSmithCLI([]string{"export-local"}); err == nil {
		t.Fatal("expected an error for an unknown smith subcommand")
	}
}
