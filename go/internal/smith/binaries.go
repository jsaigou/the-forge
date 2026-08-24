// SPDX-License-Identifier: Apache-2.0

package smith

// binaries.go implements the read side of smith P6's binary/dependency
// tracker (docs/v5-smith.md §4.9 FR6): resolving a tracked binary's
// installed version (Deps.BinaryVersion) and its source tree's checked-out
// commit (gitTreeVersion, below). checks_binaries.go turns the two into a
// finding; smith.go's TrackedBinaries reads the operator-curated list
// (smith.binaries.tracked) the check iterates.
//
// Deliberately descoped from the original plan's third probe (upstream
// release, via GitHub) — the same kind of deliberate deferral P5 made for
// camofox (docs/v5-smith.md §4.8 amendment #2): the installed-vs-source-tree
// divergence is FR6's real, measured, high-value signal (the "built binary
// lags its own tree's HEAD" finding this module exists to catch), and it
// needs neither a network call nor a per-fork guess at where each tracked
// binary's upstream release feed even lives. Upstream tracking can be added
// later as a third, additive probe without touching this shape.

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// binaryVersionTimeout bounds Deps.BinaryVersion's exec, independent of
// whatever timeout the caller's context carries.
const binaryVersionTimeout = 5 * time.Second

// BinaryPathAllowed guards Deps.BinaryVersion's production implementation
// (main.go) before it execs anything: path must be absolute, contain no
// shell metacharacters (same injection guard shape as execute.go's
// restartAllowed), and exist as a regular file. Exported so main.go's
// production exec wiring can re-validate (not just trust) a path pulled
// from settings JSON — smith.binaries.tracked is operator-editable, not a
// compile-time constant — defense in depth even though it isn't reachable
// through any smith action's settings_change path (SettingBinariesTracked
// is not on execute.go's allowedSmithSettingsKeys).
func BinaryPathAllowed(path string) (bool, string) {
	if path == "" {
		return false, "empty path"
	}
	if !filepath.IsAbs(path) {
		return false, "path must be absolute"
	}
	if strings.ContainsAny(path, "\t\n;|&$`") {
		return false, "path contains disallowed characters"
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, "stat: " + err.Error()
	}
	if info.IsDir() {
		return false, "path is a directory"
	}
	return true, ""
}

// gitVersionShaRe pulls a parenthesized 7-40 hex char commit hash out of a
// version string — the shape both llama.cpp ("version: 10122 (8bf3c1130)")
// and most other C/C++ tool --version output uses.
var gitVersionShaRe = regexp.MustCompile(`\(([0-9a-f]{7,40})\)`)

// extractCommitHash returns the parenthesized short hash embedded in an
// --version string, or "" when none is found — installed versions that
// don't embed a hash (a plain semver, a Python package version) simply
// never compare against a source tree.
func extractCommitHash(versionOutput string) string {
	m := gitVersionShaRe.FindStringSubmatch(versionOutput)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// gitTreeVersion resolves root/.git's checked-out commit without shelling
// out to git — plain file reads only (HEAD, then either the loose ref file
// or a packed-refs fallback for a gc'd tree). Returns ("", err) when root
// isn't a git worktree or the ref can't be resolved.
func gitTreeVersion(root string) (string, error) {
	headRaw, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err != nil {
		return "", err
	}
	head := strings.TrimSpace(string(headRaw))
	ref, isSymbolic := strings.CutPrefix(head, "ref: ")
	if !isSymbolic {
		return head, nil // detached HEAD — the SHA is right there
	}

	if sha, err := os.ReadFile(filepath.Join(root, ".git", filepath.FromSlash(ref))); err == nil {
		return strings.TrimSpace(string(sha)), nil
	}
	// Loose ref file absent — the tree has been gc'd and the ref lives in
	// packed-refs instead (real git behavior, not a fallback for a bug).
	packed, err := os.ReadFile(filepath.Join(root, ".git", "packed-refs"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if strings.HasSuffix(line, " "+ref) {
			sha, _, ok := strings.Cut(line, " ")
			if ok {
				return sha, nil
			}
		}
	}
	return "", os.ErrNotExist
}

// installedProbe wraps Deps.BinaryVersion with its own bounded timeout,
// nil-tolerant.
func installedProbe(ctx context.Context, fn func(context.Context, string) (string, error), path string) (string, bool) {
	if fn == nil || path == "" {
		return "", false
	}
	pctx, cancel := context.WithTimeout(ctx, binaryVersionTimeout)
	defer cancel()
	v, err := fn(pctx, path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(v), true
}
