// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/launch"
	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

var radIntegrationArgs = []string{"--tp", "1", "--kv-cache-dtype", "fp8", "--max-num-seqs", "4", "--num-speculative-tokens", "4"}

type radIDs struct{ build, variant, weight, goodCfg int64 }

// seedRadianceDB builds a catalog with a Radiance engine (from migration 0106), a build carrying launch_env, a variant, a .rad artifact and two
// configs: one valid and one missing --max-num-seqs. No benchmark is registered: that is the test's step.
func seedRadianceDB(t *testing.T) (string, radIDs) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cat := db.Catalog()
	if _, err := cat.CreateSlot(ctx, store.Slot{Name: "a1", Unit: "forge-a1", Port: 8080, Label: "A1", SortOrder: 1}); err != nil {
		t.Fatal(err)
	}
	eng, err := cat.EngineByName(ctx, "Radiance")
	if err != nil || eng.Kind != "radiance" {
		t.Fatalf("Radiance engine: %+v %v", eng, err)
	}
	format, err := cat.FormatByName(ctx, "rad")
	if err != nil {
		t.Fatal(err)
	}
	var ids radIDs
	mdl, _ := cat.CreateModel(ctx, store.Model{Name: "M"})
	if ids.variant, err = cat.CreateVariant(ctx, store.Variant{ModelID: mdl, Name: "FP8 (Radiance .rad)"}); err != nil {
		t.Fatal(err)
	}
	if ids.weight, err = cat.CreateArtifact(ctx, store.Artifact{VariantID: ids.variant, FormatID: format.ID, FilePath: "/models/m.rad", ArtifactType: "weight"}); err != nil {
		t.Fatal(err)
	}
	if ids.build, err = cat.CreateBuild(ctx, store.Build{EngineID: eng.ID, Name: "radiance-test", Backend: "rocm", BinaryPath: "/opt/radiance/src/build/bin/radiance",
		LaunchEnv: map[string]string{"RADIANCE_HOME": "/opt/radiance/src/build/radiance_home", "RADIANCE_KERNELS": "libhalo"}}); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, args []string) int64 {
		id, err := cat.CreateConfig(ctx, store.Config{Name: name, VariantID: ids.variant, WeightArtifactID: ids.weight, EngineID: eng.ID, BuildID: ids.build, NCtx: 262144, ExtraArgs: args})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	ids.goodCfg = mk("rad-good", radIntegrationArgs)
	mk("rad-no-seqs", []string{"--tp", "1", "--num-speculative-tokens", "4"})
	return path, ids
}

func TestRadianceLaunchCheck(t *testing.T) {
	path, _ := seedRadianceDB(t)
	before, _ := os.ReadFile(path)
	var out, errb bytes.Buffer
	if code := runLaunchCheck([]string{"-db", path}, &out, &errb); code != 1 {
		t.Fatalf("exit %d, want 1 (one config refused)\nout: %s\nerr: %s", code, out.String(), errb.String())
	}
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 {
			lines[f[0]] = l
		}
	}
	if l := lines["rad-good"]; !strings.Contains(l, "radiance") || !strings.HasSuffix(strings.TrimSpace(l), "OK") {
		t.Errorf("rad-good: %q", l)
	}
	if l := lines["rad-no-seqs"]; !strings.Contains(l, "--max-num-seqs is required") {
		t.Errorf("rad-no-seqs: %q", l)
	}
	if !strings.Contains(out.String(), "2 configs, 1 ok, 1 refused") {
		t.Errorf("summary: %s", out.String())
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("launch-check changed the database")
	}
}

type idleSys struct{}

func (idleSys) Start(context.Context, string) error { return nil }
func (idleSys) Stop(context.Context, string) error  { return nil }
func (idleSys) State(context.Context, string) (collector.UnitState, error) {
	return collector.UnitState{ActiveState: "inactive", SubState: "dead"}, nil
}
func (idleSys) MainPID(context.Context, string) (uint32, error) { return 0, nil }

// Catalog to row to fit gate: the merged config carries the build's launch_env into the slot_launches row, and the fit gate refuses the config until a
// benchmark that records its tuple is registered, then admits it; a benchmark at another tuple does not count.
func TestRadianceCatalogToRowAndFitGate(t *testing.T) {
	path, ids := seedRadianceDB(t)
	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	ctx := context.Background()
	cfg, err := config.LoadFromStore(ctx, ro)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := newMergedConfigProvider(func() *config.Config { return cfg }, ro.Catalog()).buildMerged(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mode := merged.Modes["rad-good"]
	svc := mode.Services[0]
	if svc.EngineKind != "radiance" || svc.BuildID != ids.build || svc.LaunchEnv["RADIANCE_KERNELS"] != "libhalo" {
		t.Fatalf("service = %+v", svc)
	}

	row, err := engine.BuildSlotLaunchRow(cfg, "a1", "rad-good", mode.ConfigID, svc)
	if err != nil {
		t.Fatal(err)
	}
	if row.EngineKind != "radiance" || row.Exe != "/opt/radiance/src/build/bin/radiance" || row.Env["RADIANCE_HOME"] != "/opt/radiance/src/build/radiance_home" || len(row.Env) != 2 {
		t.Errorf("row exe/env = %s %v", row.Exe, row.Env)
	}
	if got := strings.Join(row.Argv, " "); got != "--model /models/m.rad --served-model-name rad-good --host 127.0.0.1 --port 8080 --max-model-len 262144 --tp 1 --kv-cache-dtype fp8 --max-num-seqs 4 --num-speculative-tokens 4" {
		t.Errorf("argv = %s", got)
	}

	// The fit gate over the real registry (CuratedMemory) and a real engine.Manager.
	getCfg := func() *config.Config { return merged }
	// A new registry per lookup: the real one caches its catalog snapshot for a few seconds, which would hide the benchmark written below.
	freshReg := func() registry.Registry { return registry.New(ro.Catalog(), getCfg, nil) }
	procRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(procRoot, "meminfo"), []byte("MemTotal: 125829120 kB\nMemAvailable: 104857600 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gpuRoot := filepath.Join(t.TempDir(), "card0", "device")
	if err := os.MkdirAll(gpuRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{"vendor": "0x1002", "mem_info_gtt_used": "10737418240", "mem_info_gtt_total": "128849018880"} {
		if err := os.WriteFile(filepath.Join(gpuRoot, f), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m, err := engine.NewManager(engine.Deps{
		Cfg: getCfg, Sys: idleSys{}, GPU: &collector.GPU{DRMRoot: filepath.Dir(filepath.Dir(gpuRoot))}, Proc: collector.Proc{Root: procRoot},
		WeightEstimateBytes: func(id int64) (int64, bool) { return freshReg().WeightEstimateBytes(id) }, Logf: t.Logf,
		CuratedFigures: func(id int64) []engine.CuratedFigure {
			var out []engine.CuratedFigure
			for _, f := range freshReg().CuratedMemory(id) {
				out = append(out, engine.CuratedFigure{Bytes: f.Bytes, Tuple: f.Tuple})
			}
			return out
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan := func() engine.Plan {
		t.Helper()
		p, err := m.FitPlan("rad-good")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := plan(); p.Fits || !strings.Contains(p.Message, "no measured memory figure") {
		t.Fatalf("without a benchmark: %+v", p)
	}

	// Register benchmarks through a writable handle: first at another tuple, then at this config's tuple.
	rw, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	other := launch.NewMemoryTuple(ids.build, ids.variant, ids.weight, 262144, []string{"--tp", "1", "--kv-cache-dtype", "fp8", "--max-num-seqs", "8", "--num-speculative-tokens", "4"})
	mine := launch.NewMemoryTuple(ids.build, ids.variant, ids.weight, 262144, radIntegrationArgs)
	add := func(tup launch.MemoryTuple) {
		t.Helper()
		if _, err := rw.Catalog().CreateBenchmark(ctx, store.Benchmark{Metric: "safe_memory_bytes", Value: "54117000000", Source: "self_measured",
			SubjectType: "config", SubjectID: ids.goodCfg, Tuple: tup.Key()}); err != nil {
			t.Fatal(err)
		}
	}
	add(other)
	if p := plan(); p.Fits || !strings.Contains(p.Message, "other tuples") {
		t.Fatalf("benchmark at another tuple: %+v", p)
	}
	add(mine)
	if p := plan(); !p.Fits || int64(p.NeedBytes) != 54117000000 {
		t.Fatalf("benchmark at this tuple: %+v", p)
	}
}
