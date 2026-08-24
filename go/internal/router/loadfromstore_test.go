// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// TestLoadFromStoreEmpty proves LoadFromStore behaves like ParseConfig on an
// absent [router] table: no infra.router settings key set, so every field
// falls back to applyDefaults() (TOML decommission Phase 1,
// docs/v5-toml-decommission.md §4). Backends/Routes stay nil — ADR-0007
// leaves no schema for them here; see LoadFromStore's doc comment.
func TestLoadFromStoreEmpty(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg, err := LoadFromStore(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if cfg.ConnectTimeoutS != 5 {
		t.Errorf("connect_timeout_s default = %v, want 5", cfg.ConnectTimeoutS)
	}
	if cfg.BusyMode != BusyWait {
		t.Errorf("busy_mode default = %q, want %q", cfg.BusyMode, BusyWait)
	}
	if cfg.Backends != nil || cfg.Routes != nil {
		t.Errorf("Backends/Routes = %+v/%+v, want nil (no schema replacement — ADR-0007)",
			cfg.Backends, cfg.Routes)
	}
}

// TestLoadFromStorePopulated exercises the real path: the infra.router
// settings key set, mirroring what the Phase 2 cutover migration will write.
func TestLoadFromStorePopulated(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	raw, err := json.Marshal(RouterConfig{
		ListenPort:      8085,
		ConnectTimeoutS: 3,
		EmbeddingURL:    "http://localhost:8083",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Settings().Set(ctx, "infra.router", raw); err != nil {
		t.Fatalf("Set infra.router: %v", err)
	}

	cfg, err := LoadFromStore(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if cfg.ListenPort != 8085 {
		t.Errorf("listen_port = %d, want 8085", cfg.ListenPort)
	}
	if cfg.ConnectTimeoutS != 3 {
		t.Errorf("connect_timeout_s = %v, want 3", cfg.ConnectTimeoutS)
	}
	if cfg.EmbeddingURL != "http://localhost:8083" {
		t.Errorf("embedding_url = %q", cfg.EmbeddingURL)
	}
	// RequestTimeoutS wasn't set — stays 0 (unbounded), no forced default.
	if cfg.RequestTimeoutS != 0 {
		t.Errorf("request_timeout_s default = %v, want 0 (unbounded)", cfg.RequestTimeoutS)
	}
}

// TestLoadFromStoreRejectsBadEmbeddingURL proves validate() still runs
// against a store-sourced config, not just a file-sourced one.
func TestLoadFromStoreRejectsBadEmbeddingURL(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	raw, err := json.Marshal(RouterConfig{EmbeddingURL: "not-a-url"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Settings().Set(ctx, "infra.router", raw); err != nil {
		t.Fatalf("Set infra.router: %v", err)
	}

	if _, err := LoadFromStore(ctx, db); err == nil {
		t.Error("invalid embedding_url must be rejected")
	}
}
