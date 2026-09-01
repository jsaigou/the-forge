// SPDX-License-Identifier: Apache-2.0

// Package launchers embeds the canonical content of every static
// /usr/local/lib/forge/*.sh launcher script — the single source of truth
// scripts/migrate-foundry-to-forge.sh installs from, and now also what
// smith's restore_unit_launcher procedure (procedures/restore_unit_launcher.go)
// restores a MISSING launcher from.
//
// These files live inside the go module tree (not their historical home,
// scripts/) specifically so go:embed can reach them — go:embed patterns may
// not contain ".." and cannot cross a module boundary, and scripts/ is a
// sibling of go/, not a descendant. This is also what makes them
// self-healing on a host with no repo checkout at all: the deployed forge
// binary carries its own launcher content, so a restore needs nothing but
// the binary already running. Keep this the ONLY copy — the exact bug this
// package exists to prevent (forge-comfyui crash-looped a week undetected,
// 2026-09-01) was a launcher script with no tracked source anywhere.
package launchers

import "embed"

//go:embed start-a1.sh start-a2.sh start-a3.sh start-a4.sh start-comfyui.sh
var files embed.FS

// Content returns the embedded content for a launcher's basename (e.g.
// "start-comfyui.sh"), and whether one exists. Never errors — an unknown
// name simply has no canonical copy to restore from.
func Content(basename string) ([]byte, bool) {
	b, err := files.ReadFile(basename)
	if err != nil {
		return nil, false
	}
	return b, true
}
