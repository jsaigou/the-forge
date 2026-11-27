// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeConfigCat answers only what ConfigWrites asks of the catalog; any other call panics on the nil embedded interface, which is the point.
type fakeConfigCat struct {
	store.Catalog
	configs   map[int64]store.Config
	artifacts map[int64]store.Artifact
	variants  map[int64]bool
	engines   []store.Engine
	builds    map[int64]bool
	tiers     map[int64]bool
	nextID    int64
	failRead  string // "variant" | "artifact" | "engines" | "build" | "name": that read fails with a store error
}

var errCat = errors.New("disk I/O error")

func nf(what string, id int64) error { return fmt.Errorf("%w: %s %d", store.ErrNotFound, what, id) }

func (f *fakeConfigCat) ConfigByName(_ context.Context, name string) (store.Config, error) {
	if f.failRead == "name" {
		return store.Config{}, errCat
	}
	for _, c := range f.configs {
		if c.Name == name {
			return c, nil
		}
	}
	return store.Config{}, nf("config", 0)
}
func (f *fakeConfigCat) GetVariant(_ context.Context, id int64) (store.Variant, error) {
	if f.failRead == "variant" {
		return store.Variant{}, errCat
	}
	if f.variants[id] {
		return store.Variant{ID: id}, nil
	}
	return store.Variant{}, nf("variant", id)
}
func (f *fakeConfigCat) GetArtifact(_ context.Context, id int64) (store.Artifact, error) {
	if f.failRead == "artifact" {
		return store.Artifact{}, errCat
	}
	if a, ok := f.artifacts[id]; ok {
		return a, nil
	}
	return store.Artifact{}, nf("artifact", id)
}
func (f *fakeConfigCat) ListEngines(context.Context) ([]store.Engine, error) {
	if f.failRead == "engines" {
		return nil, errCat
	}
	return f.engines, nil
}
func (f *fakeConfigCat) GetBuild(_ context.Context, id int64) (store.Build, error) {
	if f.failRead == "build" {
		return store.Build{}, errCat
	}
	if f.builds[id] {
		return store.Build{ID: id}, nil
	}
	return store.Build{}, nf("build", id)
}
func (f *fakeConfigCat) GetCapabilityTier(_ context.Context, id int64) (store.CapabilityTier, error) {
	if f.tiers[id] {
		return store.CapabilityTier{ID: id}, nil
	}
	return store.CapabilityTier{}, nf("tier", id)
}
func (f *fakeConfigCat) CreateConfig(_ context.Context, c store.Config) (int64, error) {
	f.nextID++
	c.ID = f.nextID
	f.configs[c.ID] = c
	return c.ID, nil
}
func (f *fakeConfigCat) UpdateConfig(_ context.Context, c store.Config) error {
	if _, ok := f.configs[c.ID]; !ok {
		return nf("config", c.ID)
	}
	f.configs[c.ID] = c
	return nil
}
func (f *fakeConfigCat) GetConfig(_ context.Context, id int64) (store.Config, error) {
	if c, ok := f.configs[id]; ok {
		return c, nil
	}
	return store.Config{}, nf("config", id)
}
func (f *fakeConfigCat) DeleteConfig(_ context.Context, id int64) error {
	if _, ok := f.configs[id]; !ok {
		return nf("config", id)
	}
	delete(f.configs, id)
	return nil
}

func newFakeConfigCat() *fakeConfigCat {
	return &fakeConfigCat{
		configs:   map[int64]store.Config{},
		artifacts: map[int64]store.Artifact{1: {ID: 1, ArtifactType: "weight"}, 2: {ID: 2, ArtifactType: "mmproj"}},
		variants:  map[int64]bool{1: true},
		engines:   []store.Engine{{ID: 1}},
		builds:    map[int64]bool{1: true},
		tiers:     map[int64]bool{1: true},
	}
}

func validConfig(name string) store.Config {
	return store.Config{Name: name, VariantID: 1, WeightArtifactID: 1, EngineID: 1, BuildID: 1}
}

func wantInvalid(t *testing.T, err error, field string) *store.ConfigInvalid {
	t.Helper()
	var inv *store.ConfigInvalid
	if !errors.As(err, &inv) || inv.Fields[field] == "" {
		t.Fatalf("want an invalid-config error on %q, got %v", field, err)
	}
	return inv
}

func TestConfigWrites_CreateValidatesSavesAndAnnouncesTheChange(t *testing.T) {
	cat, ctx, changed := newFakeConfigCat(), context.Background(), 0
	w := store.ConfigWrites{Cat: cat, Changed: func() { changed++ }}

	got, err := w.Create(ctx, validConfig("qwen36"))
	if err != nil || got.ID == 0 || got.Name != "qwen36" {
		t.Fatalf("create: %+v %v", got, err)
	}
	if changed != 1 {
		t.Errorf("the change hook ran %d times, want 1", changed)
	}

	for name, tc := range map[string]struct {
		mutate func(*store.Config)
		field  string
		code   string
	}{
		"bad name":            {func(c *store.Config) { c.Name = "Not Valid" }, "name", ""},
		"duplicate name":      {func(c *store.Config) { c.Name = "qwen36" }, "name", "already_exists"},
		"no variant":          {func(c *store.Config) { c.VariantID = 0 }, "variant_id", "required"},
		"unknown variant":     {func(c *store.Config) { c.VariantID = 9 }, "variant_id", ""},
		"unknown weight":      {func(c *store.Config) { c.WeightArtifactID = 9 }, "weight_artifact_id", ""},
		"weight is an mmproj": {func(c *store.Config) { c.WeightArtifactID = 2 }, "weight_artifact_id", ""},
		"unknown engine":      {func(c *store.Config) { c.EngineID = 9 }, "engine_id", ""},
		"no build":            {func(c *store.Config) { c.BuildID = 0 }, "build_id", "required"},
		"unknown build":       {func(c *store.Config) { c.BuildID = 9 }, "build_id", ""},
		"mmproj is a weight":  {func(c *store.Config) { c.MMProjArtifactID = 1 }, "mmproj_artifact_id", ""},
		"unknown tier":        {func(c *store.Config) { c.CapabilityTierID = 9 }, "capability_tier_id", ""},
		"negative context":    {func(c *store.Config) { c.NCtx = -1 }, "n_ctx", "must_be_non_negative_integer"},
		"bad status":          {func(c *store.Config) { c.Status = "great" }, "status", ""},
		"bad effort":          {func(c *store.Config) { c.ReasoningEffortDefault = "max" }, "reasoning_effort_default", ""},
		"bad modality": {func(c *store.Config) {
			m := []string{"smell"}
			c.Modalities = &m
		}, "modalities", ""},
		"bad output modality": {func(c *store.Config) {
			m := []string{"telepathy"}
			c.OutputModalities = &m
		}, "output_modalities", ""},
	} {
		c := validConfig("other")
		tc.mutate(&c)
		inv := wantInvalid(t, func() error { _, err := w.Create(ctx, c); return err }(), tc.field)
		if tc.code != "" && inv.Codes[tc.field] != tc.code {
			t.Errorf("%s: code %q, want %q", name, inv.Codes[tc.field], tc.code)
		}
	}
	if changed != 1 {
		t.Errorf("a refused write announced a change (%d)", changed)
	}
}

func TestConfigWrites_UpdateDoesNotCollideWithItself(t *testing.T) {
	cat, ctx := newFakeConfigCat(), context.Background()
	w := store.ConfigWrites{Cat: cat}
	a, _ := w.Create(ctx, validConfig("a"))
	b, _ := w.Create(ctx, validConfig("b"))

	a.NCtx = 4096
	if got, err := w.Update(ctx, a); err != nil || got.NCtx != 4096 {
		t.Fatalf("update in place: %+v %v", got, err)
	}
	b.Name = "a"
	wantInvalid(t, func() error { _, err := w.Update(ctx, b); return err }(), "name")
	// An update is validated like a create: a bad field refuses it and leaves the stored Config as it was.
	bad := a
	bad.Status = "great"
	wantInvalid(t, func() error { _, err := w.Update(ctx, bad); return err }(), "status")
	if cat.configs[a.ID].Status == "great" {
		t.Error("a refused update changed the stored Config")
	}
	missing := validConfig("ghost")
	missing.ID = 99
	if _, err := w.Update(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("updating a stranger: %v", err)
	}
}

// A failed read is not a passed check: every lookup the rules make must surface its error instead of reading as "valid" or "does not exist".
func TestConfigWrites_StoreFailureIsNotAVerdict(t *testing.T) {
	for _, read := range []string{"name", "variant", "artifact", "engines", "build"} {
		cat := newFakeConfigCat()
		cat.failRead = read
		c := validConfig("x")
		c.MMProjArtifactID = 2
		_, err := store.ConfigWrites{Cat: cat}.Create(context.Background(), c)
		var inv *store.ConfigInvalid
		if err == nil || errors.As(err, &inv) {
			t.Errorf("%s read failing: %v, want the store error", read, err)
		}
		if len(cat.configs) != 0 {
			t.Errorf("%s read failing: a Config was saved anyway", read)
		}
	}
}

func TestConfigWrites_DeleteRefusesALoadedConfigUnlessForced(t *testing.T) {
	cat, ctx, changed := newFakeConfigCat(), context.Background(), 0
	w := store.ConfigWrites{Cat: cat, Changed: func() { changed++ }, Loaded: func(n string) bool { return n == "live" }}
	live, _ := w.Create(ctx, validConfig("live"))
	idle, _ := w.Create(ctx, validConfig("idle"))
	changed = 0

	if c, err := w.Delete(ctx, live.ID, false); !errors.Is(err, store.ErrConfigLoaded) || c.Name != "live" {
		t.Fatalf("loaded without force: %+v %v", c, err)
	}
	if _, ok := cat.configs[live.ID]; !ok || changed != 0 {
		t.Fatal("a refused delete changed something")
	}
	if _, err := w.Delete(ctx, live.ID, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if _, err := w.Delete(ctx, idle.ID, false); err != nil || changed != 2 {
		t.Fatalf("idle: %v changed=%d", err, changed)
	}
	if _, err := w.Delete(ctx, 99, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a stranger: %v", err)
	}
}

func TestConfigWrites_SetBuildChangesOnlyTheBuild(t *testing.T) {
	cat, ctx, changed := newFakeConfigCat(), context.Background(), 0
	cat.builds[2] = true
	w := store.ConfigWrites{Cat: cat, Changed: func() { changed++ }}
	created, err := w.Create(ctx, validConfig("qwen36"))
	if err != nil {
		t.Fatal(err)
	}
	changed = 0

	// An operator edits the Config after a long-running writer read it: SetBuild must keep that edit.
	edited := created
	edited.NCtx, edited.Visibility = 65536, "hidden"
	if _, err := w.Update(ctx, edited); err != nil {
		t.Fatal(err)
	}
	changed = 0

	got, err := w.SetBuild(ctx, created.ID, 2)
	if err != nil || got.BuildID != 2 {
		t.Fatalf("set build: %+v %v", got, err)
	}
	if stored := cat.configs[created.ID]; stored.BuildID != 2 || stored.NCtx != 65536 || stored.Visibility != "hidden" {
		t.Errorf("SetBuild wrote more than the build, or lost the interim edit: %+v", stored)
	}
	if changed != 1 {
		t.Errorf("the change hook ran %d times, want 1", changed)
	}
	// Already pointing there: nothing is written and nothing is announced.
	if _, err := w.SetBuild(ctx, created.ID, 2); err != nil || changed != 1 {
		t.Errorf("no-op SetBuild: %v, changed=%d", err, changed)
	}
	// A Build that does not exist, a missing id, an unknown Config: refused, nothing changed.
	for name, tc := range map[string]struct {
		id, build int64
		invalid   bool
	}{"unknown build": {created.ID, 9, true}, "no build": {created.ID, 0, true}, "unknown config": {99, 1, false}} {
		_, err := w.SetBuild(ctx, tc.id, tc.build)
		var inv *store.ConfigInvalid
		if err == nil || errors.As(err, &inv) != tc.invalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	if cat.configs[created.ID].BuildID != 2 || changed != 1 {
		t.Error("a refused SetBuild changed something")
	}
	// A store failure reading the Build is an error, not "does not exist".
	cat.failRead = "build"
	if _, err := w.SetBuild(ctx, created.ID, 1); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Errorf("build read failing: %v", err)
	}
	var inv *store.ConfigInvalid
	if _, err := w.SetBuild(ctx, created.ID, 1); errors.As(err, &inv) {
		t.Error("a failed build read must not read as an invalid build")
	}
}
