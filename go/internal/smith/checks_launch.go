// SPDX-License-Identifier: Apache-2.0

package smith

// checks_launch.go — ground-truth checks for the slot launch row (ADR-0026, Radiance phase 1b step f7). The row in slot_launches is what
// `forge slot-exec` execs, so three independent comparisons can each catch a different kind of drift:
//
//	slot_launch_matches_process   the running process is not what the armed row says (someone started the engine by hand, or the row was restored)
//	slot_launch_matches_catalog   the catalog now builds a different launch than the one running (expected after a catalog edit; info)
//	launcher_matches_embedded     the installed /usr/local/lib/forge/start-aN.sh shim differs from the one embedded in this binary
//
// All inputs are injectable (CheckEnv.SlotLaunches, .Proc, .LauncherDir, .ReadFile) so the checks run in unit tests on any OS, and each one skips
// cleanly (an info "skipped:" finding, never a failure) when its prerequisite does not exist: no store, no /proc, no armed row, no installed file.
// None of them writes anything: Smith reports, the existing procedures and the operator fix.
//
// Secret hygiene: a launch argument can be a key or a path. These checks report the INDEX of the first differing argument and counts, never an
// argument or environment value.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/smith/launchers"
	"github.com/jsaigou/the-forge/internal/store"
)

// defaultLauncherDir is where the slot shims are installed on a host (docs/deployment.md).
const defaultLauncherDir = "/usr/local/lib/forge"

// armedSlotLaunches returns the armed rows, or a skip reason when they cannot be read. A read error is a skip, not a failure: a store that predates
// the table is a prerequisite that does not exist yet.
func (e *CheckEnv) armedSlotLaunches(ctx context.Context) ([]store.SlotLaunch, string) {
	list := e.SlotLaunches
	if list == nil && e.Store != nil {
		list = e.Store.SlotLaunches().List
	}
	if list == nil {
		return nil, "no slot launch store"
	}
	rows, err := list(ctx)
	if err != nil {
		return nil, "cannot read slot launch rows: " + err.Error()
	}
	var armed []store.SlotLaunch
	for _, r := range rows {
		if r.Armed {
			armed = append(armed, r)
		}
	}
	if len(armed) == 0 {
		return nil, "no armed slot launch row"
	}
	return armed, ""
}

// activeSlot reports the main PID of slot's engine, or ok=false when the slot is empty, in transition, or its unit has no main process.
func activeSlot(snap *collector.Snapshot, slot string) (pid uint32, ok bool) {
	st, found := snap.Slots[slot]
	if !found || st.Mode == "" || st.Loading != nil || st.Unloading != nil {
		return 0, false
	}
	u, found := snap.Units[st.Unit]
	if !found || u.ActiveState != "active" || u.MainPID == 0 {
		return 0, false
	}
	return u.MainPID, true
}

// firstArgDiff returns the index of the first position where want and got differ (a length difference counts at the shorter length), or -1 if equal.
func firstArgDiff(want, got []string) int {
	n := len(want)
	if len(got) < n {
		n = len(got)
	}
	for i := 0; i < n; i++ {
		if want[i] != got[i] {
			return i
		}
	}
	if len(want) != len(got) {
		return n
	}
	return -1
}

// runSlotLaunchMatchesProcess compares /proc/<MainPID>/cmdline with [exe]+argv from the armed row. It is kind-independent: slot-exec execs the engine,
// so the main PID IS the engine for every kind.
func runSlotLaunchMatchesProcess(ctx context.Context, env *CheckEnv) Finding {
	const id = "slot_launch_matches_process"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	armed, why := env.armedSlotLaunches(ctx)
	if why != "" {
		return skipFinding(id, why)
	}
	proc := env.Proc
	if proc == nil {
		proc = engine.NewHostProc("")
	}

	type mismatch struct {
		Slot     string `json:"slot"`
		ArgIndex int    `json:"first_differing_arg_index"`
		RowArgs  int    `json:"row_arg_count"`
		ProcArgs int    `json:"process_arg_count"`
	}
	var checked, mismatches []mismatch
	var unreadable []string
	for _, row := range armed {
		pid, ok := activeSlot(env.Snap, row.Slot)
		if !ok {
			continue // empty slot or a transition in progress: nothing running to compare
		}
		got, err := proc.Cmdline(pid)
		if err != nil {
			if errors.Is(err, engine.ErrProcUnavailable) {
				return skipFinding(id, "process inspection unavailable on this host")
			}
			unreadable = append(unreadable, row.Slot) // the process may have exited since the snapshot; cannot verify, not a mismatch
			continue
		}
		want := append([]string{row.Exe}, row.Argv...)
		m := mismatch{Slot: row.Slot, ArgIndex: firstArgDiff(want, got), RowArgs: len(want), ProcArgs: len(got)}
		checked = append(checked, m)
		if m.ArgIndex >= 0 {
			mismatches = append(mismatches, m)
		}
	}
	sort.Slice(mismatches, func(i, j int) bool { return mismatches[i].Slot < mismatches[j].Slot })
	sort.Strings(unreadable)
	ev := map[string]any{"checked": len(checked), "mismatches": mismatches, "unreadable": unreadable}
	if len(checked) == 0 {
		if len(unreadable) > 0 {
			return skipFinding(id, fmt.Sprintf("could not read the command line of %d active slot(s)", len(unreadable)))
		}
		return skipFinding(id, "no active slot has an armed launch row")
	}
	if len(mismatches) > 0 {
		first := mismatches[0]
		summary := fmt.Sprintf("slot %s: the running process's command line differs from its armed launch row at argument index %d (row has %d, process has %d) — it was probably started outside the scheduler",
			first.Slot, first.ArgIndex, first.RowArgs, first.ProcArgs)
		if len(mismatches) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(mismatches)-1)
		}
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: summary,
			SummaryKey: "checks.slot_launch_matches_process.mismatch",
			Params:     map[string]any{"slot": first.Slot, "arg_index": first.ArgIndex, "row_count": first.RowArgs, "process_count": first.ProcArgs, "more_count": len(mismatches) - 1},
			Evidence:   ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:    fmt.Sprintf("%d active slot(s) run exactly the command line of their armed launch row", len(checked)),
		SummaryKey: "checks.slot_launch_matches_process.ok",
		Params:     map[string]any{"count": len(checked)},
		Evidence:   ev}
}

// serviceForSlot picks the service of mode that runs in slot: the one whose port role is the slot, else the first (what a load uses). It mirrors the
// selection in engine.adoptService (unexported); the launch itself is built only by engine.BuildSlotLaunchRow.
func serviceForSlot(mode config.Mode, slot string) (config.Service, bool) {
	for _, s := range mode.Services {
		if s.PortRole == slot {
			return s, true
		}
	}
	if len(mode.Services) > 0 {
		s := mode.Services[0]
		s.PortRole = slot
		return s, true
	}
	return config.Service{}, false
}

// runSlotLaunchMatchesCatalog rebuilds each armed row's launch from the current catalog (engine.BuildSlotLaunchRow, the builder every load uses) and
// compares spec hashes. A difference is expected after a catalog edit, so it is info.
func runSlotLaunchMatchesCatalog(ctx context.Context, env *CheckEnv) Finding {
	const id = "slot_launch_matches_catalog"
	cfg := env.cfg()
	if cfg == nil {
		return skipFinding(id, "no config source to rebuild launches from")
	}
	armed, why := env.armedSlotLaunches(ctx)
	if why != "" {
		return skipFinding(id, why)
	}
	type diff struct {
		Slot   string `json:"slot"`
		Config string `json:"config"`
		Reason string `json:"reason"`
	}
	var diffs []diff
	checked := 0
	for _, row := range armed {
		checked++
		mode, ok := cfg.Modes[row.ConfigName]
		if !ok {
			diffs = append(diffs, diff{row.Slot, row.ConfigName, "config is no longer in the catalog"})
			continue
		}
		svc, ok := serviceForSlot(mode, row.Slot)
		if !ok {
			diffs = append(diffs, diff{row.Slot, row.ConfigName, "config has no service"})
			continue
		}
		var cid int64
		if row.ConfigID != nil {
			cid = *row.ConfigID
		}
		want, err := engine.BuildSlotLaunchRow(cfg, row.Slot, row.ConfigName, cid, svc)
		if err != nil {
			diffs = append(diffs, diff{row.Slot, row.ConfigName, "the catalog no longer builds a valid launch: " + err.Error()})
			continue
		}
		wantHash := store.SlotLaunchSpecHash(want.Exe, want.Argv, want.Env, want.Cwd)
		gotHash := row.SpecHash
		if gotHash == "" {
			gotHash = store.SlotLaunchSpecHash(row.Exe, row.Argv, row.Env, row.Cwd)
		}
		if wantHash != gotHash {
			diffs = append(diffs, diff{row.Slot, row.ConfigName, "spec_hash differs"})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Slot < diffs[j].Slot })
	ev := map[string]any{"checked": checked, "differences": diffs}
	if len(diffs) > 0 {
		first := diffs[0]
		summary := fmt.Sprintf("slot %s: the launch the catalog would build for %q differs from the armed row (%s) — expected after a catalog edit; reload the slot to apply it",
			first.Slot, first.Config, first.Reason)
		if len(diffs) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(diffs)-1)
		}
		return Finding{CheckID: id, Severity: SeverityInfo, Summary: summary,
			SummaryKey: "checks.slot_launch_matches_catalog.differs",
			Params:     map[string]any{"slot": first.Slot, "config": first.Config, "reason": first.Reason, "more_count": len(diffs) - 1},
			Evidence:   ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:    fmt.Sprintf("the catalog rebuilds the same launch as %d armed row(s)", checked),
		SummaryKey: "checks.slot_launch_matches_catalog.ok",
		Params:     map[string]any{"count": checked},
		Evidence:   ev}
}

// runLauncherMatchesEmbedded compares each installed slot shim with the copy embedded in this binary. Only a PRESENT file that differs is reported: a
// missing one is restore_unit_launcher's job (binary_paths), and Smith never overwrites an existing launcher.
func runLauncherMatchesEmbedded(_ context.Context, env *CheckEnv) Finding {
	const id = "launcher_matches_embedded"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	dir := env.LauncherDir
	if dir == "" {
		dir = defaultLauncherDir
	}
	read := env.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	slots := make([]string, 0, len(env.Snap.Slots))
	for name := range env.Snap.Slots {
		slots = append(slots, name)
	}
	sort.Strings(slots)

	sum := func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
	type differs struct {
		Slot      string `json:"slot"`
		Path      string `json:"path"`
		Installed string `json:"installed_sha256"`
		Embedded  string `json:"embedded_sha256"`
	}
	var diffs []differs
	var unreadable []string
	checked, missing := 0, 0
	for _, slot := range slots {
		base := "start-" + slot + ".sh"
		want, ok := launchers.Content(base)
		if !ok {
			continue // no embedded shim for this slot name
		}
		path := filepath.Join(dir, base)
		got, err := read(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing++
			continue
		case err != nil:
			unreadable = append(unreadable, slot)
			continue
		}
		checked++
		if !bytes.Equal(got, want) {
			diffs = append(diffs, differs{Slot: slot, Path: path, Installed: sum(got), Embedded: sum(want)})
		}
	}
	ev := map[string]any{"checked": checked, "missing": missing, "unreadable": unreadable, "differences": diffs}
	if len(diffs) > 0 {
		first := diffs[0]
		summary := fmt.Sprintf("installed launcher %s differs from the shim embedded in this forge binary — it will not be overwritten; compare and reinstall deliberately", first.Path)
		if len(diffs) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(diffs)-1)
		}
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: summary,
			SummaryKey: "checks.launcher_matches_embedded.differs",
			Params:     map[string]any{"path": first.Path, "more_count": len(diffs) - 1},
			Evidence:   ev}
	}
	if checked == 0 {
		return skipFinding(id, "no installed slot launcher to compare")
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:    fmt.Sprintf("%d installed slot launcher(s) match the embedded shim", checked),
		SummaryKey: "checks.launcher_matches_embedded.ok",
		Params:     map[string]any{"count": checked},
		Evidence:   ev}
}
