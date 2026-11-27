// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// fakeArtifactCat answers only what ArtifactWrites asks of the catalog.
type fakeArtifactCat struct {
	store.Catalog
	artifacts map[int64]store.Artifact
	configs   []store.Config
	variants  map[int64]bool
	formats   []store.Format
	quants    []store.Quantization
	nextID    int64
	failRead  string // "variant" | "formats" | "quants" | "artifacts" | "configs"
}

func (f *fakeArtifactCat) GetVariant(_ context.Context, id int64) (store.Variant, error) {
	if f.failRead == "variant" {
		return store.Variant{}, errCat
	}
	if f.variants[id] {
		return store.Variant{ID: id}, nil
	}
	return store.Variant{}, nf("variant", id)
}
func (f *fakeArtifactCat) ListFormats(context.Context) ([]store.Format, error) {
	if f.failRead == "formats" {
		return nil, errCat
	}
	return f.formats, nil
}
func (f *fakeArtifactCat) ListQuantizations(context.Context) ([]store.Quantization, error) {
	if f.failRead == "quants" {
		return nil, errCat
	}
	return f.quants, nil
}
func (f *fakeArtifactCat) ListArtifacts(context.Context) ([]store.Artifact, error) {
	if f.failRead == "artifacts" {
		return nil, errCat
	}
	var out []store.Artifact
	for _, a := range f.artifacts {
		out = append(out, a)
	}
	return out, nil
}
func (f *fakeArtifactCat) ListConfigs(context.Context) ([]store.Config, error) {
	if f.failRead == "configs" {
		return nil, errCat
	}
	return f.configs, nil
}
func (f *fakeArtifactCat) GetArtifact(_ context.Context, id int64) (store.Artifact, error) {
	if a, ok := f.artifacts[id]; ok {
		return a, nil
	}
	return store.Artifact{}, nf("artifact", id)
}
func (f *fakeArtifactCat) CreateArtifact(_ context.Context, a store.Artifact) (int64, error) {
	f.nextID++
	a.ID = f.nextID
	f.artifacts[a.ID] = a
	return a.ID, nil
}
func (f *fakeArtifactCat) UpdateArtifact(_ context.Context, a store.Artifact) error {
	if _, ok := f.artifacts[a.ID]; !ok {
		return nf("artifact", a.ID)
	}
	f.artifacts[a.ID] = a
	return nil
}
func (f *fakeArtifactCat) DeleteArtifact(_ context.Context, id int64) error {
	if _, ok := f.artifacts[id]; !ok {
		return nf("artifact", id)
	}
	delete(f.artifacts, id)
	return nil
}

func minimalGGUFBytes() []byte {
	var kv bytes.Buffer
	w := func(v any) { _ = binary.Write(&kv, binary.LittleEndian, v) }
	str := func(s string) { w(uint64(len(s))); kv.WriteString(s) }
	str("general.architecture")
	w(uint32(8))
	str("qwen3")
	str("general.file_type")
	w(uint32(4))
	w(uint32(18))
	str("general.parameter_count")
	w(uint32(10))
	w(uint64(8_190_000_000))
	str("qwen3.context_length")
	w(uint32(4))
	w(uint32(262144))
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint32(0x46554747))
	_ = binary.Write(&out, binary.LittleEndian, uint32(3))
	_ = binary.Write(&out, binary.LittleEndian, uint64(0))
	_ = binary.Write(&out, binary.LittleEndian, uint64(4))
	out.Write(kv.Bytes())
	return out.Bytes()
}

func newArtifactWrites(t *testing.T) (store.ArtifactWrites, *fakeArtifactCat, string, *int) {
	t.Helper()
	root := t.TempDir()
	cat := &fakeArtifactCat{
		artifacts: map[int64]store.Artifact{}, variants: map[int64]bool{1: true},
		formats: []store.Format{{ID: 1, Name: "GGUF"}, {ID: 2, Name: "safetensors"}}, quants: []store.Quantization{{ID: 1}},
	}
	changed := 0
	return store.ArtifactWrites{Cat: cat, Changed: func() { changed++ }, ModelsDir: func() string { return root }}, cat, root, &changed
}

func writeFile(t *testing.T, root, rel string, data []byte) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func goodArtifact(rel string) store.Artifact {
	return store.Artifact{VariantID: 1, FormatID: 1, QuantizationID: 1, FilePath: rel, ArtifactType: "weight"}
}

func TestArtifactWrites_CreateReadsTheFileAndGuardsThePath(t *testing.T) {
	w, cat, root, changed := newArtifactWrites(t)
	ctx := context.Background()
	writeFile(t, root, "q/model.gguf", minimalGGUFBytes())
	writeFile(t, root, "q/notes.bin", []byte("0123456789"))

	got, err := w.Create(ctx, goodArtifact("q/model.gguf"))
	if err != nil || got.ID == 0 || got.GGUFArch != "qwen3" || got.GGUFTrainedCtx != 262144 || got.GGUFParameterCount != "8190000000" || got.FileSizeBytes == 0 {
		t.Fatalf("create: %+v %v", got, err)
	}
	if *changed != 1 {
		t.Errorf("change hook ran %d times, want 1", *changed)
	}
	// File facts come from the file, not the caller; an mmproj is auxiliary.
	forged := goodArtifact("q/model.gguf")
	forged.ArtifactType, forged.GGUFArch, forged.FileSizeBytes = "mmproj", "llama", 1
	forged.ShardSetID = "set"
	if m, err := w.Create(ctx, forged); err != nil || m.GGUFArch != "qwen3" || m.FileSizeBytes == 1 || !m.IsAuxiliary {
		t.Errorf("forged facts / mmproj: %+v %v", m, err)
	}
	// A non-GGUF file takes its size from the file system.
	other := goodArtifact("q/notes.bin")
	other.FormatID = 2
	if o, err := w.Create(ctx, other); err != nil || o.FileSizeBytes != 10 {
		t.Errorf("non-gguf size: %+v %v", o, err)
	}
	*changed = 0

	for name, tc := range map[string]struct {
		mutate func(*store.Artifact)
		field  string
		code   string
	}{
		"no variant":          {func(a *store.Artifact) { a.VariantID = 0 }, "variant_id", "required"},
		"unknown variant":     {func(a *store.Artifact) { a.VariantID = 9 }, "variant_id", ""},
		"bad type":            {func(a *store.Artifact) { a.ArtifactType = "lora" }, "artifact_type", ""},
		"no format":           {func(a *store.Artifact) { a.FormatID = 0 }, "format_id", "required"},
		"unknown format":      {func(a *store.Artifact) { a.FormatID = 9 }, "format_id", ""},
		"unknown quant":       {func(a *store.Artifact) { a.QuantizationID = 9 }, "quantization_id", ""},
		"no path":             {func(a *store.Artifact) { a.FilePath = "" }, "file_path", "required"},
		"escapes the root":    {func(a *store.Artifact) { a.FilePath = "../outside.gguf" }, "file_path", ""},
		"absolute outside":    {func(a *store.Artifact) { a.FilePath = "/etc/passwd" }, "file_path", ""},
		"shell characters":    {func(a *store.Artifact) { a.FilePath = "q/a;b.gguf" }, "file_path", ""},
		"missing file":        {func(a *store.Artifact) { a.FilePath = "q/nope.gguf" }, "file_path", ""},
		"not a gguf":          {func(a *store.Artifact) { a.FilePath = "q/notes.bin" }, "file_path", ""},
		"gguf is a directory": {func(a *store.Artifact) { a.FilePath = "q" }, "file_path", ""},
		"duplicate file":      {func(a *store.Artifact) {}, "file_path", ""},
	} {
		a := goodArtifact("q/model.gguf")
		tc.mutate(&a)
		var inv *store.Invalid
		_, err := w.Create(ctx, a)
		if !errors.As(err, &inv) || inv.Fields[tc.field] == "" || (tc.code != "" && inv.Codes[tc.field] != tc.code) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if *changed != 0 || len(cat.artifacts) != 3 {
		t.Errorf("refused writes changed something: changed=%d artifacts=%d", *changed, len(cat.artifacts))
	}
	// An unconfigured models directory refuses every write.
	w.ModelsDir = func() string { return "" }
	if _, err := w.Create(ctx, goodArtifact("q/model.gguf")); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Errorf("unconfigured models dir: %v", err)
	}
}

func TestArtifactWrites_UpdateDoesNotCollideWithItself(t *testing.T) {
	w, _, root, _ := newArtifactWrites(t)
	ctx := context.Background()
	writeFile(t, root, "m.gguf", minimalGGUFBytes())
	a, err := w.Create(ctx, goodArtifact("m.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	a.SHA256 = "abc"
	if got, err := w.Update(ctx, a.ID, a); err != nil || got.SHA256 != "abc" {
		t.Fatalf("update in place: %+v %v", got, err)
	}
	if _, err := w.Update(ctx, 99, a); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("updating a stranger: %v", err)
	}
}

// A failed read is not a passed check: each list the rules depend on, failing, must surface its error and write nothing. The duplicate guard and
// the in-use guard used to be skipped silently.
func TestArtifactWrites_StoreFailureIsNotAVerdict(t *testing.T) {
	for _, read := range []string{"variant", "formats", "quants", "artifacts"} {
		w, cat, root, _ := newArtifactWrites(t)
		writeFile(t, root, "m.gguf", minimalGGUFBytes())
		cat.failRead = read
		_, err := w.Create(context.Background(), goodArtifact("m.gguf"))
		var inv *store.Invalid
		if err == nil || errors.As(err, &inv) || len(cat.artifacts) != 0 {
			t.Errorf("%s read failing: %v (written: %d)", read, err, len(cat.artifacts))
		}
	}
	w, cat, root, _ := newArtifactWrites(t)
	writeFile(t, root, "m.gguf", minimalGGUFBytes())
	a, _ := w.Create(context.Background(), goodArtifact("m.gguf"))
	cat.failRead = "configs"
	var inUse *store.ArtifactInUseError
	if err := w.Delete(context.Background(), a.ID); err == nil || errors.As(err, &inUse) || len(cat.artifacts) != 1 {
		t.Errorf("delete with the config list failing must refuse with that error: %v", err)
	}
}

func TestArtifactWrites_DeleteRefusedWhileAConfigUsesIt(t *testing.T) {
	w, cat, root, changed := newArtifactWrites(t)
	ctx := context.Background()
	writeFile(t, root, "m.gguf", minimalGGUFBytes())
	writeFile(t, root, "mm.gguf", minimalGGUFBytes())
	weight, _ := w.Create(ctx, goodArtifact("m.gguf"))
	mm := goodArtifact("mm.gguf")
	mm.ArtifactType = "mmproj"
	proj, _ := w.Create(ctx, mm)
	cat.configs = []store.Config{{Name: "qwen36", WeightArtifactID: weight.ID, MMProjArtifactID: proj.ID}}
	*changed = 0

	var inUse *store.ArtifactInUseError
	for _, id := range []int64{weight.ID, proj.ID} {
		if err := w.Delete(ctx, id); !errors.As(err, &inUse) || inUse.Configs[0] != "qwen36" {
			t.Errorf("delete %d while used: %v", id, err)
		}
	}
	if len(cat.artifacts) != 2 || *changed != 0 {
		t.Fatal("a refused delete changed something")
	}
	cat.configs = nil
	if err := w.Delete(ctx, weight.ID); err != nil || len(cat.artifacts) != 1 || *changed != 1 {
		t.Errorf("delete: %v artifacts=%d changed=%d", err, len(cat.artifacts), *changed)
	}
	if err := w.Delete(ctx, 99); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleting a stranger: %v", err)
	}
}

// A download takes long enough for the Artifact to be edited meanwhile: SetFile re-reads the row and changes only the file's location, size,
// hash and (when given) header facts.
func TestArtifactWrites_SetFileKeepsEverythingElse(t *testing.T) {
	w, cat, root, changed := newArtifactWrites(t)
	ctx := context.Background()
	writeFile(t, root, "m.gguf", minimalGGUFBytes())
	a, err := w.Create(ctx, goodArtifact("m.gguf"))
	if err != nil {
		t.Fatal(err)
	}
	edited := cat.artifacts[a.ID]
	edited.ShardSetID, edited.QuantizationID = "set-7", 1 // an edit made after the downloader looked
	cat.artifacts[a.ID] = edited
	*changed = 0

	got, err := w.SetFile(ctx, a.ID, store.ArtifactFile{FilePath: "new/m.gguf", FileSizeBytes: 42, SHA256: "abc",
		GGUF: &store.ArtifactGGUFFacts{Arch: "llama", TrainedCtx: 8192, ParameterCount: "7000000000", QuantType: "Q4_K_M"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.FilePath != "new/m.gguf" || got.FileSizeBytes != 42 || got.SHA256 != "abc" || got.GGUFArch != "llama" || got.GGUFQuantType != "Q4_K_M" {
		t.Errorf("file not recorded: %+v", got)
	}
	if stored := cat.artifacts[a.ID]; stored.ShardSetID != "set-7" || stored.VariantID != 1 || stored.ArtifactType != "weight" || *changed != 1 {
		t.Errorf("SetFile wrote more than the file, or lost the interim edit: %+v changed=%d", stored, *changed)
	}
	// Without header facts (a non-GGUF file) the stored ones are left alone.
	if got, err := w.SetFile(ctx, a.ID, store.ArtifactFile{FilePath: "n.bin", FileSizeBytes: 1}); err != nil || got.GGUFArch != "llama" {
		t.Errorf("nil GGUF facts must leave the header facts: %+v %v", got, err)
	}
	if _, err := w.SetFile(ctx, 99, store.ArtifactFile{}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown artifact: %v", err)
	}
}

func TestArtifactWrites_BackfillOnlyFillsMissingFacts(t *testing.T) {
	w, cat, _, changed := newArtifactWrites(t)
	ctx := context.Background()
	cat.artifacts[1] = store.Artifact{ID: 1, VariantID: 1, FilePath: "a.gguf", ArtifactType: "weight"}
	cat.artifacts[2] = store.Artifact{ID: 2, VariantID: 1, FilePath: "b.gguf", ArtifactType: "weight", GGUFArch: "set-by-operator"}
	facts := store.ArtifactGGUFFacts{Arch: "qwen3", TrainedCtx: 4096, ParameterCount: "1", QuantType: "Q8_0"}

	if ok, err := w.BackfillGGUFFacts(ctx, 1, facts); err != nil || !ok || cat.artifacts[1].GGUFArch != "qwen3" || *changed != 1 {
		t.Errorf("backfill of an empty row: ok=%v err=%v %+v changed=%d", ok, err, cat.artifacts[1], *changed)
	}
	// Facts that appeared after the caller listed the rows are kept.
	if ok, err := w.BackfillGGUFFacts(ctx, 2, facts); err != nil || ok || cat.artifacts[2].GGUFArch != "set-by-operator" || *changed != 1 {
		t.Errorf("backfill over existing facts: ok=%v err=%v %+v changed=%d", ok, err, cat.artifacts[2], *changed)
	}
}
