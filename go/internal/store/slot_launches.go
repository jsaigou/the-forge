// SPDX-License-Identifier: Apache-2.0

package store

// The launch of a slot as a database row (ADR-0026 section 1, Radiance Phase 1b step a). The engine manager is the single writer: it Puts a row after
// the slot's old unit is gone and before it starts the new one. The row carries the executable, the argument vector and the variables to ADD to the
// environment; `forge slot-exec <slot>` reads it through OpenReadOnly and exec()s the engine. There is no secret in a row and no column for one.
//
// Rules in one place, at the boundary: a Put that breaks them is refused with *SlotLaunchInvalid and nothing is written. Errors are typed
// (ErrNotFound, *SlotLaunchInvalid, ErrSlotLaunchMalformed) or wrapped store errors; none is swallowed.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SlotLaunchFormatVersion is the row layout this build writes and understands. slot-exec refuses a higher number it does not know.
const SlotLaunchFormatVersion = 1

// Size limits. They bound a hostile or runaway row; a real launch is a few dozen arguments and a handful of variables.
const (
	maxSlotLaunchArgs     = 4096
	maxSlotLaunchArgBytes = 64 * 1024
	maxSlotLaunchEnvVars  = 256
	maxSlotLaunchEnvBytes = 64 * 1024 // per value
	maxSlotLaunchTotal    = 1 << 20   // all argv + env bytes together
	maxSlotLaunchPathLen  = 4096      // exe, cwd, model_path, mmproj_path
	maxSlotLaunchNameLen  = 256       // config_name, engine_kind, backend, written_by
)

var (
	// slotLaunchSlotRe is the engine package's slotNameRe (internal/engine/slotname.go); the store may not import engine, so the pattern is repeated and
	// a test pins the two together by example.
	slotLaunchSlotRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	slotLaunchEnvRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// ErrSlotLaunchMalformed is returned when a stored row cannot be decoded (bad JSON in argv_json or env_json). slot-exec maps it to exit 78.
var ErrSlotLaunchMalformed = errors.New("store: slot launch row is malformed")

// SlotLaunchInvalid lists what is wrong with a SlotLaunch passed to Put; Fields maps a field name to the reason.
type SlotLaunchInvalid struct {
	Fields map[string]string
}

func (e *SlotLaunchInvalid) Error() string {
	keys := make([]string, 0, len(e.Fields))
	for k := range e.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+e.Fields[k])
	}
	return "store: invalid slot launch: " + strings.Join(parts, "; ")
}

// SlotLaunch is one slot's launch row. Env holds variables to ADD to the inherited environment.
type SlotLaunch struct {
	Slot          string
	Generation    int64 // set by Put; ignored on input
	FormatVersion int   // set by Put; ignored on input
	Armed         bool  // Put always writes armed = true; ignored on input
	ConfigID      *int64
	ConfigName    string
	EngineKind    string
	Backend       string
	Exe           string
	Argv          []string
	Env           map[string]string
	Cwd           string
	Port          int
	ModelPath     string
	MmprojPath    string
	NCtx          int
	SpecHash      string // computed by Put from Exe, Argv, Env and Cwd; ignored on input
	WrittenAt     time.Time
	WrittenBy     string
}

// ValidSlotLaunchSlot reports whether name is a legal slot name.
func ValidSlotLaunchSlot(name string) bool { return slotLaunchSlotRe.MatchString(name) }

// SlotLaunchSpecHash is the sha256 (hex) of the canonical form of the launch: a JSON object {exe, argv, env, cwd}, env keys sorted (encoding/json
// sorts map keys), nil argv and env normalised to empty. It is drift and audit evidence, not a secret; it depends only on what is executed.
func SlotLaunchSpecHash(exe string, argv []string, env map[string]string, cwd string) string {
	if argv == nil {
		argv = []string{}
	}
	if env == nil {
		env = map[string]string{}
	}
	b, _ := json.Marshal(struct {
		Exe  string            `json:"exe"`
		Argv []string          `json:"argv"`
		Env  map[string]string `json:"env"`
		Cwd  string            `json:"cwd"`
	}{exe, argv, env, cwd}) // marshalling strings and string maps cannot fail
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Validate checks l as Put would and returns nil when it is acceptable.
func (l SlotLaunch) Validate() *SlotLaunchInvalid {
	f := map[string]string{}
	if !slotLaunchSlotRe.MatchString(l.Slot) {
		f["slot"] = "must match ^[a-z0-9][a-z0-9-]{0,31}$"
	}
	nonEmptyName := func(field, v string, required bool) {
		switch {
		case required && v == "":
			f[field] = "is required"
		case len(v) > maxSlotLaunchNameLen:
			f[field] = fmt.Sprintf("is longer than %d bytes", maxSlotLaunchNameLen)
		case strings.IndexByte(v, 0) >= 0:
			f[field] = "contains a NUL byte"
		}
	}
	nonEmptyName("config_name", l.ConfigName, true)
	nonEmptyName("engine_kind", l.EngineKind, true)
	nonEmptyName("backend", l.Backend, false)
	nonEmptyName("written_by", l.WrittenBy, false)

	pathField := func(field, v string, required, mustAbs bool) {
		switch {
		case v == "" && required:
			f[field] = "is required"
		case v == "":
		case len(v) > maxSlotLaunchPathLen:
			f[field] = fmt.Sprintf("is longer than %d bytes", maxSlotLaunchPathLen)
		case strings.IndexByte(v, 0) >= 0:
			f[field] = "contains a NUL byte"
		case mustAbs && !filepath.IsAbs(v):
			f[field] = "must be an absolute path"
		}
	}
	pathField("exe", l.Exe, true, true)
	pathField("cwd", l.Cwd, false, true)
	pathField("model_path", l.ModelPath, false, false)
	pathField("mmproj_path", l.MmprojPath, false, false)

	if l.Port < 1 || l.Port > 65535 {
		f["port"] = "must be between 1 and 65535"
	}
	if l.NCtx < 0 {
		f["n_ctx"] = "must not be negative"
	}

	total := 0
	if len(l.Argv) > maxSlotLaunchArgs {
		f["argv"] = fmt.Sprintf("has more than %d arguments", maxSlotLaunchArgs)
	} else {
		for i, a := range l.Argv {
			if strings.IndexByte(a, 0) >= 0 {
				f["argv"] = fmt.Sprintf("argument %d contains a NUL byte", i)
				break
			}
			if len(a) > maxSlotLaunchArgBytes {
				f["argv"] = fmt.Sprintf("argument %d is longer than %d bytes", i, maxSlotLaunchArgBytes)
				break
			}
			total += len(a)
		}
	}
	if len(l.Env) > maxSlotLaunchEnvVars {
		f["env"] = fmt.Sprintf("has more than %d variables", maxSlotLaunchEnvVars)
	} else {
		for k, v := range l.Env {
			if !slotLaunchEnvRe.MatchString(k) {
				f["env"] = fmt.Sprintf("variable name %q must match ^[A-Z][A-Z0-9_]*$", k)
				break
			}
			if strings.IndexByte(v, 0) >= 0 {
				f["env"] = fmt.Sprintf("value of %s contains a NUL byte", k)
				break
			}
			if len(v) > maxSlotLaunchEnvBytes {
				f["env"] = fmt.Sprintf("value of %s is longer than %d bytes", k, maxSlotLaunchEnvBytes)
				break
			}
			total += len(k) + len(v)
		}
	}
	if _, bad := f["argv"]; !bad {
		if _, bad := f["env"]; !bad && total > maxSlotLaunchTotal {
			f["argv"] = fmt.Sprintf("argv and env together are larger than %d bytes", maxSlotLaunchTotal)
		}
	}
	if len(f) == 0 {
		return nil
	}
	return &SlotLaunchInvalid{Fields: f}
}

type slotLaunchView struct{ d *DB }

// SlotLaunches returns the slot_launches API. Only the engine manager writes (ADR-0026 section 2).
func (d *DB) SlotLaunches() slotLaunchView { return slotLaunchView{d} }

// Put validates l and replaces the slot's row in one transaction, returning the new generation (1 for a first write, previous + 1 otherwise). The
// row is written armed, with the current format version and a spec hash computed here. An audit entry (slot, config, generation, spec_hash; never argv
// or env content) is written in the same transaction.
func (v slotLaunchView) Put(ctx context.Context, l SlotLaunch) (int64, error) {
	if bad := l.Validate(); bad != nil {
		return 0, bad
	}
	argv := l.Argv
	if argv == nil {
		argv = []string{}
	}
	env := l.Env
	if env == nil {
		env = map[string]string{}
	}
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		return 0, fmt.Errorf("store: slot_launches.put %q: encode argv: %w", l.Slot, err)
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return 0, fmt.Errorf("store: slot_launches.put %q: encode env: %w", l.Slot, err)
	}
	hash := SlotLaunchSpecHash(l.Exe, argv, env, l.Cwd)
	at := l.WrittenAt
	if at.IsZero() {
		at = time.Now()
	}
	var cfgID any
	if l.ConfigID != nil {
		cfgID = *l.ConfigID
	}

	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: slot_launches.put %q: begin: %w", l.Slot, err)
	}
	defer tx.Rollback() // no-op after Commit
	var gen int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO slot_launches (slot, generation, format_version, armed, config_id, config_name, engine_kind, backend, exe, argv_json, env_json,
		   cwd, port, model_path, mmproj_path, n_ctx, spec_hash, written_at, written_by)
		 VALUES (?, 1, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(slot) DO UPDATE SET
		   generation = slot_launches.generation + 1, format_version = excluded.format_version, armed = 1, config_id = excluded.config_id,
		   config_name = excluded.config_name, engine_kind = excluded.engine_kind, backend = excluded.backend, exe = excluded.exe,
		   argv_json = excluded.argv_json, env_json = excluded.env_json, cwd = excluded.cwd, port = excluded.port,
		   model_path = excluded.model_path, mmproj_path = excluded.mmproj_path, n_ctx = excluded.n_ctx, spec_hash = excluded.spec_hash,
		   written_at = excluded.written_at, written_by = excluded.written_by
		 RETURNING generation`,
		l.Slot, SlotLaunchFormatVersion, cfgID, l.ConfigName, l.EngineKind, l.Backend, l.Exe, string(argvJSON), string(envJSON),
		l.Cwd, l.Port, l.ModelPath, l.MmprojPath, l.NCtx, hash, unixOf(at), l.WrittenBy).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("store: slot_launches.put %q: %w", l.Slot, err)
	}
	detail := fmt.Sprintf("config=%s generation=%d spec_hash=%s", l.ConfigName, gen, hash)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (ts, actor, action, target, detail, remote_addr) VALUES (?, ?, ?, ?, ?, NULL)`,
		unixOf(time.Now()), "engine", "slot_launch.put", l.Slot, detail); err != nil {
		return 0, fmt.Errorf("store: slot_launches.put %q: audit: %w", l.Slot, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: slot_launches.put %q: commit: %w", l.Slot, err)
	}
	auditView{v.d}.mirror(AuditEntry{TS: time.Now(), Actor: "engine", Action: "slot_launch.put", Target: l.Slot, Detail: detail})
	return gen, nil
}

// Get returns the slot's row, or ErrNotFound.
func (v slotLaunchView) Get(ctx context.Context, slot string) (SlotLaunch, error) {
	return getSlotLaunch(ctx, v.d.sql, slot)
}

// List returns every row ordered by slot.
func (v slotLaunchView) List(ctx context.Context) ([]SlotLaunch, error) {
	rows, err := v.d.sql.QueryContext(ctx, `SELECT `+slotLaunchCols+` FROM slot_launches ORDER BY slot`)
	if err != nil {
		return nil, fmt.Errorf("store: slot_launches.list: %w", err)
	}
	defer rows.Close()
	var out []SlotLaunch
	for rows.Next() {
		l, err := scanSlotLaunch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: slot_launches.list: %w", err)
	}
	return out, nil
}

// Disarm is a compare-and-set: it clears armed only if the row is still at generation, so a late unload can never disarm a newer row. It reports
// whether it changed anything; (false, nil) means the row is gone, already disarmed, or at another generation, which is not an error.
func (v slotLaunchView) Disarm(ctx context.Context, slot string, generation int64) (bool, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE slot_launches SET armed = 0 WHERE slot = ? AND generation = ? AND armed = 1`, slot, generation)
	if err != nil {
		return false, fmt.Errorf("store: slot_launches.disarm %q: %w", slot, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: slot_launches.disarm %q: %w", slot, err)
	}
	return n > 0, nil
}

// Delete removes the slot's row, or returns ErrNotFound.
func (v slotLaunchView) Delete(ctx context.Context, slot string) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM slot_launches WHERE slot = ?`, slot)
	if err != nil {
		return fmt.Errorf("store: slot_launches.delete %q: %w", slot, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: slot_launches.delete %q: %w", slot, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

const slotLaunchCols = `slot, generation, format_version, armed, config_id, config_name, engine_kind, backend, exe, argv_json, env_json, cwd, port,
	model_path, mmproj_path, n_ctx, spec_hash, written_at, written_by`

type slotLaunchQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getSlotLaunch(ctx context.Context, q slotLaunchQuerier, slot string) (SlotLaunch, error) {
	row := q.QueryRowContext(ctx, `SELECT `+slotLaunchCols+` FROM slot_launches WHERE slot = ?`, slot)
	l, err := scanSlotLaunch(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SlotLaunch{}, ErrNotFound
	}
	return l, err
}

type rowScanner interface{ Scan(dest ...any) error }

func scanSlotLaunch(r rowScanner) (SlotLaunch, error) {
	var (
		l                 SlotLaunch
		armed             int
		cfgID             sql.NullInt64
		argvJSON, envJSON string
		writtenAt         int64
	)
	if err := r.Scan(&l.Slot, &l.Generation, &l.FormatVersion, &armed, &cfgID, &l.ConfigName, &l.EngineKind, &l.Backend, &l.Exe, &argvJSON,
		&envJSON, &l.Cwd, &l.Port, &l.ModelPath, &l.MmprojPath, &l.NCtx, &l.SpecHash, &writtenAt, &l.WrittenBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SlotLaunch{}, err
		}
		return SlotLaunch{}, fmt.Errorf("store: slot_launches scan: %w", err)
	}
	l.Armed = armed == 1
	if cfgID.Valid {
		id := cfgID.Int64
		l.ConfigID = &id
	}
	l.WrittenAt = time.Unix(writtenAt, 0).UTC()
	if err := json.Unmarshal([]byte(argvJSON), &l.Argv); err != nil || l.Argv == nil {
		return l, fmt.Errorf("%w: slot %q argv_json is not a JSON array of strings", ErrSlotLaunchMalformed, l.Slot)
	}
	if err := json.Unmarshal([]byte(envJSON), &l.Env); err != nil || l.Env == nil {
		return l, fmt.Errorf("%w: slot %q env_json is not a JSON object of strings", ErrSlotLaunchMalformed, l.Slot)
	}
	return l, nil
}
