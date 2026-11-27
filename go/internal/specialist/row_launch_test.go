// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// ADR-0029: the pool hosts a Radiance engine, whose launch is a slot_launches row run by `forge slot-exec`, not a llama-server command line.

var roots = []string{"/opt/forge/models"}

func haruSpec() LaunchSpec {
	return LaunchSpec{Row: true, Engine: "radiance", Bin: "/opt/radiance/strix-halo-haru/build/bin/radiance", Model: "/opt/forge/models/Haru-4B/haru-bf16.rad",
		Alias: "haru-4b", Backend: "rocm", Ctx: 14500,
		Args: []string{"--max-num-seqs", "2", "--num-speculative-tokens", "0", "--tp", "1", "--duplex-max-lanes", "2", "--duplex-max-ctx", "14500"}}
}

func (e *env) addDuplex(t *testing.T, name, config string) {
	t.Helper()
	if _, err := e.sp.CreateOccupant(context.Background(), store.Occupant{Name: name, Kind: KindCatalogConfig, Policy: store.PolicyOnDemand, Enabled: true,
		Functions: []string{store.FnDuplex}, Params: map[string]string{"config": config}}); err != nil {
		t.Fatal(err)
	}
}

func TestRowLaunch_ValidationRenderingAndTheLauncher(t *testing.T) {
	spec := haruSpec()
	if err := ValidateLaunchSpec(spec, roots); err != nil {
		t.Fatalf("a Radiance config was refused by the pool: %v", err)
	}
	bad := map[string]func(*LaunchSpec){
		"no engine kind":          func(s *LaunchSpec) { s.Engine = "" },
		"relative binary":         func(s *LaunchSpec) { s.Bin = "radiance" },
		"model outside the roots": func(s *LaunchSpec) { s.Model = "/etc/passwd" },
		"zero context":            func(s *LaunchSpec) { s.Ctx = 0 },
		"alias with a slash":      func(s *LaunchSpec) { s.Alias = "a/b" },
		"argument with newline":   func(s *LaunchSpec) { s.Args = append(s.Args, "a\nb") },
	}
	for name, mut := range bad {
		s := haruSpec()
		mut(&s)
		if err := ValidateLaunchSpec(s, roots); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The env file carries no engine knowledge, and there is no args file content: the row carries the launch.
	env := RenderCatalogEnv(store.Occupant{Name: "haru-4b"}, "s7", 8106, spec)
	if !strings.Contains(env, "FORGE_POOL_KIND=catalog-launch\n") || strings.Contains(env, "P_BIN") || strings.Contains(env, "P_MODEL") || strings.Contains(env, "radiance") {
		t.Errorf("env file: %q", env)
	}
	if RenderCatalogArgs(spec) != "" {
		t.Error("a row-launched engine has no args file")
	}
	// The launcher: the real start-pool.sh, in its dry run, runs exactly `forge slot-exec <slot>` and nothing else.
	cmd := exec.Command("bash", "../../../systemd/start-pool.sh", "s7")
	cmd.Env = append([]string{"PATH=/usr/bin:/bin:/usr/local/bin", "FORGE_POOL_DRYRUN=1"}, strings.Split(strings.TrimSpace(env), "\n")...)
	out, err := cmd.CombinedOutput()
	if _, ok := err.(*exec.ExitError); ok {
		t.Fatalf("launcher refused the catalog-launch kind: %s", out)
	} else if err != nil {
		t.Skipf("bash is not runnable here: %v", err)
	}
	if !strings.Contains(string(out), "/opt/forge/forge slot-exec s7") || strings.Count(strings.TrimSpace(string(out)), "\n") != 1 {
		t.Errorf("launcher dry run = %q, want cwd line and `/opt/forge/forge slot-exec s7`", out)
	}
	// An unknown kind is still refused, and the message names the new one.
	cmd = exec.Command("bash", "../../../systemd/start-pool.sh", "s7")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin", "FORGE_POOL_DRYRUN=1", "FORGE_POOL_KIND=catalog-nope", "FORGE_POOL_PORT=8106"}
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "catalog-launch") {
		t.Errorf("unknown kind: %v %q", err, out)
	}
	_ = filepath.Join
	_ = os.Stat
}

// The sequence that makes a row-launched start safe: the unit is down, then the armed row is written, then the unit starts; stopping disarms the row.
// orderedRows records what the slot's unit was doing at the moment a row was written or disarmed.
type orderedRows struct {
	SlotLaunchStore
	sys           *fakeSys
	unit          string
	putWhileUp    []bool
	disarmWhileUp []bool
}

func (o *orderedRows) Put(ctx context.Context, l store.SlotLaunch) (int64, error) {
	o.sys.mu.Lock()
	o.putWhileUp = append(o.putWhileUp, o.sys.active[o.unit])
	o.sys.mu.Unlock()
	return o.SlotLaunchStore.Put(ctx, l)
}

func (o *orderedRows) Disarm(ctx context.Context, slot string, gen int64) (bool, error) {
	o.sys.mu.Lock()
	o.disarmWhileUp = append(o.disarmWhileUp, o.sys.active[o.unit])
	o.sys.mu.Unlock()
	return o.SlotLaunchStore.Disarm(ctx, slot, gen)
}

func TestRowLaunch_RowIsWrittenBeforeTheUnitStartsAndDisarmedAfterItStops(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ord := &orderedRows{SlotLaunchStore: e.rows, sys: e.sys, unit: "forge-pool@s1"}
	e.m.d.SlotLaunches = ord
	e.cat.specs["haru"] = haruSpec()
	e.addDuplex(t, "haru-4b", "haru")
	e.health(8100, true)

	if _, err := e.m.Ensure(ctx, "haru-4b"); err != nil {
		t.Fatal(err)
	}
	if len(ord.putWhileUp) != 1 || ord.putWhileUp[0] {
		t.Fatalf("the row must be written exactly once, while the unit is down: %v", ord.putWhileUp)
	}
	row, err := e.rows.Get(ctx, "s1")
	if err != nil || !row.Armed || row.ConfigName != "haru" || row.EngineKind != "radiance" || row.Port != 8100 || row.WrittenBy != "specialist" {
		t.Fatalf("row after the start: %+v, %v", row, err)
	}
	env, _ := os.ReadFile(filepath.Join(e.dir, "s1.env"))
	if !strings.Contains(string(env), "FORGE_POOL_KIND=catalog-launch") {
		t.Errorf("env file: %s", env)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "s1.args")); err == nil {
		if b, _ := os.ReadFile(filepath.Join(e.dir, "s1.args")); len(b) != 0 {
			t.Errorf("an args file was written for a row-launched engine: %q", b)
		}
	}
	if err := e.m.Unload(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	row, err = e.rows.Get(ctx, "s1")
	if err != nil || row.Armed {
		t.Fatalf("the row must be disarmed once the unit is down: %+v, %v", row, err)
	}
	for i, up := range ord.disarmWhileUp {
		if up {
			t.Errorf("disarm %d happened while the unit was still up: a row must not be disarmed under a running engine", i)
		}
	}
	if len(ord.disarmWhileUp) == 0 {
		t.Error("the row was never disarmed through the store")
	}
}

func TestRowLaunch_AFailedRowWriteStartsNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.SlotLaunches = e.rows
	spec := haruSpec()
	e.cat.specs["haru"] = spec
	e.addDuplex(t, "haru-4b", "haru")
	e.health(8100, true)
	e.cat.rowErr = errors.New("the build is retired")
	var ue *UnavailableError
	if _, err := e.m.Ensure(ctx, "haru-4b"); !errors.As(err, &ue) || !strings.Contains(ue.Reason, "the build is retired") {
		t.Fatalf("a row that cannot be built: %v", err)
	}
	if len(e.sys.starts) != 0 {
		t.Errorf("the unit was started with no row to launch from: %v", e.sys.starts)
	}
	if _, err := e.rows.Get(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a row exists for a launch that was refused: %v", err)
	}
}

func TestRowLaunch_NoRowStoreIsASpecificRefusal(t *testing.T) {
	e := newEnv(t)
	e.cat.specs["haru"] = haruSpec()
	e.addDuplex(t, "haru-4b", "haru")
	e.health(8100, true)
	var ue *UnavailableError
	_, err := e.m.Ensure(context.Background(), "haru-4b")
	if !errors.As(err, &ue) || !strings.Contains(ue.Reason, "no row store") {
		t.Fatalf("without a row store: %v", err)
	}
	if len(e.sys.starts) != 0 {
		t.Errorf("a unit was started: %v", e.sys.starts)
	}
}

// A llama-server member is untouched by all of this: no row is written for it.
func TestRowLaunch_ALlamaMemberWritesNoRow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.SlotLaunches = e.rows
	e.cat.specs["d1-2b"] = d1Spec()
	e.addCatalog(t, "d1-member", "d1-2b", store.PolicyOnDemand, "")
	e.health(8100, true)
	if _, err := e.m.Ensure(ctx, "d1-member"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rows.Get(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a llama-server member got a launch row: %v", err)
	}
}

// The same lease that protects any pool member protects a Radiance one: a duplex session keeps it, because the engine itself reports no activity.
func TestRowLaunch_ADuplexLeaseResolvesThroughTheRowLaunchedMember(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.m.d.SlotLaunches = e.rows
	e.cat.specs["haru"] = haruSpec()
	e.addDuplex(t, "haru-4b", "haru")
	e.health(8100, true)
	tg, err := e.m.ResolveDuplex(ctx, "haru-4b")
	if err != nil || tg.Occupant != "haru-4b" || tg.LeaseID == "" || tg.BaseURL != "http://127.0.0.1:8100" {
		t.Fatalf("ResolveDuplex through a row-launched member: %+v, %v", tg, err)
	}
	if !e.m.RenewLease(tg.LeaseID) {
		t.Error("the lease did not renew")
	}
}
