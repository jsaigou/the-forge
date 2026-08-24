// SPDX-License-Identifier: Apache-2.0

// Command kbsync regenerates smith's embedded knowledge-base corpus
// (go/internal/smith/kb/corpus/*.md) from
// kb/manifest.json against the repo's real documentation files
// (docs/v5-smith.md §4.7). Invoked via `go generate ./internal/smith/...`
// (see the //go:generate directive in internal/smith/kb.go), or directly:
//
//	cd go && go run ./cmd/kbsync
//
// kb_sync_test.go re-runs the same extraction (via the kbgen package this
// tool is a thin wrapper around) and fails the build if the committed
// corpus differs — editing a source doc without re-running this tool is
// caught, not silently stale.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jsaigou/the-forge/internal/smith/kb/kbgen"
)

func main() {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "kbsync: cannot resolve own source path")
		os.Exit(1)
	}
	// go/cmd/kbsync/main.go -> go/cmd -> go -> repo root.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	n, err := kbgen.Sync(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "kbsync: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "kbsync: wrote %d files under go/internal/smith/kb/\n", n)
}
