// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_handlers.go — Sprint MODEL CATALOG Phase 3 (docs/v5-modes-config-editable.md
// §Phase 3). CRUD APIs for the model catalog: models, variants, configs,
// offerings, benchmarks, notes, services + the filesystem browse endpoint for
// the Config editor's model picker + icon upload.
//
// All mutating routes are requireRole(admin) + requireAssurance(page.settings).
// The InvalidateConfig hook (Deps.InvalidateConfig) is called after any
// mutation to configs/variants/models/artifacts/services/offerings so the
// merged-config seam picks up the change immediately (the 5s TTL otherwise
// delays visibility).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// ── Wire shapes ──────────────────────────────────────────────────────────────

// genealogyJSON mirrors store.Genealogy — the level above Family
// (product/QA sprint, 2026-07-29; see store.Genealogy's doc comment).
type genealogyJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Logo is an Icon manifest slug or an uploaded data-URL (Sprint I — icon
	// inheritance hierarchy). Inherited by families/models/configs that
	// don't set their own; see registry.resolveLogos.
	Logo string `json:"logo"`
	// LogoDark is a dark-theme variant override (Phase 3); "" falls back to
	// Logo. Same inheritance chain, resolved together — see
	// registry.resolveLogos's doc comment.
	LogoDark string `json:"logo_dark"`
}

type familyJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	GenealogyID int64  `json:"genealogy_id"`
	// Logo — see genealogyJSON.Logo's doc comment; same inheritance chain.
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
}

type modelJSON struct {
	ID             int64    `json:"id"`
	FamilyID       int64    `json:"family_id"`
	Name           string   `json:"name"`
	Architecture   string   `json:"architecture"`
	ParameterCount string   `json:"parameter_count"`
	Description    string   `json:"description"`
	Creator        string   `json:"creator"`
	LicenseName    string   `json:"license_name"`
	LicenseURL     string   `json:"license_url"`
	HFRepo         string   `json:"hf_repo"`
	// Logo is an Icon manifest slug (web/src/assets/icons/manifest.ts) or an
	// uploaded data-URL (PUT …/icon with paths.icons_dir unset — Sprint A2).
	Logo           string   `json:"logo"`
	LogoDark       string   `json:"logo_dark"` // dark-theme override; "" falls back to Logo
	KeyFeatures    []string `json:"key_features"`
	// Modalities (Sprint J1) is what this model's architecture supports:
	// a subset of text/vision/audio. See store.Model.Modalities's doc
	// comment and registry.resolveModalities for how a Config narrows this.
	Modalities []string `json:"modalities"`
	// Visibility is visible (Models gallery) or hidden (Settings only) —
	// model-level decommission flag (0062). Default "visible".
	Visibility string `json:"visibility"`
}

type variantJSON struct {
	ID                  int64  `json:"id"`
	ModelID             int64  `json:"model_id"`
	Name                string `json:"name"`
	DerivationType      string `json:"derivation_type"`
	SourceVariantID     int64  `json:"source_variant_id"`
	TrainedCtx          int    `json:"trained_ctx"`
	IsAbliterated       bool   `json:"is_abliterated"`
	AbliterationQuality string `json:"abliteration_quality"`
}

type artifactJSON struct {
	ID                 int64  `json:"id"`
	VariantID          int64  `json:"variant_id"`
	QuantizationID     int64  `json:"quantization_id"`
	FormatID           int64  `json:"format_id"`
	FilePath           string `json:"file_path"`
	ShardSetID         string `json:"shard_set_id"`
	IsAuxiliary        bool   `json:"is_auxiliary"`
	ArtifactType       string `json:"artifact_type"`
	Missing            bool   `json:"missing"`
	SHA256             string `json:"sha256"`
	FileSizeBytes      int64  `json:"file_size_bytes"`
	GGUFArch           string `json:"gguf_arch"`
	GGUFTrainedCtx     int    `json:"gguf_trained_ctx"`
	GGUFParameterCount string `json:"gguf_parameter_count"`
	GGUFQuantType      string `json:"gguf_quant_type"`
}

type engineJSON struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type buildJSON struct {
	ID         int64  `json:"id"`
	EngineID   int64  `json:"engine_id"`
	Name       string `json:"name"`
	BinaryPath string `json:"binary_path"`
	Backend    string `json:"backend"`
	Reason     string `json:"reason"`
}

type configJSON struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	EngineID         int64    `json:"engine_id"`
	BuildID          int64    `json:"build_id"`
	MMProjArtifactID int64    `json:"mmproj_artifact_id"`
	NCtx             int      `json:"n_ctx"`
	Parallel         int      `json:"parallel"`
	ExtraArgs        []string `json:"extra_args"`
	Status           string   `json:"status"`
	Visibility       string   `json:"visibility"`
	IsDefault        bool     `json:"is_default"`
	Fingerprint      string   `json:"fingerprint"`
	// Logo is a config-level icon override (Sprint I) — wins over the
	// model/family/genealogy chain when set; "" inherits. See
	// genealogyJSON.Logo's doc comment.
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
	// Modalities overrides the model's default when this config can't
	// deliver everything the model architecturally supports (missing
	// mmproj, a build lacking mtmd support, a quantization that stripped a
	// modality). nil = derive (store.Config.Modalities's doc comment);
	// omitzero keeps a derive-mode config's response body clean instead of
	// emitting a literal `"modalities":null` on every card-adjacent config
	// read.
	Modalities *[]string `json:"modalities,omitzero"`
}

type offeringJSON struct {
	ID                 int64    `json:"id"`
	ModelID            int64    `json:"model_id"`
	VariantID          int64    `json:"variant_id"`
	Provider           string   `json:"provider"`
	WireModel          string   `json:"wire_model"`
	PriceInPer1M       float64  `json:"price_in_per_1m"`
	PriceOutPer1M      float64  `json:"price_out_per_1m"`
	PriceCachedInPer1M *float64 `json:"price_cached_in_per_1m,omitempty"`
	Currency           string   `json:"currency"`
	ContextLength      int      `json:"context_length"`
	Enabled            bool     `json:"enabled"`
	// Priority (multi-provider routing sprint, 2026-08-06): preference rank
	// among the offerings of one model — lowest value wins; see
	// store.Offering.Priority.
	Priority int `json:"priority"`
}

type benchmarkJSON struct {
	ID          int64  `json:"id"`
	Metric      string `json:"metric"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	SourceURL   string `json:"source_url"`
	SourceDate  string `json:"source_date"`
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Notes       string `json:"notes"`
}

type noteJSON struct {
	ID          int64  `json:"id"`
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Author      string `json:"author"`
	Body        string `json:"body"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type serviceJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Color       string `json:"color"`
	Unit        string `json:"unit"`
	HealthCheck string `json:"health_check"`
}

type modelFileJSON struct {
	Path        string  `json:"path"`
	SizeBytes   int64   `json:"size_bytes"`
	Arch        string  `json:"arch"`
	TrainedCtx  int     `json:"trained_ctx"`
	IsShardSet  bool    `json:"is_shard_set"`
}

// ── Conversion helpers ───────────────────────────────────────────────────────

func familyToJSON(f store.Family) familyJSON {
	return familyJSON{ID: f.ID, Name: f.Name, GenealogyID: f.GenealogyID, Logo: f.Logo, LogoDark: f.LogoDark}
}

func genealogyToJSON(g store.Genealogy) genealogyJSON {
	return genealogyJSON{ID: g.ID, Name: g.Name, Logo: g.Logo, LogoDark: g.LogoDark}
}

func modelToJSON(m store.Model) modelJSON {
	return modelJSON{
		ID: m.ID, FamilyID: m.FamilyID, Name: m.Name, Architecture: m.Architecture,
		ParameterCount: m.ParameterCount, Description: m.Description, Creator: m.Creator,
		LicenseName: m.LicenseName, LicenseURL: m.LicenseURL, HFRepo: m.HFRepo,
		Logo: m.Logo, LogoDark: m.LogoDark, KeyFeatures: m.KeyFeatures, Modalities: m.Modalities,
		Visibility: m.Visibility,
	}
}

func variantToJSON(v store.Variant) variantJSON {
	return variantJSON{
		ID: v.ID, ModelID: v.ModelID, Name: v.Name, DerivationType: v.DerivationType,
		SourceVariantID: v.SourceVariantID, TrainedCtx: v.TrainedCtx,
		IsAbliterated: v.IsAbliterated, AbliterationQuality: v.AbliterationQuality,
	}
}

func artifactToJSON(a store.Artifact) artifactJSON {
	return artifactJSON{
		ID: a.ID, VariantID: a.VariantID, QuantizationID: a.QuantizationID,
		FormatID: a.FormatID, FilePath: a.FilePath, ShardSetID: a.ShardSetID,
		IsAuxiliary: a.IsAuxiliary, ArtifactType: a.ArtifactType, Missing: a.Missing,
		SHA256: a.SHA256, FileSizeBytes: a.FileSizeBytes, GGUFArch: a.GGUFArch,
		GGUFTrainedCtx: a.GGUFTrainedCtx, GGUFParameterCount: a.GGUFParameterCount,
		GGUFQuantType: a.GGUFQuantType,
	}
}

func engineToJSON(e store.Engine) engineJSON {
	return engineJSON{ID: e.ID, Name: e.Name}
}

func buildToJSON(b store.Build) buildJSON {
	return buildJSON{ID: b.ID, EngineID: b.EngineID, Name: b.Name,
		BinaryPath: b.BinaryPath, Backend: b.Backend, Reason: b.Reason}
}

func configToJSON(c store.Config) configJSON {
	return configJSON{
		ID: c.ID, Name: c.Name, VariantID: c.VariantID,
		WeightArtifactID: c.WeightArtifactID, EngineID: c.EngineID, BuildID: c.BuildID,
		MMProjArtifactID: c.MMProjArtifactID, NCtx: c.NCtx, Parallel: c.Parallel,
		ExtraArgs: c.ExtraArgs, Status: c.Status, Visibility: c.Visibility,
		IsDefault: c.IsDefault, Fingerprint: c.Fingerprint, Logo: c.Logo, LogoDark: c.LogoDark,
		Modalities: c.Modalities,
	}
}

func offeringToJSON(o store.Offering) offeringJSON {
	return offeringJSON{
		ID: o.ID, ModelID: o.ModelID, VariantID: o.VariantID, Provider: o.ProviderName,
		WireModel: o.WireModel, PriceInPer1M: o.PriceInPer1M,
		PriceOutPer1M: o.PriceOutPer1M, PriceCachedInPer1M: o.PriceCachedInPer1M,
		Currency: o.Currency, ContextLength: o.ContextLength, Enabled: o.Enabled,
		Priority: o.Priority,
	}
}

func benchmarkToJSON(b store.Benchmark) benchmarkJSON {
	return benchmarkJSON{
		ID: b.ID, Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate, SubjectType: b.SubjectType,
		SubjectID: b.SubjectID, Notes: b.Notes,
	}
}

func noteToJSON(n store.Note) noteJSON {
	return noteJSON{
		ID: n.ID, SubjectType: n.SubjectType, SubjectID: n.SubjectID,
		Author: n.Author, Body: n.Body,
		CreatedAt: n.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: n.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func serviceToJSON(s store.Service) serviceJSON {
	return serviceJSON{
		ID: s.ID, Name: s.Name, Label: s.Label, Description: s.Description,
		Icon: s.Icon, Color: s.Color, Unit: s.Unit, HealthCheck: s.HealthCheck,
	}
}

// parseID extracts the {id} path value as an int64. Returns false on parse
// failure.
func parseID(r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// invalidateCfg calls the InvalidateConfig hook if wired (after mutations).
func (s *Server) invalidateCfg() {
	if s.deps.InvalidateConfig != nil {
		s.deps.InvalidateConfig()
	}
}

// catalogCtx returns a context with a 5s timeout for catalog DB operations.
func catalogCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// ── Filesystem browse: GET /api/v1/models/files ──────────────────────────────

// handleModelFiles — walks cfg.Paths.ModelsDir recursively, shard-aware,
// returns {path, sizeMB, arch, trainedCtx, isShardSet} per GGUF. This is the
// model picker for the Config editor (Q1 of the grill — the registry is card
// data, not file discovery).
func (s *Server) handleModelFiles(w http.ResponseWriter, r *http.Request) {
	dir := s.deps.Config().Paths.ModelsDir
	if dir == "" {
		writeJSON(w, http.StatusOK, []modelFileJSON{})
		return
	}

	var files []modelFileJSON
	shardSeen := map[string]bool{}

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".gguf") {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)

		// Shard detection: filenames like "model-00001-of-00005.gguf".
		shardID, isShard := shardSetID(rel)
		if isShard {
			if shardSeen[shardID] {
				return nil // already reported this shard set
			}
			shardSeen[shardID] = true
		}

		fi, err := os.Stat(path)
		if err != nil {
			return nil
		}
		sizeBytes := fi.Size()

		// Read GGUF header metadata (arch + trained_ctx). Non-fatal on error.
		var arch string
		var trainedCtx int
		if md, err := gguf.ReadMetadata(path); err == nil {
			arch = md.Architecture
			trainedCtx = md.TrainedCtx
		}

		files = append(files, modelFileJSON{
			Path:       rel,
			SizeBytes:  sizeBytes,
			Arch:       arch,
			TrainedCtx: trainedCtx,
			IsShardSet: isShard,
		})
		return nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models dir walk failed")
		return
	}

	if files == nil {
		files = []modelFileJSON{}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	writeJSON(w, http.StatusOK, files)
}

// shardSetID extracts the shard-set identifier from a filename like
// "model-00001-of-00005.gguf" → "model". Returns ("", false) for non-shard
// files.
func shardSetID(filename string) (string, bool) {
	base := strings.TrimSuffix(filename, ".gguf")
	base = strings.TrimSuffix(base, ".GGUF")
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return "", false
	}
	tail := base[idx+1:]
	if !strings.HasPrefix(tail, "0000") && !strings.HasPrefix(tail, "0001") {
		return "", false
	}
	// Must match "-NNNNN-of-NNNNN" pattern.
	parts := strings.SplitN(base, "-of-", 2)
	if len(parts) != 2 {
		return "", false
	}
	prefix := parts[0]
	// Strip the trailing "-NNNNN" from prefix.
	lastDash := strings.LastIndex(prefix, "-")
	if lastDash < 0 {
		return "", false
	}
	return prefix[:lastDash], true
}

// ── Enum list handlers ───────────────────────────────────────────────────────

func (s *Server) handleCatalogFamiliesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []familyJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListFamilies(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "families query failed")
		return
	}
	out := make([]familyJSON, 0, len(list))
	for _, f := range list {
		out = append(out, familyToJSON(f))
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Family CRUD (product/QA sprint, 2026-07-29 — previously list-only) ─────
// Mirrors the Model CRUD pattern above exactly.

type familyBody struct {
	Name        string `json:"name"`
	GenealogyID int64  `json:"genealogy_id"`
	Logo        string `json:"logo"`
	LogoDark    string `json:"logo_dark"`
}

func (s *Server) validateFamily(ctx context.Context, b familyBody) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	if s.deps.Catalog != nil && b.GenealogyID != 0 {
		if _, err := s.deps.Catalog.GetGenealogy(ctx, b.GenealogyID); err != nil {
			fields["genealogy_id"] = "does not exist"
		}
	}
	return fields
}

func (s *Server) handleCatalogFamilyGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	f, err := cat.GetFamily(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "family not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, familyToJSON(f))
}

func (s *Server) handleCatalogFamilyCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b familyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateFamily(r.Context(), b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateFamily(ctx, store.Family{Name: b.Name, GenealogyID: b.GenealogyID, Logo: b.Logo, LogoDark: b.LogoDark})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_family_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	f, _ := cat.GetFamily(ctx, id)
	writeJSON(w, http.StatusCreated, familyToJSON(f))
}

func (s *Server) handleCatalogFamilyUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b familyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateFamily(r.Context(), b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.UpdateFamily(ctx, store.Family{ID: id, Name: b.Name, GenealogyID: b.GenealogyID, Logo: b.Logo, LogoDark: b.LogoDark}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "family not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_family_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	f, _ := cat.GetFamily(ctx, id)
	writeJSON(w, http.StatusOK, familyToJSON(f))
}

// handleCatalogFamilyDelete deletes a family. Models referencing it fall
// back to no family (ON DELETE SET NULL) rather than being refused or
// cascade-deleted — a family is an organizational label, not ownership
// (unlike a model→variant relationship, which is refused with dependents).
func (s *Server) handleCatalogFamilyDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteFamily(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "family not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_family_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Genealogy CRUD (product/QA sprint, 2026-07-29) ──────────────────────────

type genealogyBody struct {
	Name     string `json:"name"`
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
}

func validateGenealogy(b genealogyBody) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	return fields
}

func (s *Server) handleCatalogGenealogiesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []genealogyJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListGenealogies(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "genealogies query failed")
		return
	}
	out := make([]genealogyJSON, 0, len(list))
	for _, g := range list {
		out = append(out, genealogyToJSON(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogGenealogyGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	g, err := cat.GetGenealogy(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "genealogy not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, genealogyToJSON(g))
}

func (s *Server) handleCatalogGenealogyCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b genealogyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateGenealogy(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateGenealogy(ctx, store.Genealogy{Name: b.Name, Logo: b.Logo, LogoDark: b.LogoDark})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_genealogy_create", strconv.FormatInt(id, 10), b.Name)
	g, _ := cat.GetGenealogy(ctx, id)
	writeJSON(w, http.StatusCreated, genealogyToJSON(g))
}

func (s *Server) handleCatalogGenealogyUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b genealogyBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateGenealogy(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.UpdateGenealogy(ctx, store.Genealogy{ID: id, Name: b.Name, Logo: b.Logo, LogoDark: b.LogoDark}); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "genealogy not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_genealogy_update", strconv.FormatInt(id, 10), b.Name)
	g, _ := cat.GetGenealogy(ctx, id)
	writeJSON(w, http.StatusOK, genealogyToJSON(g))
}

// handleCatalogGenealogyDelete deletes a genealogy. Families referencing it
// fall back to no genealogy (ON DELETE SET NULL) rather than being refused.
func (s *Server) handleCatalogGenealogyDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteGenealogy(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "genealogy not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_genealogy_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogQuantizationsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []store.Quantization{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListQuantizations(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "quantizations query failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCatalogFormatsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []store.Format{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListFormats(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "formats query failed")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCatalogEnginesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []engineJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListEngines(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "engines query failed")
		return
	}
	out := make([]engineJSON, 0, len(list))
	for _, e := range list {
		out = append(out, engineToJSON(e))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogBuildsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []buildJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListBuilds(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "builds query failed")
		return
	}
	out := make([]buildJSON, 0, len(list))
	for _, b := range list {
		out = append(out, buildToJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogArtifactsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []artifactJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Artifact
	if vid := r.URL.Query().Get("variant_id"); vid != "" {
		id, err := strconv.ParseInt(vid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"variant_id": "must be an integer"})
			return
		}
		list, err = cat.ListArtifactsForVariant(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "artifacts query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListArtifacts(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "artifacts query failed")
			return
		}
	}
	out := make([]artifactJSON, 0, len(list))
	for _, a := range list {
		out = append(out, artifactToJSON(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Model CRUD ────────────────────────────────────────────────────────────────

func (s *Server) handleCatalogModelsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []modelJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListModels(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models query failed")
		return
	}
	out := make([]modelJSON, 0, len(list))
	for _, m := range list {
		out = append(out, modelToJSON(m))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogModelGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	m, err := cat.GetModel(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, modelToJSON(m))
}

func (s *Server) handleCatalogModelCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b modelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateModel(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateModel(ctx, store.Model{
		FamilyID: b.FamilyID, Name: b.Name, Architecture: b.Architecture,
		ParameterCount: b.ParameterCount, Description: b.Description, Creator: b.Creator,
		LicenseName: b.LicenseName, LicenseURL: b.LicenseURL, HFRepo: b.HFRepo,
		Logo: b.Logo, LogoDark: b.LogoDark, KeyFeatures: b.KeyFeatures, Modalities: b.Modalities,
		Visibility: b.Visibility,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_create", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	m, _ := cat.GetModel(ctx, id)
	writeJSON(w, http.StatusCreated, modelToJSON(m))
}

func (s *Server) handleCatalogModelUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b modelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateModel(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateModel(ctx, store.Model{
		ID: id, FamilyID: b.FamilyID, Name: b.Name, Architecture: b.Architecture,
		ParameterCount: b.ParameterCount, Description: b.Description, Creator: b.Creator,
		LicenseName: b.LicenseName, LicenseURL: b.LicenseURL, HFRepo: b.HFRepo,
		Logo: b.Logo, LogoDark: b.LogoDark, KeyFeatures: b.KeyFeatures, Modalities: b.Modalities,
		Visibility: b.Visibility,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_update", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	m, _ := cat.GetModel(ctx, id)
	writeJSON(w, http.StatusOK, modelToJSON(m))
}

func (s *Server) handleCatalogModelDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteModel(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "model not found")
			return
		}
		writeError(w, http.StatusConflict, "model has dependent variants — delete those first")
		return
	}
	s.audit(r, identity(r).Name, "catalog_model_delete", strconv.FormatInt(id, 10), withReason("", r.URL.Query().Get("reason")))
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogModelValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b modelBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateModel(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// ── Variant CRUD ─────────────────────────────────────────────────────────────

func (s *Server) handleCatalogVariantsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []variantJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Variant
	if mid := r.URL.Query().Get("model_id"); mid != "" {
		id, err := strconv.ParseInt(mid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"model_id": "must be an integer"})
			return
		}
		list, err = cat.ListVariantsForModel(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "variants query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListVariants(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "variants query failed")
			return
		}
	}
	out := make([]variantJSON, 0, len(list))
	for _, v := range list {
		out = append(out, variantToJSON(v))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogVariantGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	v, err := cat.GetVariant(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "variant not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, variantToJSON(v))
}

func (s *Server) handleCatalogVariantCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b variantBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateVariant(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateVariant(ctx, store.Variant{
		ModelID: b.ModelID, Name: b.Name, DerivationType: b.DerivationType,
		SourceVariantID: b.SourceVariantID, TrainedCtx: b.TrainedCtx,
		IsAbliterated: b.IsAbliterated, AbliterationQuality: b.AbliterationQuality,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_variant_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	v, _ := cat.GetVariant(ctx, id)
	writeJSON(w, http.StatusCreated, variantToJSON(v))
}

func (s *Server) handleCatalogVariantUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b variantBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateVariant(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateVariant(ctx, store.Variant{
		ID: id, ModelID: b.ModelID, Name: b.Name, DerivationType: b.DerivationType,
		SourceVariantID: b.SourceVariantID, TrainedCtx: b.TrainedCtx,
		IsAbliterated: b.IsAbliterated, AbliterationQuality: b.AbliterationQuality,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "variant not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_variant_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	v, _ := cat.GetVariant(ctx, id)
	writeJSON(w, http.StatusOK, variantToJSON(v))
}

func (s *Server) handleCatalogVariantDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteVariant(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "variant not found")
			return
		}
		writeError(w, http.StatusConflict, "variant has dependent configs — delete those first")
		return
	}
	s.audit(r, identity(r).Name, "catalog_variant_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogVariantValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b variantBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateVariant(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// ── Config CRUD ──────────────────────────────────────────────────────────────

func (s *Server) handleCatalogConfigsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []configJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Config
	if vid := r.URL.Query().Get("variant_id"); vid != "" {
		id, err := strconv.ParseInt(vid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"variant_id": "must be an integer"})
			return
		}
		list, err = cat.ListConfigsForVariant(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "configs query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListConfigs(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "configs query failed")
			return
		}
	}
	out := make([]configJSON, 0, len(list))
	for _, c := range list {
		out = append(out, configToJSON(c))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogConfigGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	c, err := cat.GetConfig(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, configToJSON(c))
}

func (s *Server) handleCatalogConfigCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b configBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateConfig(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateConfig(ctx, store.Config{
		Name: b.Name, VariantID: b.VariantID, WeightArtifactID: b.WeightArtifactID,
		EngineID: b.EngineID, BuildID: b.BuildID, MMProjArtifactID: b.MMProjArtifactID,
		NCtx: b.NCtx, Parallel: b.Parallel, ExtraArgs: b.ExtraArgs,
		Status: b.Status, Visibility: b.Visibility, IsDefault: b.IsDefault,
		Fingerprint: b.Fingerprint, Logo: b.Logo, LogoDark: b.LogoDark, Modalities: b.Modalities,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_create", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	c, _ := cat.GetConfig(ctx, id)
	writeJSON(w, http.StatusCreated, configToJSON(c))
}

func (s *Server) handleCatalogConfigUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b configBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateConfig(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateConfig(ctx, store.Config{
		ID: id, Name: b.Name, VariantID: b.VariantID, WeightArtifactID: b.WeightArtifactID,
		EngineID: b.EngineID, BuildID: b.BuildID, MMProjArtifactID: b.MMProjArtifactID,
		NCtx: b.NCtx, Parallel: b.Parallel, ExtraArgs: b.ExtraArgs,
		Status: b.Status, Visibility: b.Visibility, IsDefault: b.IsDefault,
		Fingerprint: b.Fingerprint, Logo: b.Logo, LogoDark: b.LogoDark, Modalities: b.Modalities,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_update", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	s.invalidateCfg()
	c, _ := cat.GetConfig(ctx, id)
	writeJSON(w, http.StatusOK, configToJSON(c))
}

func (s *Server) handleCatalogConfigDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	c, err := cat.GetConfig(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	// Delete semantics (Q4): refuse to delete a Config currently loaded unless
	// force=true is set.
	if !r.URL.Query().Has("force") && s.isConfigLoaded(c.Name) {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error":  "config is currently loaded",
			"config": c.Name,
		})
		return
	}
	if err := cat.DeleteConfig(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_delete", strconv.FormatInt(id, 10), withReason(c.Name, r.URL.Query().Get("reason")))
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogConfigValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b configBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateConfig(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// isConfigLoaded checks whether the collector snapshot shows any slot with
// this config name loaded.
func (s *Server) isConfigLoaded(name string) bool {
	if s.deps.Snapshots == nil {
		return false
	}
	snap := s.deps.Snapshots.Current()
	if snap == nil {
		return false
	}
	for _, ss := range snap.Slots {
		if ss.Mode == name {
			return true
		}
	}
	return false
}

// ── Offering CRUD ────────────────────────────────────────────────────────────

func (s *Server) handleCatalogOfferingsList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []offeringJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Offering
	if mid := r.URL.Query().Get("model_id"); mid != "" {
		id, err := strconv.ParseInt(mid, 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"model_id": "must be an integer"})
			return
		}
		list, err = cat.ListOfferingsForModel(ctx, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "offerings query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListOfferings(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "offerings query failed")
			return
		}
	}
	out := make([]offeringJSON, 0, len(list))
	for _, o := range list {
		out = append(out, offeringToJSON(o))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogOfferingGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	o, err := cat.GetOffering(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "offering not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, offeringToJSON(o))
}

func (s *Server) handleCatalogOfferingCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b offeringBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateOffering(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	providerID, ok := s.resolveOfferingProviderID(ctx, b.Provider)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "provider does not exist")
		return
	}
	priority := 100 // column DEFAULT — omitted body means "no preference"
	if b.Priority != nil {
		priority = *b.Priority
	}
	id, err := cat.CreateOffering(ctx, store.Offering{
		ModelID: b.ModelID, VariantID: b.VariantID, ProviderID: providerID,
		WireModel: b.WireModel, PriceInPer1M: b.PriceInPer1M,
		PriceOutPer1M: b.PriceOutPer1M, PriceCachedInPer1M: b.PriceCachedInPer1M,
		Currency: b.Currency, ContextLength: b.ContextLength, Enabled: b.Enabled,
		Priority: priority,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_offering_create", strconv.FormatInt(id, 10), b.WireModel)
	s.invalidateCfg()
	o, _ := cat.GetOffering(ctx, id)
	writeJSON(w, http.StatusCreated, offeringToJSON(o))
}

func (s *Server) handleCatalogOfferingUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b offeringBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateOffering(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	existing, err := cat.GetOffering(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "offering not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	providerID, ok := s.resolveOfferingProviderID(ctx, b.Provider)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "provider does not exist")
		return
	}
	priority := existing.Priority // omitted body field preserves the rank
	if b.Priority != nil {
		priority = *b.Priority
	}
	err = cat.UpdateOffering(ctx, store.Offering{
		ID: id, ModelID: b.ModelID, VariantID: b.VariantID, ProviderID: providerID,
		WireModel: b.WireModel, PriceInPer1M: b.PriceInPer1M,
		PriceOutPer1M: b.PriceOutPer1M, PriceCachedInPer1M: b.PriceCachedInPer1M,
		Currency: b.Currency, ContextLength: b.ContextLength, Enabled: b.Enabled,
		Priority: priority,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_offering_update", strconv.FormatInt(id, 10), b.WireModel)
	s.invalidateCfg()
	o, _ := cat.GetOffering(ctx, id)
	writeJSON(w, http.StatusOK, offeringToJSON(o))
}

func (s *Server) handleCatalogOfferingDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteOffering(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "offering not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_offering_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogOfferingValidate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b offeringBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateOffering(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// ── Benchmark CRUD ───────────────────────────────────────────────────────────

func (s *Server) handleCatalogBenchmarksList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []benchmarkJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Benchmark
	if st := r.URL.Query().Get("subject_type"); st != "" {
		sid, err := strconv.ParseInt(r.URL.Query().Get("subject_id"), 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"subject_id": "must be an integer"})
			return
		}
		list, err = cat.ListBenchmarksForSubject(ctx, st, sid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "benchmarks query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListBenchmarks(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "benchmarks query failed")
			return
		}
	}
	out := make([]benchmarkJSON, 0, len(list))
	for _, b := range list {
		out = append(out, benchmarkToJSON(b))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogBenchmarkGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	b, err := cat.GetBenchmark(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, benchmarkToJSON(b))
}

func (s *Server) handleCatalogBenchmarkCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b benchmarkBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateBenchmark(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateBenchmark(ctx, store.Benchmark{
		Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate,
		SubjectType: b.SubjectType, SubjectID: b.SubjectID, Notes: b.Notes,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_benchmark_create", strconv.FormatInt(id, 10), b.Metric)
	bm, _ := cat.GetBenchmark(ctx, id)
	writeJSON(w, http.StatusCreated, benchmarkToJSON(bm))
}

func (s *Server) handleCatalogBenchmarkUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b benchmarkBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	// Fetched before validation (not just after, as every other update
	// handler in this file does) so validateBenchmarkUpdate can grandfather
	// a pre-existing offering-scoped row — see its doc comment.
	existing, err := cat.GetBenchmark(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	if fields := validateBenchmarkUpdate(b, existing); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	err = cat.UpdateBenchmark(ctx, store.Benchmark{
		ID: id, Metric: b.Metric, Value: b.Value, Source: b.Source,
		SourceURL: b.SourceURL, SourceDate: b.SourceDate,
		SubjectType: b.SubjectType, SubjectID: b.SubjectID, Notes: b.Notes,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_benchmark_update", strconv.FormatInt(id, 10), b.Metric)
	bm, _ := cat.GetBenchmark(ctx, id)
	writeJSON(w, http.StatusOK, benchmarkToJSON(bm))
}

func (s *Server) handleCatalogBenchmarkDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteBenchmark(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "benchmark not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_benchmark_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogBenchmarkValidate(w http.ResponseWriter, r *http.Request) {
	var b benchmarkBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateBenchmark(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// ── Note CRUD ────────────────────────────────────────────────────────────────

func (s *Server) handleCatalogNotesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []noteJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	var list []store.Note
	if st := r.URL.Query().Get("subject_type"); st != "" {
		sid, err := strconv.ParseInt(r.URL.Query().Get("subject_id"), 10, 64)
		if err != nil {
			writeValidationError(w, map[string]string{"subject_id": "must be an integer"})
			return
		}
		list, err = cat.ListNotesForSubject(ctx, st, sid)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "notes query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListNotes(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "notes query failed")
			return
		}
	}
	out := make([]noteJSON, 0, len(list))
	for _, n := range list {
		out = append(out, noteToJSON(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogNoteGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	n, err := cat.GetNote(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "note not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, noteToJSON(n))
}

func (s *Server) handleCatalogNoteCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b noteBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateNote(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateNote(ctx, store.Note{
		SubjectType: b.SubjectType, SubjectID: b.SubjectID,
		Author: b.Author, Body: b.Body,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_note_create", strconv.FormatInt(id, 10), "")
	n, _ := cat.GetNote(ctx, id)
	writeJSON(w, http.StatusCreated, noteToJSON(n))
}

func (s *Server) handleCatalogNoteUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b noteBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := validateNote(b); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateNote(ctx, store.Note{
		ID: id, SubjectType: b.SubjectType, SubjectID: b.SubjectID,
		Author: b.Author, Body: b.Body,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "note not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_note_update", strconv.FormatInt(id, 10), "")
	n, _ := cat.GetNote(ctx, id)
	writeJSON(w, http.StatusOK, noteToJSON(n))
}

func (s *Server) handleCatalogNoteDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteNote(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "note not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_note_delete", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Service CRUD ──────────────────────────────────────────────────────────────

func (s *Server) handleCatalogServicesList(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeJSON(w, http.StatusOK, []serviceJSON{})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	list, err := cat.ListServices(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "services query failed")
		return
	}
	out := make([]serviceJSON, 0, len(list))
	for _, sv := range list {
		out = append(out, serviceToJSON(sv))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCatalogServiceGet(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	sv, err := cat.GetService(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, serviceToJSON(sv))
}

func (s *Server) handleCatalogServiceCreate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	var b serviceBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateService(r.Context(), b, 0); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	id, err := cat.CreateService(ctx, store.Service{
		Name: b.Name, Label: b.Label, Description: b.Description,
		Icon: b.Icon, Color: b.Color, Unit: b.Unit, HealthCheck: b.HealthCheck,
	})
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_service_create", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	sv, _ := cat.GetService(ctx, id)
	writeJSON(w, http.StatusCreated, serviceToJSON(sv))
}

func (s *Server) handleCatalogServiceUpdate(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	var b serviceBody
	if fields := decodeJSONBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	if fields := s.validateService(r.Context(), b, id); len(fields) > 0 {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	err := cat.UpdateService(ctx, store.Service{
		ID: id, Name: b.Name, Label: b.Label, Description: b.Description,
		Icon: b.Icon, Color: b.Color, Unit: b.Unit, HealthCheck: b.HealthCheck,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_service_update", strconv.FormatInt(id, 10), b.Name)
	s.invalidateCfg()
	sv, _ := cat.GetService(ctx, id)
	writeJSON(w, http.StatusOK, serviceToJSON(sv))
}

func (s *Server) handleCatalogServiceDelete(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	if err := cat.DeleteService(ctx, id); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "service not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_service_delete", strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Icon upload: PUT /api/v1/catalog/{genealogies,families,models,configs}/{id}/icon ──
// Sprint I generalizes the original model-only upload handler to all four
// levels of the icon inheritance chain (registry.resolveLogos) — same mime
// whitelist, size cap, and IconsDir-vs-data-URL fallback for every subject.
// Phase 3 adds a light/dark variant selector (?variant=dark), rather than a
// second endpoint, since everything except which column gets written and
// which on-disk filename suffix is used is identical.

var allowedIconTypes = map[string]bool{
	"image/webp": true, "image/png": true, "image/jpeg": true,
	"image/svg+xml": true, "video/webm": true,
}

const maxIconSize = 1 << 20 // 1 MB

// handleCatalogIconUpload is the shared implementation. subject names the
// on-disk filename prefix and audit action ("model", "family", "genealogy",
// "config"); setLogo/setLogoDark persist the resolved logo value for id —
// which one runs is chosen by the request's ?variant=light|dark query param
// (default light) so the two existing single-purpose setters (Set*Logo,
// Set*LogoDark) don't need a combined-write variant of their own.
func (s *Server) handleCatalogIconUpload(w http.ResponseWriter, r *http.Request, subject, notFoundMsg string, setLogo, setLogoDark func(ctx context.Context, id int64, logo string) error) {
	id, ok := parseID(r)
	if !ok {
		writeValidationError(w, map[string]string{"id": "must be an integer"})
		return
	}
	dark := r.URL.Query().Get("variant") == "dark"
	if err := r.ParseMultipartForm(maxIconSize); err != nil {
		writeValidationError(w, map[string]string{"file": "must be ≤1 MB"})
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeValidationError(w, map[string]string{"file": "is required (multipart field 'file')"})
		return
	}
	defer file.Close()

	ct := header.Header.Get("Content-Type")
	if ct == "" {
		ext := strings.ToLower(filepath.Ext(header.Filename))
		ct = extToContentType(ext)
	}
	if !allowedIconTypes[ct] {
		writeValidationError(w, map[string]string{"file": "must be WebM, JPG, PNG, WebP, or SVG"})
		return
	}

	// Read the file content (already capped at maxIconSize by ParseMultipartForm).
	// io.ReadAll, not a single Read: Read is not guaranteed to fill the buffer.
	data, err := io.ReadAll(file)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to read icon")
		return
	}

	// Save to IconsDir if configured, else store as a data URL in the logo field.
	// The dark variant gets its own filename suffix so it never collides
	// with the light file for the same subject/id.
	var logo string
	iconsDir := s.deps.Config().Paths.IconsDir
	if iconsDir != "" {
		ext := filepath.Ext(header.Filename)
		if ext == "" {
			ext = contentTypeToExt(ct)
		}
		suffix := ""
		if dark {
			suffix = "-dark"
		}
		filename := fmt.Sprintf("%s-%d%s%s", subject, id, suffix, ext)
		dst := filepath.Join(iconsDir, filename)
		if err := os.MkdirAll(iconsDir, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create icons dir")
			return
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to write icon")
			return
		}
		logo = filename
	} else {
		logo = "data:" + ct + ";base64," + base64Encode(data)
	}

	ctx, cancel := catalogCtx(r)
	defer cancel()
	set := setLogo
	auditSuffix := ""
	if dark {
		set = setLogoDark
		auditSuffix = "_dark"
	}
	if err := set(ctx, id, logo); err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, notFoundMsg)
			return
		}
		writeInternalError(w, err)
		return
	}
	s.audit(r, identity(r).Name, "catalog_"+subject+"_icon"+auditSuffix, strconv.FormatInt(id, 10), "")
	s.invalidateCfg()
	key := "logo"
	if dark {
		key = "logo_dark"
	}
	writeJSON(w, http.StatusOK, map[string]string{key: logo})
}

func (s *Server) handleCatalogModelIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "model", "model not found", cat.SetModelLogo, cat.SetModelLogoDark)
}

func (s *Server) handleCatalogFamilyIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "family", "family not found", cat.SetFamilyLogo, cat.SetFamilyLogoDark)
}

func (s *Server) handleCatalogGenealogyIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "genealogy", "genealogy not found", cat.SetGenealogyLogo, cat.SetGenealogyLogoDark)
}

func (s *Server) handleCatalogConfigIcon(w http.ResponseWriter, r *http.Request) {
	cat := s.deps.Catalog
	if cat == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	s.handleCatalogIconUpload(w, r, "config", "config not found", cat.SetConfigLogo, cat.SetConfigLogoDark)
}

func extToContentType(ext string) string {
	switch ext {
	case ".webp":
		return "image/webp"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".svg":
		return "image/svg+xml"
	case ".webm":
		return "video/webm"
	}
	return ""
}

func contentTypeToExt(ct string) string {
	switch ct {
	case "image/webp":
		return ".webp"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/svg+xml":
		return ".svg"
	case "video/webm":
		return ".webm"
	}
	return ".bin"
}

func base64Encode(data []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(data); i += 3 {
		b1 := data[i]
		var b2, b3 byte
		has2, has3 := i+1 < len(data), i+2 < len(data)
		if has2 {
			b2 = data[i+1]
		}
		if has3 {
			b3 = data[i+2]
		}
		sb.WriteByte(chars[b1>>2])
		sb.WriteByte(chars[((b1&0x03)<<4)|(b2>>4)])
		if has2 {
			sb.WriteByte(chars[((b2&0x0f)<<2)|(b3>>6)])
		} else {
			sb.WriteByte('=')
		}
		if has3 {
			sb.WriteByte(chars[b3&0x3f])
		} else {
			sb.WriteByte('=')
		}
	}
	return sb.String()
}

// maxAuditReasonLen bounds an operator-supplied change-reason note before it
// is folded into audit_log.detail (Sprint C) — a free-text field gets the
// same treatment as any other operator string reaching an audit record,
// truncated rather than rejected so an over-long note never blocks a save.
const maxAuditReasonLen = 500

// withReason folds an optional operator-supplied "why this change" note
// into an audit detail string (Sprint C). Reason is never persisted on the
// model/config row itself — only here, in audit_log.detail — which is why
// it needs a real read surface (see handleAuditList) rather than being
// write-only ceremony. detail may be empty (e.g. handleCatalogModelDelete
// today).
func withReason(detail, reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return detail
	}
	if len(reason) > maxAuditReasonLen {
		reason = reason[:maxAuditReasonLen]
	}
	if detail == "" {
		return "reason: " + reason
	}
	return detail + " — reason: " + reason
}

// ── Request bodies ───────────────────────────────────────────────────────────

type modelBody struct {
	FamilyID       int64    `json:"family_id"`
	Name           string   `json:"name"`
	Architecture   string   `json:"architecture"`
	ParameterCount string   `json:"parameter_count"`
	Description    string   `json:"description"`
	Creator        string   `json:"creator"`
	LicenseName    string   `json:"license_name"`
	LicenseURL     string   `json:"license_url"`
	HFRepo         string   `json:"hf_repo"`
	// Logo is an Icon manifest slug (web/src/assets/icons/manifest.ts) or an
	// uploaded data-URL (PUT …/icon with paths.icons_dir unset — Sprint A2).
	Logo        string   `json:"logo"`
	LogoDark    string   `json:"logo_dark"`
	KeyFeatures []string `json:"key_features"`
	// Modalities (Sprint J1) is a subset of text/vision/audio -- validated
	// against that enum in validateModel. See modelJSON.Modalities.
	Modalities []string `json:"modalities"`
	// Visibility is visible (Models gallery) or hidden (Settings only).
	// Empty on create means "visible". Validated in validateModel.
	Visibility string `json:"visibility"`
	// Reason is an optional operator note on WHY this change was made —
	// Sprint C. Never persisted on the model row itself, only folded into
	// the audit_log detail (withReason) so it has a real read surface
	// (GET /api/v1/audit) instead of being pure write-only ceremony.
	Reason string `json:"reason"`
}

type variantBody struct {
	ModelID             int64  `json:"model_id"`
	Name                string `json:"name"`
	DerivationType      string `json:"derivation_type"`
	SourceVariantID     int64  `json:"source_variant_id"`
	TrainedCtx          int    `json:"trained_ctx"`
	IsAbliterated       bool   `json:"is_abliterated"`
	AbliterationQuality string `json:"abliteration_quality"`
}

type configBody struct {
	Name             string   `json:"name"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	EngineID         int64    `json:"engine_id"`
	BuildID          int64    `json:"build_id"`
	MMProjArtifactID int64    `json:"mmproj_artifact_id"`
	NCtx             int      `json:"n_ctx"`
	Parallel         int      `json:"parallel"`
	ExtraArgs        []string `json:"extra_args"`
	Status           string   `json:"status"`
	Visibility       string   `json:"visibility"`
	IsDefault        bool     `json:"is_default"`
	Fingerprint      string   `json:"fingerprint"`
	// Logo is a config-level icon override (Sprint I); "" inherits via the
	// model/family/genealogy chain. See genealogyJSON.Logo's doc comment.
	Logo     string `json:"logo"`
	LogoDark string `json:"logo_dark"`
	// Modalities overrides the model default; nil = derive. See
	// configJSON.Modalities.
	Modalities *[]string `json:"modalities,omitzero"`
	// Reason is an optional operator note on WHY this change was made —
	// Sprint C. See modelBody.Reason's doc comment; same treatment here.
	Reason string `json:"reason"`
}

type offeringBody struct {
	ModelID       int64   `json:"model_id"`
	VariantID     int64   `json:"variant_id"`
	Provider      string  `json:"provider"`
	WireModel     string  `json:"wire_model"`
	PriceInPer1M  float64 `json:"price_in_per_1m"`
	PriceOutPer1M float64 `json:"price_out_per_1m"`
	Currency      string  `json:"currency"`
	ContextLength int     `json:"context_length"`
	Enabled       bool    `json:"enabled"`
	// Priority (multi-provider routing sprint, 2026-08-06): pointer so an
	// omitted field means "default" on create (100) and "preserve" on
	// update — a bare int zero-value would silently make every new offering
	// the TOP priority (lowest value wins).
	Priority *int `json:"priority"`
	// PriceCachedInPer1M is the provider's discounted cache-hit input rate
	// (e.g. DeepSeek); nil/omitted means unmodelled — see store.Offering's
	// doc comment.
	PriceCachedInPer1M *float64 `json:"price_cached_in_per_1m"`
}

type benchmarkBody struct {
	Metric      string `json:"metric"`
	Value       string `json:"value"`
	Source      string `json:"source"`
	SourceURL   string `json:"source_url"`
	SourceDate  string `json:"source_date"`
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Notes       string `json:"notes"`
}

type noteBody struct {
	SubjectType string `json:"subject_type"`
	SubjectID   int64  `json:"subject_id"`
	Author      string `json:"author"`
	Body        string `json:"body"`
}

type serviceBody struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Icon        string `json:"icon"`
	Color       string `json:"color"`
	Unit        string `json:"unit"`
	HealthCheck string `json:"health_check"`
}

// ── Validation ───────────────────────────────────────────────────────────────

// validModalities is the Sprint J1 modality enum. Rejecting anything outside
// it at the API boundary is the whole point of typing this column instead of
// leaving it a free-text key_features string.
var validModalities = map[string]bool{"text": true, "vision": true, "audio": true}

// validateModalityList checks every entry against validModalities, reporting
// the first offender by name (matches this file's one-message-per-field
// validation style rather than aggregating every bad entry).
func validateModalityList(mods []string) string {
	for _, m := range mods {
		if !validModalities[m] {
			return fmt.Sprintf("unknown modality %q (must be text, vision, or audio)", m)
		}
	}
	return ""
}

// validateModel checks field constraints + family existence.
func (s *Server) validateModel(ctx context.Context, b modelBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	} else if len(b.Name) > 256 {
		fields["name"] = "must be ≤256 characters"
	}
	if msg := validateModalityList(b.Modalities); msg != "" {
		fields["modalities"] = msg
	}
	switch b.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be visible or hidden"
	}
	if s.deps.Catalog != nil && b.FamilyID != 0 {
		families, err := s.deps.Catalog.ListFamilies(ctx)
		if err != nil {
			fields["family_id"] = "could not verify"
		} else {
			found := false
			for _, f := range families {
				if f.ID == b.FamilyID {
					found = true
					break
				}
			}
			if !found {
				fields["family_id"] = "does not exist"
			}
		}
	}
	return fields
}

// validateVariant checks field constraints + model existence.
func (s *Server) validateVariant(ctx context.Context, b variantBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	if b.Name == "" {
		fields["name"] = "is required"
	}
	if b.ModelID == 0 {
		fields["model_id"] = "is required"
	} else if s.deps.Catalog != nil {
		if _, err := s.deps.Catalog.GetModel(ctx, b.ModelID); err != nil {
			fields["model_id"] = "does not exist"
		}
	}
	if b.SourceVariantID != 0 && s.deps.Catalog != nil {
		if _, err := s.deps.Catalog.GetVariant(ctx, b.SourceVariantID); err != nil {
			fields["source_variant_id"] = "does not exist"
		}
	}
	switch b.DerivationType {
	case "", "abliteration", "finetune", "merge", "mtp-head-add", "uncensor", "base":
	default:
		fields["derivation_type"] = "must be one of: abliteration, finetune, merge, mtp-head-add, uncensor, base"
	}
	return fields
}

// validateConfig checks field constraints + referential integrity + name uniqueness.
func (s *Server) validateConfig(ctx context.Context, b configBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	cat := s.deps.Catalog

	if !modeNameRE.MatchString(b.Name) {
		fields["name"] = "must match " + modeNamePattern
	}
	if b.Modalities != nil {
		if msg := validateModalityList(*b.Modalities); msg != "" {
			fields["modalities"] = msg
		}
	}
	if cat != nil {
		// Name uniqueness (unless updating the same config).
		if existing, err := cat.ConfigByName(ctx, b.Name); err == nil && existing.ID != excludeID {
			fields["name"] = "already exists"
		}
		// Variant existence.
		if b.VariantID == 0 {
			fields["variant_id"] = "is required"
		} else if _, err := cat.GetVariant(ctx, b.VariantID); err != nil {
			fields["variant_id"] = "does not exist"
		}
		// Weight artifact existence + type check.
		if b.WeightArtifactID == 0 {
			fields["weight_artifact_id"] = "is required"
		} else if a, err := cat.GetArtifact(ctx, b.WeightArtifactID); err != nil {
			fields["weight_artifact_id"] = "does not exist"
		} else if a.ArtifactType != "weight" {
			fields["weight_artifact_id"] = "must be a weight artifact"
		}
		// Engine existence.
		if b.EngineID == 0 {
			fields["engine_id"] = "is required"
		} else {
			engines, _ := cat.ListEngines(ctx)
			found := false
			for _, e := range engines {
				if e.ID == b.EngineID {
					found = true
					break
				}
			}
			if !found {
				fields["engine_id"] = "does not exist"
			}
		}
		// Build existence — required, not optional. A Config with no Build
		// has no defined backend/binary; historically this silently defaulted
		// to launching the vulkan binary regardless of what the model
		// actually needed (see store.Build's Backend doc comment). Every
		// Config must reference a real Build explicitly.
		if b.BuildID == 0 {
			fields["build_id"] = "is required"
		} else {
			builds, _ := cat.ListBuilds(ctx)
			found := false
			for _, bd := range builds {
				if bd.ID == b.BuildID {
					found = true
					break
				}
			}
			if !found {
				fields["build_id"] = "does not exist"
			}
		}
		// MMProj artifact existence + type check (optional).
		if b.MMProjArtifactID != 0 {
			if a, err := cat.GetArtifact(ctx, b.MMProjArtifactID); err != nil {
				fields["mmproj_artifact_id"] = "does not exist"
			} else if a.ArtifactType != "mmproj" {
				fields["mmproj_artifact_id"] = "must be an mmproj artifact"
			}
		}
	}
	if b.NCtx < 0 {
		fields["n_ctx"] = "must be ≥ 0"
	}
	if b.Parallel < 0 {
		fields["parallel"] = "must be ≥ 0"
	}
	switch b.Status {
	case "", "unverified", "verified":
	default:
		fields["status"] = "must be unverified or verified"
	}
	switch b.Visibility {
	case "", "visible", "hidden":
	default:
		fields["visibility"] = "must be visible or hidden"
	}
	return fields
}

// validateOffering checks field constraints + model/provider existence.
// resolveOfferingProviderID resolves an offering body's provider NAME
// (the wire shape kept "provider" as a name for a stable, human-readable
// dropdown value — see offeringJSON) to the real FK to write (0042).
// validateOffering already confirmed the name exists via ProviderExists;
// this re-resolves rather than threading an id through validation because
// the two checks serve different purposes (existence vs. the id itself) and
// a create/update body is small enough that the extra lookup is free.
func (s *Server) resolveOfferingProviderID(ctx context.Context, name string) (int64, bool) {
	if s.deps.Routing == nil {
		return 0, false
	}
	p, ok, err := s.deps.Routing.ProviderByName(ctx, name)
	if err != nil || !ok {
		return 0, false
	}
	return p.ID, true
}

func (s *Server) validateOffering(ctx context.Context, b offeringBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	cat := s.deps.Catalog

	if b.WireModel == "" {
		fields["wire_model"] = "is required"
	}
	if b.Provider == "" {
		fields["provider"] = "is required"
	}
	if cat != nil {
		if b.ModelID == 0 {
			fields["model_id"] = "is required"
		} else if _, err := cat.GetModel(ctx, b.ModelID); err != nil {
			fields["model_id"] = "does not exist"
		}
		if b.VariantID != 0 {
			if _, err := cat.GetVariant(ctx, b.VariantID); err != nil {
				fields["variant_id"] = "does not exist"
			}
		}
		if b.Provider != "" {
			exists, err := cat.ProviderExists(ctx, b.Provider)
			if err != nil {
				fields["provider"] = "could not verify"
			} else if !exists {
				fields["provider"] = "does not exist"
			}
		}
		// (provider, wire_model) uniqueness — a duplicate row for the same
		// provider + wire name is always a data error (the router can't
		// distinguish them), and was freely creatable before 2026-08-06.
		if b.Provider != "" && b.WireModel != "" {
			if all, err := cat.ListOfferings(ctx); err == nil {
				for _, o := range all {
					if o.ID != excludeID && o.ProviderName == b.Provider && o.WireModel == b.WireModel {
						fields["wire_model"] = "already offered by this provider (offering " + strconv.FormatInt(o.ID, 10) + ")"
						break
					}
				}
			}
		}
	}
	if b.Priority != nil && *b.Priority < 0 {
		fields["priority"] = "must be >= 0"
	}
	if b.Currency != "" && !currencyRE.MatchString(b.Currency) {
		fields["currency"] = "must be a 3-letter ISO 4217 code"
	}
	return fields
}

// validateBenchmark enforces the F7 fabrication-prevention gate (published
// requires source_url + source_date) + subject/value field constraints.
// subject_type is restricted to model/variant/config (Phase 8, pre-release
// feedback sprint): registry.go's loadSnapshot only ever indexed those
// three — an "offering"-scoped benchmark was validated, stored, and
// listable, but reached no card anywhere. See validateBenchmarkUpdate for
// why UPDATE still needs to accept "offering" in one narrow case.
func validateBenchmark(b benchmarkBody) map[string]string {
	return validateBenchmarkFields(b, false)
}

// validateBenchmarkUpdate grandfathers a pre-existing offering-scoped row:
// a PUT that leaves an already-offering-scoped row's subject untouched must
// still succeed, or that row becomes permanently unsavable through the only
// UI that can reach it (the form can no longer even represent "offering" as
// a selectable value once new ones are rejected, so every edit attempt
// would either silently re-home the row to a different subject or fail).
// Re-scoping an offering row TO model/variant/config is unaffected — it
// already goes through the strict path since the new subject isn't
// "offering". Create never grandfathers; only PUT has an existing row to
// compare against.
func validateBenchmarkUpdate(b benchmarkBody, existing store.Benchmark) map[string]string {
	grandfathered := existing.SubjectType == "offering" &&
		b.SubjectType == "offering" && b.SubjectID == existing.SubjectID
	return validateBenchmarkFields(b, grandfathered)
}

func validateBenchmarkFields(b benchmarkBody, allowExistingOffering bool) map[string]string {
	fields := map[string]string{}
	if b.Metric == "" {
		fields["metric"] = "is required"
	}
	if b.Value == "" {
		fields["value"] = "is required"
	}
	switch b.Source {
	case "published", "self_measured", "provider_reported":
	default:
		fields["source"] = "must be published, self_measured, or provider_reported"
	}
	// F7 gate: published requires source_url + source_date.
	if b.Source == "published" {
		if b.SourceURL == "" {
			fields["source_url"] = "is required when source is 'published'"
		}
		if b.SourceDate == "" {
			fields["source_date"] = "is required when source is 'published'"
		}
	}
	switch b.SubjectType {
	case "model", "variant", "config":
	case "offering":
		if !allowExistingOffering {
			fields["subject_type"] = "must be model, variant, or config"
		}
	default:
		fields["subject_type"] = "must be model, variant, or config"
	}
	if b.SubjectID == 0 {
		fields["subject_id"] = "is required"
	}
	if b.SourceDate != "" {
		if _, err := time.Parse("2006-01-02", b.SourceDate); err != nil {
			fields["source_date"] = "must be YYYY-MM-DD"
		}
	}
	return fields
}

// validateNote checks field constraints.
func validateNote(b noteBody) map[string]string {
	fields := map[string]string{}
	switch b.SubjectType {
	case "model", "config", "offering":
	default:
		fields["subject_type"] = "must be model, config, or offering"
	}
	if b.SubjectID == 0 {
		fields["subject_id"] = "is required"
	}
	if b.Body == "" {
		fields["body"] = "is required"
	}
	if b.Author == "" {
		fields["author"] = "is required"
	}
	return fields
}

// validateService checks field constraints + name uniqueness.
func (s *Server) validateService(ctx context.Context, b serviceBody, excludeID int64) map[string]string {
	fields := map[string]string{}
	if !modeNameRE.MatchString(b.Name) {
		fields["name"] = "must match " + modeNamePattern
	}
	if s.deps.Catalog != nil {
		if existing, err := s.deps.Catalog.ServiceByName(ctx, b.Name); err == nil && existing.ID != excludeID {
			fields["name"] = "already exists"
		}
	}
	return fields
}
