// SPDX-License-Identifier: Apache-2.0

package smith

// NEUTRALIZED FOR PUBLIC EXPORT: the corpus-sync guard that lived here
// validates the embedded KB corpus against internal-only docs (private
// sprint/incident logs) that do not ship in the public tree.
// repoRootForTest is kept below because other tests share it.

import (
	"path/filepath"
	"runtime"
	"testing"
)

// repoRootForTest resolves the repo root from this test file's own path
// (go/internal/smith/kb_sync_test.go -> go/internal/smith -> go/internal ->
// go -> repo root), the same three-levels-up convention cmd/kbsync uses
// from go/cmd/kbsync/main.go.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve own source path")
	}
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}
