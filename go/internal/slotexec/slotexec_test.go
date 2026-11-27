// SPDX-License-Identifier: Apache-2.0

package slotexec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

const canary = "CANARY-VALUE-7731"

type fakeDB struct {
	row      store.SlotLaunch
	rowErr   error
	settings map[string][]byte
	setErr   error
	closed   int
}

func (f *fakeDB) GetSlotLaunch(_ context.Context, _ string) (store.SlotLaunch, error) {
	return f.row, f.rowErr
}
func (f *fakeDB) GetSetting(_ context.Context, key string) ([]byte, error) {
	if f.setErr != nil {
		return nil, f.setErr
	}
	v, ok := f.settings[key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}
func (f *fakeDB) Close() error { f.closed++; return nil }

type execCall struct {
	path      string
	argv, env []string
}

type harness struct {
	db       *fakeDB
	openErr  error
	openPath string
	calls    []execCall
	execErr  error
	chdirs   []string
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	environ  []string
}

func (h *harness) deps() Deps {
	return Deps{
		Open: func(p string) (Reader, error) {
			h.openPath = p
			if h.openErr != nil {
				return nil, h.openErr
			}
			return h.db, nil
		},
		Exec: func(p string, a, e []string) error {
			h.calls = append(h.calls, execCall{p, a, e})
			return h.execErr
		},
		Chdir:   func(d string) error { h.chdirs = append(h.chdirs, d); return nil },
		Environ: h.environ,
		Stderr:  &h.stderr,
		Stdout:  &h.stdout,
	}
}

// root makes a build root with one executable and returns (root, exe).
func root(t *testing.T) (string, string) {
	t.Helper()
	r, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(r, "engine")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r, exe
}

func newHarness(t *testing.T) (*harness, string) {
	r, exe := root(t)
	h := &harness{
		db: &fakeDB{
			row: store.SlotLaunch{
				Slot: "a1", Generation: 3, FormatVersion: store.SlotLaunchFormatVersion, Armed: true,
				ConfigName: "cfg", Exe: exe, Argv: []string{"--port", "8081", "--model", "/m"},
				Env: map[string]string{"HSA_X": canary}, SpecHash: "hash",
			},
			settings: map[string][]byte{SettingBuildRoots: []byte(`["` + r + `"]`)},
		},
		environ: []string{"PATH=/usr/bin", "HOME=/home/testuser", "FORGE_API_KEY=" + canary + "-inherited", "FOUNDRY_TOKEN=x", "FORGE_DB=/x.db", "FORGEX=keep"},
	}
	return h, r
}

func TestExitPaths(t *testing.T) {
	outside, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outsideExe := filepath.Join(outside, "evil")
	if err := os.WriteFile(outsideExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		args  []string
		setup func(h *harness, root string)
		want  int
		msg   string // substring expected on stderr
	}{
		{"ok dry run", []string{"a1", "--dry-run"}, nil, ExitOK, "generation 3"},
		{"help", []string{"--help"}, nil, ExitOK, ""},
		{"no slot", nil, nil, ExitUsage, "not valid"},
		{"bad slot name", []string{"A1/../x"}, nil, ExitUsage, "not valid"},
		{"unknown flag", []string{"a1", "--bogus"}, nil, ExitUsage, "unknown flag"},
		{"two slots", []string{"a1", "a2"}, nil, ExitUsage, "unexpected argument"},
		{"db flag without value", []string{"a1", "-db"}, nil, ExitUsage, "needs a value"},
		{"missing database", []string{"a1"}, func(h *harness, _ string) { h.openErr = store.ErrDBMissing }, ExitUnavailable, "cannot read the state database"},
		{"schema too old", []string{"a1"}, func(h *harness, _ string) { h.openErr = store.ErrSchemaTooOld }, ExitUnavailable, "cannot read the state database"},
		{"no row", []string{"a1"}, func(h *harness, _ string) { h.db.rowErr = store.ErrNotFound }, ExitNoRow, "no launch row"},
		{"row unreadable", []string{"a1"}, func(h *harness, _ string) { h.db.rowErr = errors.New("disk I/O error") }, ExitUnavailable, "disk I/O"},
		{"row malformed", []string{"a1"}, func(h *harness, _ string) { h.db.rowErr = store.ErrSlotLaunchMalformed }, ExitBadRow, "malformed"},
		{"disarmed", []string{"a1"}, func(h *harness, _ string) { h.db.row.Armed = false }, ExitDisarmed, "disarmed"},
		{"unknown format_version", []string{"a1"}, func(h *harness, _ string) { h.db.row.FormatVersion = 99 }, ExitBadRow, "format_version 99"},
		{"format_version zero", []string{"a1"}, func(h *harness, _ string) { h.db.row.FormatVersion = 0 }, ExitBadRow, "format_version 0"},
		{"exe outside roots", []string{"a1"}, func(h *harness, _ string) { h.db.row.Exe = outsideExe }, ExitBadExe, "allowed root"},
		{"exe relative", []string{"a1"}, func(h *harness, _ string) { h.db.row.Exe = "engine" }, ExitBadExe, "absolute"},
		{"exe is directory", []string{"a1"}, func(h *harness, r string) { h.db.row.Exe = r }, ExitBadExe, "directory"},
		{"exe missing", []string{"a1"}, func(h *harness, r string) { h.db.row.Exe = filepath.Join(r, "nope") }, ExitBadExe, "does not exist"},
		{"exe not executable", []string{"a1"}, func(h *harness, r string) {
			p := filepath.Join(r, "plain")
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			h.db.row.Exe = p
		}, ExitBadExe, "not executable"},
		{"exe symlink escapes roots", []string{"a1"}, func(h *harness, r string) {
			p := filepath.Join(r, "link")
			if err := os.Symlink(outsideExe, p); err != nil {
				t.Fatal(err)
			}
			h.db.row.Exe = p
		}, ExitBadExe, "outside the allowed roots"},
		{"exe path traversal", []string{"a1"}, func(h *harness, r string) { h.db.row.Exe = r + "/../" + filepath.Base(outside) + "/evil" }, ExitBadExe, ""},
		{"exe with shell metacharacter", []string{"a1"}, func(h *harness, r string) { h.db.row.Exe = r + "/en;gine" }, ExitBadExe, "disallowed"},
		{"exe with NUL", []string{"a1"}, func(h *harness, r string) { h.db.row.Exe = r + "/engine\x00x" }, ExitBadRow, "NUL"},
		{"cwd not a directory", []string{"a1"}, func(h *harness, r string) { h.db.row.Cwd = filepath.Join(r, "engine") }, ExitBadExe, "working directory"},
		{"cwd relative", []string{"a1"}, func(h *harness, _ string) { h.db.row.Cwd = "rel" }, ExitBadExe, "working directory"},
		{"roots setting malformed", []string{"a1"}, func(h *harness, _ string) { h.db.settings[SettingBuildRoots] = []byte(`{"x":1}`) }, ExitBadRow, "catalog.build_roots"},
		{"roots setting unreadable", []string{"a1"}, func(h *harness, _ string) { h.db.setErr = errors.New("boom") }, ExitUnavailable, "boom"},
		{"env name lowercase", []string{"a1"}, func(h *harness, _ string) { h.db.row.Env = map[string]string{"hsa": "v"} }, ExitBadRow, "environment variable name"},
		{"env name with dash", []string{"a1"}, func(h *harness, _ string) { h.db.row.Env = map[string]string{"A-B": "v"} }, ExitBadRow, "environment variable name"},
		{"env name with equals", []string{"a1"}, func(h *harness, _ string) { h.db.row.Env = map[string]string{"A=B": "v"} }, ExitBadRow, "environment variable name"},
		{"env name empty", []string{"a1"}, func(h *harness, _ string) { h.db.row.Env = map[string]string{"": "v"} }, ExitBadRow, "environment variable name"},
		{"env name leading digit", []string{"a1"}, func(h *harness, _ string) { h.db.row.Env = map[string]string{"1A": "v"} }, ExitBadRow, "environment variable name"},
		{"env value NUL", []string{"a1"}, func(h *harness, _ string) { h.db.row.Env = map[string]string{"A": "v\x00"} }, ExitBadRow, "NUL"},
		{"argv NUL", []string{"a1"}, func(h *harness, _ string) { h.db.row.Argv = []string{"ok", "bad\x00"} }, ExitBadRow, "NUL"},
		{"exec not found", []string{"a1"}, func(h *harness, _ string) { h.execErr = syscall.ENOENT }, ExitNotFound, "exec of"},
		{"exec denied", []string{"a1"}, func(h *harness, _ string) { h.execErr = syscall.EACCES }, ExitNotExec, "exec of"},
		{"exec succeeds (fake returns)", []string{"a1"}, nil, ExitOK, "generation 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, r := newHarness(t)
			if tc.setup != nil {
				tc.setup(h, r)
			}
			got := Run(tc.args, h.deps())
			if got != tc.want {
				t.Fatalf("exit = %d, want %d\nstderr: %s", got, tc.want, h.stderr.String())
			}
			if tc.msg != "" && !strings.Contains(h.stderr.String(), tc.msg) {
				t.Errorf("stderr missing %q:\n%s", tc.msg, h.stderr.String())
			}
			if got != ExitOK && got < ExitNotExec && len(h.calls) != 0 {
				t.Errorf("exec was called on a refused launch: %+v", h.calls)
			}
			if strings.Contains(h.stderr.String()+h.stdout.String(), canary) {
				t.Errorf("a variable value leaked into output:\n%s%s", h.stderr.String(), h.stdout.String())
			}
		})
	}
}

func TestDBPathSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		environ []string
		want    string
	}{
		{"default", []string{"a1", "--dry-run"}, nil, DefaultDB},
		{"env", []string{"a1", "--dry-run"}, []string{"FORGE_DB=/e.db"}, "/e.db"},
		{"flag beats env", []string{"a1", "--dry-run", "-db", "/f.db"}, []string{"FORGE_DB=/e.db"}, "/f.db"},
		{"flag equals form", []string{"-db=/g.db", "a1", "--dry-run"}, nil, "/g.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newHarness(t)
			h.environ = tc.environ
			if got := Run(tc.args, h.deps()); got != ExitOK {
				t.Fatalf("exit %d: %s", got, h.stderr.String())
			}
			if h.openPath != tc.want {
				t.Errorf("opened %q, want %q", h.openPath, tc.want)
			}
		})
	}
}

func TestRootsAreOnlyWhatTheSettingListsPlusDefaults(t *testing.T) {
	// No install location is hard-coded: /opt/radiance is refused unless catalog.build_roots lists it, and accepted (it reaches the existence check, since the
	// file does not exist in the test) once it does.
	h, _ := newHarness(t)
	h.db.row.Exe = "/opt/radiance/bin/engine"
	if got := Run([]string{"a1"}, h.deps()); got != ExitBadExe {
		t.Fatalf("exit %d", got)
	}
	if !strings.Contains(h.stderr.String(), "allowed root") && !strings.Contains(h.stderr.String(), "outside") {
		t.Errorf("an unlisted root should be refused for being outside the roots: %s", h.stderr.String())
	}

	h2, _ := newHarness(t)
	h2.db.settings[SettingBuildRoots] = []byte(`["/opt/radiance"]`)
	h2.db.row.Exe = "/opt/radiance/bin/engine"
	if got := Run([]string{"a1"}, h2.deps()); got != ExitBadExe {
		t.Fatalf("exit %d", got)
	}
	if strings.Contains(h2.stderr.String(), "allowed root") || strings.Contains(h2.stderr.String(), "outside") {
		t.Errorf("a listed root was refused as outside the roots: %s", h2.stderr.String())
	}
}

func TestUnsetRootsUseDefaults(t *testing.T) {
	h, _ := newHarness(t)
	delete(h.db.settings, SettingBuildRoots)
	// the temp exe is not under the default roots
	if got := Run([]string{"a1"}, h.deps()); got != ExitBadExe {
		t.Fatalf("exit %d, want %d", got, ExitBadExe)
	}
}

func TestExecReceivesExactlyTheRow(t *testing.T) {
	h, r := newHarness(t)
	link := filepath.Join(r, "link")
	if err := os.Symlink(h.db.row.Exe, link); err != nil {
		t.Fatal(err)
	}
	real := h.db.row.Exe
	h.db.row.Exe = link // symlink inside the roots is fine; exec gets the resolved target
	h.db.row.Argv = []string{"--port", "8081", "a b", "", "--x=y z", "\xff\xfe", "naïve"}
	h.db.row.Env = map[string]string{"HSA_X": canary, "PATH": "/row/bin"}
	h.db.row.Cwd = r

	if got := Run([]string{"a1"}, h.deps()); got != ExitOK {
		t.Fatalf("exit %d: %s", got, h.stderr.String())
	}
	if len(h.calls) != 1 {
		t.Fatalf("exec calls = %d", len(h.calls))
	}
	c := h.calls[0]
	if c.path != real {
		t.Errorf("exec path = %q, want resolved %q", c.path, real)
	}
	wantArgv := append([]string{link}, h.db.row.Argv...)
	if !reflect.DeepEqual(c.argv, wantArgv) {
		t.Errorf("argv = %q, want %q", c.argv, wantArgv)
	}
	wantEnv := []string{"HOME=/home/testuser", "FORGEX=keep", "HSA_X=" + canary, "PATH=/row/bin"}
	got := append([]string{}, c.env...)
	if len(got) != 4 {
		t.Fatalf("env = %q", got)
	}
	for _, w := range wantEnv {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("env missing %q: %q", w, got)
		}
	}
	if !reflect.DeepEqual(h.chdirs, []string{r}) {
		t.Errorf("chdir = %q", h.chdirs)
	}
	if h.db.closed < 1 {
		t.Error("database not closed before exec")
	}
}

func TestEnvScrubbing(t *testing.T) {
	inherited := []string{
		"PATH=/usr/bin", "LANG=C", "FORGE_X=1", "FORGE_=2", "FOUNDRY_Y=3", "FOUNDRY_API_KEY=" + canary, "FORGE=keep", "MYFORGE_Z=keep", "malformed", "=novalue",
	}
	env, names := buildEnv(inherited, map[string]string{"B": "2", "A": "1", "LANG": "row"})
	want := []string{"A=1", "B=2", "FORGE=keep", "LANG=row", "MYFORGE_Z=keep", "PATH=/usr/bin"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
	if !reflect.DeepEqual(names, []string{"A", "B", "LANG"}) {
		t.Errorf("names = %q", names)
	}
}

func TestRedaction(t *testing.T) {
	t.Run("dry run and log never carry a row env value", func(t *testing.T) {
		h, _ := newHarness(t)
		h.db.row.Env = map[string]string{"HSA_X": canary, "OTHER": canary + "2"}
		if got := Run([]string{"a1", "--dry-run"}, h.deps()); got != ExitOK {
			t.Fatalf("exit %d", got)
		}
		out := h.stdout.String() + h.stderr.String()
		if strings.Contains(out, canary) {
			t.Errorf("value leaked:\n%s", out)
		}
		for _, name := range []string{"HSA_X", "OTHER"} {
			if !strings.Contains(h.stdout.String(), name) {
				t.Errorf("dry-run does not name %s:\n%s", name, h.stdout.String())
			}
		}
		if len(h.calls) != 0 || len(h.chdirs) != 0 {
			t.Error("dry run executed or changed directory")
		}
		for _, want := range []string{"/m", "--port"} {
			if !strings.Contains(h.stdout.String(), want) {
				t.Errorf("dry-run output lacks %q:\n%s", want, h.stdout.String())
			}
		}
	})
	t.Run("secret-named flags are masked", func(t *testing.T) {
		got := RedactArgv([]string{"--port", "1", "--api-key", canary, "--hf-token=" + canary, "--Password", canary, "-k", "keep", "--model", "m"})
		joined := strings.Join(got, " ")
		if strings.Contains(joined, canary) {
			t.Errorf("secret leaked: %s", joined)
		}
		for _, want := range []string{`"--port" "1"`, `"***"`, `"--hf-token=***"`, `"keep"`, `"m"`} {
			if !strings.Contains(joined, want) {
				t.Errorf("missing %s in %s", want, joined)
			}
		}
	})
	t.Run("odd bytes are escaped in output", func(t *testing.T) {
		h, _ := newHarness(t)
		h.db.row.Argv = []string{"a\nb", "\x1b[31m", "\xff"}
		if got := Run([]string{"a1", "--dry-run"}, h.deps()); got != ExitOK {
			t.Fatalf("exit %d", got)
		}
		out := h.stdout.String() + h.stderr.String()
		if strings.ContainsAny(out, "\x1b\xff") || strings.Contains(h.stdout.String(), "a\nb") {
			t.Errorf("raw control bytes in output: %q", out)
		}
	})
}

func TestEnvNameOK(t *testing.T) {
	for k, want := range map[string]bool{"A": true, "HSA_OVERRIDE_GFX_VERSION": true, "A1_B2": true, "a": false, "_A": false, "1A": false, "A-B": false, "": false, "Ä": false} {
		if envNameOK(k) != want {
			t.Errorf("envNameOK(%q) = %v", k, !want)
		}
	}
}
