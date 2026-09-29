// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

// captureStdout redirects os.Stdout for the duration of fn and returns what
// was written — mint-key's whole job is printing the token there.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()
	w.Close()

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

func TestMintKeyRouter(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	out := captureStdout(t, func() {
		if err := runMintKey([]string{
			"-db", dbPath, "-kind", "router", "-name", "opencode-examplehost",
		}); err != nil {
			t.Fatalf("mint-key: %v", err)
		}
	})
	token := strings.TrimSpace(out)
	if !strings.HasPrefix(token, "sk-router-") {
		t.Fatalf("token = %q, want sk-router-* prefix", token)
	}

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	auth := authz.New(db)
	if _, err := auth.VerifyBearer(token, authz.KindRouter); err != nil {
		t.Fatalf("minted token does not verify: %v", err)
	}
	if _, err := auth.VerifyBearer(token, authz.KindForge); err == nil {
		t.Fatal("a router key must not open the dashboard (KindForge)")
	}
}

func TestMintKeyForgeRequiresRole(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	err := runMintKey([]string{"-db", dbPath, "-kind", "forge", "-name", "someone"})
	if err == nil {
		t.Fatal("expected an error when -role is omitted for -kind forge")
	}
}

func TestMintKeyRotatesSameName(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")

	var first, second string
	first = strings.TrimSpace(captureStdout(t, func() {
		if err := runMintKey([]string{"-db", dbPath, "-kind", "mcp", "-name", "agent-x"}); err != nil {
			t.Fatalf("mint-key #1: %v", err)
		}
	}))
	second = strings.TrimSpace(captureStdout(t, func() {
		if err := runMintKey([]string{"-db", dbPath, "-kind", "mcp", "-name", "agent-x"}); err != nil {
			t.Fatalf("mint-key #2: %v", err)
		}
	}))

	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	auth := authz.New(db)
	if _, err := auth.VerifyBearer(first, authz.KindMCP); err == nil {
		t.Fatal("first key should have been revoked by the second mint of the same name")
	}
	if _, err := auth.VerifyBearer(second, authz.KindMCP); err != nil {
		t.Fatalf("second (current) key does not verify: %v", err)
	}
}
