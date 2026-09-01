// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
)

// withForgeLibDir points forgeLibDir at a fresh t.TempDir() for the
// duration of the test, restoring the real value after — the real
// /usr/local/lib/forge doesn't exist on a dev machine, so
// launcherInstallAllowed would fail closed (EvalSymlinks errors) on every
// case without this.
func withForgeLibDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	orig := forgeLibDir
	forgeLibDir = dir
	t.Cleanup(func() { forgeLibDir = orig })
	return dir
}

func TestLauncherInstallAllowed_Table(t *testing.T) {
	dir := withForgeLibDir(t)
	existing := filepath.Join(dir, "start-comfyui.sh")
	if err := os.WriteFile(existing, []byte("already here"), 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"missing launcher with a real embedded copy", filepath.Join(dir, "start-a1.sh"), true},
		{"already exists — never overwrite", existing, false},
		{"no canonical embedded copy for this basename", filepath.Join(dir, "start-nonexistent.sh"), false},
		{"empty path", "", false},
		{"relative path", "start-a1.sh", false},
		{"not directly under forgeLibDir (subdirectory)", filepath.Join(dir, "sub", "start-a1.sh"), false},
		{"parent traversal", filepath.Join(dir, "..", filepath.Base(dir), "start-a1.sh"), true}, // resolves back inside — same as deleteAllowed's own precedent
		{"escapes forgeLibDir entirely", filepath.Join(filepath.Dir(dir), "start-a1.sh"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content, ok, reason := launcherInstallAllowed(c.path)
			if ok != c.want {
				t.Errorf("launcherInstallAllowed(%q) = %v (%s), want %v", c.path, ok, reason, c.want)
			}
			if ok && len(content) == 0 {
				t.Errorf("launcherInstallAllowed(%q) allowed but returned empty content", c.path)
			}
		})
	}
}

// TestLauncherInstallAllowed_ParentSymlinkEscape proves a unit's ExecStart
// path can't smuggle installation somewhere outside forgeLibDir via a
// symlinked parent directory — the classic deleteAllowed-precedent escape,
// this time on the write side.
func TestLauncherInstallAllowed_ParentSymlinkEscape(t *testing.T) {
	withForgeLibDir(t)
	outsideDir := t.TempDir()
	fakeLib := filepath.Join(t.TempDir(), "forge-lib-symlink")
	if err := os.Symlink(outsideDir, fakeLib); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}
	// A unit whose reported ExecStartPath sits under a directory that LOOKS
	// like forgeLibDir by name but is actually a symlink elsewhere must
	// still be refused — only the real, configured forgeLibDir counts.
	if _, ok, _ := launcherInstallAllowed(filepath.Join(fakeLib, "start-a1.sh")); ok {
		t.Error("launcherInstallAllowed allowed a path under an unrelated directory")
	}
}

func TestDispatchInstallLauncher_HappyPath(t *testing.T) {
	dir := withForgeLibDir(t)
	target := filepath.Join(dir, "start-a3.sh")

	var wrote []byte
	var wrotePath string
	s := New(Deps{
		Logf: func(string, ...any) {},
		Source: collector.NewStatic(&collector.Snapshot{
			Units: map[string]collector.UnitState{"forge-a3": {ExecStartPath: target}},
		}),
		InstallLauncherFile: func(_ context.Context, path string, content []byte) error {
			wrotePath, wrote = path, content
			return os.WriteFile(path, content, 0o755)
		},
	})

	detail, _ := json.Marshal(installLauncherDetail{Unit: "forge-a3"})
	a := &Action{Kind: KindInstallLauncher, Detail: detail}
	unit, err := s.dispatchInstallLauncher(context.Background(), a)
	if err != nil {
		t.Fatalf("dispatchInstallLauncher: %v", err)
	}
	if unit != "forge-a3" {
		t.Errorf("returned unit = %q, want forge-a3", unit)
	}
	if wrotePath != target {
		t.Errorf("wrote to %q, want %q", wrotePath, target)
	}
	if len(wrote) == 0 {
		t.Error("wrote empty content")
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("file not actually created: %v", err)
	}
}

func TestDispatchInstallLauncher_RefusesWhenFileAlreadyExists(t *testing.T) {
	dir := withForgeLibDir(t)
	target := filepath.Join(dir, "start-a4.sh")
	if err := os.WriteFile(target, []byte("operator's own file"), 0o755); err != nil {
		t.Fatal(err)
	}

	called := false
	s := New(Deps{
		Logf: func(string, ...any) {},
		Source: collector.NewStatic(&collector.Snapshot{
			Units: map[string]collector.UnitState{"forge-a4": {ExecStartPath: target}},
		}),
		InstallLauncherFile: func(context.Context, string, []byte) error {
			called = true
			return nil
		},
	})

	detail, _ := json.Marshal(installLauncherDetail{Unit: "forge-a4"})
	a := &Action{Kind: KindInstallLauncher, Detail: detail}
	_, err := s.dispatchInstallLauncher(context.Background(), a)
	if !errors.Is(err, ErrLauncherNotAllowed) {
		t.Fatalf("err = %v, want ErrLauncherNotAllowed (never overwrite an existing file)", err)
	}
	if called {
		t.Error("InstallLauncherFile was called despite the file already existing")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "operator's own file" {
		t.Error("the existing file's content was touched")
	}
}
