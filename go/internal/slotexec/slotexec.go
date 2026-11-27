// SPDX-License-Identifier: Apache-2.0

// Package slotexec implements `forge slot-exec <slot>` (ADR-0026 section 4): read the slot's launch row through the read-only opener, re-check the
// executable against the build roots (the second wall), build a scrubbed environment and exec() the engine so it becomes the unit's main PID.
//
// Everything with a side effect is injected through Deps (database opener, exec, chdir, environment, output), so every exit path is unit-testable
// without exec'ing. Run never returns after a successful real exec.
package slotexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/jsaigou/the-forge/internal/buildguard"
	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/i18n"
	"github.com/jsaigou/the-forge/internal/radianceyaml"
	"github.com/jsaigou/the-forge/internal/store"
)

// Exit codes (sysexits-style, stable). 66, 73, 75 and 78 are "permanently bad": the unit lists them in RestartPreventExitStatus.
const (
	ExitOK          = 0
	ExitUsage       = 64
	ExitNoRow       = 66
	ExitUnavailable = 69 // database missing, unreadable, schema too old: retried
	ExitBadExe      = 73
	ExitDisarmed    = 75
	ExitBadRow      = 78
	ExitNotExec     = 126 // exec failed: found but not runnable
	ExitNotFound    = 127 // exec failed: not found
)

// DefaultDB is the database every CLI verb defaults to.
const DefaultDB = "/var/lib/forge/forge.db"

// DefaultConfigDir is where the settings file of a Radiance 2.x slot is written (one file per slot, replaced at every launch). It sits under
// /var/lib/forge, which the slot units may write (ReadWritePaths) and the daemon already owns.
const DefaultConfigDir = "/var/lib/forge/slot-config"

// SettingBuildRoots is the setting the catalog build guard reads.
const SettingBuildRoots = "catalog.build_roots"

// Reader is the read-only surface slot-exec needs; *store.ReadOnlyDB satisfies it.
type Reader interface {
	GetSlotLaunch(ctx context.Context, slot string) (store.SlotLaunch, error)
	GetSetting(ctx context.Context, key string) ([]byte, error)
	Close() error
}

// Deps are the injectable effects.
type Deps struct {
	Open    func(path string) (Reader, error)
	Exec    func(path string, argv, env []string) error // syscall.Exec in production
	Chdir   func(dir string) error
	Environ []string // KEY=VALUE, as os.Environ()
	Stderr  io.Writer
	Stdout  io.Writer
	// ConfigDir is where a Radiance 2.x slot's settings file is written; empty means DefaultConfigDir. WriteConfig writes it (atomically,
	// owner-only) and returns the path; nil in tests that do not launch a 2.x engine.
	ConfigDir   string
	WriteConfig func(dir, name string, data []byte) (string, error)
}

// Real returns the production dependencies.
func Real() Deps {
	return Deps{
		Open: func(path string) (Reader, error) {
			r, err := store.OpenReadOnly(path)
			if err != nil {
				return nil, err
			}
			return r, nil
		},
		Exec:        syscall.Exec,
		Chdir:       os.Chdir,
		Environ:     os.Environ(),
		Stderr:      os.Stderr,
		Stdout:      os.Stdout,
		ConfigDir:   DefaultConfigDir,
		WriteConfig: writeConfigFile,
	}
}

// writeConfigFile writes data to dir/name atomically with mode 0600 (the directory is created 0700): a temporary file in the same directory, synced,
// then renamed over the old one, so an engine that starts while this runs sees either the old file or the new one, never half of one. The file holds
// no secret (Radiance runs keyless on 127.0.0.1) but is owner-only anyway.
func writeConfigFile(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	fail := func(err error) (string, error) {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	dst := filepath.Join(dir, name)
	if err := os.Rename(tmpName, dst); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return dst, nil
}

type options struct {
	slot   string
	db     string
	dryRun bool
	help   bool
}

func parseArgs(args []string, environ []string) (options, error) {
	var o options
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help" || a == "-help":
			o.help = true
		case a == "--dry-run" || a == "-dry-run":
			o.dryRun = true
		case a == "-db" || a == "--db":
			if i+1 >= len(args) {
				return o, errors.New(i18n.T("forge.cli.slot_exec.err_flag_value", a))
			}
			i++
			o.db = args[i]
		case strings.HasPrefix(a, "-db=") || strings.HasPrefix(a, "--db="):
			_, v, _ := strings.Cut(a, "=")
			o.db = v
		case strings.HasPrefix(a, "-"):
			return o, errors.New(i18n.T("forge.cli.slot_exec.err_unknown_flag", strconv.Quote(a)))
		case o.slot != "":
			return o, errors.New(i18n.T("forge.cli.slot_exec.err_extra_arg", strconv.Quote(a)))
		default:
			o.slot = a
		}
	}
	if o.db == "" {
		o.db = getenv(environ, "FORGE_DB")
	}
	if o.db == "" {
		o.db = DefaultDB
	}
	return o, nil
}

func getenv(environ []string, key string) string {
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// Run executes the verb with args (everything after "slot-exec") and returns the process exit code. Messages go to d.Stderr (the journal); --dry-run
// output goes to d.Stdout.
func Run(args []string, d Deps) int {
	logf := func(key string, a ...any) { fmt.Fprintln(d.Stderr, "forge slot-exec: "+i18n.T(key, a...)) }

	opts, err := parseArgs(args, d.Environ)
	if err != nil {
		fmt.Fprintln(d.Stderr, "forge slot-exec: "+err.Error())
		fmt.Fprintln(d.Stderr, i18n.T("forge.cli.slot_exec.usage"))
		return ExitUsage
	}
	if opts.help {
		fmt.Fprintln(d.Stdout, i18n.T("forge.cli.slot_exec.usage"))
		fmt.Fprintln(d.Stdout, i18n.T("forge.cli.slot_exec.exit_codes"))
		return ExitOK
	}
	if opts.slot == "" || !store.ValidSlotLaunchSlot(opts.slot) {
		logf("forge.cli.slot_exec.err_bad_slot", strconv.Quote(opts.slot))
		fmt.Fprintln(d.Stderr, i18n.T("forge.cli.slot_exec.usage"))
		return ExitUsage
	}

	ctx := context.Background()
	db, err := d.Open(opts.db)
	if err != nil {
		logf("forge.cli.slot_exec.err_db", opts.db, err.Error())
		return ExitUnavailable
	}
	defer db.Close()

	row, err := db.GetSlotLaunch(ctx, opts.slot)
	switch {
	case errors.Is(err, store.ErrNotFound):
		logf("forge.cli.slot_exec.err_no_row", opts.slot)
		return ExitNoRow
	case errors.Is(err, store.ErrSlotLaunchMalformed):
		logf("forge.cli.slot_exec.err_malformed", opts.slot, err.Error())
		return ExitBadRow
	case err != nil:
		logf("forge.cli.slot_exec.err_db", opts.db, err.Error())
		return ExitUnavailable
	}
	if !row.Armed {
		logf("forge.cli.slot_exec.err_disarmed", opts.slot, row.Generation)
		return ExitDisarmed
	}
	if row.FormatVersion != store.SlotLaunchFormatVersion {
		logf("forge.cli.slot_exec.err_format", opts.slot, row.FormatVersion, store.SlotLaunchFormatVersion)
		return ExitBadRow
	}
	if err := checkRowShape(row); err != nil {
		logf("forge.cli.slot_exec.err_malformed", opts.slot, err.Error())
		return ExitBadRow
	}

	roots, err := loadRoots(ctx, db)
	if err != nil {
		if errors.Is(err, errRootsMalformed) {
			logf("forge.cli.slot_exec.err_roots_malformed", err.Error())
			return ExitBadRow
		}
		logf("forge.cli.slot_exec.err_db", opts.db, err.Error())
		return ExitUnavailable
	}
	exe, reason := resolveExecutable(row.Exe, roots)
	if reason != "" {
		logf("forge.cli.slot_exec.err_exe", strconv.Quote(row.Exe), reason)
		return ExitBadExe
	}
	if row.Cwd != "" {
		if !filepath.IsAbs(row.Cwd) {
			logf("forge.cli.slot_exec.err_cwd", strconv.Quote(row.Cwd))
			return ExitBadExe
		}
		if fi, err := os.Stat(row.Cwd); err != nil || !fi.IsDir() {
			logf("forge.cli.slot_exec.err_cwd", strconv.Quote(row.Cwd))
			return ExitBadExe
		}
	}

	env, names := buildEnv(d.Environ, row.Env)
	argv := append([]string{row.Exe}, row.Argv...)

	// A Radiance 2.x engine refuses command-line flags and reads one YAML file (`radiance --config FILE`). The row keeps the flag form on purpose (the
	// spec hash, launch-check and drift detection are all defined over it); the translation happens here, at the moment of launch, and its failure
	// is a bad row, not a bad host.
	var yamlText string
	if row.EngineKind == enginekind.Radiance2 {
		yamlText, err = radianceyaml.FromArgv(row.Argv)
		if err != nil {
			logf("forge.cli.slot_exec.err_config", opts.slot, err.Error())
			return ExitBadRow
		}
	}

	logf("forge.cli.slot_exec.launching", opts.slot, row.ConfigName, row.Generation, row.SpecHash, strconv.Quote(row.Exe), len(row.Argv),
		strings.Join(RedactArgv(row.Argv), " "), strings.Join(names, ","))
	if opts.dryRun {
		describe(d.Stdout, row, names)
		if yamlText != "" {
			fmt.Fprintln(d.Stdout, i18n.T("forge.cli.slot_exec.dry_config", filepath.Join(configDir(d), opts.slot+".yaml")))
			fmt.Fprint(d.Stdout, yamlText)
		}
		return ExitOK
	}
	if row.Cwd != "" {
		if err := d.Chdir(row.Cwd); err != nil {
			logf("forge.cli.slot_exec.err_chdir", strconv.Quote(row.Cwd), err.Error())
			return ExitBadExe
		}
	}
	if yamlText != "" {
		if d.WriteConfig == nil {
			logf("forge.cli.slot_exec.err_config", opts.slot, "no way to write the settings file is configured")
			return ExitUnavailable
		}
		path, werr := d.WriteConfig(configDir(d), opts.slot+".yaml", []byte(yamlText))
		if werr != nil {
			logf("forge.cli.slot_exec.err_config", opts.slot, werr.Error())
			return ExitUnavailable // a full or unwritable disk is a host problem the unit may retry
		}
		argv = []string{row.Exe, "--config", path}
		logf("forge.cli.slot_exec.config_written", path, len(yamlText))
	}
	// Release the database before replacing the process image (the descriptor would otherwise leak into the engine).
	db.Close()
	err = d.Exec(exe, argv, env)
	// Exec only returns on failure (a fake returns nil in tests).
	if err == nil {
		return ExitOK
	}
	logf("forge.cli.slot_exec.err_exec", strconv.Quote(row.Exe), err.Error())
	if errors.Is(err, syscall.ENOENT) {
		return ExitNotFound
	}
	return ExitNotExec
}

// checkRowShape refuses values that cannot be passed to exec or break the row contract, before anything is logged or run.
func checkRowShape(r store.SlotLaunch) error {
	if strings.IndexByte(r.Exe, 0) >= 0 {
		return errors.New("exe contains a NUL byte")
	}
	for i, a := range r.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return fmt.Errorf("argv[%d] contains a NUL byte", i)
		}
	}
	for k, v := range r.Env {
		if !envNameOK(k) {
			return fmt.Errorf("environment variable name %q is not ^[A-Z][A-Z0-9_]*$", k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("environment variable %s contains a NUL byte", k)
		}
	}
	return nil
}

func envNameOK(k string) bool {
	if k == "" || k[0] < 'A' || k[0] > 'Z' {
		return false
	}
	for i := 1; i < len(k); i++ {
		c := k[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

var errRootsMalformed = errors.New("catalog.build_roots is not a JSON array of strings")

// loadRoots reads catalog.build_roots from the same database (unset means the guard's defaults). No install location is hard-coded here: an engine under another root (for example /opt/radiance) is
// allowed only because the operator listed that root in the setting, which the build guard requires anyway before such a build can be registered. A setting that exists but
// cannot be parsed is an error, never a fall back to the defaults (they can be wider than what the operator configured).
func loadRoots(ctx context.Context, db Reader) ([]string, error) {
	var configured []string
	raw, err := db.GetSetting(ctx, SettingBuildRoots)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &configured); err != nil {
			return nil, fmt.Errorf("%w: %v", errRootsMalformed, err)
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		return nil, err
	}
	return buildguard.Roots(configured), nil
}

// resolveExecutable is the second wall, in one place: exe must be an absolute path to a regular executable file under one of the roots both as
// written and after resolving symlinks. It returns the symlink-resolved path (what is exec'd, so a link swapped after the check cannot redirect the
// exec) or a human reason. Its result is passed by value to the exec function; nothing else supplies an executable path.
func resolveExecutable(exe string, roots []string) (string, string) {
	if reason := buildguard.CheckBinary(exe, roots); reason != "" {
		return "", reason
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(exe))
	if err != nil {
		return "", err.Error()
	}
	return real, ""
}

// buildEnv returns the child environment (KEY=VALUE, sorted by name) and the sorted names of the variables the row adds: the inherited
// environment minus FORGE_* and FOUNDRY_*, with the row's variables applied over it.
func buildEnv(inherited []string, add map[string]string) ([]string, []string) {
	m := map[string]string{}
	for _, kv := range inherited {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.HasPrefix(k, "FORGE_") || strings.HasPrefix(k, "FOUNDRY_") {
			continue
		}
		m[k] = v
	}
	names := make([]string, 0, len(add))
	for k, v := range add {
		m[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out, names
}

// secretFlagName follows the ADR's logging rule: a flag whose name contains key, token, secret or password has its value masked in output. Broader
// than the builders' refusal rule on purpose; over-masking a log line is harmless.
func secretFlagName(a string) bool {
	if !strings.HasPrefix(a, "-") {
		return false
	}
	name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
	name = strings.ToLower(name)
	for _, w := range []string{"key", "token", "secret", "password"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// RedactArgv returns argv quoted for output with secret values replaced by ***: the value after a secret-named flag, and the value of --flag=value.
func RedactArgv(argv []string) []string {
	out := make([]string, len(argv))
	maskNext := false
	for i, a := range argv {
		switch {
		case maskNext:
			out[i] = `"***"`
			maskNext = false
		case secretFlagName(a):
			if name, _, hasVal := strings.Cut(a, "="); hasVal {
				out[i] = strconv.Quote(name + "=***")
			} else {
				out[i] = strconv.Quote(a)
				maskNext = true
			}
		default:
			out[i] = strconv.Quote(a)
		}
	}
	return out
}

// configDir is the directory of Radiance 2.x settings files for these Deps.
func configDir(d Deps) string {
	if d.ConfigDir != "" {
		return d.ConfigDir
	}
	return DefaultConfigDir
}

func describe(w io.Writer, row store.SlotLaunch, names []string) {
	fmt.Fprintln(w, i18n.T("forge.cli.slot_exec.dry_exe", strconv.Quote(row.Exe)))
	fmt.Fprintln(w, i18n.T("forge.cli.slot_exec.dry_argv", strings.Join(RedactArgv(row.Argv), " ")))
	fmt.Fprintln(w, i18n.T("forge.cli.slot_exec.dry_env", strings.Join(names, ",")))
	if row.Cwd != "" {
		fmt.Fprintln(w, i18n.T("forge.cli.slot_exec.dry_cwd", strconv.Quote(row.Cwd)))
	}
}
