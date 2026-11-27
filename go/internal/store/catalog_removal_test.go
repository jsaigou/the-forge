// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

type removalFixture struct {
	t    *testing.T
	ctx  context.Context
	db   *store.DB
	cat  store.Catalog
	root string
	w    store.CatalogRemovals
	fmt  int64
	eng  int64
}

func newRemovalFixture(t *testing.T) *removalFixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cat := db.Catalog()
	formats, err := cat.ListFormats(ctx)
	if err != nil || len(formats) == 0 {
		t.Fatalf("formats: %v", err)
	}
	engines, err := cat.ListEngines(ctx)
	if err != nil || len(engines) == 0 {
		t.Fatalf("engines: %v", err)
	}
	root := t.TempDir()
	f := &removalFixture{t: t, ctx: ctx, db: db, cat: cat, root: root, fmt: formats[0].ID, eng: engines[0].ID}
	f.w = store.CatalogRemovals{Cat: cat, ModelsDir: func() string { return root }}
	return f
}

func (f *removalFixture) must(id int64, err error) int64 {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *removalFixture) file(rel string, size int) string {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *removalFixture) model(name string) (modelID, variantID int64) {
	f.t.Helper()
	modelID = f.must(f.cat.CreateModel(f.ctx, store.Model{Name: name, Visibility: "visible"}))
	variantID = f.must(f.cat.CreateVariant(f.ctx, store.Variant{ModelID: modelID, Name: "q"}))
	return modelID, variantID
}

func (f *removalFixture) artifact(variantID int64, path, kind string) int64 {
	f.t.Helper()
	return f.must(f.cat.CreateArtifact(f.ctx, store.Artifact{VariantID: variantID, FormatID: f.fmt, FilePath: path, ArtifactType: kind}))
}

func (f *removalFixture) config(name string, variantID, weight int64, args ...string) int64 {
	f.t.Helper()
	return f.must(f.cat.CreateConfig(f.ctx, store.Config{Name: name, VariantID: variantID, WeightArtifactID: weight, EngineID: f.eng,
		Status: "unverified", Visibility: "visible", ExtraArgs: args}))
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestRemoveModelWithConfigsAndFiles(t *testing.T) {
	f := newRemovalFixture(t)
	modelID, v := f.model("Groot")
	weight := f.file("Groot/weights-00001-of-00002.gguf", 10)
	shard2 := f.file("Groot/weights-00002-of-00002.gguf", 5)
	proj := f.file("Groot/mmproj.gguf", 3)
	wa := f.artifact(v, "Groot/weights-00001-of-00002.gguf", "weight")
	f.artifact(v, proj, "mmproj")
	cid := f.config("groot", v, wa)
	f.must(f.cat.CreateBenchmark(f.ctx, store.Benchmark{Metric: "decode_tps", Value: "1", Source: "self_measured", SubjectType: "config", SubjectID: cid}))
	f.must(f.cat.CreateBenchmark(f.ctx, store.Benchmark{Metric: "coding", Value: "1", Source: "self_measured", SubjectType: "model", SubjectID: modelID}))
	otherModel, ov := f.model("Other")
	keep := f.file("other.gguf", 1)
	f.config("other", ov, f.artifact(ov, "other.gguf", "weight"))
	f.must(f.cat.CreateBenchmark(f.ctx, store.Benchmark{Metric: "coding", Value: "2", Source: "self_measured", SubjectType: "model", SubjectID: otherModel}))

	p, err := f.w.Plan(f.ctx, store.RemoveKindModel, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Configs) != 1 || p.Configs[0].Name != "groot" || p.Benchmarks != 2 || len(p.Blockers) != 0 || len(p.Files) != 3 {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := f.w.Execute(f.ctx, store.RemoveKindModel, modelID, true, ""); !errors.Is(err, store.ErrRemovalChanged) {
		t.Fatalf("deleting files without the confirmed token: err = %v", err)
	}
	res, err := f.w.Execute(f.ctx, store.RemoveKindModel, modelID, true, p.Token)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.FilesDeleted) != 3 || res.BytesFreed != 18 || len(res.FileErrors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	for _, gone := range []string{weight, shard2, proj, filepath.Join(f.root, "Groot")} {
		if exists(gone) {
			t.Errorf("%s still exists", gone)
		}
	}
	if !exists(keep) || !exists(f.root) {
		t.Fatal("another model's file or the models directory was removed")
	}
	if _, err := f.cat.GetModel(f.ctx, modelID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("model still there: %v", err)
	}
	if _, err := f.cat.GetConfig(f.ctx, cid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("config still there: %v", err)
	}
	refs, err := f.cat.RemovalReferences(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs.Benchmarks) != 1 {
		t.Errorf("benchmarks left = %v, want only the other model's", refs.Benchmarks)
	}
}

func TestRemoveModelKeepsFilesByDefault(t *testing.T) {
	f := newRemovalFixture(t)
	modelID, v := f.model("M")
	weight := f.file("m.gguf", 4)
	f.config("m", v, f.artifact(v, "m.gguf", "weight"))
	if _, err := f.w.Execute(f.ctx, store.RemoveKindModel, modelID, false, ""); err != nil {
		t.Fatal(err)
	}
	if !exists(weight) {
		t.Fatal("file deleted although not asked for")
	}
}

func TestRemovalKeepsSharedReferencedAndOutsideFiles(t *testing.T) {
	f := newRemovalFixture(t)
	modelID, v := f.model("M")
	_, ov := f.model("Other")
	weight := f.file("m.gguf", 4)
	shared := f.file("shared.gguf", 4)
	draft := f.file("draft/d.gguf", 4)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "x.gguf")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(f.root, "linked")); err != nil {
		t.Fatal(err)
	}
	wa := f.artifact(v, "m.gguf", "weight")
	f.artifact(v, "shared.gguf", "mmproj")
	f.artifact(v, "draft/d.gguf", "weight")
	f.artifact(v, "linked/x.gguf", "weight") // lexically inside, really outside
	f.artifact(v, "gone.gguf", "weight")
	f.config("m", v, wa)
	f.artifact(ov, shared, "mmproj") // the same file under another model, by absolute path
	f.config("other", ov, f.artifact(ov, f.file("o.gguf", 1), "weight"), "--spec-draft-model="+draft)

	p, err := f.w.Plan(f.ctx, store.RemoveKindModel, modelID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{weight: "", shared: store.KeepShared, draft: store.KeepReferenced,
		filepath.Join(f.root, "linked/x.gguf"): store.KeepOutside, filepath.Join(f.root, "gone.gguf"): store.KeepMissing}
	for _, file := range p.Files {
		w, ok := want[file.Path]
		if !ok || file.Keep != w || file.Delete != (w == "") {
			t.Errorf("%s: delete=%v keep=%q, want keep=%q", file.Path, file.Delete, file.Keep, w)
		}
		delete(want, file.Path)
	}
	if len(want) != 0 {
		t.Errorf("files missing from plan: %v", want)
	}
	if _, err := f.w.Execute(f.ctx, store.RemoveKindModel, modelID, true, p.Token); err != nil {
		t.Fatal(err)
	}
	if exists(weight) || !exists(shared) || !exists(draft) || !exists(outside) {
		t.Fatalf("wrong files deleted: weight=%v shared=%v draft=%v outside=%v", exists(weight), exists(shared), exists(draft), exists(outside))
	}
}

func TestRemovalIgnoresDirectoriesAtOrAboveTheModelsRoot(t *testing.T) {
	f := newRemovalFixture(t)
	modelID, v := f.model("M")
	f.config("m", v, f.artifact(v, f.file("sub/m.gguf", 1), "weight"))
	for key, value := range map[string]string{
		"catalog.build_roots": `["` + filepath.Dir(f.root) + `","/"]`,
		"infra.paths":         `{"models_dir":"` + f.root + `"}`,
	} {
		if err := f.db.Settings().Set(f.ctx, key, []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.w.Plan(f.ctx, store.RemoveKindModel, modelID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Files) != 1 || !p.Files[0].Delete {
		t.Fatalf("files = %+v", p.Files)
	}
}

func TestRemovalBlockers(t *testing.T) {
	f := newRemovalFixture(t)
	modelID, v := f.model("M")
	_, ov := f.model("Other")
	wa := f.artifact(v, f.file("m.gguf", 1), "weight")
	cid := f.config("m", v, wa)
	f.config("borrower", ov, wa) // another model's config using this file's row
	if err := f.db.Settings().Set(f.ctx, "smith.model", []byte(`"m"`)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Sched().SaveReservation(f.ctx, store.ReservationRow{Label: "demo", Model: "m", Start: time.Now().Add(-time.Hour),
		End: time.Now().Add(time.Hour), Scope: "bay", Bay: "a1", CreatedBy: "t"}); err != nil {
		t.Fatal(err)
	}
	f.w.Loaded = func(n string) bool { return n == "m" }

	p, err := f.w.Plan(f.ctx, store.RemoveKindModel, modelID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, b := range p.Blockers {
		got[b.Code] = true
	}
	for _, code := range []string{store.BlockConfigLoaded, store.BlockSetting, store.BlockReservation, store.BlockSharedArtifact} {
		if !got[code] {
			t.Errorf("missing blocker %s in %+v", code, p.Blockers)
		}
	}
	if _, err := f.w.Execute(f.ctx, store.RemoveKindModel, modelID, false, p.Token); !errors.Is(err, store.ErrRemovalBlocked) {
		t.Fatalf("err = %v", err)
	}
	if _, err := f.cat.GetConfig(f.ctx, cid); err != nil {
		t.Fatalf("a blocked removal deleted something: %v", err)
	}
}

func TestRemoveOneConfig(t *testing.T) {
	f := newRemovalFixture(t)
	_, v := f.model("M")
	own := f.file("own.gguf", 2)
	both := f.file("both.gguf", 2)
	ownA := f.artifact(v, "own.gguf", "weight")
	bothA := f.artifact(v, "both.gguf", "mmproj")
	first := f.must(f.cat.CreateConfig(f.ctx, store.Config{Name: "first", VariantID: v, WeightArtifactID: ownA, MMProjArtifactID: bothA,
		EngineID: f.eng, Status: "unverified", Visibility: "visible"}))
	second := f.config("second", v, bothA)

	p, err := f.w.Plan(f.ctx, store.RemoveKindConfig, first)
	if err != nil {
		t.Fatal(err)
	}
	if p.LastConfigOfModel || len(p.Files) != 1 || p.Files[0].Path != own {
		t.Fatalf("plan = %+v", p)
	}
	if _, err := f.w.Execute(f.ctx, store.RemoveKindConfig, first, true, p.Token); err != nil {
		t.Fatal(err)
	}
	if exists(own) || !exists(both) {
		t.Fatalf("own=%v both=%v", exists(own), exists(both))
	}
	if _, err := f.cat.GetArtifact(f.ctx, ownA); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the deleted file's artifact row is still there: %v", err)
	}
	p, err = f.w.Plan(f.ctx, store.RemoveKindConfig, second)
	if err != nil {
		t.Fatal(err)
	}
	if !p.LastConfigOfModel {
		t.Error("the only remaining config was not reported as the last one")
	}
}

func TestRemovalRefusesAStalePlan(t *testing.T) {
	f := newRemovalFixture(t)
	modelID, v := f.model("M")
	wa := f.artifact(v, f.file("m.gguf", 1), "weight")
	f.config("m", v, wa)
	p, err := f.w.Plan(f.ctx, store.RemoveKindModel, modelID)
	if err != nil {
		t.Fatal(err)
	}
	f.config("added-later", v, wa)
	if _, err := f.w.Execute(f.ctx, store.RemoveKindModel, modelID, true, p.Token); !errors.Is(err, store.ErrRemovalChanged) {
		t.Fatalf("err = %v", err)
	}
}
