// SPDX-License-Identifier: Apache-2.0

package smith

// checks_comfyui.go implements smith P6's ComfyUI module (docs/v5-smith.md
// §4.9 FR7): a fast liveness check (comfyui_health) and a deep-sweep-only
// dependency-map check (comfyui_prune) whose findings are the only route
// through which a delete_files proposal can ever be created (propose.go's
// proposeComfyUIPrune). The map itself — the four refusal guardrails, the
// two ground-truth facts about ForgeHost's real layout that make a naive parse
// unsafe — lives in internal/smith/comfyui; this file only turns a
// comfyui.MapResult into a Finding.
//
// Deliberately NOT measuring GTT footprint here (the plan's original
// design also asked for this): that needs ComfyUI pid discovery added to
// internal/collector (finding the running comfy process, then
// Proc.GPUMemoryBytes on it) — real, separate collector work, scoped out
// of this sprint the same way Track B scoped out the /props build_info
// extension. Flagged, not silently dropped; comfyui_health's job here is
// reachability only (unit state + port), which is what §4.9's pruning gate
// actually depends on.

import (
	"context"
	"fmt"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/smith/comfyui"
)

// runComfyUIHealth reports ComfyUI's systemd unit state + port reachability
// — the same shape as always_on_ports (checks.go), split into its own check
// because ComfyUI's port (3001) is not in cfg.Ports (docs/v5-smith.md §4.9's
// gap: "ComfyUI is invisible to it").
func runComfyUIHealth(_ context.Context, env *CheckEnv) Finding {
	const id = "comfyui_health"
	if !env.ComfyUIEnabled {
		return Finding{}
	}
	var unit collector.UnitState
	unitActive := true
	if env.ComfyUIUnit != "" && env.Snap != nil {
		unit = env.Snap.Units[env.ComfyUIUnit]
		unitActive = unit.Active()
	}
	portUp := true
	if env.ComfyUIPort != 0 && env.Dial != nil {
		portUp = env.Dial(env.ComfyUIPort)
	}
	ev := map[string]any{
		"unit":                  env.ComfyUIUnit,
		"unit_active":           unitActive,
		"unit_state":            unit.ActiveState,
		"unit_substate":         unit.SubState,
		"unit_result":           unit.Result,
		"unit_exec_main_status": unit.ExecMainStatus,
		"port":                  env.ComfyUIPort,
		"port_up":               portUp,
	}
	if !unitActive || !portUp {
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary: fmt.Sprintf("ComfyUI unreachable — %s (pruning/GTT checks unavailable until it's running)",
				comfyUIDownReason(env.ComfyUIUnit, unitActive, unit.ActiveState, unit.Result, unit.ExecMainStatus, env.ComfyUIPort)),
			Evidence: ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK, Summary: "ComfyUI reachable", Evidence: ev}
}

// comfyUIDownReason explains, in operator language, WHY ComfyUI is
// unreachable. "Unreachable" alone hides the fix: an OOM-killed unit, a
// crash, a clean stop, and a running unit with a dead port are four very
// different problems. Returned as a fragment that reads after either
// "ComfyUI unreachable — " (finding summary) or "ComfyUI is unreachable: "
// (chat answer).
func comfyUIDownReason(unit string, active bool, state, result string, execMainStatus int32, port int) string {
	if !active {
		switch {
		case state == "failed" && result == "oom-kill":
			return fmt.Sprintf("the %s unit was OOM-killed", unit)
		case state == "failed" && execMainStatus == 203:
			return fmt.Sprintf("the %s unit's ExecStart program is missing or not executable (systemd 203/EXEC — check binary_paths for the exact path)", unit)
		case state == "failed" && result != "":
			return fmt.Sprintf("the %s unit crashed (failed: %s)", unit, result)
		case state == "failed":
			return fmt.Sprintf("the %s unit crashed", unit)
		case state != "":
			return fmt.Sprintf("the %s unit isn't running (it's %s)", unit, state)
		default:
			if unit == "" {
				return "it isn't monitored (no unit configured)"
			}
			return fmt.Sprintf("the %s unit isn't running", unit)
		}
	}
	return fmt.Sprintf("the %s unit is running but port %d isn't answering", unit, port)
}

// comfyUIKeepGuidance is the operator-facing answer to "I want to keep this
// file, how do I get it off the list" — found live 2026-08-11/12: a first
// real run correctly listed several files the operator actually
// wants to keep (real krea2 LoRAs, not abandoned) because "unreferenced by
// a currently-saved workflow" only ever means exactly that — it can never
// distinguish "genuinely abandoned" from "not wired into a saved workflow
// yet". The tool can't read intent, but it CAN make intent traceable: this
// is the same text surfaced on both the finding and the delete_files
// proposal's own detail (deleteFilesDetail.Guidance), so it's visible
// wherever an operator is looking when they decide.
const comfyUIKeepGuidance = "To keep a file listed here, open ComfyUI and load or build a workflow that uses it " +
	"(one of ComfyUI's own template workflows featuring this model works too), then save that workflow into a " +
	"configured workflow_dir. Re-run this check afterward — a file named by any saved workflow, or by anything " +
	"currently in ComfyUI's queue/history, is automatically excluded from this list on the next sweep."

// runComfyUIPrune builds the workflow dependency map and reports prune
// candidates or the specific guardrail that refused to build one. Deep-only
// — BuildMap makes 3 real HTTP calls plus a full recursive walk of every
// configured model root, not a quick-sweep-budget operation.
func runComfyUIPrune(ctx context.Context, env *CheckEnv) Finding {
	const id = "comfyui_prune"
	if !env.ComfyUIEnabled {
		return Finding{}
	}
	if env.ComfyUI == nil {
		return skipFinding(id, "comfyui client not wired")
	}
	if len(env.ComfyUIModelRoots) == 0 {
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary: "no comfyui model roots configured (smith.comfyui.model_roots is empty)"}
	}

	res := comfyui.BuildMap(ctx, env.ComfyUI, env.ComfyUIModelRoots, env.ComfyUIWorkflowDirs)
	ev := map[string]any{
		"buildable":             res.Buildable,
		"refusal_reason":        res.RefusalReason,
		"refusal_detail":        res.RefusalDetail,
		"workflow_files_found":  res.WorkflowFilesFound,
		"workflows_parsed":      res.WorkflowsParsed,
		"zero_loader_workflows": res.ZeroLoaderWorkflows,
		"unknown_classes":       res.UnknownClasses,
		"missing_from_roots":    res.MissingFromRoots,
		"referenced_count":      res.ReferencedCount,
		"inventory_count":       res.InventoryCount,
		"candidates":            res.Candidates,
	}
	if !res.Buildable {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("dependency map unbuildable (%s): %s — no deletion proposal is possible until this clears", res.RefusalReason, res.RefusalDetail),
			Evidence: ev, KBRefs: []string{"smith:comfyui-pruning-guardrails"}}
	}
	if len(res.Candidates) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:  fmt.Sprintf("dependency map built (%d referenced, %d inventoried); nothing unreferenced", res.ReferencedCount, res.InventoryCount),
			Evidence: ev}
	}
	var totalBytes int64
	for _, c := range res.Candidates {
		totalBytes += c.SizeBytes
	}
	ev["guidance"] = comfyUIKeepGuidance
	return Finding{CheckID: id, Severity: SeverityInfo,
		Summary:  fmt.Sprintf("%d unreferenced ComfyUI model file(s) found (%.1f GB reclaimable) — review before approving; see guidance", len(res.Candidates), float64(totalBytes)/(1<<30)),
		Evidence: ev, KBRefs: []string{"smith:comfyui-pruning-guardrails"}}
}
