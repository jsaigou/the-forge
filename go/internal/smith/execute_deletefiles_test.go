// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeleteAllowed_PathEscapeTable(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "checkpoints", "model.safetensors")
	if err := os.MkdirAll(filepath.Dir(inside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "not_a_model.safetensors")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A symlink inside root pointing OUTSIDE it — the classic escape.
	symlinkEscape := filepath.Join(root, "checkpoints", "escape.safetensors")
	if err := os.Symlink(outside, symlinkEscape); err != nil {
		t.Skipf("symlink not supported in this environment: %v", err)
	}

	dirPath := filepath.Join(root, "checkpoints")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"inside root", inside, true},
		{"absolute outside root", outside, false},
		{"relative path", "checkpoints/model.safetensors", false},
		{"directory not file", dirPath, false},
		{"empty path", "", false},
		{"symlink escaping root", symlinkEscape, false},
		{"nonexistent path", filepath.Join(root, "checkpoints", "missing.safetensors"), false},
		{"traversal-looking but resolves inside", filepath.Join(root, "checkpoints", "..", "checkpoints", "model.safetensors"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := deleteAllowed([]string{root}, c.path)
			if ok != c.want {
				t.Errorf("deleteAllowed(%q) = %v (%s), want %v", c.path, ok, reason, c.want)
			}
		})
	}
}

func TestDeleteAllowed_NoRootsConfigured(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "x.safetensors")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := deleteAllowed(nil, f); ok {
		t.Error("expected deleteAllowed to refuse when zero roots are configured")
	}
}

func TestDispatchDeleteFiles_RevalidatesAtDispatchTime(t *testing.T) {
	// A file that WAS under a configured root at propose time, but the
	// setting has since changed (root removed) — dispatch must re-check
	// against the CURRENT setting, not trust the stored detail blindly.
	db := openDB(t)
	deleted := []string{}
	s := New(Deps{
		Store: db, Logf: func(string, ...any) {},
		DeleteFile: func(_ context.Context, path string) error {
			deleted = append(deleted, path)
			return nil
		},
		Settings: db.Settings(),
	})
	oldRoot := t.TempDir()
	f := filepath.Join(oldRoot, "checkpoints", "x.safetensors")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// smith.comfyui.model_roots is left unset (empty) — simulating "the
	// root this file lived under is no longer configured".
	id := seedApproved(t, s, KindDeleteFiles, RiskHigh, mustJSON(t, deleteFilesDetail{
		Files: []deleteFileEntry{{Path: f, FolderType: "checkpoints", SizeBytes: 1}},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Errorf("status = %s, want failed (root no longer configured)", a.Status)
	}
	if len(deleted) != 0 {
		t.Error("DeleteFile must never be called when re-validation fails")
	}
}

func TestDispatchDeleteFiles_Success(t *testing.T) {
	db := openDB(t)
	root := t.TempDir()
	f1 := filepath.Join(root, "checkpoints", "a.safetensors")
	f2 := filepath.Join(root, "checkpoints", "b.safetensors")
	for _, p := range []string{f1, f2} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var deleted []string
	s := New(Deps{
		Store: db, Logf: func(string, ...any) {},
		DeleteFile: func(_ context.Context, path string) error {
			deleted = append(deleted, path)
			return os.Remove(path)
		},
		Settings: db.Settings(),
	})
	if err := db.Settings().Set(context.Background(), SettingComfyUIModelRoots, mustJSON(t, []string{root})); err != nil {
		t.Fatal(err)
	}
	id := seedApproved(t, s, KindDeleteFiles, RiskHigh, mustJSON(t, deleteFilesDetail{
		Files: []deleteFileEntry{
			{Path: f1, FolderType: "checkpoints", SizeBytes: 100},
			{Path: f2, FolderType: "checkpoints", SizeBytes: 200},
		},
		TotalBytes: 300,
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want 2 files", deleted)
	}
	if _, err := os.Stat(f1); !os.IsNotExist(err) {
		t.Error("f1 should have been removed from disk")
	}
	if a.Result == nil || a.Result.Message == "" {
		t.Fatal("expected a non-empty result message")
	}
	if !strings.Contains(a.Result.Message, "reclaimed") {
		t.Errorf("message = %q, want it to mention reclaimed bytes", a.Result.Message)
	}
}

func TestDispatchDeleteFiles_UnwiredFailsClosed(t *testing.T) {
	db := openDB(t)
	root := t.TempDir()
	f := filepath.Join(root, "checkpoints", "a.safetensors")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Deps{Store: db, Logf: func(string, ...any) {}, Settings: db.Settings()})
	if err := db.Settings().Set(context.Background(), SettingComfyUIModelRoots, mustJSON(t, []string{root})); err != nil {
		t.Fatal(err)
	}
	id := seedApproved(t, s, KindDeleteFiles, RiskHigh, mustJSON(t, deleteFilesDetail{
		Files: []deleteFileEntry{{Path: f, FolderType: "checkpoints", SizeBytes: 1}},
	}))
	s.executeAction(context.Background(), id)

	a, err := s.GetAction(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAction: %v", err)
	}
	if a.Status != StatusFailed {
		t.Errorf("status = %s, want failed (Deps.DeleteFile nil)", a.Status)
	}
	// ErrDeleteUnwired crosses a JSON round trip in storage, so this checks
	// the wrapped error's message text rather than errors.Is against the
	// stored string.
	if a.Result == nil || !strings.Contains(a.Result.Error, ErrDeleteUnwired.Error()) {
		t.Errorf("Result.Error = %+v, want it to mention %q", a.Result, ErrDeleteUnwired.Error())
	}
}
