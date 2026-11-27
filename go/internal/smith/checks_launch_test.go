// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/smith/launchers"
	"github.com/jsaigou/the-forge/internal/store"
)

type fakeProcInspector struct {
	cmd map[uint32][]string
	err error
}

func (f fakeProcInspector) Cmdline(pid uint32) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.cmd[pid]
	if !ok {
		return nil, os.ErrNotExist
	}
	return c, nil
}
func (f fakeProcInspector) Environ(uint32) (map[string]string, error) { return nil, nil }

// launchSnap is a snapshot with one active slot a1 whose unit main PID is pid.
func launchSnap(mode string, pid uint32) *collector.Snapshot {
	snap := snapWith(collector.Metrics{})
	snap.Slots["a1"] = collector.SlotState{Slot: "a1", Unit: "forge-a1", Mode: mode, Port: 8080}
	snap.Units["forge-a1"] = collector.UnitState{ActiveState: "active", MainPID: pid}
	return snap
}

func rowsOf(rows ...store.SlotLaunch) func(context.Context) ([]store.SlotLaunch, error) {
	return func(context.Context) ([]store.SlotLaunch, error) { return rows, nil }
}

func armedRow(kind string, argv ...string) store.SlotLaunch {
	return store.SlotLaunch{Slot: "a1", Armed: true, ConfigName: "m", EngineKind: kind, Exe: "/opt/x/engine", Argv: argv}
}

func TestSlotLaunchMatchesProcess(t *testing.T) {
	ctx := context.Background()
	// Kind-independent: the same comparison holds for every engine kind.
	for _, kind := range []string{"llamacpp", "vllm", "radiance"} {
		t.Run("match "+kind, func(t *testing.T) {
			env := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: rowsOf(armedRow(kind, "--port", "8080")),
				Proc: fakeProcInspector{cmd: map[uint32][]string{10: {"/opt/x/engine", "--port", "8080"}}}}
			if f := runSlotLaunchMatchesProcess(ctx, env); f.Severity != SeverityOK {
				t.Fatalf("got %s %q, want ok", f.Severity, f.Summary)
			}
		})
	}
	t.Run("mismatch names index and never the value", func(t *testing.T) {
		env := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: rowsOf(armedRow("radiance", "--api-key", "sk-row-secret", "--port", "8080")),
			Proc: fakeProcInspector{cmd: map[uint32][]string{10: {"/opt/x/engine", "--api-key", "sk-proc-secret", "--port", "8080"}}}}
		f := runSlotLaunchMatchesProcess(ctx, env)
		if f.Severity != SeverityWarn {
			t.Fatalf("severity = %s, want warn", f.Severity)
		}
		if !strings.Contains(f.Summary, "slot a1") || !strings.Contains(f.Summary, "index 2") {
			t.Errorf("summary = %q, want slot a1 and index 2", f.Summary)
		}
		all := fmt.Sprintf("%s %v %v", f.Summary, f.Evidence, f.Params)
		if strings.Contains(all, "secret") {
			t.Errorf("finding leaks an argument value: %s", all)
		}
	})
	t.Run("length difference reports the shorter length as the index", func(t *testing.T) {
		env := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: rowsOf(armedRow("llamacpp", "-c", "4096")),
			Proc: fakeProcInspector{cmd: map[uint32][]string{10: {"/opt/x/engine", "-c", "4096", "--extra"}}}}
		if f := runSlotLaunchMatchesProcess(ctx, env); f.Severity != SeverityWarn || !strings.Contains(f.Summary, "index 3") {
			t.Fatalf("got %s %q", f.Severity, f.Summary)
		}
	})
	t.Run("unarmed row is skipped", func(t *testing.T) {
		r := armedRow("llamacpp")
		r.Armed = false
		env := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: rowsOf(r), Proc: fakeProcInspector{err: errors.New("must not be read")}}
		if f := runSlotLaunchMatchesProcess(ctx, env); f.Severity != SeverityInfo || !strings.HasPrefix(f.Summary, "skipped:") {
			t.Fatalf("got %s %q, want skip", f.Severity, f.Summary)
		}
	})
	t.Run("unreadable /proc for one process is not a mismatch", func(t *testing.T) {
		env := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: rowsOf(armedRow("llamacpp")), Proc: fakeProcInspector{cmd: map[uint32][]string{}}}
		if f := runSlotLaunchMatchesProcess(ctx, env); f.Severity != SeverityInfo || !strings.HasPrefix(f.Summary, "skipped:") {
			t.Fatalf("got %s %q, want skip", f.Severity, f.Summary)
		}
	})
	t.Run("no /proc on this host skips", func(t *testing.T) {
		env := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: rowsOf(armedRow("llamacpp")), Proc: fakeProcInspector{err: engine.ErrProcUnavailable}}
		if f := runSlotLaunchMatchesProcess(ctx, env); !strings.HasPrefix(f.Summary, "skipped:") {
			t.Fatalf("got %q, want skip", f.Summary)
		}
	})
	t.Run("empty slot, transition and no store are skipped", func(t *testing.T) {
		snap := launchSnap("", 10)
		if f := runSlotLaunchMatchesProcess(ctx, &CheckEnv{Snap: snap, SlotLaunches: rowsOf(armedRow("llamacpp"))}); !strings.HasPrefix(f.Summary, "skipped:") {
			t.Errorf("empty slot: %q", f.Summary)
		}
		snap = launchSnap("m", 10)
		s := snap.Slots["a1"]
		s.Loading = &collector.Transition{}
		snap.Slots["a1"] = s
		if f := runSlotLaunchMatchesProcess(ctx, &CheckEnv{Snap: snap, SlotLaunches: rowsOf(armedRow("llamacpp"))}); !strings.HasPrefix(f.Summary, "skipped:") {
			t.Errorf("loading slot: %q", f.Summary)
		}
		if f := runSlotLaunchMatchesProcess(ctx, &CheckEnv{Snap: launchSnap("m", 10)}); !strings.HasPrefix(f.Summary, "skipped:") {
			t.Errorf("no store: %q", f.Summary)
		}
		bad := &CheckEnv{Snap: launchSnap("m", 10), SlotLaunches: func(context.Context) ([]store.SlotLaunch, error) { return nil, errors.New("no such table") }}
		if f := runSlotLaunchMatchesProcess(ctx, bad); !strings.HasPrefix(f.Summary, "skipped:") {
			t.Errorf("store error: %q", f.Summary)
		}
	})
}

func launchCatalogFixture(t *testing.T) (*config.Config, store.SlotLaunch) {
	t.Helper()
	cfg := &config.Config{
		Paths: config.Paths{SysconfigDir: t.TempDir(), ModelsDir: t.TempDir(), StateDir: t.TempDir()},
		Slots: map[string]config.Slot{"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1}},
		Modes: map[string]config.Mode{"gemma": {Services: []config.Service{{
			Model: "gemma.gguf", Alias: "gemma", Context: 131072, PortRole: "a1", Backend: "vulkan", StartupTimeoutS: 120,
		}}}},
	}
	row, err := engine.BuildSlotLaunchRow(cfg, "a1", "gemma", 0, cfg.Modes["gemma"].Services[0])
	if err != nil {
		t.Fatalf("BuildSlotLaunchRow: %v", err)
	}
	row.Armed = true
	row.SpecHash = store.SlotLaunchSpecHash(row.Exe, row.Argv, row.Env, row.Cwd)
	return cfg, row
}

func TestSlotLaunchMatchesCatalog(t *testing.T) {
	ctx := context.Background()
	cfg, row := launchCatalogFixture(t)
	env := &CheckEnv{Cfg: func() *config.Config { return cfg }, SlotLaunches: rowsOf(row)}
	if f := runSlotLaunchMatchesCatalog(ctx, env); f.Severity != SeverityOK {
		t.Fatalf("unchanged catalog: %s %q", f.Severity, f.Summary)
	}
	// A catalog edit changes the rebuilt launch: info, never warn.
	m := cfg.Modes["gemma"]
	m.Services[0].Context = 65536
	f := runSlotLaunchMatchesCatalog(ctx, env)
	if f.Severity != SeverityInfo || strings.HasPrefix(f.Summary, "skipped:") || !strings.Contains(f.Summary, "slot a1") {
		t.Fatalf("edited catalog: %s %q, want info difference", f.Severity, f.Summary)
	}
	// Config removed from the catalog.
	delete(cfg.Modes, "gemma")
	if f := runSlotLaunchMatchesCatalog(ctx, env); f.Severity != SeverityInfo || !strings.Contains(f.Summary, "no longer in the catalog") {
		t.Fatalf("removed config: %s %q", f.Severity, f.Summary)
	}
	// Prerequisites absent.
	if f := runSlotLaunchMatchesCatalog(ctx, &CheckEnv{SlotLaunches: rowsOf(row)}); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Errorf("no cfg: %q", f.Summary)
	}
	unarmed := row
	unarmed.Armed = false
	if f := runSlotLaunchMatchesCatalog(ctx, &CheckEnv{Cfg: func() *config.Config { return cfg }, SlotLaunches: rowsOf(unarmed)}); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Errorf("unarmed: %q", f.Summary)
	}
}

func TestLauncherMatchesEmbedded(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	want, ok := launchers.Content("start-a1.sh")
	if !ok {
		t.Fatal("no embedded start-a1.sh")
	}
	snap := snapWith(collector.Metrics{})
	snap.Slots["a1"] = collector.SlotState{Slot: "a1"}
	env := &CheckEnv{Snap: snap, LauncherDir: dir}

	// Missing file: the restore procedure's business, so a skip here, not a finding.
	if f := runLauncherMatchesEmbedded(ctx, env); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Fatalf("missing file: %s %q", f.Severity, f.Summary)
	}
	path := filepath.Join(dir, "start-a1.sh")
	if err := os.WriteFile(path, want, 0o755); err != nil {
		t.Fatal(err)
	}
	if f := runLauncherMatchesEmbedded(ctx, env); f.Severity != SeverityOK {
		t.Fatalf("identical: %s %q", f.Severity, f.Summary)
	}
	if err := os.WriteFile(path, append(append([]byte{}, want...), []byte("# edited\n")...), 0o755); err != nil {
		t.Fatal(err)
	}
	f := runLauncherMatchesEmbedded(ctx, env)
	if f.Severity != SeverityWarn || !strings.Contains(f.Summary, path) {
		t.Fatalf("differs: %s %q", f.Severity, f.Summary)
	}
	after, _ := os.ReadFile(path)
	if !strings.HasSuffix(string(after), "# edited\n") {
		t.Error("the check overwrote the installed launcher")
	}
	// Unreadable (not missing) is not reported as a difference.
	env.ReadFile = func(string) ([]byte, error) { return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission} }
	if f := runLauncherMatchesEmbedded(ctx, env); f.Severity == SeverityWarn {
		t.Fatalf("unreadable reported as warn: %q", f.Summary)
	}
	if f := runLauncherMatchesEmbedded(ctx, &CheckEnv{}); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Errorf("no snapshot: %q", f.Summary)
	}
}

func TestSlotModelIdentityFallsBackToModelsID(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"served-name"}]}`))
	}))
	defer srv.Close()
	port, _ := strconv.Atoi(srv.URL[strings.LastIndex(srv.URL, ":")+1:])

	mk := func(kind, alias string) *CheckEnv {
		cfg := &config.Config{Modes: map[string]config.Mode{"m": {Services: []config.Service{{Alias: alias, EngineKind: kind}}}}}
		snap := snapWith(collector.Metrics{})
		snap.Slots["a1"] = collector.SlotState{Slot: "a1", Mode: "m", Port: port}
		return &CheckEnv{Snap: snap, Cfg: func() *config.Config { return cfg }, HTTP: &http.Client{Timeout: 2 * time.Second}}
	}
	// A kind that serves no /props (vLLM today, Radiance once it is in the table) is identified by /v1/models.
	if f := runSlotModelIdentity(ctx, mk("vllm", "served-name")); f.Severity != SeverityOK {
		t.Fatalf("matching id: %s %q", f.Severity, f.Summary)
	}
	if f := runSlotModelIdentity(ctx, mk("vllm", "other-name")); f.Severity != SeverityCrit {
		t.Fatalf("differing id: %s %q", f.Severity, f.Summary)
	}
	// llama.cpp is unchanged: no /props alias means nothing to compare, and /v1/models is never consulted.
	if f := runSlotModelIdentity(ctx, mk("", "other-name")); !strings.HasPrefix(f.Summary, "skipped:") {
		t.Fatalf("llama.cpp without alias: %s %q", f.Severity, f.Summary)
	}
	// An unreachable engine is not a mismatch.
	env := mk("vllm", "served-name")
	srv.Close()
	if f := runSlotModelIdentity(ctx, env); f.Severity == SeverityCrit {
		t.Fatalf("unreachable engine reported crit: %q", f.Summary)
	}
}

func TestLaunchChecksRegistered(t *testing.T) {
	want := map[string]bool{"slot_launch_matches_process": true, "slot_launch_matches_catalog": true, "launcher_matches_embedded": true, "radiance_host_prereqs": true}
	for _, c := range registry {
		delete(want, c.ID)
	}
	if len(want) != 0 {
		t.Errorf("not registered: %v", want)
	}
	for _, c := range registry {
		if c.ID == "radiance_host_prereqs" && c.Skip == nil {
			t.Error("radiance_host_prereqs must carry a Skip so hosts without Radiance get no finding")
		}
	}
	seen := map[string]bool{}
	for _, c := range registry {
		if seen[c.ID] {
			t.Errorf("duplicate check id %s", c.ID)
		}
		seen[c.ID] = true
	}
}
