// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// One-time move from the four fixed Voice & Speech rows (`tts.engines`) to the default engine per kind (`tts.defaults`)
// (docs/speech-routing-plan.md, P4). The old rows could name a pool engine (`pool_member`); that engine becomes the default for its kind. Everything
// else in the old key (unit names, URLs, resident/available modes) described how the daemon rendered forge-tts's environment, which it no longer does,
// so it is dropped. The old key is removed so it cannot be mistaken for live settings.
//
// It runs when the database is opened, after the schema migrations, and is idempotent: with no `tts.engines` it does nothing.

const (
	settingTTSEnginesRetired = "tts.engines"
	settingTTSDefaults       = "tts.defaults"
)

// speechKinds are the kinds of speech an engine can serve, named as forge-tts and the pool name them (specialist.SpeechModes). Kept here because the
// specialist package imports this one.
var speechKinds = []string{"base", "customvoice", "voicedesign", "kokoro"}

// retiredEngineRow is the part of a stored `tts.engines` row the migration reads.
type retiredEngineRow struct {
	Enabled    bool   `json:"enabled"`
	Mode       string `json:"mode"`
	PoolMember string `json:"pool_member"`
}

// SpeechDefaultsMigration reports what migrateTTSEnginesToDefaults did, for the log and for tests.
type SpeechDefaultsMigration struct {
	Ran      bool              // a `tts.engines` key existed and was removed
	Defaults map[string]string // the defaults written (kind -> engine name); empty when none could be derived
	Kept     bool              // `tts.defaults` already held a choice, so it was left as it was
	Skipped  []string          // one human line per old row that did not become a default, with the reason
}

func migrateTTSEnginesToDefaults(ctx context.Context, db *sql.DB) (SpeechDefaultsMigration, error) {
	var res SpeechDefaultsMigration
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("store: tts.engines migration: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	var oldRaw []byte
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingTTSEnginesRetired).Scan(&oldRaw); {
	case errors.Is(err, sql.ErrNoRows):
		return res, nil // already migrated, or never had the key
	case err != nil:
		return res, fmt.Errorf("store: tts.engines migration: read %s: %w", settingTTSEnginesRetired, err)
	}
	res.Ran = true

	var existing []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, settingTTSDefaults).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return res, fmt.Errorf("store: tts.engines migration: read %s: %w", settingTTSDefaults, err)
	}
	// Defaults that hold at least one choice belong to the operator: keep them. A missing value, or {}, is "unset".
	if hasAnyDefault(existing) {
		res.Kept = true
	} else {
		var rows map[string]retiredEngineRow
		if err := json.Unmarshal(oldRaw, &rows); err != nil {
			return res, fmt.Errorf("store: tts.engines migration: %s is not the JSON object it should be (nothing was changed): %w", settingTTSEnginesRetired, err)
		}
		res.Defaults = map[string]string{}
		kinds := append([]string(nil), speechKinds...)
		sort.Strings(kinds)
		for _, kind := range kinds {
			row, ok := rows[kind]
			if !ok {
				continue
			}
			switch {
			case !row.Enabled || row.Mode == "disabled":
				res.Skipped = append(res.Skipped, kind+": the row was turned off")
				continue
			case row.PoolMember == "":
				res.Skipped = append(res.Skipped, kind+": the row ran no pool engine (it used its own unit and address, which no longer exist)")
				continue
			}
			why, err := speechEngineProblem(ctx, tx, row.PoolMember, kind)
			if err != nil {
				return res, err
			}
			if why != "" {
				res.Skipped = append(res.Skipped, kind+": "+why)
				continue
			}
			res.Defaults[kind] = row.PoolMember
		}
		raw, err := json.Marshal(res.Defaults)
		if err != nil {
			return res, fmt.Errorf("store: tts.engines migration: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			settingTTSDefaults, raw, unixOf(time.Now())); err != nil {
			return res, fmt.Errorf("store: tts.engines migration: write %s: %w", settingTTSDefaults, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, settingTTSEnginesRetired); err != nil {
		return res, fmt.Errorf("store: tts.engines migration: remove %s: %w", settingTTSEnginesRetired, err)
	}
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("store: tts.engines migration: commit: %w", err)
	}
	return res, nil
}

func hasAnyDefault(raw []byte) bool {
	var m map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil {
		return false
	}
	return len(m) > 0
}

// speechEngineProblem checks that name is a registered pool engine of the given kind that serves speech: "" when it is, else the reason it is not.
func speechEngineProblem(ctx context.Context, tx *sql.Tx, name, kind string) (string, error) {
	var occKind, params, functions string
	err := tx.QueryRowContext(ctx, `SELECT kind, params, functions FROM specialist_occupants WHERE name = ?`, name).Scan(&occKind, &params, &functions)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Sprintf("the engine %q no longer exists", name), nil
	}
	if err != nil {
		return "", fmt.Errorf("store: tts.engines migration: look up engine %q: %w", name, err)
	}
	var fns []string
	if err := json.Unmarshal([]byte(functions), &fns); err != nil {
		return "", fmt.Errorf("store: tts.engines migration: engine %q has unreadable functions %q: %w", name, functions, err)
	}
	serves := false
	for _, f := range fns {
		if f == "speech" {
			serves = true
		}
	}
	if !serves {
		return fmt.Sprintf("the engine %q does not serve speech", name), nil
	}
	have := ""
	switch occKind {
	case "kokoro":
		have = "kokoro"
	case "qwen-tts-server":
		var p map[string]string
		if err := json.Unmarshal([]byte(params), &p); err != nil {
			return "", fmt.Errorf("store: tts.engines migration: engine %q has unreadable params: %w", name, err)
		}
		have = p["alias"]
	}
	if have != kind {
		if have == "" {
			have = "no recognised kind"
		}
		return fmt.Sprintf("the engine %q is a %s engine, not %s", name, have, kind), nil
	}
	return "", nil
}

func logSpeechDefaultsMigration(res SpeechDefaultsMigration) {
	if !res.Ran {
		return
	}
	switch {
	case res.Kept:
		log.Printf("store: removed the retired %s setting; %s already held a choice and was kept", settingTTSEnginesRetired, settingTTSDefaults)
	case len(res.Defaults) == 0:
		log.Printf("store: removed the retired %s setting; no default engine could be derived from it", settingTTSEnginesRetired)
	default:
		kinds := make([]string, 0, len(res.Defaults))
		for k := range res.Defaults {
			kinds = append(kinds, k)
		}
		sort.Strings(kinds)
		var parts []string
		for _, k := range kinds {
			parts = append(parts, k+"="+res.Defaults[k])
		}
		log.Printf("store: moved the retired %s setting to %s (%s)", settingTTSEnginesRetired, settingTTSDefaults, strings.Join(parts, ", "))
	}
	for _, s := range res.Skipped {
		log.Printf("store: retired %s row not carried over: %s", settingTTSEnginesRetired, s)
	}
}
