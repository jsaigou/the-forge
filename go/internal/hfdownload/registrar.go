// SPDX-License-Identifier: Apache-2.0

package hfdownload

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/store"
)

// enrichWaitTimeout bounds how long registration waits for the
// concurrently-running asset enrichment (enrich.go) to finish before
// proceeding without it — enrichment is best-effort and must never hold
// up a job that's otherwise ready to register.
const enrichWaitTimeout = 5 * time.Second

// registrar.go — Phase 5: turns a fully-verified download into catalog
// rows via store.Catalog.RegisterDownloadedModel (one atomic transaction —
// see that method's doc comment for why a bundled write exists at all).
// The Config lands status=unverified, visibility=hidden: reviewable in
// Settings -> Catalog, never launchable from the switcher until an
// operator promotes it. A sharded model's trailing shards each get their
// own Artifact row (ExtraArtifacts, same ShardSetID as the primary/weight
// one Config.WeightArtifactID points at) — purely for catalog
// completeness, since llama-server only ever needs the first shard's path.

// registerAndFinish runs after every file in job has verified. It repoints
// an existing Config's artifact (job.ConfigName != "") or auto-registers a
// brand-new Model/Variant/Artifact/Config row set, then marks the job
// done. Every step here uses a fresh background context — job.State
// transitions must not be cut short by the (already-cancelled, if this
// path was reached via pause/cancel racing registration) worker context.
func (s *Service) registerAndFinish(ctx context.Context, jobID int64, job store.ModelDownloadRow, enrichCh <-chan Enrichment) {
	if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, "registering", ""); err != nil {
		s.d.logf("hfdownload: worker %d: state->registering: %v", jobID, err)
	}
	s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": "registering", "error": ""})

	files, err := s.d.Store.ModelDownloads().ListFiles(ctx, jobID)
	if err != nil {
		s.failJob(ctx, jobID, fmt.Errorf("list files for registration: %w", err))
		return
	}
	primary, mmproj, extraShards := splitFiles(files)
	if primary == nil {
		s.failJob(ctx, jobID, fmt.Errorf("no primary weight file among %d downloaded files", len(files)))
		return
	}

	cfg := s.d.Cfg()
	finalPath := filepath.Join(cfg.Paths.ModelsDir, primary.DestRelPath)
	md, err := gguf.ReadMetadata(finalPath)
	if err != nil && strings.HasSuffix(strings.ToLower(primary.Filename), ".gguf") {
		s.failJob(ctx, jobID, fmt.Errorf("re-read finalized GGUF: %w", err))
		return
	}

	if job.ConfigName != "" {
		if err := s.repointExistingConfig(ctx, job, primary, md); err != nil {
			s.failJob(ctx, jobID, err)
			return
		}
		s.finishDone(ctx, jobID, 0, 0)
		return
	}

	var enrichment Enrichment
	select {
	case enrichment = <-enrichCh:
	case <-time.After(enrichWaitTimeout):
		s.d.logf("hfdownload: job %d: enrichment still pending after %s — registering without it", jobID, enrichWaitTimeout)
	}

	bundle, err := s.buildBundle(ctx, job, primary, mmproj, extraShards, md, enrichment)
	if err != nil {
		s.failJob(ctx, jobID, err)
		return
	}
	res, err := s.d.Store.Catalog().RegisterDownloadedModel(ctx, bundle)
	if err != nil {
		s.failJob(ctx, jobID, fmt.Errorf("register catalog rows: %w", err))
		return
	}
	if err := s.d.Store.ModelDownloads().SetCreatedConfig(ctx, jobID, res.ConfigID); err != nil {
		s.d.logf("hfdownload: job %d: set_created_config: %v", jobID, err)
	}
	s.finishDone(ctx, jobID, res.ConfigID, res.ModelID)
}

func (s *Service) finishDone(ctx context.Context, jobID, configID, modelID int64) {
	if err := s.d.Store.ModelDownloads().UpdateState(ctx, jobID, "done", ""); err != nil {
		s.d.logf("hfdownload: worker %d: state->done: %v", jobID, err)
	}
	s.d.publish(EventDone, map[string]any{"job_id": jobID, "config_id": configID, "model_id": modelID})
	s.d.publish(EventStateChanged, map[string]any{"job_id": jobID, "state": "done", "error": ""})
}

// splitFiles picks the lowest-sort_order file as the weight artifact,
// any OTHER file whose name contains "mmproj" (case-insensitive — the
// real naming convention every mmproj file in this catalog already uses,
// e.g. mmproj-31b-F32.gguf) as the vision projector, and every remaining
// weight file (a sharded GGUF's trailing shards) as extras.
func splitFiles(files []store.ModelDownloadFileRow) (primary, mmproj *store.ModelDownloadFileRow, extras []store.ModelDownloadFileRow) {
	weight := make([]*store.ModelDownloadFileRow, 0, len(files))
	for i := range files {
		f := &files[i]
		if strings.Contains(strings.ToLower(f.Filename), "mmproj") {
			mmproj = f
			continue
		}
		weight = append(weight, f)
	}
	for _, f := range weight {
		if primary == nil || f.SortOrder < primary.SortOrder {
			primary = f
		}
	}
	for _, f := range weight {
		if f != primary {
			extras = append(extras, *f)
		}
	}
	return primary, mmproj, extras
}

// buildBundle assembles the full store.ModelBundle for a brand-new
// registration.
func (s *Service) buildBundle(ctx context.Context, job store.ModelDownloadRow, primary, mmprojFile *store.ModelDownloadFileRow, extraShards []store.ModelDownloadFileRow, md gguf.Metadata, enrichment Enrichment) (store.ModelBundle, error) {
	cat := s.d.Store.Catalog()
	ggufFormat, err := cat.FormatByName(ctx, "GGUF")
	if err != nil {
		return store.ModelBundle{}, fmt.Errorf("resolve GGUF format: %w", err)
	}
	engine, err := cat.EngineByName(ctx, "llama.cpp")
	if err != nil {
		return store.ModelBundle{}, fmt.Errorf("resolve llama.cpp engine: %w", err)
	}

	quant := hf.ExtractQuant(primary.Filename)
	// quantizations has no CreateQuantization write path (engine/quant/
	// format are static-seeded enums, not catalog CRUD entities) — a
	// label the seed table doesn't know registers with QuantizationID=0
	// (NULL, "unknown") rather than failing the whole registration; the
	// artifacts.quantization_id column is already nullable for exactly
	// this "we don't know" case elsewhere in the catalog.
	quantID := s.lookupQuantizationID(ctx, cat, quant)

	sizeBytes := primary.BytesTotal
	shardSetID := ""
	if s.isSharded(job.ID) {
		shardSetID = fmt.Sprintf("dl-%d", job.ID)
	}

	repoBase := creatorFromRepo(job.Repo)
	modelName := job.Repo
	if i := strings.Index(job.Repo, "/"); i > 0 {
		modelName = job.Repo[i+1:]
	}
	variantName := quant
	if variantName == "" {
		variantName = "default"
	}
	trainedCtx := md.TrainedCtx
	nCtx := trainedCtx
	if nCtx <= 0 {
		nCtx = 4096 // conservative fallback when the GGUF header carries no context_length KV
	}

	b := store.ModelBundle{
		Model: store.Model{
			Name: modelName, HFRepo: job.Repo, Creator: repoBase,
			Description: enrichment.Description, LicenseName: enrichment.LicenseName,
			Architecture: md.Architecture, ParameterCount: ggufParamString(md),
			Logo: enrichment.Logo, LogoDark: enrichment.LogoDark,
			KeyFeatures: enrichment.Tags, Visibility: "visible",
		},
		Variant: store.Variant{Name: variantName, TrainedCtx: trainedCtx},
		Artifact: store.Artifact{
			FormatID: ggufFormat.ID, QuantizationID: quantID,
			FilePath: primary.DestRelPath, ShardSetID: shardSetID,
			FileSizeBytes: sizeBytes, ArtifactType: "weight",
			GGUFArch: md.Architecture, GGUFTrainedCtx: md.TrainedCtx,
			GGUFParameterCount: ggufParamString(md), GGUFQuantType: md.QuantType,
			SHA256: sha256OrEmpty(primary),
		},
		Config: store.Config{
			Name: slugify(modelName) + "-" + slugify(variantName),
			EngineID: engine.ID, NCtx: nCtx, Parallel: 1,
			ExtraArgs:  []string{"--no-mmap", "--jinja", "--flash-attn", "on", "--threads", "16"},
			Status:     "unverified",
			Visibility: "hidden",
		},
	}
	if buildID := s.resolveBuildID(ctx, cat, sizeBytes); buildID != 0 {
		b.Config.BuildID = buildID
	}
	if mmprojFile != nil {
		mmProjMD, mmErr := gguf.ReadMetadata(filepath.Join(s.d.Cfg().Paths.ModelsDir, mmprojFile.DestRelPath))
		if mmErr == nil {
			b.MMProj = &store.Artifact{
				FormatID: ggufFormat.ID, FilePath: mmprojFile.DestRelPath,
				FileSizeBytes: mmprojFile.BytesTotal, ArtifactType: "mmproj",
				GGUFArch: mmProjMD.Architecture, GGUFTrainedCtx: mmProjMD.TrainedCtx,
				GGUFParameterCount: ggufParamString(mmProjMD), GGUFQuantType: mmProjMD.QuantType,
			}
		} else {
			s.d.logf("hfdownload: job %d: mmproj %s failed a GGUF re-read (skipping mmproj link): %v", job.ID, mmprojFile.Filename, mmErr)
		}
	}
	for _, f := range extraShards {
		b.ExtraArtifacts = append(b.ExtraArtifacts, store.Artifact{
			FormatID: ggufFormat.ID, QuantizationID: quantID,
			FilePath: f.DestRelPath, ShardSetID: shardSetID,
			FileSizeBytes: f.BytesTotal, ArtifactType: "weight",
			// Every shard of a real sharded GGUF carries the same top-level
			// metadata KVs (arch/ctx/params/quant) — reuse primary's md
			// rather than re-reading each trailing shard's header.
			GGUFArch: md.Architecture, GGUFTrainedCtx: md.TrainedCtx,
			GGUFParameterCount: ggufParamString(md), GGUFQuantType: md.QuantType,
			SHA256: f.SHA256Actual,
		})
	}
	return b, nil
}

// lookupQuantizationID resolves quant against the seeded vocabulary,
// normalizing the one known separator mismatch between this package's
// extractor (hyphenated "UD-Q4_K_XL") and migration 0008's seed
// (underscored "UD_Q4_K_XL") before giving up and returning 0 (unknown).
func (s *Service) lookupQuantizationID(ctx context.Context, cat store.Catalog, quant string) int64 {
	if quant == "" {
		return 0
	}
	if q, err := cat.QuantizationByName(ctx, quant); err == nil {
		return q.ID
	}
	normalized := strings.ReplaceAll(quant, "UD-", "UD_")
	if normalized != quant {
		if q, err := cat.QuantizationByName(ctx, normalized); err == nil {
			return q.ID
		}
	}
	return 0
}

// resolveBuildID picks a Build whose Backend matches what totalBytes
// requires (ROCm above the Vulkan ceiling, Vulkan otherwise) — never
// derived from a name substring (store.Build.Backend's own doc comment
// records the live incident that rule prevents). Returns 0 (NULL) when no
// matching Build exists; a human still has to install one, but the
// generated Config at least never claims the wrong backend.
func (s *Service) resolveBuildID(ctx context.Context, cat store.Catalog, totalBytes int64) int64 {
	want := "vulkan"
	if totalBytes > vulkanCeilingBytes {
		want = "rocm"
	}
	builds, err := cat.ListBuilds(ctx)
	if err != nil {
		return 0
	}
	for _, b := range builds {
		if b.Backend == want {
			return b.ID
		}
	}
	return 0
}

func (s *Service) isSharded(jobID int64) bool {
	files, err := s.d.Store.ModelDownloads().ListFiles(context.Background(), jobID)
	if err != nil {
		return false
	}
	n := 0
	for _, f := range files {
		if !strings.Contains(strings.ToLower(f.Filename), "mmproj") {
			n++
		}
	}
	return n > 1
}

func ggufParamString(md gguf.Metadata) string {
	if md.ParameterCount <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", md.ParameterCount)
}

func sha256OrEmpty(f *store.ModelDownloadFileRow) string {
	if f == nil {
		return ""
	}
	return f.SHA256Actual
}

// slugify lowercases s and replaces every run of non-alphanumeric
// characters with a single hyphen, trimming leading/trailing hyphens —
// used to build a stable, human-legible configs.name.
func slugify(s string) string {
	var b strings.Builder
	prevDash := true // suppress a leading hyphen
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// repointExistingConfig is fetch_model's old single-file behavior,
// preserved as an option (job.ConfigName != ""): repoint an EXISTING
// Config's weight artifact at the newly downloaded file rather than
// registering a brand-new Model.
func (s *Service) repointExistingConfig(ctx context.Context, job store.ModelDownloadRow, primary *store.ModelDownloadFileRow, md gguf.Metadata) error {
	cat := s.d.Store.Catalog()
	cfgRow, err := cat.ConfigByName(ctx, job.ConfigName)
	if err != nil {
		return fmt.Errorf("resolve config %q: %w", job.ConfigName, err)
	}
	art, err := cat.GetArtifact(ctx, cfgRow.WeightArtifactID)
	if err != nil {
		return fmt.Errorf("weight artifact for config %q: %w", job.ConfigName, err)
	}
	art.FilePath = primary.DestRelPath
	art.FileSizeBytes = primary.BytesTotal
	art.SHA256 = sha256OrEmpty(primary)
	if strings.HasSuffix(strings.ToLower(primary.Filename), ".gguf") {
		art.GGUFArch = md.Architecture
		art.GGUFTrainedCtx = md.TrainedCtx
		art.GGUFParameterCount = ggufParamString(md)
		art.GGUFQuantType = md.QuantType
	}
	if err := cat.UpdateArtifact(ctx, art); err != nil {
		return fmt.Errorf("repoint artifact for config %q: %w", job.ConfigName, err)
	}
	return nil
}
