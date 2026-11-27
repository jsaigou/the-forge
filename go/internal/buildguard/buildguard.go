// SPDX-License-Identifier: Apache-2.0

// Package buildguard holds the rules for registering and retiring a catalog build (ADR-0020 F8), so the builds API
// and smith's build_refresh apply the same guards instead of two copies: a build's binary is an executable file under
// an allowed root, and a build is not retired while a config runs on it.
package buildguard

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// DefaultRoots are the directories a build binary may live under when the catalog.build_roots setting is unset:
// the llama.cpp trees, forge's own files and the shared library directory.
var DefaultRoots = []string{"/opt/forge", "/opt/forge", "/usr/local/lib/forge"}

// Roots returns the allowed roots from the setting's value (nil or empty means DefaultRoots), cleaned, dropping any
// entry that is not an absolute path.
func Roots(configured []string) []string {
	roots := configured
	if len(roots) == 0 {
		roots = DefaultRoots
	}
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		if filepath.IsAbs(r) {
			out = append(out, filepath.Clean(r))
		}
	}
	return out
}

func underRoot(path string, roots []string) bool {
	for _, r := range roots {
		if path == r || strings.HasPrefix(path, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// CheckBinary applies the binary guards and returns a reason, or "" when the path is acceptable: absolute, no shell
// metacharacters, under a root (also after resolving symlinks, so a link cannot smuggle in a binary from outside),
// and a regular executable file.
func CheckBinary(path string, roots []string) string {
	switch {
	case !filepath.IsAbs(path):
		return "must be an absolute path"
	case strings.ContainsAny(path, "\t\n;|&$`"):
		return "contains disallowed characters"
	}
	clean := filepath.Clean(path)
	if !underRoot(clean, roots) {
		return "must be under an allowed root (" + strings.Join(roots, ", ") + "; setting catalog.build_roots)"
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "does not exist or cannot be read: " + err.Error()
	}
	// A root may itself be reached through a symlink (/var -> /private/var), so compare against both spellings.
	resolved := append([]string{}, roots...)
	for _, r := range roots {
		if rr, err := filepath.EvalSymlinks(r); err == nil {
			resolved = append(resolved, rr)
		}
	}
	if !underRoot(real, resolved) {
		return "resolves (through a symlink) outside the allowed roots"
	}
	info, err := os.Stat(real)
	switch {
	case err != nil:
		return "cannot be read: " + err.Error()
	case info.IsDir():
		return "is a directory, not a binary"
	case !info.Mode().IsRegular():
		return "is not a regular file"
	case info.Mode()&0o111 == 0:
		return "is not executable"
	}
	return ""
}

// ConfigsUsing names the configs that run on build id.
func ConfigsUsing(cfgs []store.Config, id int64) []string {
	var names []string
	for _, c := range cfgs {
		if c.BuildID == id {
			names = append(names, c.Name)
		}
	}
	return names
}
