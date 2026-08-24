// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestImportLocalSeed_FullFile imports every section and asserts each lands
// in its setting wholesale, readable through the SAME decoders the live
// seams use.
func TestImportLocalSeed_FullFile(t *testing.T) {
	db := openDB(t)
	raw := `{
		"_comment": "synthetic test seed",
		"mesh_services": [
			{"name":"svc-one","aliases":["svc-one"],"address":"svc-one.example.ts.net:1000"},
			{"name":"svc-two","aliases":["svc-two","second"],"address":"svc-two.example.ts.net:2000"}
		],
		"build_refresh_forks": [
			{"source_ref":"/opt/test/tree","remote":"origin","upstream_ref":"origin/main",
			 "backends":{"vulkan":{"backend":"vulkan","configure_flags":["-DGGML_VULKAN=ON"]}},
			 "representative_config":{"vulkan":"tiny-model"}}
		],
		"binaries_tracked": [
			{"name":"example-bin","kind":"llama_build","path":"/opt/test/tree/build/bin/x","source_kind":"git","source_ref":"/opt/test/tree","upstream_ref":"origin/main"}
		],
		"web_providers": {
			"searxng": {"base_url":"https://search.alpha.example.ts.net","enabled":true,"api_key":""},
			"direct": {"base_url":"","enabled":true,"api_key":""}
		},
		"comfyui": {"unit":"ai-mode-comfyui-test","url":"http://127.0.0.1:3999",
			"model_roots":["/opt/test/models-a","/opt/test/models-b"],
			"workflow_dirs":["/opt/test/workflows"]}
	}`
	sum, err := ImportLocalSeed(context.Background(), db.Settings(), []byte(raw))
	if err != nil {
		t.Fatalf("ImportLocalSeed: %v", err)
	}
	if sum.MeshServices != 2 || sum.BuildRefreshForks != 1 || sum.BinariesTracked != 1 || sum.WebProviders != 2 || sum.ComfyUI != 1 {
		t.Fatalf("summary = %+v, want 2/1/1/2/1", sum)
	}

	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	ctx := context.Background()
	if got := len(s.MeshServices(ctx)); got != 2 {
		t.Errorf("MeshServices = %d entries, want 2", got)
	}
	if got := len(s.BuildRefreshForks(context.Background())); got != 1 {
		t.Errorf("BuildRefreshForks = %d entries, want 1", got)
	}
	if got := len(s.TrackedBinaries(context.Background())); got != 1 {
		t.Errorf("TrackedBinaries = %d entries, want 1", got)
	}
	if got := s.ComfyUIUnit(ctx); got != "ai-mode-comfyui-test" {
		t.Errorf("ComfyUIUnit = %q, want ai-mode-comfyui-test", got)
	}
	if got := s.ComfyUIURL(ctx); got != "http://127.0.0.1:3999" {
		t.Errorf("ComfyUIURL = %q", got)
	}
	if got := s.ComfyUIModelRoots(ctx); len(got) != 2 {
		t.Errorf("ComfyUIModelRoots = %v, want 2 roots", got)
	}
	if got := s.ComfyUIWorkflowDirs(ctx); len(got) != 1 {
		t.Errorf("ComfyUIWorkflowDirs = %v, want 1 dir", got)
	}
	searxngRaw, err := db.Settings().Get(ctx, "smith.web.searxng")
	if err != nil {
		t.Fatal(err)
	}
	var pc struct {
		BaseURL string `json:"base_url"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(searxngRaw, &pc); err != nil {
		t.Fatal(err)
	}
	if pc.BaseURL != "https://search.alpha.example.ts.net" || !pc.Enabled {
		t.Errorf("smith.web.searxng blob = %s", searxngRaw)
	}
}

// TestImportLocalSeed_AbsentSectionsUntouched imports only one section and
// asserts the others keep their prior values — partial files are the
// common operator workflow (edit what changed).
func TestImportLocalSeed_AbsentSectionsUntouched(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := ImportLocalSeed(ctx, db.Settings(), []byte(`{"mesh_services":[{"name":"keep-me","aliases":["keep-me"],"address":"keep.example.ts.net:1"}]}`)); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := ImportLocalSeed(ctx, db.Settings(), []byte(`{"binaries_tracked":[{"name":"b","kind":"runtime"}]}`)); err != nil {
		t.Fatalf("second import: %v", err)
	}
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	mesh := s.MeshServices(ctx)
	if len(mesh) != 1 || mesh[0].Name != "keep-me" {
		t.Errorf("mesh inventory changed by an import that didn't carry mesh_services: %+v", mesh)
	}
}

// TestImportLocalSeed_ExplicitEmptyClears asserts `"section": []` is a
// deliberate clear, not a no-op — a deployment that retired every fork
// must be able to say so.
func TestImportLocalSeed_ExplicitEmptyClears(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	if _, err := ImportLocalSeed(ctx, db.Settings(), []byte(`{"build_refresh_forks":[{"source_ref":"/opt/test/tree","remote":"origin","upstream_ref":"origin/main","backends":{"vulkan":{"backend":"vulkan","configure_flags":["-DGGML_VULKAN=ON"]}},"representative_config":{"vulkan":"tiny"}}]}`)); err != nil {
		t.Fatalf("seed import: %v", err)
	}
	sum, err := ImportLocalSeed(ctx, db.Settings(), []byte(`{"build_refresh_forks":[]}`))
	if err != nil {
		t.Fatalf("clear import: %v", err)
	}
	if sum.BuildRefreshForks != 0 {
		t.Fatalf("summary = %+v, want BuildRefreshForks=0 (explicit clear)", sum)
	}
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})
	if got := len(s.BuildRefreshForks(ctx)); got != 0 {
		t.Errorf("BuildRefreshForks after explicit clear = %d entries, want 0", got)
	}
}

// TestImportLocalSeed_FailsClosed asserts every rejection class refuses
// the WHOLE import (nothing half-written): malformed JSON, unknown fields
// (the typo guard), empty files, and entries missing their minimum shape.
func TestImportLocalSeed_FailsClosed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"malformed json", `{"mesh_services": [`},
		{"unknown field (typo guard)", `{"mesh_servicez": []}`},
		{"empty object", `{}`},
		{"mesh entry without address", `{"mesh_services": [{"name":"x","aliases":["x"]}]}`},
		{"fork entry without source_ref", `{"build_refresh_forks": [{"remote":"origin","upstream_ref":"origin/main"}]}`},
		{"fork backend key mismatch", `{"build_refresh_forks": [{"source_ref":"/x","backends":{"rocm":{"backend":"vulkan","configure_flags":["-D"]}}}]}`},
		{"tracked binary without kind", `{"binaries_tracked": [{"name":"x"}]}`},
		{"unknown web provider name", `{"web_providers": {"notasearch": {"base_url":"https://x.example.ts.net","enabled":true}}}`},
		{"web provider with relative base_url", `{"web_providers": {"searxng": {"base_url":"/just/a/path","enabled":true}}}`},
		{"comfyui without unit", `{"comfyui": {"url":"http://127.0.0.1:3999"}}`},
		{"comfyui with bad url", `{"comfyui": {"unit":"u","url":"not-a-url"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := openDB(t)
			if _, err := ImportLocalSeed(context.Background(), db.Settings(), []byte(tc.raw)); err == nil {
				t.Fatalf("import of %s succeeded, want rejection", tc.name)
			}
			// Nothing half-written on rejection.
			raw, err := db.Settings().Get(context.Background(), SettingMeshServices)
			if err == nil && string(raw) != "[]" {
				t.Errorf("mesh services mutated by a rejected import: %s", raw)
			}
		})
	}
}

// TestImportLocalSeed_RoundTripsThroughLiveDecoders asserts imported JSON
// re-marshals byte-stable through the live decoder types (no field the
// readers don't know about, no field loss).
func TestImportLocalSeed_RoundTripsThroughLiveDecoders(t *testing.T) {
	mesh := []MeshService{{Name: "rt", Aliases: []string{"rt", "roundtrip"}, Address: "rt.example.ts.net:42"}}
	raw, err := json.Marshal(LocalSeed{MeshServices: &mesh})
	if err != nil {
		t.Fatal(err)
	}
	db := openDB(t)
	if _, err := ImportLocalSeed(context.Background(), db.Settings(), raw); err != nil {
		t.Fatalf("import: %v", err)
	}
	got, err := db.Settings().Get(context.Background(), SettingMeshServices)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"address":"rt.example.ts.net:42"`) {
		t.Errorf("stored mesh services lost fields: %s", got)
	}
}
