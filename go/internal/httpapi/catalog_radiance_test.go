// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/launch"
	"github.com/jsaigou/the-forge/internal/store"
)

func radianceBuildBody(f *buildsFixture, engine int64, name, bin string, env map[string]string) string {
	m := map[string]any{"engine_id": engine, "name": name, "binary_path": bin, "backend": "rocm"}
	if env != nil {
		m["launch_env"] = env
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// The build write API validates launch_env: allowed roots (build roots plus catalog.launch_env_roots), names, secrets, the engine kind, and it is
// a full replace on PUT.
func TestBuildLaunchEnvAPI(t *testing.T) {
	f := newBuildsFixture(t)
	rad, err := f.db.Catalog().EngineByName(context.Background(), "Radiance")
	if err != nil {
		t.Fatal(err)
	}
	llama, err := f.db.Catalog().EngineByName(context.Background(), "llama.cpp")
	if err != nil {
		t.Fatal(err)
	}
	bin := f.binary(t, "radiance/bin/radiance", 0o755)
	home := f.root + "/radiance/radiance_home"
	rocm := t.TempDir() // outside the build roots, like /opt/forge/rocm-therock-10.1
	env := map[string]string{"RADIANCE_HOME": home, "RADIANCE_KERNELS": "libhalo", "LD_LIBRARY_PATH": rocm + "/lib"}

	// The ROCm tree is outside catalog.build_roots: refused until it is named in catalog.launch_env_roots.
	code, body := f.send(t, "POST", "/api/v1/catalog/builds", radianceBuildBody(f, rad.ID, "rad", bin, env))
	if code != 422 || !strings.Contains(body["fields"].(map[string]any)["launch_env"].(string), "LD_LIBRARY_PATH") {
		t.Fatalf("outside roots: %d %v", code, body)
	}
	roots, _ := json.Marshal([]string{rocm})
	if err := f.db.Settings().Set(context.Background(), "catalog.launch_env_roots", roots); err != nil {
		t.Fatal(err)
	}
	code, body = f.send(t, "POST", "/api/v1/catalog/builds", radianceBuildBody(f, rad.ID, "rad", bin, env))
	if code != 201 {
		t.Fatalf("with launch_env_roots: %d %v", code, body)
	}
	if got, _ := body["launch_env"].(map[string]any); got["RADIANCE_KERNELS"] != "libhalo" || len(got) != 3 {
		t.Errorf("response launch_env = %v", body["launch_env"])
	}
	id := idOf(body)
	if code, got := f.send(t, "GET", "/api/v1/catalog/builds/"+id, ""); code != 200 || got["launch_env"] == nil {
		t.Errorf("GET: %d %v", code, got)
	}

	// Rejections, each reported on launch_env.
	for name, bad := range map[string]map[string]string{
		"secret name":   {"HF_TOKEN": "x"},
		"lowercase":     {"home": "x"},
		"relative path": {"A": "./lib"},
		"newline":       {"A": "a\nb"},
		"forge prefix":  {"FORGE_A": "b"},
	} {
		c, b := f.send(t, "POST", "/api/v1/catalog/builds", radianceBuildBody(f, rad.ID, "bad-"+strings.ReplaceAll(name, " ", "-"), bin, bad))
		if c != 422 || b["fields"].(map[string]any)["launch_env"] == nil {
			t.Errorf("%s: %d %v", name, c, b)
		}
	}
	// A kind that ignores the environment refuses one.
	c, b := f.send(t, "POST", "/api/v1/catalog/builds", radianceBuildBody(f, llama.ID, "ll", bin, map[string]string{"A": "b"}))
	if c != 422 || !strings.Contains(b["fields"].(map[string]any)["launch_env"].(string), "not used") {
		t.Errorf("llama.cpp engine: %d %v", c, b)
	}
	// PUT is a full replace: omitting launch_env clears it.
	c, b = f.send(t, "PUT", "/api/v1/catalog/builds/"+id, radianceBuildBody(f, rad.ID, "rad", bin, nil))
	if c != 200 || b["launch_env"] != nil {
		t.Errorf("PUT without launch_env: %d %v", c, b)
	}
	// The validate endpoint applies the same rules.
	c, b = f.send(t, "POST", "/api/v1/catalog/builds/validate", radianceBuildBody(f, rad.ID, "v", bin, map[string]string{"X_SECRET": "y"}))
	if c != 422 {
		t.Errorf("validate: %d %v", c, b)
	}
}

// A safe_memory_bytes benchmark for a config of a NeedsCuratedMemory engine must record its tuple; a tuple must parse and belongs only to a
// config-scoped safe_memory_bytes figure.
func TestBenchmarkTupleAPI(t *testing.T) {
	f := newBuildsFixture(t)
	ctx := context.Background()
	rad, _ := f.db.Catalog().EngineByName(ctx, "Radiance")
	llama, _ := f.db.Catalog().EngineByName(ctx, "llama.cpp")
	radCfg, err := f.db.Catalog().CreateConfig(ctx, store.Config{Name: "rad-cfg", VariantID: f.pre.variantID, WeightArtifactID: f.pre.artifactID, EngineID: rad.ID, BuildID: f.pre.buildID, NCtx: 4096})
	if err != nil {
		t.Fatal(err)
	}
	llCfg, err := f.db.Catalog().CreateConfig(ctx, store.Config{Name: "ll-cfg", VariantID: f.pre.variantID, WeightArtifactID: f.pre.artifactID, EngineID: llama.ID, BuildID: f.pre.buildID, NCtx: 4096})
	if err != nil {
		t.Fatal(err)
	}
	post := func(m map[string]any) (int, map[string]any) {
		m["source"] = "self_measured"
		b, _ := json.Marshal(m)
		return f.send(t, "POST", "/api/v1/catalog/benchmarks", string(b))
	}
	tup := launch.NewMemoryTuple(f.pre.buildID, f.pre.variantID, f.pre.artifactID, 4096, []string{"--max-num-seqs", "4"}).Key()

	if c, b := post(map[string]any{"metric": "safe_memory_bytes", "value": "1", "subject_type": "config", "subject_id": radCfg}); c != 422 || b["fields"].(map[string]any)["tuple"] == nil {
		t.Errorf("radiance figure without tuple: %d %v", c, b)
	}
	c, b := post(map[string]any{"metric": "safe_memory_bytes", "value": "1", "subject_type": "config", "subject_id": radCfg, "tuple": tup})
	if c != 201 || b["tuple"] != tup {
		t.Fatalf("radiance figure with tuple: %d %v", c, b)
	}
	if c, b := post(map[string]any{"metric": "safe_memory_bytes", "value": "1", "subject_type": "config", "subject_id": radCfg, "tuple": `{"build_id":1}`}); c != 422 {
		t.Errorf("malformed tuple: %d %v", c, b)
	}
	if c, b := post(map[string]any{"metric": "decode_tps", "value": "1", "subject_type": "config", "subject_id": radCfg, "tuple": tup}); c != 422 {
		t.Errorf("tuple on another metric: %d %v", c, b)
	}
	if c, b := post(map[string]any{"metric": "safe_memory_bytes", "value": "1", "subject_type": "variant", "subject_id": f.pre.variantID, "tuple": tup}); c != 422 {
		t.Errorf("tuple on a variant figure: %d %v", c, b)
	}
	// A llama.cpp config's figure needs no tuple (unchanged behaviour).
	if c, b := post(map[string]any{"metric": "safe_memory_bytes", "value": "1", "subject_type": "config", "subject_id": llCfg}); c != 201 {
		t.Errorf("llama.cpp figure: %d %v", c, b)
	}
}
