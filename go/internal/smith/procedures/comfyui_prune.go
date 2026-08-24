// SPDX-License-Identifier: Apache-2.0

package procedures

import "time"

// comfyUIPruneProcedure wraps the existing atomic delete_files operation
// (smith's dispatchDeleteFiles) as a procedure — the third of Sprint 3's
// atomic-to-procedure wraps (docs/v5-smith.md §13). files_json carries a
// JSON-encoded []deleteFileEntry (a plain map[string]string param can't
// hold a list); jsonArrayParamAllowed only checks well-formedness — every
// individual path is re-validated against the currently configured ComfyUI
// model roots by the op handler itself (deleteAllowed), same as the atomic
// dispatch this replaces.
var comfyUIPruneProcedure = Procedure{
	ID:    "comfyui_prune",
	Title: "Prune unused ComfyUI model files",
	Impact: Impact{
		NeedsMaintenance: false,
		EstDuration:      10 * time.Second,
	},
	Params: []Param{{Name: "files_json", Allowed: jsonArrayParamAllowed}},
	Steps: []Step{
		{
			Title:          "Delete the identified files",
			Why:            "these files were already identified as unused ComfyUI model weights, evidenced and confirmed at proposal time.",
			Op:             "delete_comfyui_files",
			Timeout:        30 * time.Second,
			VerifyCheckIDs: []string{"comfyui_health", "disk_space"},
			OnFail:         FailAbort,
		},
	},
}
