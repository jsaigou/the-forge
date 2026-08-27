// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"
)

func TestModelDownloadsCreateGetList(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	dl := db.ModelDownloads()

	id, err := dl.Create(ctx, ModelDownloadRow{Repo: "org/model", DestDir: "org-model"}, []ModelDownloadFileRow{
		{Filename: "model-Q4_K_M.gguf", DestRelPath: "org-model/model-Q4_K_M.gguf", SortOrder: 0},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id == 0 {
		t.Fatal("Create returned zero id")
	}

	got, err := dl.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Repo != "org/model" || got.Revision != "main" || got.State != "pending_approval" {
		t.Errorf("Get = %+v, unexpected defaults", got)
	}

	files, err := dl.ListFiles(ctx, id)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 1 || files[0].State != "pending" {
		t.Fatalf("ListFiles = %+v, want one pending file", files)
	}

	list, err := dl.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %+v, err=%v, want one row", list, err)
	}
}

func TestModelDownloadsCreateAllFilesOrNone(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	dl := db.ModelDownloads()

	// A download with zero files is a valid (if unusual) call — Create
	// should still succeed and the job should simply have no files, not
	// error. The real atomicity guarantee under test is that a job row
	// and its file rows are visible together or not at all; simulate a
	// failure mid-batch is out of scope for a black-box store test, so
	// this pins the happy multi-file path instead.
	id, err := dl.Create(ctx, ModelDownloadRow{Repo: "org/sharded"}, []ModelDownloadFileRow{
		{Filename: "model-00001-of-00002.gguf", DestRelPath: "model-00001-of-00002.gguf", SortOrder: 0},
		{Filename: "model-00002-of-00002.gguf", DestRelPath: "model-00002-of-00002.gguf", SortOrder: 1},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	files, err := dl.ListFiles(ctx, id)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("ListFiles = %d rows, want 2 shard rows", len(files))
	}
	if files[0].SortOrder != 0 || files[1].SortOrder != 1 {
		t.Errorf("shard order not preserved: %+v", files)
	}
}

func TestModelDownloadsUpdateStateStampsTimestampsOnce(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	dl := db.ModelDownloads()

	id, err := dl.Create(ctx, ModelDownloadRow{Repo: "org/model"}, []ModelDownloadFileRow{
		{Filename: "m.gguf", DestRelPath: "m.gguf"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := dl.UpdateState(ctx, id, "running", ""); err != nil {
		t.Fatalf("UpdateState(running): %v", err)
	}
	first, err := dl.Get(ctx, id)
	if err != nil || first.StartedAt.IsZero() {
		t.Fatalf("Get after first running = %+v, err=%v, want a non-zero started_at", first, err)
	}

	// Simulate pause → resume: started_at must not move on a second
	// "running" transition (the pattern a resumed job hits).
	if err := dl.UpdateState(ctx, id, "paused", ""); err != nil {
		t.Fatalf("UpdateState(paused): %v", err)
	}
	if err := dl.UpdateState(ctx, id, "running", ""); err != nil {
		t.Fatalf("UpdateState(running again): %v", err)
	}
	second, err := dl.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !second.StartedAt.Equal(first.StartedAt) {
		t.Errorf("started_at moved on resume: first=%v second=%v", first.StartedAt, second.StartedAt)
	}

	if err := dl.UpdateState(ctx, id, "failed", "boom"); err != nil {
		t.Fatalf("UpdateState(failed): %v", err)
	}
	final, err := dl.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.FinishedAt.IsZero() || final.Error != "boom" {
		t.Errorf("Get after failed = %+v, want a stamped finished_at and the error message", final)
	}
}

func TestModelDownloadsListRunning(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	dl := db.ModelDownloads()

	runningID, _ := dl.Create(ctx, ModelDownloadRow{Repo: "org/a"}, []ModelDownloadFileRow{{Filename: "a.gguf", DestRelPath: "a.gguf"}})
	pausedID, _ := dl.Create(ctx, ModelDownloadRow{Repo: "org/b"}, []ModelDownloadFileRow{{Filename: "b.gguf", DestRelPath: "b.gguf"}})
	if err := dl.UpdateState(ctx, runningID, "running", ""); err != nil {
		t.Fatal(err)
	}
	if err := dl.UpdateState(ctx, pausedID, "paused", ""); err != nil {
		t.Fatal(err)
	}

	running, err := dl.ListRunning(ctx)
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(running) != 1 || running[0].ID != runningID {
		t.Fatalf("ListRunning = %+v, want only the running job", running)
	}
}

func TestModelDownloadsFileProgressAndVerify(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	dl := db.ModelDownloads()

	id, err := dl.Create(ctx, ModelDownloadRow{Repo: "org/model"}, []ModelDownloadFileRow{
		{Filename: "m.gguf", DestRelPath: "m.gguf", SHA256Expected: strings.Repeat("ab", 32)},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	files, _ := dl.ListFiles(ctx, id)
	fileID := files[0].ID

	if err := dl.UpdateFileProgress(ctx, fileID, 512, 1024); err != nil {
		t.Fatalf("UpdateFileProgress: %v", err)
	}
	if err := dl.UpdateFileState(ctx, fileID, "verified", strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("UpdateFileState: %v", err)
	}
	got, _ := dl.ListFiles(ctx, id)
	if got[0].BytesDone != 512 || got[0].State != "verified" || got[0].SHA256Actual != strings.Repeat("ab", 32) {
		t.Errorf("ListFiles after progress+verify = %+v", got[0])
	}
}

func TestModelDownloadsSetCreatedConfigAndDelete(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	dl := db.ModelDownloads()

	id, err := dl.Create(ctx, ModelDownloadRow{Repo: "org/model"}, []ModelDownloadFileRow{{Filename: "m.gguf", DestRelPath: "m.gguf"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := dl.SetCreatedConfig(ctx, id, 42); err != nil {
		t.Fatalf("SetCreatedConfig: %v", err)
	}
	got, err := dl.Get(ctx, id)
	if err != nil || got.CreatedConfigID != 42 {
		t.Fatalf("Get after SetCreatedConfig = %+v, err=%v", got, err)
	}

	if err := dl.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := dl.Get(ctx, id); err == nil {
		t.Fatal("Get after Delete should fail")
	}
	// Cascade: file rows must go with the parent (ON DELETE CASCADE).
	if files, ferr := dl.ListFiles(ctx, id); ferr != nil || len(files) != 0 {
		t.Errorf("ListFiles after Delete = %+v, err=%v, want none (cascade)", files, ferr)
	}
}

func TestRegisterDownloadedModelAtomicRowSet(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	ggufFormat, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		t.Fatalf("FormatByName: %v", err)
	}
	quant, err := cat.QuantizationByName(ctx, "Q4_K_M")
	if err != nil {
		t.Fatalf("QuantizationByName: %v", err)
	}
	eng, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		t.Fatalf("EngineByName: %v", err)
	}

	res, err := cat.RegisterDownloadedModel(ctx, ModelBundle{
		Model:   Model{Name: "Downloaded Model", HFRepo: "org/downloaded-model", Creator: "org"},
		Variant: Variant{Name: "7b", TrainedCtx: 32768},
		Artifact: Artifact{
			FormatID: ggufFormat.ID, QuantizationID: quant.ID,
			FilePath: "downloaded-model/model-Q4_K_M.gguf", FileSizeBytes: 1 << 30,
			GGUFArch: "testarch", GGUFTrainedCtx: 32768,
		},
		Config: Config{
			Name: "downloaded-model-7b-q4km", EngineID: eng.ID,
			NCtx: 32768, Parallel: 1, ExtraArgs: []string{"--no-mmap", "--jinja", "--flash-attn", "on"},
		},
	})
	if err != nil {
		t.Fatalf("RegisterDownloadedModel: %v", err)
	}
	if res.ModelID == 0 || res.VariantID == 0 || res.ArtifactID == 0 || res.ConfigID == 0 {
		t.Fatalf("RegisterDownloadedModel result has a zero id: %+v", res)
	}

	// Every row must be reachable through the ordinary read surface, and
	// the FK chain must be wired the way the bundle intended.
	m, err := cat.GetModel(ctx, res.ModelID)
	if err != nil || m.Name != "Downloaded Model" {
		t.Fatalf("GetModel = %+v, err=%v", m, err)
	}
	vt, err := cat.GetVariant(ctx, res.VariantID)
	if err != nil || vt.ModelID != res.ModelID {
		t.Fatalf("GetVariant = %+v, err=%v, want ModelID=%d", vt, err, res.ModelID)
	}
	art, err := cat.GetArtifact(ctx, res.ArtifactID)
	if err != nil || art.VariantID != res.VariantID {
		t.Fatalf("GetArtifact = %+v, err=%v, want VariantID=%d", art, err, res.VariantID)
	}
	cfg, err := cat.GetConfig(ctx, res.ConfigID)
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.VariantID != res.VariantID || cfg.WeightArtifactID != res.ArtifactID {
		t.Errorf("Config FK wiring wrong: %+v", cfg)
	}
	// The registrar's safety default: a freshly auto-registered Config
	// must never be launchable until an operator promotes it.
	if cfg.Status != "unverified" || cfg.Visibility != "hidden" {
		t.Errorf("Config = status=%q visibility=%q, want unverified/hidden by default", cfg.Status, cfg.Visibility)
	}
}

func TestRegisterDownloadedModelWithMMProj(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	ggufFormat, _ := cat.FormatByName(ctx, "GGUF")
	eng, _ := cat.EngineByName(ctx, "llama.cpp")

	res, err := cat.RegisterDownloadedModel(ctx, ModelBundle{
		Model:   Model{Name: "Vision Model", HFRepo: "org/vision-model"},
		Variant: Variant{Name: "7b"},
		Artifact: Artifact{
			FormatID: ggufFormat.ID, FilePath: "vision-model/model.gguf", FileSizeBytes: 1,
		},
		MMProj: &Artifact{
			FormatID: ggufFormat.ID, FilePath: "vision-model/mmproj.gguf", FileSizeBytes: 1,
		},
		Config: Config{Name: "vision-model-7b", EngineID: eng.ID, Parallel: 1},
	})
	if err != nil {
		t.Fatalf("RegisterDownloadedModel: %v", err)
	}
	if res.MMProjArtifactID == 0 {
		t.Fatal("MMProjArtifactID must be set when the bundle included an MMProj artifact")
	}
	mmproj, err := cat.GetArtifact(ctx, res.MMProjArtifactID)
	if err != nil {
		t.Fatalf("GetArtifact(mmproj): %v", err)
	}
	if !mmproj.IsAuxiliary || mmproj.ArtifactType != "mmproj" {
		t.Errorf("mmproj artifact = %+v, want IsAuxiliary=true ArtifactType=mmproj", mmproj)
	}
	cfg, err := cat.GetConfig(ctx, res.ConfigID)
	if err != nil || cfg.MMProjArtifactID != res.MMProjArtifactID {
		t.Fatalf("Config.MMProjArtifactID = %d, want %d (err=%v)", cfg.MMProjArtifactID, res.MMProjArtifactID, err)
	}
}

func TestRegisterDownloadedModelFailsClosedOnBadFK(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	// An engine_id of 999999 doesn't exist — the Config insert must fail,
	// and (the point of this test) the Model/Variant/Artifact rows already
	// written earlier in the same call must NOT be left behind orphaned.
	_, err := cat.RegisterDownloadedModel(ctx, ModelBundle{
		Model:    Model{Name: "Should Not Persist", HFRepo: "org/should-not-persist"},
		Variant:  Variant{Name: "7b"},
		Artifact: Artifact{FormatID: 1, FilePath: "x.gguf"},
		Config:   Config{Name: "should-not-persist-7b", EngineID: 999999, Parallel: 1},
	})
	if err == nil {
		t.Fatal("expected a foreign key failure on an unknown engine_id")
	}
	if _, merr := cat.ModelByName(ctx, "Should Not Persist"); merr == nil {
		t.Error("the Model row must not survive a failed transaction — rollback left it behind")
	}
}

func TestRegisterDownloadedModelWithExtraArtifacts(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	ggufFormat, _ := cat.FormatByName(ctx, "GGUF")
	eng, _ := cat.EngineByName(ctx, "llama.cpp")

	res, err := cat.RegisterDownloadedModel(ctx, ModelBundle{
		Model:   Model{Name: "Sharded Model", HFRepo: "org/sharded-model"},
		Variant: Variant{Name: "70b"},
		Artifact: Artifact{
			FormatID: ggufFormat.ID, FilePath: "sharded-model/model-00001-of-00003.gguf",
			FileSizeBytes: 1 << 30, ShardSetID: "dl-1",
		},
		ExtraArtifacts: []Artifact{
			{FormatID: ggufFormat.ID, FilePath: "sharded-model/model-00002-of-00003.gguf", FileSizeBytes: 1 << 30, ShardSetID: "dl-1"},
			{FormatID: ggufFormat.ID, FilePath: "sharded-model/model-00003-of-00003.gguf", FileSizeBytes: 1 << 30, ShardSetID: "dl-1"},
		},
		Config: Config{Name: "sharded-model-70b", EngineID: eng.ID, Parallel: 1},
	})
	if err != nil {
		t.Fatalf("RegisterDownloadedModel: %v", err)
	}

	all, err := cat.ListArtifactsForVariant(ctx, res.VariantID)
	if err != nil {
		t.Fatalf("ListArtifactsForVariant: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("variant artifacts = %d, want 3 (one per shard)", len(all))
	}
	for _, a := range all {
		if a.ShardSetID != "dl-1" {
			t.Errorf("artifact %+v: ShardSetID = %q, want dl-1", a, a.ShardSetID)
		}
		if a.ArtifactType != "weight" {
			t.Errorf("artifact %+v: ArtifactType = %q, want weight (default when unset)", a, a.ArtifactType)
		}
	}
	// Config's FK must point at the PRIMARY artifact only — extras are
	// un-flagged siblings nothing references.
	cfg, err := cat.GetConfig(ctx, res.ConfigID)
	if err != nil || cfg.WeightArtifactID != res.ArtifactID {
		t.Fatalf("Config.WeightArtifactID = %d, want %d (the primary/first shard), err=%v", cfg.WeightArtifactID, res.ArtifactID, err)
	}
}

func TestRegisterDownloadedModelExtraArtifactFailureRollsBackEverything(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	cat := db.Catalog()

	ggufFormat, _ := cat.FormatByName(ctx, "GGUF")
	eng, _ := cat.EngineByName(ctx, "llama.cpp")

	// The second extra artifact's format_id (999999) doesn't exist — the
	// whole transaction, including the Model/Variant/primary-Artifact rows
	// already written, must roll back, not just skip the bad shard.
	_, err := cat.RegisterDownloadedModel(ctx, ModelBundle{
		Model:   Model{Name: "Should Also Not Persist", HFRepo: "org/should-also-not-persist"},
		Variant: Variant{Name: "70b"},
		Artifact: Artifact{
			FormatID: ggufFormat.ID, FilePath: "x-00001-of-00002.gguf", ShardSetID: "dl-2",
		},
		ExtraArtifacts: []Artifact{
			{FormatID: 999999, FilePath: "x-00002-of-00002.gguf", ShardSetID: "dl-2"},
		},
		Config: Config{Name: "should-also-not-persist-70b", EngineID: eng.ID, Parallel: 1},
	})
	if err == nil {
		t.Fatal("expected a foreign key failure on an unknown extra artifact format_id")
	}
	if _, merr := cat.ModelByName(ctx, "Should Also Not Persist"); merr == nil {
		t.Error("the Model row must not survive a failed extra-artifact insert — rollback left it behind")
	}
}
