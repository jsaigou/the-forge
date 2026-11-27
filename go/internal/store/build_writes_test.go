// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeBuildCat answers only what BuildWrites asks of the catalog.
type fakeBuildCat struct {
	store.Catalog
	builds   map[int64]store.Build
	configs  []store.Config
	engines  []store.Engine
	nextID   int64
	failRead string // "engines" | "builds" | "configs"
}

func (f *fakeBuildCat) ListEngines(context.Context) ([]store.Engine, error) {
	if f.failRead == "engines" {
		return nil, errCat
	}
	return f.engines, nil
}
func (f *fakeBuildCat) ListBuilds(context.Context) ([]store.Build, error) {
	if f.failRead == "builds" {
		return nil, errCat
	}
	var out []store.Build
	for _, b := range f.builds {
		out = append(out, b)
	}
	return out, nil
}
func (f *fakeBuildCat) ListConfigs(context.Context) ([]store.Config, error) {
	if f.failRead == "configs" {
		return nil, errCat
	}
	return f.configs, nil
}
func (f *fakeBuildCat) GetBuild(_ context.Context, id int64) (store.Build, error) {
	if b, ok := f.builds[id]; ok {
		return b, nil
	}
	return store.Build{}, nf("build", id)
}
func (f *fakeBuildCat) CreateBuild(_ context.Context, b store.Build) (int64, error) {
	f.nextID++
	b.ID = f.nextID
	f.builds[b.ID] = b
	return b.ID, nil
}
func (f *fakeBuildCat) UpdateBuild(_ context.Context, b store.Build) error {
	if _, ok := f.builds[b.ID]; !ok {
		return nf("build", b.ID)
	}
	f.builds[b.ID] = b
	return nil
}
func (f *fakeBuildCat) DeleteBuild(_ context.Context, id int64) error {
	if _, ok := f.builds[id]; !ok {
		return nf("build", id)
	}
	delete(f.builds, id)
	return nil
}

func newBuildWrites() (store.BuildWrites, *fakeBuildCat, *int) {
	cat := &fakeBuildCat{builds: map[int64]store.Build{}, engines: []store.Engine{{ID: 1}}}
	changed := 0
	return store.BuildWrites{Cat: cat, Changed: func() { changed++ },
		CheckBinary: func(_ context.Context, p string) (string, error) {
			if strings.HasPrefix(p, "/opt/") {
				return "", nil
			}
			return "must be under a build root", nil
		}}, cat, &changed
}

func goodBuild(name string) store.Build {
	return store.Build{EngineID: 1, Name: name, BinaryPath: "/opt/llama/bin/llama-server", Backend: "rocm"}
}

func TestBuildWrites_ValidateAndCreate(t *testing.T) {
	w, cat, changed := newBuildWrites()
	ctx := context.Background()
	got, err := w.Create(ctx, goodBuild("rocm-a"))
	if err != nil || got.ID == 0 || *changed != 1 {
		t.Fatalf("create: %+v %v changed=%d", got, err, *changed)
	}
	if _, err := w.Create(ctx, store.Build{EngineID: 1, Name: "vllm-local", Backend: "vllm"}); err != nil {
		t.Errorf("a vllm build needs no binary: %v", err)
	}
	if _, err := w.Create(ctx, store.Build{EngineID: 1, Name: "old", Retired: true, Backend: "rocm"}); err != nil {
		t.Errorf("a retired build needs no binary: %v", err)
	}
	// A legacy prefix is stripped and a retired build is stored without a path.
	if r, err := w.Create(ctx, store.Build{EngineID: 1, Name: store.RetiredBuildPrefix + "older", Retired: true, BinaryPath: "/opt/x/y", Backend: "rocm"}); err != nil || r.Name != "older" || r.BinaryPath != "" {
		t.Errorf("normalizing: %+v %v", r, err)
	}
	*changed = 0

	for name, tc := range map[string]struct {
		mutate func(*store.Build)
		field  string
		code   string
	}{
		"no name":        {func(b *store.Build) { b.Name = " " }, "name", "required"},
		"duplicate live": {func(b *store.Build) { b.Name = "rocm-a" }, "name", "already_exists"},
		"bad backend":    {func(b *store.Build) { b.Backend = "cuda" }, "backend", ""},
		"no engine":      {func(b *store.Build) { b.EngineID = 0 }, "engine_id", "required"},
		"unknown engine": {func(b *store.Build) { b.EngineID = 9 }, "engine_id", ""},
		"no binary":      {func(b *store.Build) { b.BinaryPath = "" }, "binary_path", "required"},
		"binary refused": {func(b *store.Build) { b.BinaryPath = "/tmp/evil" }, "binary_path", ""},
	} {
		b := goodBuild("fresh")
		tc.mutate(&b)
		var inv *store.Invalid
		_, err := w.Create(ctx, b)
		if !errors.As(err, &inv) || inv.Fields[tc.field] == "" || (tc.code != "" && inv.Codes[tc.field] != tc.code) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A retired row of the same name does not block a new live one.
	cat.builds[50] = store.Build{ID: 50, EngineID: 1, Name: "rocm-b", Retired: true}
	if _, err := w.Create(ctx, goodBuild("rocm-b")); err != nil {
		t.Errorf("a retired duplicate must not block: %v", err)
	}
	if *changed != 1 {
		t.Errorf("refused writes announced a change: %d", *changed)
	}
}

// Every read the rules depend on, failing, must surface as an error and write nothing. These all used to fail open.
func TestBuildWrites_StoreFailureIsNotAVerdict(t *testing.T) {
	for _, read := range []string{"engines", "builds"} {
		w, cat, _ := newBuildWrites()
		cat.failRead = read
		_, err := w.Create(context.Background(), goodBuild("x"))
		var inv *store.Invalid
		if err == nil || errors.As(err, &inv) || len(cat.builds) != 0 {
			t.Errorf("%s read failing: %v (builds written: %d)", read, err, len(cat.builds))
		}
	}
	w, _, _ := newBuildWrites()
	w.CheckBinary = func(context.Context, string) (string, error) { return "", errCat }
	if _, err := w.Create(context.Background(), goodBuild("x")); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("unreadable build roots: %v", err)
	}
}

func TestBuildWrites_RetireAndDeleteGuards(t *testing.T) {
	w, cat, changed := newBuildWrites()
	ctx := context.Background()
	live, _ := w.Create(ctx, goodBuild("live"))
	cat.configs = []store.Config{{ID: 1, Name: "qwen36", BuildID: live.ID}}
	*changed = 0

	// A live build in use cannot be retired by an update, or by Retire; a failing Config list refuses it too.
	retire := goodBuild("live")
	retire.Retired = true
	var inUse *store.BuildInUseError
	if _, _, err := w.Update(ctx, live.ID, retire); !errors.As(err, &inUse) || inUse.Configs[0] != "qwen36" {
		t.Errorf("update-retire while in use: %v", err)
	}
	if _, err := w.Retire(ctx, live.ID, " — x"); !errors.As(err, &inUse) {
		t.Errorf("Retire while in use: %v", err)
	}
	cat.failRead = "configs"
	if _, err := w.Retire(ctx, live.ID, ""); err == nil || errors.As(err, &inUse) || cat.builds[live.ID].Retired {
		t.Errorf("Retire with the Config list failing must refuse with that error: %v", err)
	}
	cat.failRead = ""
	if *changed != 0 {
		t.Errorf("refused writes announced a change")
	}

	// Free the build, then Retire changes only the retired flag, path and reason, and is idempotent.
	cat.configs = nil
	got, err := w.Retire(ctx, live.ID, " — retired by cleanup")
	if err != nil || !got.Retired || got.BinaryPath != "" || got.Reason != " — retired by cleanup" || got.Name != "live" || got.Backend != "rocm" {
		t.Fatalf("retire: %+v %v", got, err)
	}
	if _, err := w.Retire(ctx, live.ID, " — again"); err != nil || cat.builds[live.ID].Reason != " — retired by cleanup" || *changed != 1 {
		t.Errorf("second Retire should be a no-op: %v reason=%q changed=%d", err, cat.builds[live.ID].Reason, *changed)
	}

	// Delete: a live build is refused; a retired one in use is refused; a retired free one goes.
	other, _ := w.Create(ctx, goodBuild("other"))
	var notRetired *store.BuildNotRetiredError
	if _, err := w.Delete(ctx, other.ID); !errors.As(err, &notRetired) {
		t.Errorf("delete a live build: %v", err)
	}
	cat.configs = []store.Config{{Name: "qwen36", BuildID: live.ID}}
	if _, err := w.Delete(ctx, live.ID); !errors.As(err, &inUse) {
		t.Errorf("delete a retired build still in use: %v", err)
	}
	cat.configs = nil
	if d, err := w.Delete(ctx, live.ID); err != nil || d.Name != "live" {
		t.Errorf("delete: %+v %v", d, err)
	}
	if _, err := w.Delete(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete a stranger: %v", err)
	}
}
