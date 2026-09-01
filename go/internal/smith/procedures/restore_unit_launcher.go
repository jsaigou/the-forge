// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// restoreUnitLauncherProcedure installs a missing /usr/local/lib/forge/*.sh
// launcher script from smith's own embedded canonical copy
// (internal/smith/launchers) and restarts the unit — the first procedure
// that makes a previously-unfixable class of failure fixable: forge-comfyui
// crash-looped on systemd 203/EXEC for a week (2026-09-01) because its
// launcher had no repo-tracked source and no restart could ever help (a
// missing binary restarts into the exact same failure). See
// docs/v5-smith.md §13 and go/internal/smith/execute.go's
// launcherInstallAllowed for the real allowlist (never overwrites; a unit's
// ExecStartPath is always re-derived from live systemd state, never an
// operator/LLM-supplied path).
//
// The install step's SELinux relabel to bin_t needs no elevated privilege
// on ForgeHost — `chcon` on a file the calling user already owns succeeds
// unprivileged under the targeted policy's unconfined_u (verified live,
// 2026-09-01; see cmd/forge/main.go's smithInstallLauncherFile). Should a
// future host's policy be stricter, this still fails cleanly at the
// install step (never a false success) rather than reporting an install
// that didn't actually leave an executable file behind.
var restoreUnitLauncherProcedure = Procedure{
	ID:    "restore_unit_launcher",
	Title: "Restore a missing unit launcher script",
	Impact: Impact{
		NeedsMaintenance: false,
		EstDuration:      20 * time.Second,
	},
	Params: []Param{{Name: "unit", Allowed: shallowTokenAllowed}},
	Steps: []Step{
		{
			Title:   "Install the launcher script",
			Why:     "the unit's ExecStart program is missing (systemd 203/EXEC) — restore it from smith's embedded canonical copy before attempting a restart, since restarting alone would just repeat the same failure.",
			Op:      "install_unit_launcher",
			Timeout: 10 * time.Second,
			OnFail:  FailAbort,
		},
		{
			Title:          "Restart the unit",
			Why:            "the launcher is in place; restart so the unit picks it up.",
			Op:             "restart_unit",
			Timeout:        30 * time.Second,
			VerifyCheckIDs: []string{"binary_paths"},
			OnFail:         FailAbort,
		},
	},
}
