// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"testing"
)

// Upgrade from 105: the Radiance engine and the rad format are seeded, every existing build and benchmark row is untouched (new columns NULL), an
// engine row an operator already named Radiance keeps its values, and re-applying the seed is a no-op.
func TestMigration0106And0107Upgrade(t *testing.T) {
	sqlDB := openThrough(t, 105)
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`INSERT INTO engines (name) VALUES ('Radiance')`); err != nil { // operator-added, kind defaults to llamacpp
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO builds (engine_id, name, binary_path, backend, reason, retired) VALUES (1, 'old', '/opt/x/llama-server', 'rocm', 'r', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO benchmarks (metric, value, source, subject_type, subject_id) VALUES ('safe_memory_bytes', '123', 'self_measured', 'config', 1)`); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"migrations/0106_radiance_engine.sql", "migrations/0107_benchmark_tuple.sql"} {
		body, err := migrationsFS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	var kind string
	if err := sqlDB.QueryRow(`SELECT kind FROM engines WHERE name = 'Radiance'`).Scan(&kind); err != nil || kind != "llamacpp" {
		t.Errorf("an operator-added Radiance row must keep its kind, got %q, %v", kind, err)
	}
	var n int
	if err := sqlDB.QueryRow(`SELECT COUNT(*) FROM formats WHERE name = 'rad'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("rad format rows = %d, %v", n, err)
	}
	var env, tup any
	if err := sqlDB.QueryRow(`SELECT launch_env FROM builds WHERE name = 'old'`).Scan(&env); err != nil || env != nil {
		t.Errorf("old build launch_env = %v, %v; want NULL", env, err)
	}
	var val string
	if err := sqlDB.QueryRow(`SELECT value, tuple_json FROM benchmarks`).Scan(&val, &tup); err != nil || val != "123" || tup != nil {
		t.Errorf("old benchmark = %q tuple %v, %v", val, tup, err)
	}
}

// A fresh database has the Radiance engine with kind radiance, the rad format, and round-trips launch_env and the benchmark tuple.
func TestFreshDBRadianceRowsAndRoundTrip(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	cat := db.Catalog()
	e, err := cat.EngineByName(ctx, "Radiance")
	if err != nil || e.Kind != "radiance" {
		t.Fatalf("engine = %+v, %v", e, err)
	}
	if f, err := cat.FormatByName(ctx, "rad"); err != nil || f.ID == 0 {
		t.Fatalf("format rad = %+v, %v", f, err)
	}
	id, err := cat.CreateBuild(ctx, Build{EngineID: e.ID, Name: "rad-1", BinaryPath: "/opt/radiance/bin/radiance", Backend: "rocm",
		LaunchEnv: map[string]string{"RADIANCE_HOME": "/opt/radiance/home", "RADIANCE_KERNELS": "libhalo"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := cat.GetBuild(ctx, id)
	if err != nil || b.LaunchEnv["RADIANCE_KERNELS"] != "libhalo" || len(b.LaunchEnv) != 2 {
		t.Fatalf("get = %+v, %v", b, err)
	}
	all, _ := cat.ListBuilds(ctx)
	if len(all) != 1 || all[0].LaunchEnv["RADIANCE_HOME"] != "/opt/radiance/home" {
		t.Errorf("list = %+v", all)
	}
	b.LaunchEnv = nil
	if err := cat.UpdateBuild(ctx, b); err != nil {
		t.Fatal(err)
	}
	if b2, _ := cat.GetBuild(ctx, id); b2.LaunchEnv != nil {
		t.Errorf("cleared launch_env came back as %v", b2.LaunchEnv)
	}
	if _, err := db.SQL().ExecContext(ctx, `UPDATE builds SET launch_env = 'not json' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.GetBuild(ctx, id); err == nil {
		t.Error("a corrupt launch_env must be an error, not an empty environment")
	}

	bid, err := cat.CreateBenchmark(ctx, Benchmark{Metric: "safe_memory_bytes", Value: "1", Source: "self_measured", SubjectType: "config", SubjectID: 1, Tuple: `{"x":1}`})
	if err != nil {
		t.Fatal(err)
	}
	if bm, _ := cat.GetBenchmark(ctx, bid); bm.Tuple != `{"x":1}` {
		t.Errorf("tuple = %q", bm.Tuple)
	}
	if list, _ := cat.ListBenchmarksForSubject(ctx, "config", 1); len(list) != 1 || list[0].Tuple != `{"x":1}` {
		t.Errorf("subject list = %+v", list)
	}
}
