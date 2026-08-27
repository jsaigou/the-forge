// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// TestMigrateFresh proves the schema applies cleanly through the latest
// migration and is idempotent-safe at the version-tracking level.
func TestMigrateFresh(t *testing.T) {
	db, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var version int
	if err := db.SQL().QueryRow(
		`SELECT MAX(version) FROM schema_migrations`,
	).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != 76 {
		t.Fatalf("schema version = %d, want 76", version)
	}

	// Every Contract 3 table (0001) plus the Sprint 0 §0.11 polish tables
	// (0002) and the Sprint 0-AUTH tables (0004) must exist. 0003
	// (headroom-aiand rename) alters data, not the table set.
	want := []string{
		"users", "sessions", "api_keys", "slot_state", "sched_queue",
		"reservations", "usage_events", "compressor_savings_totals",
		"compressor_proxies", "router_providers", "mode_history",
		"settings", "nodes", "audit_log",
		// 0002_polish.sql (provider_models dropped in 0043, Phase 7 — dead table):
		"fx_rates", "provider_state", "metric_samples",
		// 0004_auth_v2.sql:
		"identity_links", "webauthn_credentials", "totp_secrets", "recovery_codes",
		// 0006_model_profiles.sql (PROFILE track):
		"model_profiles",
		// 0008_model_catalog.sql (MODEL CATALOG track):
		"families", "models", "variants", "quantizations", "formats",
		"artifacts", "engines", "builds", "compatibilities", "configs",
		"services", "offerings", "benchmarks", "notes",
		// 0013_slots.sql (TOML decommission Phase 0):
		"slots",
		// 0015_notifications.sql (product/QA sprint):
		"notifications",
		// 0017_genealogies.sql (product/QA sprint):
		"genealogies",
		// 0018_profile_benchmarks.sql (product/QA sprint):
		"model_profile_benchmarks",
		// 0019_favorites.sql (product/QA sprint):
		"favorites",
		// 0021_cost_and_savings.sql (Dashboard cost/savings data layer):
		"compressor_savings_samples", "compressor_label_samples", "provider_credit_samples",
		// 0031_model_prefill_stats.sql (Compressor local-savings prefill sprint):
		"model_prefill_stats",
		// 0033_smith_core.sql (smith P0 — self-diagnosis agent):
		"smith_conversations", "smith_messages", "smith_investigations",
		"smith_findings", "smith_actions",
		// 0037_smith_web.sql (smith P5 — web research):
		"smith_web_cache",
		// 0066_scheduler_jobs.sql (P3 scheduler jobs):
		"scheduler_jobs",
		"smith_build_refresh_upstream",
	}
	for _, table := range want {
		var name string
		err := db.SQL().QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}

	// Two-layer rule (0063/0064): the 0037/0038 deployment seeds must not
	// survive a fresh migrate — a fresh install never observes seeded
	// deployment values, whatever storage class they carried.
	for _, key := range []string{
		"smith.web.searxng", "smith.web.firecrawl",
		"smith.comfyui.url", "smith.comfyui.unit",
		"smith.comfyui.model_roots", "smith.comfyui.workflow_dirs",
		"smith.binaries.tracked",
	} {
		var n int
		if err := db.SQL().QueryRow(
			`SELECT COUNT(*) FROM settings WHERE key=?`, key,
		).Scan(&n); err != nil {
			t.Errorf("count %s: %v", key, err)
		} else if n != 0 {
			t.Errorf("settings key %q survived fresh migration — deployment seed leaked", key)
		}
	}

	// Reservation CHECK constraints from Contract 3: bay set iff scope='bay'.
	_, err = db.SQL().Exec(
		`INSERT INTO reservations (label, model, start_ts, end_ts, scope, bay,
		 created_by, allow_agent_reschedule, allow_agent_cancellation, created_at)
		 VALUES ('x', 'm', 1, 2, 'whole_box', 'a1', 'testuser', 0, 0, 1)`,
	)
	if err == nil {
		t.Error("whole_box reservation with bay set should violate CHECK")
	}
}
