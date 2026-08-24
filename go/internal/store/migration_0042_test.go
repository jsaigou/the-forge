package store

import (
	"database/sql"
	"io/fs"
	"sort"
	"testing"
)

// openThrough applies embedded migrations up to and including maxVersion
// only — used to seed pre-0042 data for a realistic backfill test.
func openThrough(t *testing.T, maxVersion int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	sort.Strings(names)
	for _, name := range names {
		v, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("version: %v", err)
		}
		if v > maxVersion {
			continue
		}
		body, err := migrationsFS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	return db
}

func TestMigration0042Backfill(t *testing.T) {
	db := openThrough(t, 41)
	defer db.Close()

	// Seed a realistic pre-0042 world: two providers, one proxy linked via
	// the FORWARD pointer (headroom_proxies.provider), one linked only via
	// the REVERSE pointer (router_providers.headroom_proxy) to exercise the
	// desync-fallback branch, an offering, a usage_event, a config +
	// matching model_profile/model_prefill_stats row, and an orphaned
	// model_profile (mode with no matching config) to prove it gets dropped.
	stmts := []string{
		`INSERT INTO router_providers (name, api_key, headroom_proxy, created_at) VALUES ('deepseek', 'k1', 'hr-deepseek', 1000)`,
		`INSERT INTO router_providers (name, api_key, headroom_proxy, created_at) VALUES ('aiand', 'k2', '', 1000)`,
		`INSERT INTO headroom_proxies (service, port, target_url, unit, provider, created_at) VALUES ('hr-deepseek', 9001, 'http://x', 'headroom@deepseek', 'deepseek', 1000)`,
		// Reverse-only link: aiand's own row never set headroom_proxy, but
		// a DIFFERENT stale provider row points at this service (the real
		// desync shape) — exercise the COALESCE fallback.
		`INSERT INTO router_providers (name, api_key, headroom_proxy, created_at) VALUES ('stale', 'k3', 'hr-aiand', 1000)`,
		`INSERT INTO headroom_proxies (service, port, target_url, unit, created_at) VALUES ('hr-aiand', 9002, 'http://y', 'headroom@aiand', 1000)`,
	}
	// families/models/variants/configs minimal chain for offerings + model_profiles FKs.
	stmts = append(stmts,
		`INSERT INTO families (id, genealogy_id, name) VALUES (1, NULL, 'fam')`,
		`INSERT INTO models (id, family_id, name) VALUES (1, 1, 'model')`,
		`INSERT INTO variants (id, model_id, name) VALUES (1, 1, 'variant')`,
		`INSERT INTO artifacts (id, variant_id, format_id, file_path) VALUES (1, 1, (SELECT id FROM formats WHERE name = 'GGUF'), '/x.gguf')`,
		`INSERT INTO configs (id, name, variant_id, weight_artifact_id, engine_id) VALUES (1, 'myconfig', 1, 1, (SELECT id FROM engines WHERE name = 'llama.cpp'))`,
		`INSERT INTO offerings (model_id, variant_id, provider, wire_model) VALUES (1, 1, 'deepseek', 'deepseek-chat')`,
		`INSERT INTO usage_events (ts, kind, provider) VALUES (1000, 'external_request', 'deepseek')`,
		`INSERT INTO usage_events (ts, kind, provider) VALUES (1000, 'inference', NULL)`,
		`INSERT INTO model_profiles (mode, n_ctx, backend, safe_memory_bytes, prefill_tps, decode_tps, actual_n_ctx, fingerprint, measured_at) VALUES ('myconfig', 4096, 'vulkan', 1000, 1.0, 1.0, 4096, 'fp', 1000)`,
		`INSERT INTO model_profiles (mode, n_ctx, backend, safe_memory_bytes, prefill_tps, decode_tps, actual_n_ctx, fingerprint, measured_at) VALUES ('deleted-config', 4096, 'vulkan', 1000, 1.0, 1.0, 4096, 'fp2', 1000)`,
		`INSERT INTO model_prefill_stats (mode, fingerprint, first_seen, last_seen) VALUES ('myconfig', 'fp', 1000, 1000)`,
		`INSERT INTO reservations (label, model, start_ts, end_ts, scope, created_by, allow_agent_reschedule, allow_agent_cancellation, created_at) VALUES ('res1', 'model', 1000, 2000, 'whole_box', 'testuser', 0, 0, 1000)`,
	)
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}

	// Apply 0042 directly.
	body, err := migrationsFS.ReadFile("migrations/0042_surrogate_keys.sql")
	if err != nil {
		t.Fatalf("read 0042: %v", err)
	}
	if _, err := db.Exec(string(body)); err != nil {
		t.Fatalf("apply 0042: %v", err)
	}
	if _, err := db.Exec(`PRAGMA foreign_key_check`); err != nil {
		t.Fatalf("fk_check exec: %v", err)
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("fk_check query: %v", err)
	}
	for rows.Next() {
		var table, parent sql.NullString
		var rowid, fkid sql.NullInt64
		rows.Scan(&table, &rowid, &parent, &fkid)
		t.Errorf("FK violation: table=%v rowid=%v parent=%v", table, rowid, parent)
	}
	rows.Close()

	// Forward-linked proxy resolved correctly.
	var providerName string
	if err := db.QueryRow(`SELECT rp.name FROM headroom_proxies hp JOIN router_providers rp ON rp.id = hp.provider_id WHERE hp.service = 'hr-deepseek'`).Scan(&providerName); err != nil {
		t.Fatalf("forward link: %v", err)
	}
	if providerName != "deepseek" {
		t.Errorf("forward link provider = %q, want deepseek", providerName)
	}

	// Reverse-only (desync) link resolved via fallback.
	if err := db.QueryRow(`SELECT rp.name FROM headroom_proxies hp JOIN router_providers rp ON rp.id = hp.provider_id WHERE hp.service = 'hr-aiand'`).Scan(&providerName); err != nil {
		t.Fatalf("reverse link: %v", err)
	}
	if providerName != "stale" {
		t.Errorf("reverse link provider = %q, want stale", providerName)
	}

	// Offering provider_id resolves.
	var offeringProvider string
	if err := db.QueryRow(`SELECT rp.name FROM offerings o JOIN router_providers rp ON rp.id = o.provider_id`).Scan(&offeringProvider); err != nil {
		t.Fatalf("offering link: %v", err)
	}
	if offeringProvider != "deepseek" {
		t.Errorf("offering provider = %q, want deepseek", offeringProvider)
	}

	// usage_events: one with a resolved provider_id, one with NULL preserved.
	var withProvider, withoutProvider int
	db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE provider_id IS NOT NULL`).Scan(&withProvider)
	db.QueryRow(`SELECT COUNT(*) FROM usage_events WHERE provider_id IS NULL`).Scan(&withoutProvider)
	if withProvider != 1 || withoutProvider != 1 {
		t.Errorf("usage_events provider_id split = %d/%d, want 1/1", withProvider, withoutProvider)
	}

	// model_profiles: the orphaned 'deleted-config' row is gone, the live one kept with config_id.
	var profileCount int
	db.QueryRow(`SELECT COUNT(*) FROM model_profiles`).Scan(&profileCount)
	if profileCount != 1 {
		t.Errorf("model_profiles count = %d, want 1 (orphan dropped)", profileCount)
	}
	var configID int64
	if err := db.QueryRow(`SELECT config_id FROM model_profiles WHERE fingerprint = 'fp'`).Scan(&configID); err != nil {
		t.Fatalf("profile config_id: %v", err)
	}
	if configID != 1 {
		t.Errorf("profile config_id = %d, want 1", configID)
	}

	// model_prefill_stats: config_id resolved.
	var prefillConfigID int64
	if err := db.QueryRow(`SELECT config_id FROM model_prefill_stats WHERE fingerprint = 'fp'`).Scan(&prefillConfigID); err != nil {
		t.Fatalf("prefill config_id: %v", err)
	}
	if prefillConfigID != 1 {
		t.Errorf("prefill config_id = %d, want 1", prefillConfigID)
	}

	// reservations: gained an id, label preserved.
	var resID int64
	var resLabel string
	if err := db.QueryRow(`SELECT id, label FROM reservations`).Scan(&resID, &resLabel); err != nil {
		t.Fatalf("reservation: %v", err)
	}
	if resLabel != "res1" || resID == 0 {
		t.Errorf("reservation id/label = %d/%q, want nonzero/res1", resID, resLabel)
	}

	// sqlite_master: confirm REFERENCES clauses were rewritten off the
	// "_new" names by the rename (the ordering-trap claim, verified not assumed).
	var offeringsSQL string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='offerings'`).Scan(&offeringsSQL); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	if want := "router_providers_new"; contains(offeringsSQL, want) {
		t.Errorf("offerings.sql still references %q after rename:\n%s", want, offeringsSQL)
	}
	if !contains(offeringsSQL, "router_providers") {
		t.Errorf("offerings.sql lost its router_providers reference entirely:\n%s", offeringsSQL)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
