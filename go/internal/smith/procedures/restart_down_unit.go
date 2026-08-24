// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// restartDownUnitProcedure wraps the existing atomic restart_forge_unit
// operation (smith's dispatchRestartUnit) as a procedure, so approving it
// goes through the runner's post-verify + resumable-run machinery instead
// of a bare one-shot dispatch (autonomous-remediation Sprint 3,
// docs/v5-smith.md §13). No maintenance window — restarting one of the
// allowlisted units (forge-stt, headroom@*, etc.) is exactly as
// disruptive as it was as an atomic action, not more.
var restartDownUnitProcedure = Procedure{
	ID:    "restart_down_unit",
	Title: "Restart a down forge unit",
	Impact: Impact{
		NeedsMaintenance: false,
		EstDuration:      15 * time.Second,
	},
	Params: []Param{{Name: "unit", Allowed: shallowTokenAllowed}},
	Steps: []Step{
		{
			Title:          "Restart the unit",
			Why:            "the unit is down or unresponsive; a clean restart is the smallest real fix smith already performs atomically today.",
			Op:             "restart_unit",
			Timeout:        30 * time.Second,
			VerifyCheckIDs: []string{"always_on_ports"},
			OnFail:         FailAbort,
		},
	},
}
