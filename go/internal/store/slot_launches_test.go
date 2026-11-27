// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func goodLaunch(slot string) SlotLaunch {
	id := int64(7)
	return SlotLaunch{
		Slot: slot, ConfigID: &id, ConfigName: "qwen-test", EngineKind: "llamacpp", Backend: "rocm",
		Exe: "/opt/llama/bin/llama-server", Argv: []string{"--port", "8081", "-m", "/models/a.gguf"},
		Env: map[string]string{"HSA_OVERRIDE_GFX_VERSION": "11.5.1"}, Cwd: "/opt/llama",
		Port: 8081, ModelPath: "/models/a.gguf", NCtx: 8192, WrittenBy: "v0.6.test",
	}
}

func fileDB(t *testing.T) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func TestMigration0104_FreshAndUpgrade(t *testing.T) {
	// Fresh: the table exists with the ADR's columns.
	db := openTestDB(t)
	rows, err := db.SQL().Query(`SELECT name FROM pragma_table_info('slot_launches')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got[n] = true
	}
	for _, c := range []string{"slot", "generation", "format_version", "armed", "config_id", "config_name", "engine_kind", "backend", "exe", "argv_json",
		"env_json", "cwd", "port", "model_path", "mmproj_path", "n_ctx", "spec_hash", "written_at", "written_by"} {
		if !got[c] {
			t.Errorf("slot_launches lacks column %q", c)
		}
	}
	if len(got) != 19 {
		t.Errorf("slot_launches has %d columns, want 19", len(got))
	}

	// Upgrade: a database at 0103 with data keeps its data, gains the table, and applying the file twice is harmless.
	old := openThrough(t, 103)
	defer old.Close()
	if _, err := old.Exec(`INSERT INTO engines (name) VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}
	body, err := migrationsFS.ReadFile("migrations/0104_slot_launches.sql")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := old.Exec(string(body)); err != nil {
			t.Fatalf("apply 0104 (run %d): %v", i+1, err)
		}
	}
	var n int
	if err := old.QueryRow(`SELECT count(*) FROM engines WHERE name = 'kept'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("existing data lost: n=%d err=%v", n, err)
	}
	if _, err := old.Exec(`INSERT INTO slot_launches (slot, generation, config_name, engine_kind, exe, argv_json, port, spec_hash, written_at)
		VALUES ('a1', 1, 'c', 'llamacpp', '/x', '[]', 1, 'h', 0)`); err != nil {
		t.Fatalf("insert with defaults: %v", err)
	}
	if _, err := old.Exec(`UPDATE slot_launches SET armed = 2`); err == nil {
		t.Error("armed = 2 was accepted, want the CHECK constraint to refuse it")
	}
}

func TestSlotLaunch_RoundTripAndGeneration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	in := goodLaunch("a1")
	g1, err := db.SlotLaunches().Put(ctx, in)
	if err != nil || g1 != 1 {
		t.Fatalf("first Put = %d, %v; want 1", g1, err)
	}
	got, err := db.SlotLaunches().Get(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Generation != 1 || !got.Armed || got.FormatVersion != SlotLaunchFormatVersion || got.Exe != in.Exe || got.Port != 8081 ||
		got.ConfigName != "qwen-test" || got.ConfigID == nil || *got.ConfigID != 7 || got.NCtx != 8192 || got.Cwd != "/opt/llama" ||
		strings.Join(got.Argv, " ") != strings.Join(in.Argv, " ") || got.Env["HSA_OVERRIDE_GFX_VERSION"] != "11.5.1" || got.WrittenBy != "v0.6.test" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if want := SlotLaunchSpecHash(in.Exe, in.Argv, in.Env, in.Cwd); got.SpecHash != want {
		t.Errorf("spec_hash = %s, want %s", got.SpecHash, want)
	}

	// A second Put increments, re-arms and replaces every field.
	if ok, err := db.SlotLaunches().Disarm(ctx, "a1", 1); err != nil || !ok {
		t.Fatalf("disarm: %v %v", ok, err)
	}
	in2 := goodLaunch("a1")
	in2.Argv = []string{"--port", "8081"}
	in2.ConfigID = nil
	in2.Env = nil
	g2, err := db.SlotLaunches().Put(ctx, in2)
	if err != nil || g2 != 2 {
		t.Fatalf("second Put = %d, %v; want 2", g2, err)
	}
	got, _ = db.SlotLaunches().Get(ctx, "a1")
	if !got.Armed || got.ConfigID != nil || len(got.Argv) != 2 || len(got.Env) != 0 || got.SpecHash == "" {
		t.Errorf("after replace: %+v", got)
	}
	if g, _ := db.SlotLaunches().Put(ctx, goodLaunch("a2")); g != 1 {
		t.Errorf("other slot starts at generation %d, want 1", g)
	}

	list, err := db.SlotLaunches().List(ctx)
	if err != nil || len(list) != 2 || list[0].Slot != "a1" || list[1].Slot != "a2" {
		t.Errorf("List = %v, %v", list, err)
	}

	// The audit entry carries slot, config, generation and hash, and no argv or env content.
	aud, err := db.Audit().List(ctx, "slot_launch.", "a1", 10)
	if err != nil || len(aud) != 2 {
		t.Fatalf("audit = %v, %v", aud, err)
	}
	for _, e := range aud {
		if strings.Contains(e.Detail, "/models/a.gguf") || strings.Contains(e.Detail, "HSA_") || !strings.Contains(e.Detail, "spec_hash=") {
			t.Errorf("audit detail %q", e.Detail)
		}
	}

	if _, err := db.SlotLaunches().Get(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing = %v, want ErrNotFound", err)
	}
	if err := db.SlotLaunches().Delete(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := db.SlotLaunches().Delete(ctx, "a2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
}

func TestSlotLaunch_DisarmCompareAndSet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	g1, _ := db.SlotLaunches().Put(ctx, goodLaunch("a1"))
	g2, _ := db.SlotLaunches().Put(ctx, goodLaunch("a1"))
	// A late unload holding the old generation must not disarm the newer row.
	if ok, err := db.SlotLaunches().Disarm(ctx, "a1", g1); err != nil || ok {
		t.Fatalf("stale disarm = %v, %v; want false, nil", ok, err)
	}
	if l, _ := db.SlotLaunches().Get(ctx, "a1"); !l.Armed {
		t.Fatal("stale disarm changed the row")
	}
	if ok, err := db.SlotLaunches().Disarm(ctx, "a1", g2); err != nil || !ok {
		t.Fatalf("current disarm = %v, %v; want true", ok, err)
	}
	if ok, _ := db.SlotLaunches().Disarm(ctx, "a1", g2); ok {
		t.Error("second disarm reported a change")
	}
	if ok, err := db.SlotLaunches().Disarm(ctx, "missing", 1); err != nil || ok {
		t.Errorf("disarm of a missing row = %v, %v; want false, nil", ok, err)
	}
}

// Many writers race: Puts hand out distinct, gapless generations, and of many disarms of one generation exactly one wins.
func TestSlotLaunch_ConcurrentWriters(t *testing.T) {
	_, path := fileDB(t)
	// A second handle on the same file: real contention across connections, not just one serialised pool.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	db1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db1.Close()
	ctx := context.Background()

	const puts = 20
	gens := make(chan int64, puts)
	var wg sync.WaitGroup
	for i := 0; i < puts; i++ {
		wg.Add(1)
		db := db1
		if i%2 == 1 {
			db = db2
		}
		go func() {
			defer wg.Done()
			g, err := db.SlotLaunches().Put(ctx, goodLaunch("a1"))
			if err != nil {
				t.Errorf("Put: %v", err)
				return
			}
			gens <- g
		}()
	}
	wg.Wait()
	close(gens)
	seen := map[int64]bool{}
	for g := range gens {
		if seen[g] {
			t.Errorf("generation %d handed out twice", g)
		}
		seen[g] = true
	}
	for g := int64(1); g <= puts; g++ {
		if !seen[g] {
			t.Errorf("generation %d missing; not gapless", g)
		}
	}

	var won int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		db := db1
		if i%2 == 1 {
			db = db2
		}
		go func() {
			defer wg.Done()
			ok, err := db.SlotLaunches().Disarm(ctx, "a1", puts)
			if err != nil {
				t.Errorf("Disarm: %v", err)
				return
			}
			if ok {
				atomic.AddInt32(&won, 1)
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Errorf("%d disarms of the same generation succeeded, want exactly 1", won)
	}
}

func TestSlotLaunch_Validation(t *testing.T) {
	long := strings.Repeat("x", maxSlotLaunchPathLen+1)
	manyArgs := make([]string, maxSlotLaunchArgs+1)
	manyEnv := map[string]string{}
	for i := 0; i <= maxSlotLaunchEnvVars; i++ {
		manyEnv["V"+strings.Repeat("A", 3)+string(rune('A'+i%26))+string(rune('A'+i/26%26))+string(rune('A'+i/676))] = "1"
	}
	bigArgs := make([]string, 20)
	for i := range bigArgs {
		bigArgs[i] = strings.Repeat("y", maxSlotLaunchArgBytes)
	}
	cases := []struct {
		name  string
		edit  func(*SlotLaunch)
		field string
	}{
		{"slot empty", func(l *SlotLaunch) { l.Slot = "" }, "slot"},
		{"slot upper", func(l *SlotLaunch) { l.Slot = "A1" }, "slot"},
		{"slot leading dash", func(l *SlotLaunch) { l.Slot = "-a1" }, "slot"},
		{"slot slash", func(l *SlotLaunch) { l.Slot = "a/1" }, "slot"},
		{"slot too long", func(l *SlotLaunch) { l.Slot = strings.Repeat("a", 33) }, "slot"},
		{"slot NUL", func(l *SlotLaunch) { l.Slot = "a\x001" }, "slot"},
		{"exe empty", func(l *SlotLaunch) { l.Exe = "" }, "exe"},
		{"exe relative", func(l *SlotLaunch) { l.Exe = "llama-server" }, "exe"},
		{"exe NUL", func(l *SlotLaunch) { l.Exe = "/bin/a\x00b" }, "exe"},
		{"exe too long", func(l *SlotLaunch) { l.Exe = "/" + long }, "exe"},
		{"cwd relative", func(l *SlotLaunch) { l.Cwd = "opt" }, "cwd"},
		{"model_path NUL", func(l *SlotLaunch) { l.ModelPath = "a\x00" }, "model_path"},
		{"config_name empty", func(l *SlotLaunch) { l.ConfigName = "" }, "config_name"},
		{"engine_kind empty", func(l *SlotLaunch) { l.EngineKind = "" }, "engine_kind"},
		{"port zero", func(l *SlotLaunch) { l.Port = 0 }, "port"},
		{"port high", func(l *SlotLaunch) { l.Port = 65536 }, "port"},
		{"n_ctx negative", func(l *SlotLaunch) { l.NCtx = -1 }, "n_ctx"},
		{"argv NUL", func(l *SlotLaunch) { l.Argv = []string{"ok", "a\x00b"} }, "argv"},
		{"argv too many", func(l *SlotLaunch) { l.Argv = manyArgs }, "argv"},
		{"argv one too big", func(l *SlotLaunch) { l.Argv = []string{strings.Repeat("y", maxSlotLaunchArgBytes+1)} }, "argv"},
		{"argv total too big", func(l *SlotLaunch) { l.Argv = bigArgs }, "argv"},
		{"env lowercase name", func(l *SlotLaunch) { l.Env = map[string]string{"path": "x"} }, "env"},
		{"env digit first", func(l *SlotLaunch) { l.Env = map[string]string{"1A": "x"} }, "env"},
		{"env equals in name", func(l *SlotLaunch) { l.Env = map[string]string{"A=B": "x"} }, "env"},
		{"env empty name", func(l *SlotLaunch) { l.Env = map[string]string{"": "x"} }, "env"},
		{"env NUL value", func(l *SlotLaunch) { l.Env = map[string]string{"A": "x\x00"} }, "env"},
		{"env too many", func(l *SlotLaunch) { l.Env = manyEnv }, "env"},
		{"env value too big", func(l *SlotLaunch) { l.Env = map[string]string{"A": strings.Repeat("z", maxSlotLaunchEnvBytes+1)} }, "env"},
	}
	db := openTestDB(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := goodLaunch("a1")
			tc.edit(&l)
			_, err := db.SlotLaunches().Put(context.Background(), l)
			var inv *SlotLaunchInvalid
			if !errors.As(err, &inv) {
				t.Fatalf("Put = %v, want *SlotLaunchInvalid", err)
			}
			if _, ok := inv.Fields[tc.field]; !ok {
				t.Errorf("fields = %v, want one for %q", inv.Fields, tc.field)
			}
		})
	}
	if rows, _ := db.SlotLaunches().List(context.Background()); len(rows) != 0 {
		t.Errorf("a refused Put wrote %d row(s)", len(rows))
	}
	// The same slot pattern as internal/engine's slotNameRe, by example.
	for s, want := range map[string]bool{"a1": true, "a1-b": true, "0": true, strings.Repeat("a", 32): true, "a_1": false, "Z": false} {
		if ValidSlotLaunchSlot(s) != want {
			t.Errorf("ValidSlotLaunchSlot(%q) != %v", s, want)
		}
	}
	// A minimal row with empty Argv/Env and no cwd is fine.
	l := goodLaunch("a1")
	l.Argv, l.Env, l.Cwd = nil, nil, ""
	if _, err := db.SlotLaunches().Put(context.Background(), l); err != nil {
		t.Errorf("minimal launch refused: %v", err)
	}
}

func TestSlotLaunch_SpecHashIsCanonical(t *testing.T) {
	a := SlotLaunchSpecHash("/x", []string{"a", "b"}, map[string]string{"B": "2", "A": "1"}, "/c")
	b := SlotLaunchSpecHash("/x", []string{"a", "b"}, map[string]string{"A": "1", "B": "2"}, "/c")
	if a != b || len(a) != 64 {
		t.Errorf("hash not stable over map order: %s %s", a, b)
	}
	if SlotLaunchSpecHash("/x", nil, nil, "") != SlotLaunchSpecHash("/x", []string{}, map[string]string{}, "") {
		t.Error("nil and empty differ")
	}
	for name, h := range map[string]string{
		"exe":  SlotLaunchSpecHash("/y", []string{"a", "b"}, map[string]string{"A": "1", "B": "2"}, "/c"),
		"argv": SlotLaunchSpecHash("/x", []string{"ab"}, map[string]string{"A": "1", "B": "2"}, "/c"),
		"env":  SlotLaunchSpecHash("/x", []string{"a", "b"}, map[string]string{"A": "1"}, "/c"),
		"cwd":  SlotLaunchSpecHash("/x", []string{"a", "b"}, map[string]string{"A": "1", "B": "2"}, ""),
	} {
		if h == a {
			t.Errorf("changing %s did not change the hash", name)
		}
	}
}

func TestSlotLaunch_MalformedStoredRow(t *testing.T) {
	db := openTestDB(t)
	if _, err := db.SQL().Exec(`INSERT INTO slot_launches (slot, generation, config_name, engine_kind, exe, argv_json, env_json, port, spec_hash, written_at)
		VALUES ('a1', 1, 'c', 'llamacpp', '/x', '{"not":"array"}', '{}', 1, 'h', 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SlotLaunches().Get(context.Background(), "a1"); !errors.Is(err, ErrSlotLaunchMalformed) {
		t.Errorf("Get = %v, want ErrSlotLaunchMalformed", err)
	}
}

func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	db, path := fileDB(t)
	if _, err := db.SlotLaunches().Put(ctx, goodLaunch("a1")); err != nil {
		t.Fatal(err)
	}
	if err := db.Settings().Set(ctx, "catalog.build_roots", []byte(`["/opt/x"]`)); err != nil {
		t.Fatal(err)
	}

	t.Run("reads a row and a setting", func(t *testing.T) {
		ro, err := OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		l, err := ro.GetSlotLaunch(ctx, "a1")
		if err != nil || l.Generation != 1 || l.Exe != "/opt/llama/bin/llama-server" {
			t.Errorf("GetSlotLaunch = %+v, %v", l, err)
		}
		if _, err := ro.GetSlotLaunch(ctx, "zz"); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing slot = %v, want ErrNotFound", err)
		}
		if v, err := ro.GetSetting(ctx, "catalog.build_roots"); err != nil || string(v) != `["/opt/x"]` {
			t.Errorf("GetSetting = %s, %v", v, err)
		}
		if _, err := ro.GetSetting(ctx, "nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("unset setting = %v, want ErrNotFound", err)
		}
	})

	t.Run("cannot write", func(t *testing.T) {
		ro, err := OpenReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		for _, stmt := range []string{
			`UPDATE slot_launches SET armed = 0`,
			`DELETE FROM slot_launches`,
			`CREATE TABLE evil (x)`,
			`INSERT INTO settings (key, value, updated_at) VALUES ('k', 'v', 0)`,
		} {
			if _, err := ro.sql.Exec(stmt); err == nil {
				t.Errorf("%q succeeded on the read-only handle", stmt)
			}
		}
		if l, _ := db.SlotLaunches().Get(ctx, "a1"); !l.Armed {
			t.Error("row changed through the read-only handle")
		}
	})

	t.Run("reads while another connection holds a write transaction", func(t *testing.T) {
		raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		conn, err := raw.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE slot_launches SET config_name = 'uncommitted'`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			ro, err := OpenReadOnly(path)
			if err != nil {
				done <- err
				return
			}
			defer ro.Close()
			l, err := ro.GetSlotLaunch(ctx, "a1")
			if err == nil && l.ConfigName != "qwen-test" {
				err = errors.New("read an uncommitted value: " + l.ConfigName)
			}
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("read during write transaction: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("read-only open/read blocked behind a writer")
		}
		if _, err := conn.ExecContext(ctx, `ROLLBACK`); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOpenReadOnly_Refusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	t.Run("missing file is not created", func(t *testing.T) {
		p := filepath.Join(dir, "absent.db")
		if _, err := OpenReadOnly(p); !errors.Is(err, ErrDBMissing) {
			t.Fatalf("err = %v, want ErrDBMissing", err)
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "absent") {
				t.Errorf("created %s", e.Name())
			}
		}
	})
	t.Run("memory and empty paths", func(t *testing.T) {
		for _, p := range []string{"", ":memory:"} {
			if _, err := OpenReadOnly(p); err == nil {
				t.Errorf("OpenReadOnly(%q) succeeded", p)
			}
		}
	})
	t.Run("empty file is not a forge database and stays empty", func(t *testing.T) {
		p := filepath.Join(dir, "empty.db")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenReadOnly(p); !errors.Is(err, ErrSchemaTooOld) {
			t.Errorf("err = %v, want ErrSchemaTooOld", err)
		}
		if fi, _ := os.Stat(p); fi.Size() != 0 {
			t.Errorf("file grew to %d bytes: the opener wrote to it", fi.Size())
		}
	})
	t.Run("schema at 0103 is refused and left unchanged", func(t *testing.T) {
		p := filepath.Join(dir, "v103.db")
		raw, err := sql.Open("sqlite", p+"?_pragma=journal_mode(DELETE)")
		if err != nil {
			t.Fatal(err)
		}
		raw.SetMaxOpenConns(1)
		if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
			INSERT INTO schema_migrations VALUES (103, 'now')`); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		before, _ := os.ReadFile(p)
		_, err = OpenReadOnly(p)
		if !errors.Is(err, ErrSchemaTooOld) {
			t.Fatalf("err = %v, want ErrSchemaTooOld", err)
		}
		after, _ := os.ReadFile(p)
		if string(before) != string(after) {
			t.Error("the opener changed the file (it must never migrate)")
		}
		if _, err := os.Stat(p + "-wal"); err == nil {
			t.Error("created a -wal file")
		}
	})
	t.Run("version 104 without the table", func(t *testing.T) {
		p := filepath.Join(dir, "notable.db")
		raw, err := sql.Open("sqlite", p)
		if err != nil {
			t.Fatal(err)
		}
		raw.SetMaxOpenConns(1)
		if _, err := raw.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
			INSERT INTO schema_migrations VALUES (104, 'now')`); err != nil {
			t.Fatal(err)
		}
		raw.Close()
		if _, err := OpenReadOnly(p); !errors.Is(err, ErrSlotLaunchesMissing) {
			t.Errorf("err = %v, want ErrSlotLaunchesMissing", err)
		}
	})
	t.Run("file that is not a database", func(t *testing.T) {
		p := filepath.Join(dir, "junk.db")
		if err := os.WriteFile(p, []byte(strings.Repeat("not sqlite ", 200)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenReadOnly(p); err == nil {
			t.Error("opened a non-database")
		}
	})
	t.Run("path with query characters cannot inject DSN parameters", func(t *testing.T) {
		// Open's own DSN cannot name such a file, so create it under a plain name (Close checkpoints the WAL) and rename.
		plain := filepath.Join(dir, "plain.db")
		d, err := Open(plain)
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
		p := filepath.Join(dir, "odd?name#1.db")
		if err := os.Rename(plain, p); err != nil {
			t.Fatal(err)
		}
		ro, err := OpenReadOnly(p)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		ro.Close()
	})
	_ = ctx
}
