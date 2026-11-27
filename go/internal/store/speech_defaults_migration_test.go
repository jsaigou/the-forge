// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func openForSpeechMigration(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Open already ran the migration once, which removes the row migration 0074 seeds; start each test from a clean slate.
	if _, err := db.SQL().Exec(`DELETE FROM settings WHERE key IN ('tts.engines','tts.defaults')`); err != nil {
		t.Fatal(err)
	}
	return db
}

func addOccupant(t *testing.T, db *DB, name, kind, params, functions string) {
	t.Helper()
	if _, err := db.SQL().Exec(`INSERT INTO specialist_occupants (name, kind, params, functions) VALUES (?, ?, ?, ?)`, name, kind, params, functions); err != nil {
		t.Fatal(err)
	}
}

func putSetting(t *testing.T, db *DB, key, value string) {
	t.Helper()
	if err := db.Settings().Set(context.Background(), key, []byte(value)); err != nil {
		t.Fatal(err)
	}
}

func readSetting(t *testing.T, db *DB, key string) (string, bool) {
	t.Helper()
	v, err := db.Settings().Get(context.Background(), key)
	if errors.Is(err, ErrNotFound) {
		return "", false
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(v), true
}

func defaultsOf(t *testing.T, db *DB) map[string]string {
	t.Helper()
	raw, ok := readSetting(t, db, "tts.defaults")
	if !ok {
		t.Fatal("tts.defaults was not written")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("tts.defaults = %q: %v", raw, err)
	}
	return m
}

// The shape the live ForgeHost store has: base and kokoro run pool engines, customvoice is turned off, voicedesign never had one.
const liveShapedRows = `{"kokoro":{"enabled":true,"mode":"resident","unit":"","url":"","pool_member":"kokoro"},
 "customvoice":{"enabled":false,"mode":"disabled","unit":"forge-tts-custom","url":"","pool_member":"qwen3-tts-customvoice"},
 "voicedesign":{"enabled":true,"mode":"available","unit":"","url":""},
 "base":{"enabled":true,"mode":"resident","unit":"forge-tts-base","url":"http://127.0.0.1:8093","pool_member":"qwen3-tts-base"}}`

func seedLiveEngines(t *testing.T, db *DB) {
	t.Helper()
	addOccupant(t, db, "kokoro", "kokoro", `{}`, `["speech"]`)
	addOccupant(t, db, "qwen3-tts-base", "qwen-tts-server", `{"alias":"base","model":"/m/b.gguf","codec":"/m/c.gguf"}`, `["speech"]`)
	addOccupant(t, db, "qwen3-tts-customvoice", "qwen-tts-server", `{"alias":"customvoice","model":"/m/c.gguf","codec":"/m/c.gguf"}`, `["speech"]`)
}

func TestTTSEnginesMigration_LiveShapedStore(t *testing.T) {
	db := openForSpeechMigration(t)
	seedLiveEngines(t, db)
	putSetting(t, db, "tts.defaults", `{}`) // the live store holds an empty object: that is "unset"
	putSetting(t, db, "tts.engines", liveShapedRows)

	res, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || res.Kept {
		t.Fatalf("result = %+v", res)
	}
	got := defaultsOf(t, db)
	want := map[string]string{"base": "qwen3-tts-base", "kokoro": "kokoro"} // customvoice was turned off; voicedesign had no engine
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("defaults = %v, want %v", got, want)
	}
	if _, still := readSetting(t, db, "tts.engines"); still {
		t.Fatal("tts.engines must be removed")
	}
	if len(res.Skipped) != 2 {
		t.Errorf("skipped = %v, want customvoice (off) and voicedesign (no engine) explained", res.Skipped)
	}
}

func TestTTSEnginesMigration_IsIdempotent(t *testing.T) {
	db := openForSpeechMigration(t)
	seedLiveEngines(t, db)
	putSetting(t, db, "tts.engines", liveShapedRows)
	if _, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL()); err != nil {
		t.Fatal(err)
	}
	first := defaultsOf(t, db)
	// A later change by the operator must survive a second run (there is nothing left to migrate).
	putSetting(t, db, "tts.defaults", `{"base":"somebody-else"}`)
	res, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL())
	if err != nil {
		t.Fatal(err)
	}
	if res.Ran {
		t.Fatalf("a second run did something: %+v", res)
	}
	if got := defaultsOf(t, db); got["base"] != "somebody-else" || len(first) != 2 {
		t.Fatalf("defaults after a second run = %v (first run: %v)", got, first)
	}
}

func TestTTSEnginesMigration_NothingToMigrate(t *testing.T) {
	db := openForSpeechMigration(t) // no tts.engines at all
	res, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL())
	if err != nil || res.Ran {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if _, ok := readSetting(t, db, "tts.defaults"); ok {
		t.Fatal("tts.defaults must not be created when there was nothing to migrate")
	}
}

func TestTTSEnginesMigration_FreshDatabaseDoesNotKeepTheSeededRow(t *testing.T) {
	// Migration 0074 seeds tts.engines in every new database; Open moves it on, so a fresh database never carries the retired key.
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, still := readSetting(t, db, "tts.engines"); still {
		t.Fatal("a fresh database still has tts.engines after Open")
	}
	if got := defaultsOf(t, db); len(got) != 0 {
		t.Fatalf("a fresh database got defaults %v from the seeded rows (they run no pool engine)", got)
	}
}

func TestTTSEnginesMigration_RowsThatCannotBecomeDefaults(t *testing.T) {
	db := openForSpeechMigration(t)
	addOccupant(t, db, "kokoro", "kokoro", `{}`, `["speech"]`)
	addOccupant(t, db, "asr", "qwen3-asr-server", `{}`, `["transcription"]`)
	addOccupant(t, db, "design-a", "qwen-tts-server", `{"alias":"voicedesign"}`, `["speech"]`)
	putSetting(t, db, "tts.engines", `{
	 "kokoro":{"enabled":true,"mode":"resident","pool_member":""},
	 "base":{"enabled":true,"mode":"resident","pool_member":"ghost"},
	 "customvoice":{"enabled":true,"mode":"resident","pool_member":"asr"},
	 "voicedesign":{"enabled":true,"mode":"resident","pool_member":"design-a"}}`)

	res, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL())
	if err != nil {
		t.Fatal(err)
	}
	got := defaultsOf(t, db)
	if !reflect.DeepEqual(got, map[string]string{"voicedesign": "design-a"}) {
		t.Fatalf("defaults = %v, want only the valid row (empty pool_member, a missing engine and a non-speech engine are skipped)", got)
	}
	if len(res.Skipped) != 3 {
		t.Fatalf("skipped = %v, want 3 reasons", res.Skipped)
	}
	// An engine of the wrong kind is not accepted either.
	db2 := openForSpeechMigration(t)
	addOccupant(t, db2, "kokoro", "kokoro", `{}`, `["speech"]`)
	putSetting(t, db2, "tts.engines", `{"base":{"enabled":true,"mode":"resident","pool_member":"kokoro"}}`)
	if _, err := migrateTTSEnginesToDefaults(context.Background(), db2.SQL()); err != nil {
		t.Fatal(err)
	}
	if got := defaultsOf(t, db2); len(got) != 0 {
		t.Fatalf("a kokoro engine became the base default: %v", got)
	}
}

func TestTTSEnginesMigration_OperatorDefaultsAreKept(t *testing.T) {
	db := openForSpeechMigration(t)
	seedLiveEngines(t, db)
	putSetting(t, db, "tts.defaults", `{"base":"my-choice"}`)
	putSetting(t, db, "tts.engines", liveShapedRows)
	res, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran || !res.Kept {
		t.Fatalf("res = %+v", res)
	}
	if got := defaultsOf(t, db); !reflect.DeepEqual(got, map[string]string{"base": "my-choice"}) {
		t.Fatalf("defaults = %v, want the operator's choice untouched", got)
	}
	if _, still := readSetting(t, db, "tts.engines"); still {
		t.Fatal("tts.engines must still be removed")
	}
}

func TestTTSEnginesMigration_MalformedKeyChangesNothingAndReports(t *testing.T) {
	db := openForSpeechMigration(t)
	putSetting(t, db, "tts.engines", `[1,2,3]`)
	if _, err := migrateTTSEnginesToDefaults(context.Background(), db.SQL()); err == nil {
		t.Fatal("a malformed tts.engines must be reported, not skipped silently")
	}
	if _, still := readSetting(t, db, "tts.engines"); !still {
		t.Fatal("a failed migration must leave tts.engines in place")
	}
	if _, ok := readSetting(t, db, "tts.defaults"); ok {
		t.Fatal("a failed migration must not write tts.defaults")
	}
}

// Guard against the table moving: the migration reads these columns directly.
func TestTTSEnginesMigration_OccupantColumns(t *testing.T) {
	db := openForSpeechMigration(t)
	var n int
	if err := db.SQL().QueryRow(`SELECT count(*) FROM pragma_table_info('specialist_occupants') WHERE name IN ('name','kind','params','functions')`).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("specialist_occupants lacks a column the speech-defaults migration reads (found %d of 4)", n)
	}
}
