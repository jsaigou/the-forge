// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// diskUsageReportProcedure is Sprint 2's "one trivial real procedure to
// prove the path end to end" (docs/v5-smith.md §13). Deliberately boring:
// two read-only `df` calls against the two volumes an operator actually
// cares about (the catalog DB's volume and the deploy/binary volume),
// each re-verified against the real disk_space check. No maintenance
// window, no checkpoint, no rollback — those mechanisms are exercised by
// package smith's own procedure tests via a registered fixture, not by
// inventing risk in the one procedure this sprint runs live on ForgeHost.
var diskUsageReportProcedure = Procedure{
	ID:    "disk_usage_report",
	Title: "Disk usage report",
	Impact: Impact{
		NeedsMaintenance: false,
		EstDuration:      10 * time.Second,
	},
	Preconditions: nil,
	Steps: []Step{
		{
			Title:          "Check the catalog DB volume",
			Why:            "smith_procedure_runs and every other store table live here — worth knowing before the disk fills silently.",
			Argv:           []string{"df", "-h", "/var/lib/forge"},
			Timeout:        10 * time.Second,
			VerifyCheckIDs: []string{"disk_space"},
			OnFail:         FailAbort,
		},
		{
			Title:          "Check the deploy/binary volume",
			Why:            "forge itself, plus every deployed llama.cpp build, lives here.",
			Argv:           []string{"df", "-h", "/opt/forge"},
			Timeout:        10 * time.Second,
			VerifyCheckIDs: []string{"disk_space"},
			OnFail:         FailAbort,
		},
	},
}
