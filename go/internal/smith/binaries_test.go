// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractCommitHash(t *testing.T) {
	cases := map[string]string{
		"version: 10122 (8bf3c1130)\nbuilt with GNU 16.1.1": "8bf3c1130",
		"no hash here":               "",
		"(short)":                    "",          // too short to be a hash
		"(8bf3c1130) and (another7)": "8bf3c1130", // first match wins
	}
	for in, want := range cases {
		if got := extractCommitHash(in); got != want {
			t.Errorf("extractCommitHash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGitTreeVersion_DetachedHead(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	sha, err := gitTreeVersion(root)
	if err != nil {
		t.Fatalf("gitTreeVersion: %v", err)
	}
	if sha != "04b2b72cbabcdef0123456789abcdef01234567" {
		t.Errorf("sha = %q", sha)
	}
}

func TestGitTreeVersion_LooseRef(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/master\n")
	mustWriteFile(t, filepath.Join(root, ".git", "refs", "heads", "master"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	sha, err := gitTreeVersion(root)
	if err != nil {
		t.Fatalf("gitTreeVersion: %v", err)
	}
	if sha != "04b2b72cbabcdef0123456789abcdef01234567" {
		t.Errorf("sha = %q", sha)
	}
}

func TestGitTreeVersion_PackedRefsFallback(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/master\n")
	// No refs/heads/master loose file — only packed-refs, the gc'd-tree case.
	mustWriteFile(t, filepath.Join(root, ".git", "packed-refs"),
		"# pack-refs with: peeled fully-peeled sorted\n"+
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/heads/other\n"+
			"04b2b72cbabcdef0123456789abcdef01234567 refs/heads/master\n")
	sha, err := gitTreeVersion(root)
	if err != nil {
		t.Fatalf("gitTreeVersion: %v", err)
	}
	if sha != "04b2b72cbabcdef0123456789abcdef01234567" {
		t.Errorf("sha = %q", sha)
	}
}

func TestGitTreeVersion_NotAGitTree(t *testing.T) {
	if _, err := gitTreeVersion(t.TempDir()); err == nil {
		t.Error("expected an error for a non-git directory")
	}
}

func TestBinaryPathAllowed(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "llama-server")
	mustWriteFile(t, realFile, "#!/bin/sh\necho fake\n")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"real file", realFile, true},
		{"relative path", "llama-server", false},
		{"shell metachar", realFile + "; rm -rf /", false},
		{"directory not file", dir, false},
		{"nonexistent", filepath.Join(dir, "missing"), false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := BinaryPathAllowed(c.path)
			if ok != c.want {
				t.Errorf("BinaryPathAllowed(%q) = %v (%s), want %v", c.path, ok, reason, c.want)
			}
		})
	}
}

func TestInstalledProbe_NilFunc(t *testing.T) {
	if _, ok := installedProbe(context.Background(), nil, "/some/path"); ok {
		t.Error("expected ok=false with a nil BinaryVersion func")
	}
}

func TestInstalledProbe_Error(t *testing.T) {
	fn := func(context.Context, string) (string, error) { return "", errors.New("exec failed") }
	if _, ok := installedProbe(context.Background(), fn, "/some/path"); ok {
		t.Error("expected ok=false on exec error")
	}
}

func TestInstalledProbe_TrimsOutput(t *testing.T) {
	fn := func(context.Context, string) (string, error) { return "  version: 1.0  \n", nil }
	v, ok := installedProbe(context.Background(), fn, "/some/path")
	if !ok || v != "version: 1.0" {
		t.Errorf("v=%q ok=%v", v, ok)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
