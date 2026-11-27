// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
)

// ── enforcement through SwitchMode (the other load path; Load is covered in slotlaunch_test.go) ──

// With enforcement a mode that cannot be built fails the switch before anything is written or started: the slot's env/args files stay as they
// were and the previous row is untouched (build failure is order 1, nothing has changed yet).
func TestEnforceSwitchModeBuildFailureLeavesFilesAndRowAsTheyWere(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	m := newLaunchManager(t, cfg, sys, nil, db.SlotLaunches(), &fakeProc{})
	good := config.Service{Model: "gemma.gguf", Alias: "gemma", Context: 8192, Backend: "vulkan"}
	if err := wsl(m, "a1", "gemma", 1, good); err != nil {
		t.Fatal(err)
	}
	rowBefore, _ := db.SlotLaunches().Get(context.Background(), "a1")

	cfg.Modes["bad"] = config.Mode{Services: []config.Service{{Model: "m.gguf", Alias: "m", Context: 1, Backend: "bogus-backend", PortRole: "a1"}}}
	res := m.SwitchMode(context.Background(), "bad")
	if res.Success || !strings.Contains(res.Message, "could not be built") {
		t.Fatalf("res = %+v", res)
	}
	if len(sys.started) != 0 {
		t.Errorf("units started despite the failure: %v", sys.started)
	}
	// A switch stops the running mode first, and stopping disarms that slot's row (existing behaviour, TestLoadWritesRowBeforeStartAndUnloadDisarms).
	// So the only permitted change is Armed true -> false; the content, generation and hash are untouched by the failed build.
	rowAfter, _ := db.SlotLaunches().Get(context.Background(), "a1")
	if rowAfter.Armed {
		t.Error("row still armed although the slot was stopped")
	}
	rowAfter.Armed = rowBefore.Armed
	if !reflect.DeepEqual(rowBefore, rowAfter) {
		t.Errorf("row content changed by a build failure\n %+v\n %+v", rowBefore, rowAfter)
	}
}

// Row-write failure after the files are written (documented order 3): the switch fails, nothing starts, the files are the new ones and the old
// armed row is disarmed so it cannot describe them.
func TestEnforceSwitchModeRowWriteFailureFailsAndDisarms(t *testing.T) {
	cfg := testConfig(t)
	sys := newFakeSys()
	db := openLaunchStore(t)
	hs := &hookedStore{SlotLaunchStore: db.SlotLaunches()}
	m := newLaunchManager(t, cfg, sys, nil, hs, &fakeProc{})
	if err := wsl(m, "a1", "qwen", 2, cfg.Modes["qwen"].Services[0]); err != nil {
		t.Fatal(err)
	}
	hs.putErr = errors.New("disk full")
	res := m.SwitchMode(context.Background(), "gemma")
	if res.Success || !strings.Contains(res.Message, "slot launch row for a1 could not be written") {
		t.Fatalf("res = %+v", res)
	}
	if len(sys.started) != 0 {
		t.Errorf("units started despite the failure: %v", sys.started)
	}
	if r, _ := db.SlotLaunches().Get(context.Background(), "a1"); r.Armed {
		t.Error("previous row still armed after a failed row write")
	}
}

// `forge launch-check`, the load path and the adopt path must share one builder, or the preflight can pass what a load rejects. The only caller of
// launch.BuildService under engine/ and cmd/forge/ may be BuildSlotLaunchRow itself.
func TestOneBuilderPathForLoadAdoptAndLaunchCheck(t *testing.T) {
	var offenders []string
	for _, dir := range []string{".", "../../cmd/forge"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") || filepath.Base(f) == "slotlaunch.go" {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), "launch.BuildService(") {
				offenders = append(offenders, f)
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("launch.BuildService called outside engine.BuildSlotLaunchRow: %v", offenders)
	}
	src, err := os.ReadFile("slotlaunch.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "launch.BuildService("); n != 1 {
		t.Errorf("slotlaunch.go calls launch.BuildService %d times, want exactly 1 (inside BuildSlotLaunchRow)", n)
	}
}
