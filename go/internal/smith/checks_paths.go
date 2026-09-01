// SPDX-License-Identifier: Apache-2.0

package smith

// checks_paths.go — standing binary-path and slot-identity ground-truth
// checks, added after the sprint-4 follow-up investigation (2026-08-25)
// found infra.paths.rocm_bin pointing at a nonexistent binary and, more
// seriously, two live slots whose actual running llama-server process
// diverged from what the engine believed was loaded (docs/pitfalls.md's
// FOUNDRY_*/FORGE_* divergence incident). Neither finding was previously
// caught by any deterministic check: binary_versions (checks_binaries.go)
// only tracks version staleness for a curated opt-in list, and
// slot_agreement (checks.go) compares two engine-internal views that share
// the same env-file-derived blind spot rather than the actually-running
// process's own self-report.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// runBinaryPaths verifies every configured/catalog-referenced llama-server
// binary path actually exists and is executable — the standing version of
// preflight.go's checkBin, run every sweep rather than only when an
// operator happens to open Settings -> Danger Zone.
func runBinaryPaths(ctx context.Context, env *CheckEnv) Finding {
	const id = "binary_paths"
	cfg := env.cfg()
	if cfg == nil && env.Catalog == nil && env.Snap == nil {
		return skipFinding(id, "no config, catalog, or snapshot source for binary paths")
	}

	type pathStatus struct {
		Label string `json:"label"`
		Path  string `json:"path"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	// unitExecMiss is the structured (not label-string-parsed) evidence
	// proposeRestoreUnitLauncher (propose.go) reads to turn a missing
	// launcher into a restore_unit_launcher proposal — kept separate from
	// the generic pathStatus/statuses list (which also carries non-unit
	// paths like infra.paths.vulkan_bin) so the proposer never has to parse
	// a human-readable label back into a unit name.
	type unitExecMiss struct {
		Unit string `json:"unit"`
		Path string `json:"path"`
	}
	var statuses []pathStatus
	var missing []string
	var unitMisses []unitExecMiss

	// checkPath returns (attempted, ok) — attempted is false only for an
	// unconfigured (empty) path, which is not this check's concern.
	checkPath := func(label, path string) (attempted, ok bool) {
		if path == "" {
			return false, false
		}
		st := pathStatus{Label: label, Path: path}
		info, err := os.Stat(path)
		switch {
		case err != nil:
			st.Error = err.Error()
		case info.IsDir():
			st.Error = "is a directory, not a file"
		case info.Mode()&0o111 == 0:
			st.Error = "not executable"
		default:
			st.OK = true
		}
		statuses = append(statuses, st)
		if !st.OK {
			missing = append(missing, fmt.Sprintf("%s (%s)", label, path))
		}
		return true, st.OK
	}

	if cfg != nil {
		checkPath("infra.paths.vulkan_bin", cfg.Paths.VulkanBin)
		checkPath("infra.paths.rocm_bin", cfg.Paths.RocmBin)
	}
	if env.Catalog != nil {
		builds, err := env.Catalog.ListBuilds(ctx)
		if err != nil {
			return Finding{CheckID: id, Severity: SeverityWarn,
				Summary: fmt.Sprintf("could not list catalog builds: %v", err)}
		}
		for _, b := range builds {
			if b.BinaryPath == "" {
				continue // vllm/retired builds legitimately carry no binary
			}
			checkPath(fmt.Sprintf("build %q", b.Name), b.BinaryPath)
		}
	}
	if env.Snap != nil {
		// Every watched unit's own ExecStart= program — catches a unit whose
		// launcher script/binary went missing (systemd 203/EXEC) even before
		// anything tries to load or reach it. Found live 2026-09-01:
		// forge-comfyui.service crash-looped for a week on exactly this
		// (start-comfyui.sh dropped during the Foundry->Forge migration,
		// docs/pitfalls.md) with nothing catching it until an operator tried
		// to use ComfyUI. Sorted for stable output.
		units := make([]string, 0, len(env.Snap.Units))
		for name := range env.Snap.Units {
			units = append(units, name)
		}
		sort.Strings(units)
		for _, name := range units {
			path := env.Snap.Units[name].ExecStartPath
			if attempted, ok := checkPath(fmt.Sprintf("unit %s ExecStart", name), path); attempted && !ok {
				unitMisses = append(unitMisses, unitExecMiss{Unit: name, Path: path})
			}
		}
	}

	ev := map[string]any{"paths": statuses}
	if len(unitMisses) > 0 {
		ev["unit_exec_missing"] = unitMisses
	}
	if len(statuses) == 0 {
		return skipFinding(id, "no binary paths configured (infra.paths and catalog builds both empty)")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("%d of %d tracked binary path(s) missing or not executable: %s", len(missing), len(statuses), strings.Join(missing, ", ")),
			Evidence: ev, KBRefs: []string{"pitfalls:foundry-forge-env-divergence"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary: fmt.Sprintf("%d binary path(s) checked, all present and executable", len(statuses)), Evidence: ev}
}

// runSlotModelIdentity is the ground-truth check: for every loaded slot
// with a live /props scrape, compares the actually-running process's own
// self-reported model_alias against the engine's configured alias for the
// mode it believes is loaded. A mismatch means the wrong model may be
// serving real requests right now — the class of bug slot_agreement cannot
// see, since both its inputs derive from the same env-file reading the
// running process itself may have silently diverged from.
func runSlotModelIdentity(_ context.Context, env *CheckEnv) Finding {
	const id = "slot_model_identity"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	cfg := env.cfg()
	if cfg == nil {
		return skipFinding(id, "no config source for configured alias")
	}

	type slotIdentity struct {
		Slot            string `json:"slot"`
		Mode            string `json:"mode"`
		ConfiguredAlias string `json:"configured_alias"`
		ActualAlias     string `json:"actual_alias"`
	}
	var checked, mismatches []slotIdentity

	for name, st := range env.Snap.Slots {
		if st.Mode == "" {
			continue
		}
		inf, ok := env.Snap.Inference[name]
		if !ok || inf.ModelAlias == "" {
			continue // not scraped yet (or an older llama.cpp build without model_alias) — nothing to compare
		}
		m, ok := cfg.Modes[st.Mode]
		if !ok || len(m.Services) == 0 || m.Services[0].Alias == "" {
			continue // no configured alias to compare against
		}
		row := slotIdentity{Slot: name, Mode: st.Mode, ConfiguredAlias: m.Services[0].Alias, ActualAlias: inf.ModelAlias}
		checked = append(checked, row)
		if inf.ModelAlias != row.ConfiguredAlias {
			mismatches = append(mismatches, row)
		}
	}
	sort.Slice(checked, func(i, j int) bool { return checked[i].Slot < checked[j].Slot })
	sort.Slice(mismatches, func(i, j int) bool { return mismatches[i].Slot < mismatches[j].Slot })

	ev := map[string]any{"checked": checked, "mismatches": mismatches}
	if len(checked) == 0 {
		return skipFinding(id, "no loaded slot has both a live /props scrape and a configured alias to compare")
	}
	if len(mismatches) > 0 {
		first := mismatches[0]
		summary := fmt.Sprintf("slot %s: engine believes %q (mode %s) is loaded but the running process reports alias %q — wrong model may be serving requests",
			first.Slot, first.ConfiguredAlias, first.Mode, first.ActualAlias)
		if len(mismatches) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(mismatches)-1)
		}
		return Finding{CheckID: id, Severity: SeverityCrit, Summary: summary,
			Evidence: ev, KBRefs: []string{"pitfalls:foundry-forge-env-divergence"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("engine-configured alias matches the actually-running process on %d loaded slot(s)", len(checked)),
		Evidence: ev}
}
