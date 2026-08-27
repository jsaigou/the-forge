// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// catalog.go — Sprint MODEL CATALOG (docs/v5-modes-config-editable.md
// §Phase 1, ADRs 0001–0005). The store-backed read/write surface for the
// model database that replaces the models.toml + forge.toml [modes.*] +
// router.toml (remote) data smear. The toml files become a one-time seed via
// `forge migrate-v4`; Phase 2 migrates the 17 cfg.Modes[...] read sites +
// the router catalog to consult these tables.
//
// Conventions match the rest of the store: unix-seconds INTEGER timestamps,
// 0/1 booleans, JSON as TEXT. Nullable FK columns (family_id, build_id,
// mmproj_artifact_id, source_variant_id, variant_id on offerings) map to Go
// zero values (0) which the writer converts to NULL via nullInt64.

// ── Identity & Derivation ────────────────────────────────────────────────────

// Genealogy is the level above Family (product/QA sprint, 2026-07-29):
// a vendor's own release lineage (Nemotron, Gemma, Qwen, …), of which a
// Family is one generation (Nemotron 3 vs Nemotron 2 — different Families,
// same Genealogy). Optional — a Family may have no Genealogy.
type Genealogy struct {
	ID   int64
	Name string
	Logo string
	// LogoDark is a dark-theme variant override; "" falls back to Logo
	// (registry.resolveLogos, Phase 3 icon-variant work). Same inheritance
	// level as Logo — a genealogy with LogoDark set but no ancestor above it
	// (there is none) always resolves its own dark mark.
	LogoDark string
}

// Family is one generation within a Genealogy (Gemma 4, Qwen 3.6, Nemotron
// 3, …) — narrower than the pre-2026-07-29 meaning of Family, which mixed
// generation, vendor, and use-case under one name (see 0017_genealogies.sql
// for the full fix). Optional — a Model may have no Family (FamilyID == 0
// → NULL), and a Family may have no Genealogy (GenealogyID == 0 → NULL).
type Family struct {
	ID          int64
	Name        string
	GenealogyID int64
	Logo        string
	LogoDark    string // dark-theme override; "" falls back to Logo
}

// Model is one specific parameter configuration within a Family (CONTEXT.md).
// Architecture is per-Model (GGUF general.architecture); empty until the
// registry populates it from a live GGUF read (Phase 2). KeyFeatures is a
// JSON array of strings in the DB.
type Model struct {
	ID             int64
	FamilyID       int64 // 0 → NULL (no family)
	Name           string
	Architecture   string
	ParameterCount string
	Description    string
	Creator        string
	LicenseName    string
	LicenseURL     string
	HFRepo         string
	Logo           string
	LogoDark       string // dark-theme override; "" falls back to Logo
	KeyFeatures    []string
	// Modalities is what the architecture supports (subset of
	// text/vision/audio), independent of whether any given Config can
	// actually deliver it -- see Config.Modalities and
	// registry.resolveModalities (Sprint J1).
	Modalities []string
	// Visibility is visible (Models gallery) or hidden (Settings only) —
	// model-level decommission flag mirroring Config.Visibility
	// (0062_models_visibility.sql). Card endpoints filter hidden models;
	// the catalog CRUD list keeps returning them for operator management.
	Visibility string // visible | hidden
}

// Variant is a structural derivation of a Model (abliteration, finetune,
// merge, mtp-head-add, uncensor, …). Same parameter count, different tensor
// prep. The derivation graph is Variant → Variant (SourceVariantID). Each
// Variant carries trained_ctx (from the GGUF header's <arch>.context_length);
// 0 means unknown until the registry populates it.
type Variant struct {
	ID                  int64
	ModelID             int64
	Name                string
	DerivationType      string
	SourceVariantID     int64 // 0 → NULL (base variant)
	TrainedCtx          int
	IsAbliterated       bool
	AbliterationQuality string
}

// Quantization is the precision scheme (Q6_K, Q8_0, Q4_K_M, …). Enum-like —
// static-seeded in the migration. The seed path and CRUD APIs reference by ID.
type Quantization struct {
	ID   int64
	Name string
}

// Format is the container format (GGUF, safetensors). Enum-like.
type Format struct {
	ID   int64
	Name string
}

// Artifact is the concrete file on disk. Each shard is an Artifact; a shard
// set shares ShardSetID. Weight Artifacts back one (Variant, Quantization,
// Format). Auxiliary Artifacts (mmproj, tokenizer) are shared across
// Variants via Compatibility. The Missing flag is set when the file is no
// longer on disk (re-scan on access); the catalog labels rather than gates.
type Artifact struct {
	ID                 int64
	VariantID          int64
	QuantizationID     int64 // 0 → NULL (unknown)
	FormatID           int64
	FilePath           string
	ShardSetID         string // "" → NULL
	IsAuxiliary        bool
	ArtifactType       string // weight | mmproj | tokenizer
	Missing            bool
	SHA256             string // "" → NULL
	FileSizeBytes      int64
	GGUFArch           string
	GGUFTrainedCtx     int
	GGUFParameterCount string
	GGUFQuantType      string
}

// Compatibility is a factual relationship between an auxiliary Artifact
// (e.g. an mmproj) and the Variants it can serve. Usually scoped within one
// Model; cross-Model is unusual but not structurally prohibited.
type Compatibility struct {
	AuxiliaryArtifactID int64
	VariantID           int64
}

// ── Launch & Inference ───────────────────────────────────────────────────────

// Engine is the inference software (llama.cpp, vLLM). Extensible.
type Engine struct {
	ID   int64
	Name string
}

// Build is a specific compiled version of an Engine (vulkan-build,
// rocm-build, puzzle-port-branch). One Engine has many Builds.
//
// Backend is the GPU runtime this Build's binary requires: "vulkan" | "rocm"
// | "vllm". Required, explicit — never derived from Name. Found live
// (2026-07-28): deriving it from a Name substring match ("contains ROCM")
// silently defaulted to "vulkan" for any Build with no matching name (or no
// Build at all), which launched the wrong binary for nemotron (backend
// 'rocm', no custom build) and OOM'd past Vulkan's ~63GB ceiling badly
// enough to kernel-panic the host. Backend is the one fact a Build exists to
// pin down — it must always be set explicitly by the writer, never guessed
// by a reader.
type Build struct {
	ID         int64
	EngineID   int64
	Name       string
	BinaryPath string
	Backend    string
	Reason     string
}

// Config is a named, loadable recipe for running one weight Artifact with
// one Engine Build (ADR-0002 — replaces V4 Mode). The operator-facing unit
// of loading. Status is unverified (default) or verified (PROFILE-clean).
// Visibility is visible (dashboard switcher) or hidden (Settings only).
// ExtraArgs is a JSON array of strings in the DB.
type Config struct {
	ID               int64
	Name             string
	VariantID        int64
	WeightArtifactID int64
	EngineID         int64
	BuildID          int64 // 0 → NULL
	MMProjArtifactID int64 // 0 → NULL
	NCtx             int
	Parallel         int
	ExtraArgs        []string
	Status           string // unverified | verified
	Visibility       string // visible | hidden
	IsDefault        bool
	Fingerprint      string
	CreatedAt        time.Time
	Logo             string // icon override; "" inherits via registry.resolveLogos (Sprint I)
	LogoDark         string // dark-theme override; "" falls back to Logo
	// Modalities overrides the model-level default (missing mmproj, a build
	// lacking mtmd support for a modality, a modality stripped during
	// quantization). nil = derive from Model.Modalities + MMProjArtifactID
	// (registry.resolveModalities); non-nil (incl. an explicit empty slice)
	// = use verbatim, even if that means "text only" despite a capable
	// model (Sprint J1).
	Modalities *[]string
}

// Slot is one of the fixed inference bays (A1-A4) a Config gets loaded onto
// (CONTEXT.md `Slot` entry). First-class store-backed vocabulary as of the
// TOML decommission (docs/v5-toml-decommission.md §3.2, ADR-0007) — replaces
// `forge.toml`'s `[slots.*]` blocks. Callers resolve by Config name via
// the scheduler, not by Slot directly; this table exists so slot identity
// (which systemd unit, which port) is itself editable without a file edit.
type Slot struct {
	ID        int64
	Name      string // a1, a2, a3, a4
	Unit      string // systemd unit, e.g. forge-a1
	Port      int
	Label     string // A1, A2, A3, A4
	SortOrder int
}

// Service is a standalone process the dashboard manages via Start/Stop
// (ComfyUI, aligner, …). Not a model launch. Distinct from the catalog's
// model world. The V4 `creative` mode becomes a Service named `comfyui`.
type Service struct {
	ID          int64
	Name        string
	Label       string
	Description string
	Icon        string
	Color       string
	Unit        string
	HealthCheck string // raw JSON; default "{}"
}

// ── Remote Hosting ───────────────────────────────────────────────────────────

// Offering is a remote availability of a (Model, Variant) through a specific
// Provider (ADR-0003). The same Model on two Providers is two distinct
// Offerings — not interchangeable (data residency, cost, reliability).
type Offering struct {
	ID        int64
	ModelID   int64
	VariantID int64 // 0 → NULL
	// ProviderID is the real FK (0042) — write this. ProviderName is a
	// read-only join-derived projection populated by ListOfferings/
	// GetOffering for display; ignored on write.
	ProviderID   int64
	ProviderName string
	WireModel    string
	PriceInPer1M  float64
	PriceOutPer1M float64
	Currency      string
	ContextLength int
	Enabled       bool

	// Priority is the operator's preference rank among the offerings of one
	// Model (0032_provider_enabled_offering_priority.sql): when several
	// providers offer the same model, the router presents and routes the
	// enabled offering of an enabled provider with the LOWEST value (1 beats
	// 100 — same lower-first convention as slots.sort_order). Ties break by
	// provider name then offering id, so the outcome is deterministic.
	// Default 100 leaves compressor to promote specific offerings.
	Priority int

	// PriceCachedInPer1M is the provider's discounted rate for cache-hit
	// input tokens (e.g. DeepSeek), nil when unmodelled — the router's cost
	// computation must then price cached tokens at the full PriceInPer1M
	// rate (a documented upper bound, never an under-estimate).
	PriceCachedInPer1M *float64
}

// ── Annotations ──────────────────────────────────────────────────────────────

// Benchmark is a measured or reported data point about a Model, Variant,
// Config, or Offering (ADR-0005). One concept unifying capability scores,
// performance metrics, provider-reported specs, and quality assessments.
// Source is published / self_measured / provider_reported. The F7 gate
// (source_url + source_date required for published) is enforced structurally
// by a CHECK constraint in the migration. Value is TEXT to accommodate both
// numeric scores ("0.843") and text values ("High").
type Benchmark struct {
	ID          int64
	Metric      string
	Value       string
	Source      string // published | self_measured | provider_reported
	SourceURL   string
	SourceDate  string // ISO 8601 date (YYYY-MM-DD)
	SubjectType string // model | variant | config | offering
	SubjectID   int64
	Notes       string
}

// Note is an operator annotation attached to a Model, Config, or Offering.
// Distinct from Benchmark (measured/published data) — Notes are operator
// judgment.
type Note struct {
	ID          int64
	SubjectType string // model | config | offering
	SubjectID   int64
	Author      string
	Body        string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Catalog is the read/write surface for the model catalog (MODEL CATALOG
// sprint, ADR-0001). Additive to the Store interface (Contract 2 amendment).
// Phase 1 implements the write side (for the migrate-v4 seed) and the list
// side (for DoD verification + Phase 2 read-site migration). Phase 3 adds
// Get/Update/Delete + lookup methods for the CRUD APIs.
type Catalog interface {
	// ── Identity & Derivation ──
	// Genealogy CRUD (product/QA sprint, 2026-07-29 — the level above
	// Family; see Genealogy's doc comment).
	CreateGenealogy(ctx context.Context, g Genealogy) (int64, error)
	GetGenealogy(ctx context.Context, id int64) (Genealogy, error)
	UpdateGenealogy(ctx context.Context, g Genealogy) error
	DeleteGenealogy(ctx context.Context, id int64) error
	GenealogyByName(ctx context.Context, name string) (Genealogy, error)
	ListGenealogies(ctx context.Context) ([]Genealogy, error)

	CreateFamily(ctx context.Context, f Family) (int64, error)
	GetFamily(ctx context.Context, id int64) (Family, error) // ErrNotFound if missing
	UpdateFamily(ctx context.Context, f Family) error
	DeleteFamily(ctx context.Context, id int64) error
	FamilyByName(ctx context.Context, name string) (Family, error) // ErrNotFound if missing
	ListFamilies(ctx context.Context) ([]Family, error)
	CreateModel(ctx context.Context, m Model) (int64, error)
	GetModel(ctx context.Context, id int64) (Model, error) // ErrNotFound if missing
	UpdateModel(ctx context.Context, m Model) error
	DeleteModel(ctx context.Context, id int64) error
	ModelByName(ctx context.Context, name string) (Model, error)
	ListModels(ctx context.Context) ([]Model, error)
	CreateVariant(ctx context.Context, v Variant) (int64, error)
	GetVariant(ctx context.Context, id int64) (Variant, error)
	UpdateVariant(ctx context.Context, v Variant) error
	DeleteVariant(ctx context.Context, id int64) error
	ListVariants(ctx context.Context) ([]Variant, error)
	ListVariantsForModel(ctx context.Context, modelID int64) ([]Variant, error)
	CreateArtifact(ctx context.Context, a Artifact) (int64, error)
	GetArtifact(ctx context.Context, id int64) (Artifact, error)
	UpdateArtifact(ctx context.Context, a Artifact) error
	DeleteArtifact(ctx context.Context, id int64) error
	ListArtifacts(ctx context.Context) ([]Artifact, error)
	ListArtifactsForVariant(ctx context.Context, variantID int64) ([]Artifact, error)
	CreateCompatibility(ctx context.Context, c Compatibility) error
	QuantizationByName(ctx context.Context, name string) (Quantization, error)
	FormatByName(ctx context.Context, name string) (Format, error)
	ListQuantizations(ctx context.Context) ([]Quantization, error)
	ListFormats(ctx context.Context) ([]Format, error)

	// ── Launch & Inference ──
	CreateSlot(ctx context.Context, s Slot) (int64, error)
	GetSlot(ctx context.Context, id int64) (Slot, error)
	UpdateSlot(ctx context.Context, s Slot) error
	DeleteSlot(ctx context.Context, id int64) error
	SlotByName(ctx context.Context, name string) (Slot, error)
	ListSlots(ctx context.Context) ([]Slot, error)
	EngineByName(ctx context.Context, name string) (Engine, error)
	ListEngines(ctx context.Context) ([]Engine, error)
	CreateBuild(ctx context.Context, b Build) (int64, error)
	UpdateBuild(ctx context.Context, b Build) error
	ListBuilds(ctx context.Context) ([]Build, error)
	CreateConfig(ctx context.Context, c Config) (int64, error)
	GetConfig(ctx context.Context, id int64) (Config, error)
	UpdateConfig(ctx context.Context, c Config) error
	DeleteConfig(ctx context.Context, id int64) error
	ConfigByName(ctx context.Context, name string) (Config, error)
	ListConfigs(ctx context.Context) ([]Config, error)
	ListConfigsForVariant(ctx context.Context, variantID int64) ([]Config, error)
	CreateService(ctx context.Context, s Service) (int64, error)
	GetService(ctx context.Context, id int64) (Service, error)
	UpdateService(ctx context.Context, s Service) error
	DeleteService(ctx context.Context, id int64) error
	ServiceByName(ctx context.Context, name string) (Service, error)
	ListServices(ctx context.Context) ([]Service, error)

	// ── Remote Hosting ──
	CreateOffering(ctx context.Context, o Offering) (int64, error)
	GetOffering(ctx context.Context, id int64) (Offering, error)
	UpdateOffering(ctx context.Context, o Offering) error
	DeleteOffering(ctx context.Context, id int64) error
	ListOfferings(ctx context.Context) ([]Offering, error)
	ListOfferingsForModel(ctx context.Context, modelID int64) ([]Offering, error)
	ProviderExists(ctx context.Context, name string) (bool, error)

	// ── Annotations ──
	CreateBenchmark(ctx context.Context, b Benchmark) (int64, error)
	GetBenchmark(ctx context.Context, id int64) (Benchmark, error)
	UpdateBenchmark(ctx context.Context, b Benchmark) error
	DeleteBenchmark(ctx context.Context, id int64) error
	ListBenchmarks(ctx context.Context) ([]Benchmark, error)
	ListBenchmarksForSubject(ctx context.Context, subjectType string, subjectID int64) ([]Benchmark, error)
	CreateNote(ctx context.Context, n Note) (int64, error)
	GetNote(ctx context.Context, id int64) (Note, error)
	UpdateNote(ctx context.Context, n Note) error
	DeleteNote(ctx context.Context, id int64) error
	ListNotes(ctx context.Context) ([]Note, error)
	ListNotesForSubject(ctx context.Context, subjectType string, subjectID int64) ([]Note, error)

	// ── Icons (Sprint I — inheritance hierarchy) ──
	SetModelLogo(ctx context.Context, modelID int64, logo string) error
	SetFamilyLogo(ctx context.Context, familyID int64, logo string) error
	SetGenealogyLogo(ctx context.Context, genealogyID int64, logo string) error
	SetConfigLogo(ctx context.Context, configID int64, logo string) error
	// Set*LogoDark (Phase 3 icon-variant work) mirror the Set*Logo group
	// above exactly, writing the dark-theme override column instead.
	SetModelLogoDark(ctx context.Context, modelID int64, logo string) error
	SetFamilyLogoDark(ctx context.Context, familyID int64, logo string) error
	SetGenealogyLogoDark(ctx context.Context, genealogyID int64, logo string) error
	SetConfigLogoDark(ctx context.Context, configID int64, logo string) error

	// RegisterDownloadedModel (HF model-acquisition track) writes a full
	// Model+Variant+Artifact(+mmproj)+Config row set atomically. The
	// individual Create* methods above are each a standalone statement with
	// no shared transaction seam — a partial failure partway through a
	// multi-row write (e.g. Config creation failing after Model/Variant/
	// Artifact already committed) would strand an orphaned Model with no
	// Config to describe it. This method exists specifically to avoid that;
	// every field in b is written inside one BeginTx or none of it is.
	RegisterDownloadedModel(ctx context.Context, b ModelBundle) (ModelBundleResult, error)
}

// ModelBundle is the full row set one completed HF download registers
// (RegisterDownloadedModel). IDs on Model/Variant/Artifact/Config are
// ignored on input — they're assigned by the insert and returned in
// ModelBundleResult. FK fields that only make sense once earlier rows in
// the bundle exist (Variant.ModelID, Artifact.VariantID,
// Config.VariantID/WeightArtifactID/MMProjArtifactID) are filled in by
// RegisterDownloadedModel itself; any value the caller sets there is
// overwritten.
type ModelBundle struct {
	Model    Model
	Variant  Variant
	Artifact Artifact  // the weight artifact (Config.WeightArtifactID points here)
	MMProj   *Artifact // optional auxiliary artifact; nil = no mmproj
	// ExtraArtifacts are trailing shards of a sharded weight file (same
	// ShardSetID as Artifact, ArtifactType "weight"): un-flagged sibling
	// rows nothing points a FK at, purely for catalog completeness.
	ExtraArtifacts []Artifact
	Config         Config
}

// ModelBundleResult is the id set RegisterDownloadedModel assigned.
// MMProjArtifactID is 0 when the bundle had no MMProj.
type ModelBundleResult struct {
	ModelID          int64
	VariantID        int64
	ArtifactID       int64
	MMProjArtifactID int64
	ConfigID         int64
}

type catalogView struct{ d *DB }

// Catalog returns the model-catalog surface (MODEL CATALOG sprint).
func (d *DB) Catalog() Catalog { return catalogView{d} }

// nullInt64 maps 0 → NULL (for nullable FK columns where 0 is not a valid ID).
func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// jsonList marshals a []string to JSON for TEXT columns. Returns "[]" for nil
// or empty so the DB never holds a NULL-ish JSON column.
func jsonList(xs []string) string {
	if len(xs) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(xs)
	return string(b)
}

// parseJSONList unmarshals a JSON TEXT column back to []string. An empty or
// "[]" value yields a non-nil empty slice (Contract 1 §3: arrays must not be
// nil — the PWA's .map() would crash on null).
func parseJSONList(raw string) []string {
	out := []string{}
	if raw == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// nullJSONList marshals a nullable []string to a sql.NullString for TEXT
// columns where NULL is a distinct, meaningful value (Config.Modalities: nil
// = "derive", non-nil = "explicit override", including an explicit empty
// slice -- unlike jsonList, this must not collapse nil to "[]").
func nullJSONList(xs *[]string) sql.NullString {
	if xs == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: jsonList(*xs), Valid: true}
}

// parseNullJSONList is nullJSONList's inverse for scanning.
func parseNullJSONList(ns sql.NullString) *[]string {
	if !ns.Valid {
		return nil
	}
	out := parseJSONList(ns.String)
	return &out
}

// ── Identity & Derivation: implementation ────────────────────────────────────

func (v catalogView) CreateFamily(ctx context.Context, f Family) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO families (name, genealogy_id, logo, logo_dark) VALUES (?, ?, ?, ?)`,
		f.Name, nullInt64(f.GenealogyID), f.Logo, f.LogoDark)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_family: %w", err)
	}
	// INSERT OR IGNORE: if the family already exists (UNIQUE name), the row
	// isn't inserted but we still return the existing ID so the caller can
	// chain FK references. RowsAffected == 0 means it was ignored.
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := v.FamilyByName(ctx, f.Name)
		if err != nil {
			return 0, err
		}
		return existing.ID, nil
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) GetFamily(ctx context.Context, id int64) (Family, error) {
	var f Family
	var genealogyID sql.NullInt64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, genealogy_id, logo, logo_dark FROM families WHERE id = ?`, id).
		Scan(&f.ID, &f.Name, &genealogyID, &f.Logo, &f.LogoDark)
	if err == sql.ErrNoRows {
		return Family{}, fmt.Errorf("%w: family %d", ErrNotFound, id)
	}
	if err != nil {
		return Family{}, fmt.Errorf("store: catalog.get_family: %w", err)
	}
	f.GenealogyID = intOf(genealogyID)
	return f, nil
}

func (v catalogView) UpdateFamily(ctx context.Context, f Family) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE families SET name = ?, genealogy_id = ?, logo = ?, logo_dark = ? WHERE id = ?`,
		f.Name, nullInt64(f.GenealogyID), f.Logo, f.LogoDark, f.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_family: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: family %d", ErrNotFound, f.ID)
	}
	return nil
}

// DeleteFamily removes a family. Models referencing it (family_id) fall
// back to no family (ON DELETE SET NULL, migration 0008) rather than being
// deleted themselves — a family is an organizational label, not ownership.
func (v catalogView) DeleteFamily(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM families WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_family: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: family %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) FamilyByName(ctx context.Context, name string) (Family, error) {
	var f Family
	var genealogyID sql.NullInt64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, genealogy_id, logo, logo_dark FROM families WHERE name = ?`, name).
		Scan(&f.ID, &f.Name, &genealogyID, &f.Logo, &f.LogoDark)
	if err == sql.ErrNoRows {
		return Family{}, fmt.Errorf("%w: family %q", ErrNotFound, name)
	}
	if err != nil {
		return Family{}, fmt.Errorf("store: catalog.family_by_name: %w", err)
	}
	f.GenealogyID = intOf(genealogyID)
	return f, nil
}

func (v catalogView) ListFamilies(ctx context.Context) ([]Family, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name, genealogy_id, logo, logo_dark FROM families ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_families: %w", err)
	}
	defer rows.Close()
	out := []Family{}
	for rows.Next() {
		var f Family
		var genealogyID sql.NullInt64
		if err := rows.Scan(&f.ID, &f.Name, &genealogyID, &f.Logo, &f.LogoDark); err != nil {
			return nil, fmt.Errorf("store: catalog.list_families: %w", err)
		}
		f.GenealogyID = intOf(genealogyID)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ── Genealogy CRUD (product/QA sprint, 2026-07-29) ──────────────────────────
// Mirrors the Family CRUD pattern exactly — Genealogy is a plain (id, name)
// vocabulary table one level up.

func (v catalogView) CreateGenealogy(ctx context.Context, g Genealogy) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO genealogies (name, logo, logo_dark) VALUES (?, ?, ?)`, g.Name, g.Logo, g.LogoDark)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_genealogy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		existing, err := v.GenealogyByName(ctx, g.Name)
		if err != nil {
			return 0, err
		}
		return existing.ID, nil
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) GetGenealogy(ctx context.Context, id int64) (Genealogy, error) {
	var g Genealogy
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, logo, logo_dark FROM genealogies WHERE id = ?`, id).
		Scan(&g.ID, &g.Name, &g.Logo, &g.LogoDark)
	if err == sql.ErrNoRows {
		return Genealogy{}, fmt.Errorf("%w: genealogy %d", ErrNotFound, id)
	}
	if err != nil {
		return Genealogy{}, fmt.Errorf("store: catalog.get_genealogy: %w", err)
	}
	return g, nil
}

func (v catalogView) UpdateGenealogy(ctx context.Context, g Genealogy) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE genealogies SET name = ?, logo = ?, logo_dark = ? WHERE id = ?`, g.Name, g.Logo, g.LogoDark, g.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_genealogy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: genealogy %d", ErrNotFound, g.ID)
	}
	return nil
}

// DeleteGenealogy removes a genealogy. Families referencing it fall back to
// no genealogy (ON DELETE SET NULL) rather than being deleted.
func (v catalogView) DeleteGenealogy(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM genealogies WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_genealogy: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: genealogy %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) GenealogyByName(ctx context.Context, name string) (Genealogy, error) {
	var g Genealogy
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, logo, logo_dark FROM genealogies WHERE name = ?`, name).
		Scan(&g.ID, &g.Name, &g.Logo, &g.LogoDark)
	if err == sql.ErrNoRows {
		return Genealogy{}, fmt.Errorf("%w: genealogy %q", ErrNotFound, name)
	}
	if err != nil {
		return Genealogy{}, fmt.Errorf("store: catalog.genealogy_by_name: %w", err)
	}
	return g, nil
}

func (v catalogView) ListGenealogies(ctx context.Context) ([]Genealogy, error) {
	rows, err := v.d.sql.QueryContext(ctx, `SELECT id, name, logo, logo_dark FROM genealogies ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_genealogies: %w", err)
	}
	defer rows.Close()
	out := []Genealogy{}
	for rows.Next() {
		var g Genealogy
		if err := rows.Scan(&g.ID, &g.Name, &g.Logo, &g.LogoDark); err != nil {
			return nil, fmt.Errorf("store: catalog.list_genealogies: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (v catalogView) CreateModel(ctx context.Context, m Model) (int64, error) {
	if m.Visibility == "" {
		m.Visibility = "visible"
	}
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO models (family_id, name, architecture, parameter_count,
		description, creator, license_name, license_url, hf_repo, logo, logo_dark,
		key_features, modalities, visibility)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullInt64(m.FamilyID), m.Name, m.Architecture, m.ParameterCount,
		m.Description, m.Creator, m.LicenseName, m.LicenseURL, m.HFRepo, m.Logo, m.LogoDark,
		jsonList(m.KeyFeatures), jsonList(m.Modalities), m.Visibility)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_model: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListModels(ctx context.Context) ([]Model, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, family_id, name, architecture, parameter_count, description,
		creator, license_name, license_url, hf_repo, logo, logo_dark, key_features, modalities, visibility
		FROM models ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_models: %w", err)
	}
	defer rows.Close()
	out := []Model{}
	for rows.Next() {
		var m Model
		var familyID sql.NullInt64
		var kf, mod string
		if err := rows.Scan(&m.ID, &familyID, &m.Name, &m.Architecture,
			&m.ParameterCount, &m.Description, &m.Creator, &m.LicenseName,
			&m.LicenseURL, &m.HFRepo, &m.Logo, &m.LogoDark, &kf, &mod, &m.Visibility); err != nil {
			return nil, fmt.Errorf("store: catalog.list_models: %w", err)
		}
		m.FamilyID = intOf(familyID)
		m.KeyFeatures = parseJSONList(kf)
		m.Modalities = parseJSONList(mod)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (v catalogView) CreateVariant(ctx context.Context, vt Variant) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO variants (model_id, name, derivation_type, source_variant_id,
		   trained_ctx, is_abliterated, abliteration_quality)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		vt.ModelID, vt.Name, vt.DerivationType, nullInt64(vt.SourceVariantID),
		vt.TrainedCtx, boolInt(vt.IsAbliterated), vt.AbliterationQuality)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_variant: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListVariants(ctx context.Context) ([]Variant, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, model_id, name, derivation_type, source_variant_id,
		   trained_ctx, is_abliterated, abliteration_quality
		 FROM variants ORDER BY model_id, name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_variants: %w", err)
	}
	defer rows.Close()
	out := []Variant{}
	for rows.Next() {
		var vt Variant
		var sourceID sql.NullInt64
		var abl int64
		if err := rows.Scan(&vt.ID, &vt.ModelID, &vt.Name, &vt.DerivationType,
			&sourceID, &vt.TrainedCtx, &abl, &vt.AbliterationQuality); err != nil {
			return nil, fmt.Errorf("store: catalog.list_variants: %w", err)
		}
		vt.SourceVariantID = intOf(sourceID)
		vt.IsAbliterated = abl != 0
		out = append(out, vt)
	}
	return out, rows.Err()
}

func (v catalogView) CreateArtifact(ctx context.Context, a Artifact) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO artifacts (variant_id, quantization_id, format_id, file_path,
		   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
		   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
		   gguf_quant_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.VariantID, nullInt64(a.QuantizationID), a.FormatID, a.FilePath,
		nullStr(a.ShardSetID), boolInt(a.IsAuxiliary), a.ArtifactType,
		boolInt(a.Missing), nullStr(a.SHA256), a.FileSizeBytes,
		a.GGUFArch, a.GGUFTrainedCtx, a.GGUFParameterCount, a.GGUFQuantType)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_artifact: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListArtifacts(ctx context.Context) ([]Artifact, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, variant_id, quantization_id, format_id, file_path,
		   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
		   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
		   gguf_quant_type
		 FROM artifacts ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_artifacts: %w", err)
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		var qID sql.NullInt64
		var shardSet, sha sql.NullString
		var isAux, missing int64
		if err := rows.Scan(&a.ID, &a.VariantID, &qID, &a.FormatID, &a.FilePath,
			&shardSet, &isAux, &a.ArtifactType, &missing, &sha,
			&a.FileSizeBytes, &a.GGUFArch, &a.GGUFTrainedCtx,
			&a.GGUFParameterCount, &a.GGUFQuantType); err != nil {
			return nil, fmt.Errorf("store: catalog.list_artifacts: %w", err)
		}
		a.QuantizationID = intOf(qID)
		a.ShardSetID = strOf(shardSet)
		a.SHA256 = strOf(sha)
		a.IsAuxiliary = isAux != 0
		a.Missing = missing != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (v catalogView) CreateCompatibility(ctx context.Context, c Compatibility) error {
	_, err := v.d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO compatibilities (auxiliary_artifact_id, variant_id)
		 VALUES (?, ?)`, c.AuxiliaryArtifactID, c.VariantID)
	if err != nil {
		return fmt.Errorf("store: catalog.create_compatibility: %w", err)
	}
	return nil
}

func (v catalogView) QuantizationByName(ctx context.Context, name string) (Quantization, error) {
	var q Quantization
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name FROM quantizations WHERE name = ?`, name).
		Scan(&q.ID, &q.Name)
	if err == sql.ErrNoRows {
		return Quantization{}, fmt.Errorf("%w: quantization %q", ErrNotFound, name)
	}
	if err != nil {
		return Quantization{}, fmt.Errorf("store: catalog.quantization_by_name: %w", err)
	}
	return q, nil
}

func (v catalogView) FormatByName(ctx context.Context, name string) (Format, error) {
	var f Format
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name FROM formats WHERE name = ?`, name).
		Scan(&f.ID, &f.Name)
	if err == sql.ErrNoRows {
		return Format{}, fmt.Errorf("%w: format %q", ErrNotFound, name)
	}
	if err != nil {
		return Format{}, fmt.Errorf("store: catalog.format_by_name: %w", err)
	}
	return f, nil
}

// ── Launch & Inference: implementation ────────────────────────────────────────

func (v catalogView) CreateSlot(ctx context.Context, s Slot) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO slots (name, unit, port, label, sort_order)
		 VALUES (?, ?, ?, ?, ?)`,
		s.Name, s.Unit, s.Port, s.Label, s.SortOrder)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_slot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Slot with this name already exists (UNIQUE). Return the existing
		// ID so the cutover migration (§5) doesn't fail on a re-run.
		var id int64
		if err := v.d.sql.QueryRowContext(ctx,
			`SELECT id FROM slots WHERE name = ?`, s.Name).Scan(&id); err != nil {
			return 0, fmt.Errorf("store: catalog.create_slot (lookup): %w", err)
		}
		return id, nil
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListSlots(ctx context.Context) ([]Slot, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name, unit, port, label, sort_order
		 FROM slots ORDER BY sort_order, name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_slots: %w", err)
	}
	defer rows.Close()
	out := []Slot{}
	for rows.Next() {
		var s Slot
		if err := rows.Scan(&s.ID, &s.Name, &s.Unit, &s.Port, &s.Label, &s.SortOrder); err != nil {
			return nil, fmt.Errorf("store: catalog.list_slots: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (v catalogView) EngineByName(ctx context.Context, name string) (Engine, error) {
	var e Engine
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name FROM engines WHERE name = ?`, name).
		Scan(&e.ID, &e.Name)
	if err == sql.ErrNoRows {
		return Engine{}, fmt.Errorf("%w: engine %q", ErrNotFound, name)
	}
	if err != nil {
		return Engine{}, fmt.Errorf("store: catalog.engine_by_name: %w", err)
	}
	return e, nil
}

func (v catalogView) ListEngines(ctx context.Context) ([]Engine, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name FROM engines ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_engines: %w", err)
	}
	defer rows.Close()
	out := []Engine{}
	for rows.Next() {
		var e Engine
		if err := rows.Scan(&e.ID, &e.Name); err != nil {
			return nil, fmt.Errorf("store: catalog.list_engines: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (v catalogView) CreateBuild(ctx context.Context, b Build) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO builds (engine_id, name, binary_path, backend, reason)
		 VALUES (?, ?, ?, ?, ?)`,
		b.EngineID, b.Name, b.BinaryPath, b.Backend, b.Reason)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_build: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) UpdateBuild(ctx context.Context, b Build) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE builds SET engine_id=?, name=?, binary_path=?, backend=?, reason=?
		 WHERE id=?`,
		b.EngineID, b.Name, b.BinaryPath, b.Backend, b.Reason, b.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_build: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: build %d", ErrNotFound, b.ID)
	}
	return nil
}

func (v catalogView) ListBuilds(ctx context.Context) ([]Build, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, engine_id, name, binary_path, backend, reason
		 FROM builds ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_builds: %w", err)
	}
	defer rows.Close()
	out := []Build{}
	for rows.Next() {
		var b Build
		if err := rows.Scan(&b.ID, &b.EngineID, &b.Name, &b.BinaryPath, &b.Backend, &b.Reason); err != nil {
			return nil, fmt.Errorf("store: catalog.list_builds: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (v catalogView) CreateConfig(ctx context.Context, c Config) (int64, error) {
	// Apply defaults for CHECK-constrained fields (the SQL DEFAULT doesn't
	// apply when the column is explicitly inserted with a Go zero value).
	if c.Status == "" {
		c.Status = "unverified"
	}
	if c.Visibility == "" {
		c.Visibility = "visible"
	}
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id,
		   build_id, mmproj_artifact_id, n_ctx, parallel, extra_args, status,
		   visibility, is_default, fingerprint, created_at, logo, logo_dark, modalities)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.VariantID, c.WeightArtifactID, c.EngineID,
		nullInt64(c.BuildID), nullInt64(c.MMProjArtifactID),
		c.NCtx, c.Parallel, jsonList(c.ExtraArgs), c.Status, c.Visibility,
		boolInt(c.IsDefault), c.Fingerprint, unixOf(orNow(c.CreatedAt)), c.Logo, c.LogoDark,
		nullJSONList(c.Modalities))
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_config: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListConfigs(ctx context.Context) ([]Config, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name, variant_id, weight_artifact_id, engine_id, build_id,
		   mmproj_artifact_id, n_ctx, parallel, extra_args, status, visibility,
		   is_default, fingerprint, created_at, logo, logo_dark, modalities
		 FROM configs ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_configs: %w", err)
	}
	defer rows.Close()
	out := []Config{}
	for rows.Next() {
		var c Config
		var buildID, mmprojID sql.NullInt64
		var isDefault int64
		var ea string
		var createdAt int64
		var mod sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &c.VariantID, &c.WeightArtifactID,
			&c.EngineID, &buildID, &mmprojID, &c.NCtx, &c.Parallel, &ea,
			&c.Status, &c.Visibility, &isDefault, &c.Fingerprint, &createdAt, &c.Logo, &c.LogoDark, &mod); err != nil {
			return nil, fmt.Errorf("store: catalog.list_configs: %w", err)
		}
		c.BuildID = intOf(buildID)
		c.MMProjArtifactID = intOf(mmprojID)
		c.ExtraArgs = parseJSONList(ea)
		c.IsDefault = isDefault != 0
		c.CreatedAt = time.Unix(createdAt, 0).UTC()
		c.Modalities = parseNullJSONList(mod)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (v catalogView) CreateService(ctx context.Context, s Service) (int64, error) {
	hc := s.HealthCheck
	if hc == "" {
		hc = "{}"
	}
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT OR IGNORE INTO services (name, label, description, icon, color,
		   unit, health_check)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.Name, s.Label, s.Description, s.Icon, s.Color, s.Unit, hc)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Service with this name already exists (UNIQUE). Return the existing
		// ID so the caller doesn't fail on a re-seed (INSERT OR IGNORE).
		var id int64
		if err := v.d.sql.QueryRowContext(ctx,
			`SELECT id FROM services WHERE name = ?`, s.Name).Scan(&id); err != nil {
			return 0, fmt.Errorf("store: catalog.create_service (lookup): %w", err)
		}
		return id, nil
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name, label, description, icon, color, unit, health_check
		 FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_services: %w", err)
	}
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		var s Service
		if err := rows.Scan(&s.ID, &s.Name, &s.Label, &s.Description, &s.Icon,
			&s.Color, &s.Unit, &s.HealthCheck); err != nil {
			return nil, fmt.Errorf("store: catalog.list_services: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ── Remote Hosting: implementation ────────────────────────────────────────────

func (v catalogView) CreateOffering(ctx context.Context, o Offering) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO offerings (model_id, variant_id, provider_id, wire_model,
		   price_in_per_1m, price_out_per_1m, currency, context_length, enabled,
		   price_cached_in_per_1m, priority)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.ModelID, nullInt64(o.VariantID), o.ProviderID, o.WireModel,
		o.PriceInPer1M, o.PriceOutPer1M, o.Currency, o.ContextLength,
		boolInt(o.Enabled), floatPtrArg(o.PriceCachedInPer1M), o.Priority)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_offering: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

const offeringSelectCols = `o.id, o.model_id, o.variant_id, o.provider_id, rp.name, o.wire_model,
		   o.price_in_per_1m, o.price_out_per_1m, o.currency, o.context_length, o.enabled,
		   o.price_cached_in_per_1m, o.priority
		 FROM offerings o JOIN router_providers rp ON rp.id = o.provider_id`

func scanOffering(s scanner) (Offering, error) {
	var o Offering
	var varID sql.NullInt64
	var enabled int64
	var priceCachedIn sql.NullFloat64
	if err := s.Scan(&o.ID, &o.ModelID, &varID, &o.ProviderID, &o.ProviderName, &o.WireModel,
		&o.PriceInPer1M, &o.PriceOutPer1M, &o.Currency, &o.ContextLength,
		&enabled, &priceCachedIn, &o.Priority); err != nil {
		return Offering{}, err
	}
	o.VariantID = intOf(varID)
	o.Enabled = enabled != 0
	o.PriceCachedInPer1M = nullFloat64Ptr(priceCachedIn)
	return o, nil
}

func (v catalogView) ListOfferings(ctx context.Context) ([]Offering, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT `+offeringSelectCols+`
		 ORDER BY o.priority, rp.name, o.wire_model`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_offerings: %w", err)
	}
	defer rows.Close()
	out := []Offering{}
	for rows.Next() {
		o, err := scanOffering(rows)
		if err != nil {
			return nil, fmt.Errorf("store: catalog.list_offerings: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ── Annotations: implementation ──────────────────────────────────────────────

func (v catalogView) CreateBenchmark(ctx context.Context, b Benchmark) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO benchmarks (metric, value, source, source_url, source_date,
		   subject_type, subject_id, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		b.Metric, b.Value, b.Source, b.SourceURL, b.SourceDate,
		b.SubjectType, b.SubjectID, b.Notes)
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_benchmark: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListBenchmarks(ctx context.Context) ([]Benchmark, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, metric, value, source, source_url, source_date, subject_type,
		   subject_id, notes
		 FROM benchmarks ORDER BY subject_type, subject_id, metric`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_benchmarks: %w", err)
	}
	defer rows.Close()
	out := []Benchmark{}
	for rows.Next() {
		var b Benchmark
		if err := rows.Scan(&b.ID, &b.Metric, &b.Value, &b.Source, &b.SourceURL,
			&b.SourceDate, &b.SubjectType, &b.SubjectID, &b.Notes); err != nil {
			return nil, fmt.Errorf("store: catalog.list_benchmarks: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (v catalogView) CreateNote(ctx context.Context, n Note) (int64, error) {
	res, err := v.d.sql.ExecContext(ctx,
		`INSERT INTO notes (subject_type, subject_id, author, body, created_at,
		   updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		n.SubjectType, n.SubjectID, n.Author, n.Body,
		unixOf(orNow(n.CreatedAt)), unixOf(orNow(n.UpdatedAt)))
	if err != nil {
		return 0, fmt.Errorf("store: catalog.create_note: %w", err)
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (v catalogView) ListNotes(ctx context.Context) ([]Note, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, subject_type, subject_id, author, body, created_at,
		   updated_at
		 FROM notes ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_notes: %w", err)
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		var created, updated int64
		if err := rows.Scan(&n.ID, &n.SubjectType, &n.SubjectID, &n.Author,
			&n.Body, &created, &updated); err != nil {
			return nil, fmt.Errorf("store: catalog.list_notes: %w", err)
		}
		n.CreatedAt = time.Unix(created, 0).UTC()
		n.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

// ── Phase 3: Get / Update / Delete / Lookup implementations ──────────────────

func (v catalogView) GetModel(ctx context.Context, id int64) (Model, error) {
	var m Model
	var familyID sql.NullInt64
	var kf, mod string
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, family_id, name, architecture, parameter_count, description,
		creator, license_name, license_url, hf_repo, logo, logo_dark, key_features, modalities, visibility
		FROM models WHERE id = ?`, id).
		Scan(&m.ID, &familyID, &m.Name, &m.Architecture, &m.ParameterCount,
			&m.Description, &m.Creator, &m.LicenseName, &m.LicenseURL, &m.HFRepo,
			&m.Logo, &m.LogoDark, &kf, &mod, &m.Visibility)
	if err == sql.ErrNoRows {
		return Model{}, fmt.Errorf("%w: model %d", ErrNotFound, id)
	}
	if err != nil {
		return Model{}, fmt.Errorf("store: catalog.get_model: %w", err)
	}
	m.FamilyID = intOf(familyID)
	m.KeyFeatures = parseJSONList(kf)
	m.Modalities = parseJSONList(mod)
	return m, nil
}

func (v catalogView) UpdateModel(ctx context.Context, m Model) error {
	if m.Visibility == "" {
		m.Visibility = "visible"
	}
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE models SET family_id=?, name=?, architecture=?, parameter_count=?,
		description=?, creator=?, license_name=?, license_url=?, hf_repo=?,
		logo=?, logo_dark=?, key_features=?, modalities=?, visibility=?
		WHERE id=?`,
		nullInt64(m.FamilyID), m.Name, m.Architecture, m.ParameterCount,
		m.Description, m.Creator, m.LicenseName, m.LicenseURL, m.HFRepo, m.Logo, m.LogoDark,
		jsonList(m.KeyFeatures), jsonList(m.Modalities), m.Visibility, m.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_model: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: model %d", ErrNotFound, m.ID)
	}
	return nil
}

func (v catalogView) DeleteModel(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_model: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: model %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ModelByName(ctx context.Context, name string) (Model, error) {
	var m Model
	var familyID sql.NullInt64
	var kf string
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, family_id, name, architecture, parameter_count, description,
		creator, license_name, license_url, hf_repo, logo, logo_dark, key_features, visibility
		FROM models WHERE name = ?`, name).
		Scan(&m.ID, &familyID, &m.Name, &m.Architecture, &m.ParameterCount,
			&m.Description, &m.Creator, &m.LicenseName, &m.LicenseURL, &m.HFRepo,
			&m.Logo, &m.LogoDark, &kf, &m.Visibility)
	if err == sql.ErrNoRows {
		return Model{}, fmt.Errorf("%w: model %q", ErrNotFound, name)
	}
	if err != nil {
		return Model{}, fmt.Errorf("store: catalog.model_by_name: %w", err)
	}
	m.FamilyID = intOf(familyID)
	m.KeyFeatures = parseJSONList(kf)
	return m, nil
}

func (v catalogView) GetVariant(ctx context.Context, id int64) (Variant, error) {
	var vt Variant
	var sourceID sql.NullInt64
	var abl int64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, model_id, name, derivation_type, source_variant_id,
		   trained_ctx, is_abliterated, abliteration_quality
		 FROM variants WHERE id = ?`, id).
		Scan(&vt.ID, &vt.ModelID, &vt.Name, &vt.DerivationType, &sourceID,
			&vt.TrainedCtx, &abl, &vt.AbliterationQuality)
	if err == sql.ErrNoRows {
		return Variant{}, fmt.Errorf("%w: variant %d", ErrNotFound, id)
	}
	if err != nil {
		return Variant{}, fmt.Errorf("store: catalog.get_variant: %w", err)
	}
	vt.SourceVariantID = intOf(sourceID)
	vt.IsAbliterated = abl != 0
	return vt, nil
}

func (v catalogView) UpdateVariant(ctx context.Context, vt Variant) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE variants SET model_id=?, name=?, derivation_type=?,
		   source_variant_id=?, trained_ctx=?, is_abliterated=?,
		   abliteration_quality=?
		 WHERE id=?`,
		vt.ModelID, vt.Name, vt.DerivationType, nullInt64(vt.SourceVariantID),
		vt.TrainedCtx, boolInt(vt.IsAbliterated), vt.AbliterationQuality, vt.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_variant: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: variant %d", ErrNotFound, vt.ID)
	}
	return nil
}

func (v catalogView) DeleteVariant(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM variants WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_variant: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: variant %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ListVariantsForModel(ctx context.Context, modelID int64) ([]Variant, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, model_id, name, derivation_type, source_variant_id,
		   trained_ctx, is_abliterated, abliteration_quality
		 FROM variants WHERE model_id = ? ORDER BY name`, modelID)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_variants_for_model: %w", err)
	}
	defer rows.Close()
	out := []Variant{}
	for rows.Next() {
		var vt Variant
		var sourceID sql.NullInt64
		var abl int64
		if err := rows.Scan(&vt.ID, &vt.ModelID, &vt.Name, &vt.DerivationType,
			&sourceID, &vt.TrainedCtx, &abl, &vt.AbliterationQuality); err != nil {
			return nil, fmt.Errorf("store: catalog.list_variants_for_model: %w", err)
		}
		vt.SourceVariantID = intOf(sourceID)
		vt.IsAbliterated = abl != 0
		out = append(out, vt)
	}
	return out, rows.Err()
}

func (v catalogView) GetArtifact(ctx context.Context, id int64) (Artifact, error) {
	var a Artifact
	var qID sql.NullInt64
	var shardSet, sha sql.NullString
	var isAux, missing int64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, variant_id, quantization_id, format_id, file_path,
		   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
		   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
		   gguf_quant_type
		 FROM artifacts WHERE id = ?`, id).
		Scan(&a.ID, &a.VariantID, &qID, &a.FormatID, &a.FilePath,
			&shardSet, &isAux, &a.ArtifactType, &missing, &sha,
			&a.FileSizeBytes, &a.GGUFArch, &a.GGUFTrainedCtx,
			&a.GGUFParameterCount, &a.GGUFQuantType)
	if err == sql.ErrNoRows {
		return Artifact{}, fmt.Errorf("%w: artifact %d", ErrNotFound, id)
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("store: catalog.get_artifact: %w", err)
	}
	a.QuantizationID = intOf(qID)
	a.ShardSetID = strOf(shardSet)
	a.SHA256 = strOf(sha)
	a.IsAuxiliary = isAux != 0
	a.Missing = missing != 0
	return a, nil
}

func (v catalogView) UpdateArtifact(ctx context.Context, a Artifact) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE artifacts SET variant_id=?, quantization_id=?, format_id=?,
		   file_path=?, shard_set_id=?, is_auxiliary=?, artifact_type=?,
		   missing=?, sha256=?, file_size_bytes=?, gguf_arch=?,
		   gguf_trained_ctx=?, gguf_parameter_count=?, gguf_quant_type=?
		 WHERE id=?`,
		a.VariantID, nullInt64(a.QuantizationID), a.FormatID, a.FilePath,
		nullStr(a.ShardSetID), boolInt(a.IsAuxiliary), a.ArtifactType,
		boolInt(a.Missing), nullStr(a.SHA256), a.FileSizeBytes,
		a.GGUFArch, a.GGUFTrainedCtx, a.GGUFParameterCount, a.GGUFQuantType, a.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_artifact: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: artifact %d", ErrNotFound, a.ID)
	}
	return nil
}

func (v catalogView) DeleteArtifact(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_artifact: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: artifact %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ListArtifactsForVariant(ctx context.Context, variantID int64) ([]Artifact, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, variant_id, quantization_id, format_id, file_path,
		   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
		   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
		   gguf_quant_type
		 FROM artifacts WHERE variant_id = ? ORDER BY id`, variantID)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_artifacts_for_variant: %w", err)
	}
	defer rows.Close()
	out := []Artifact{}
	for rows.Next() {
		var a Artifact
		var qID sql.NullInt64
		var shardSet, sha sql.NullString
		var isAux, missing int64
		if err := rows.Scan(&a.ID, &a.VariantID, &qID, &a.FormatID, &a.FilePath,
			&shardSet, &isAux, &a.ArtifactType, &missing, &sha,
			&a.FileSizeBytes, &a.GGUFArch, &a.GGUFTrainedCtx,
			&a.GGUFParameterCount, &a.GGUFQuantType); err != nil {
			return nil, fmt.Errorf("store: catalog.list_artifacts_for_variant: %w", err)
		}
		a.QuantizationID = intOf(qID)
		a.ShardSetID = strOf(shardSet)
		a.SHA256 = strOf(sha)
		a.IsAuxiliary = isAux != 0
		a.Missing = missing != 0
		out = append(out, a)
	}
	return out, rows.Err()
}

func (v catalogView) ListQuantizations(ctx context.Context) ([]Quantization, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name FROM quantizations ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_quantizations: %w", err)
	}
	defer rows.Close()
	out := []Quantization{}
	for rows.Next() {
		var q Quantization
		if err := rows.Scan(&q.ID, &q.Name); err != nil {
			return nil, fmt.Errorf("store: catalog.list_quantizations: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func (v catalogView) ListFormats(ctx context.Context) ([]Format, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name FROM formats ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_formats: %w", err)
	}
	defer rows.Close()
	out := []Format{}
	for rows.Next() {
		var f Format
		if err := rows.Scan(&f.ID, &f.Name); err != nil {
			return nil, fmt.Errorf("store: catalog.list_formats: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (v catalogView) GetConfig(ctx context.Context, id int64) (Config, error) {
	var c Config
	var buildID, mmprojID sql.NullInt64
	var isDefault int64
	var ea string
	var createdAt int64
	var mod sql.NullString
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, variant_id, weight_artifact_id, engine_id, build_id,
		   mmproj_artifact_id, n_ctx, parallel, extra_args, status, visibility,
		   is_default, fingerprint, created_at, logo, logo_dark, modalities
		 FROM configs WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &c.VariantID, &c.WeightArtifactID, &c.EngineID,
			&buildID, &mmprojID, &c.NCtx, &c.Parallel, &ea, &c.Status,
			&c.Visibility, &isDefault, &c.Fingerprint, &createdAt, &c.Logo, &c.LogoDark, &mod)
	if err == sql.ErrNoRows {
		return Config{}, fmt.Errorf("%w: config %d", ErrNotFound, id)
	}
	if err != nil {
		return Config{}, fmt.Errorf("store: catalog.get_config: %w", err)
	}
	c.BuildID = intOf(buildID)
	c.MMProjArtifactID = intOf(mmprojID)
	c.ExtraArgs = parseJSONList(ea)
	c.IsDefault = isDefault != 0
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	c.Modalities = parseNullJSONList(mod)
	return c, nil
}

func (v catalogView) UpdateConfig(ctx context.Context, c Config) error {
	if c.Status == "" {
		c.Status = "unverified"
	}
	if c.Visibility == "" {
		c.Visibility = "visible"
	}
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE configs SET name=?, variant_id=?, weight_artifact_id=?, engine_id=?,
		   build_id=?, mmproj_artifact_id=?, n_ctx=?, parallel=?, extra_args=?,
		   status=?, visibility=?, is_default=?, fingerprint=?, logo=?, logo_dark=?, modalities=?
		 WHERE id=?`,
		c.Name, c.VariantID, c.WeightArtifactID, c.EngineID,
		nullInt64(c.BuildID), nullInt64(c.MMProjArtifactID),
		c.NCtx, c.Parallel, jsonList(c.ExtraArgs), c.Status, c.Visibility,
		boolInt(c.IsDefault), c.Fingerprint, c.Logo, c.LogoDark, nullJSONList(c.Modalities), c.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: config %d", ErrNotFound, c.ID)
	}
	return nil
}

func (v catalogView) DeleteConfig(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM configs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: config %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ConfigByName(ctx context.Context, name string) (Config, error) {
	var c Config
	var buildID, mmprojID sql.NullInt64
	var isDefault int64
	var ea string
	var createdAt int64
	var mod sql.NullString
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, variant_id, weight_artifact_id, engine_id, build_id,
		   mmproj_artifact_id, n_ctx, parallel, extra_args, status, visibility,
		   is_default, fingerprint, created_at, logo, logo_dark, modalities
		 FROM configs WHERE name = ?`, name).
		Scan(&c.ID, &c.Name, &c.VariantID, &c.WeightArtifactID, &c.EngineID,
			&buildID, &mmprojID, &c.NCtx, &c.Parallel, &ea, &c.Status,
			&c.Visibility, &isDefault, &c.Fingerprint, &createdAt, &c.Logo, &c.LogoDark, &mod)
	if err == sql.ErrNoRows {
		return Config{}, fmt.Errorf("%w: config %q", ErrNotFound, name)
	}
	if err != nil {
		return Config{}, fmt.Errorf("store: catalog.config_by_name: %w", err)
	}
	c.BuildID = intOf(buildID)
	c.MMProjArtifactID = intOf(mmprojID)
	c.ExtraArgs = parseJSONList(ea)
	c.IsDefault = isDefault != 0
	c.CreatedAt = time.Unix(createdAt, 0).UTC()
	c.Modalities = parseNullJSONList(mod)
	return c, nil
}

func (v catalogView) ListConfigsForVariant(ctx context.Context, variantID int64) ([]Config, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, name, variant_id, weight_artifact_id, engine_id, build_id,
		   mmproj_artifact_id, n_ctx, parallel, extra_args, status, visibility,
		   is_default, fingerprint, created_at, logo, logo_dark, modalities
		 FROM configs WHERE variant_id = ? ORDER BY name`, variantID)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_configs_for_variant: %w", err)
	}
	defer rows.Close()
	out := []Config{}
	for rows.Next() {
		var c Config
		var buildID, mmprojID sql.NullInt64
		var isDefault int64
		var ea string
		var createdAt int64
		var mod sql.NullString
		if err := rows.Scan(&c.ID, &c.Name, &c.VariantID, &c.WeightArtifactID,
			&c.EngineID, &buildID, &mmprojID, &c.NCtx, &c.Parallel, &ea,
			&c.Status, &c.Visibility, &isDefault, &c.Fingerprint, &createdAt, &c.Logo, &c.LogoDark, &mod); err != nil {
			return nil, fmt.Errorf("store: catalog.list_configs_for_variant: %w", err)
		}
		c.BuildID = intOf(buildID)
		c.MMProjArtifactID = intOf(mmprojID)
		c.ExtraArgs = parseJSONList(ea)
		c.IsDefault = isDefault != 0
		c.CreatedAt = time.Unix(createdAt, 0).UTC()
		c.Modalities = parseNullJSONList(mod)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (v catalogView) GetSlot(ctx context.Context, id int64) (Slot, error) {
	var s Slot
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, unit, port, label, sort_order FROM slots WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.Unit, &s.Port, &s.Label, &s.SortOrder)
	if err == sql.ErrNoRows {
		return Slot{}, fmt.Errorf("%w: slot %d", ErrNotFound, id)
	}
	if err != nil {
		return Slot{}, fmt.Errorf("store: catalog.get_slot: %w", err)
	}
	return s, nil
}

func (v catalogView) UpdateSlot(ctx context.Context, s Slot) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE slots SET name=?, unit=?, port=?, label=?, sort_order=? WHERE id=?`,
		s.Name, s.Unit, s.Port, s.Label, s.SortOrder, s.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_slot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: slot %d", ErrNotFound, s.ID)
	}
	return nil
}

func (v catalogView) DeleteSlot(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM slots WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_slot: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: slot %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) SlotByName(ctx context.Context, name string) (Slot, error) {
	var s Slot
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, unit, port, label, sort_order FROM slots WHERE name = ?`, name).
		Scan(&s.ID, &s.Name, &s.Unit, &s.Port, &s.Label, &s.SortOrder)
	if err == sql.ErrNoRows {
		return Slot{}, fmt.Errorf("%w: slot %q", ErrNotFound, name)
	}
	if err != nil {
		return Slot{}, fmt.Errorf("store: catalog.slot_by_name: %w", err)
	}
	return s, nil
}

func (v catalogView) GetService(ctx context.Context, id int64) (Service, error) {
	var s Service
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, label, description, icon, color, unit, health_check
		 FROM services WHERE id = ?`, id).
		Scan(&s.ID, &s.Name, &s.Label, &s.Description, &s.Icon, &s.Color,
			&s.Unit, &s.HealthCheck)
	if err == sql.ErrNoRows {
		return Service{}, fmt.Errorf("%w: service %d", ErrNotFound, id)
	}
	if err != nil {
		return Service{}, fmt.Errorf("store: catalog.get_service: %w", err)
	}
	return s, nil
}

func (v catalogView) UpdateService(ctx context.Context, s Service) error {
	hc := s.HealthCheck
	if hc == "" {
		hc = "{}"
	}
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE services SET name=?, label=?, description=?, icon=?, color=?,
		   unit=?, health_check=?
		 WHERE id=?`,
		s.Name, s.Label, s.Description, s.Icon, s.Color, s.Unit, hc, s.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: service %d", ErrNotFound, s.ID)
	}
	return nil
}

func (v catalogView) DeleteService(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: service %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ServiceByName(ctx context.Context, name string) (Service, error) {
	var s Service
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, name, label, description, icon, color, unit, health_check
		 FROM services WHERE name = ?`, name).
		Scan(&s.ID, &s.Name, &s.Label, &s.Description, &s.Icon, &s.Color,
			&s.Unit, &s.HealthCheck)
	if err == sql.ErrNoRows {
		return Service{}, fmt.Errorf("%w: service %q", ErrNotFound, name)
	}
	if err != nil {
		return Service{}, fmt.Errorf("store: catalog.service_by_name: %w", err)
	}
	return s, nil
}

func (v catalogView) GetOffering(ctx context.Context, id int64) (Offering, error) {
	o, err := scanOffering(v.d.sql.QueryRowContext(ctx,
		`SELECT `+offeringSelectCols+` WHERE o.id = ?`, id))
	if err == sql.ErrNoRows {
		return Offering{}, fmt.Errorf("%w: offering %d", ErrNotFound, id)
	}
	if err != nil {
		return Offering{}, fmt.Errorf("store: catalog.get_offering: %w", err)
	}
	return o, nil
}

func (v catalogView) UpdateOffering(ctx context.Context, o Offering) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE offerings SET model_id=?, variant_id=?, provider_id=?, wire_model=?,
		   price_in_per_1m=?, price_out_per_1m=?, currency=?, context_length=?,
		   enabled=?, price_cached_in_per_1m=?, priority=?
		 WHERE id=?`,
		o.ModelID, nullInt64(o.VariantID), o.ProviderID, o.WireModel,
		o.PriceInPer1M, o.PriceOutPer1M, o.Currency, o.ContextLength,
		boolInt(o.Enabled), floatPtrArg(o.PriceCachedInPer1M), o.Priority, o.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_offering: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: offering %d", ErrNotFound, o.ID)
	}
	return nil
}

func (v catalogView) DeleteOffering(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM offerings WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_offering: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: offering %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ListOfferingsForModel(ctx context.Context, modelID int64) ([]Offering, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT `+offeringSelectCols+`
		 WHERE o.model_id = ? ORDER BY o.priority, rp.name, o.wire_model`, modelID)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_offerings_for_model: %w", err)
	}
	defer rows.Close()
	out := []Offering{}
	for rows.Next() {
		o, err := scanOffering(rows)
		if err != nil {
			return nil, fmt.Errorf("store: catalog.list_offerings_for_model: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (v catalogView) ProviderExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT 1 FROM router_providers WHERE name = ? AND deleted_at IS NULL`, name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: catalog.provider_exists: %w", err)
	}
	return true, nil
}

func (v catalogView) GetBenchmark(ctx context.Context, id int64) (Benchmark, error) {
	var b Benchmark
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, metric, value, source, source_url, source_date, subject_type,
		   subject_id, notes
		 FROM benchmarks WHERE id = ?`, id).
		Scan(&b.ID, &b.Metric, &b.Value, &b.Source, &b.SourceURL, &b.SourceDate,
			&b.SubjectType, &b.SubjectID, &b.Notes)
	if err == sql.ErrNoRows {
		return Benchmark{}, fmt.Errorf("%w: benchmark %d", ErrNotFound, id)
	}
	if err != nil {
		return Benchmark{}, fmt.Errorf("store: catalog.get_benchmark: %w", err)
	}
	return b, nil
}

func (v catalogView) UpdateBenchmark(ctx context.Context, b Benchmark) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE benchmarks SET metric=?, value=?, source=?, source_url=?,
		   source_date=?, subject_type=?, subject_id=?, notes=?
		 WHERE id=?`,
		b.Metric, b.Value, b.Source, b.SourceURL, b.SourceDate,
		b.SubjectType, b.SubjectID, b.Notes, b.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_benchmark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: benchmark %d", ErrNotFound, b.ID)
	}
	return nil
}

func (v catalogView) DeleteBenchmark(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM benchmarks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_benchmark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: benchmark %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ListBenchmarksForSubject(ctx context.Context, subjectType string, subjectID int64) ([]Benchmark, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, metric, value, source, source_url, source_date, subject_type,
		   subject_id, notes
		 FROM benchmarks WHERE subject_type = ? AND subject_id = ?
		 ORDER BY metric`, subjectType, subjectID)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_benchmarks_for_subject: %w", err)
	}
	defer rows.Close()
	out := []Benchmark{}
	for rows.Next() {
		var b Benchmark
		if err := rows.Scan(&b.ID, &b.Metric, &b.Value, &b.Source, &b.SourceURL,
			&b.SourceDate, &b.SubjectType, &b.SubjectID, &b.Notes); err != nil {
			return nil, fmt.Errorf("store: catalog.list_benchmarks_for_subject: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (v catalogView) GetNote(ctx context.Context, id int64) (Note, error) {
	var n Note
	var created, updated int64
	err := v.d.sql.QueryRowContext(ctx,
		`SELECT id, subject_type, subject_id, author, body, created_at,
		   updated_at
		 FROM notes WHERE id = ?`, id).
		Scan(&n.ID, &n.SubjectType, &n.SubjectID, &n.Author, &n.Body,
			&created, &updated)
	if err == sql.ErrNoRows {
		return Note{}, fmt.Errorf("%w: note %d", ErrNotFound, id)
	}
	if err != nil {
		return Note{}, fmt.Errorf("store: catalog.get_note: %w", err)
	}
	n.CreatedAt = time.Unix(created, 0).UTC()
	n.UpdatedAt = time.Unix(updated, 0).UTC()
	return n, nil
}

func (v catalogView) UpdateNote(ctx context.Context, n Note) error {
	now := time.Now()
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE notes SET subject_type=?, subject_id=?, author=?, body=?,
		   updated_at=?
		 WHERE id=?`,
		n.SubjectType, n.SubjectID, n.Author, n.Body, unixOf(now), n.ID)
	if err != nil {
		return fmt.Errorf("store: catalog.update_note: %w", err)
	}
	if n2, _ := res.RowsAffected(); n2 == 0 {
		return fmt.Errorf("%w: note %d", ErrNotFound, n.ID)
	}
	return nil
}

func (v catalogView) DeleteNote(ctx context.Context, id int64) error {
	res, err := v.d.sql.ExecContext(ctx, `DELETE FROM notes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: catalog.delete_note: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: note %d", ErrNotFound, id)
	}
	return nil
}

func (v catalogView) ListNotesForSubject(ctx context.Context, subjectType string, subjectID int64) ([]Note, error) {
	rows, err := v.d.sql.QueryContext(ctx,
		`SELECT id, subject_type, subject_id, author, body, created_at,
		   updated_at
		 FROM notes WHERE subject_type = ? AND subject_id = ?
		 ORDER BY updated_at DESC`, subjectType, subjectID)
	if err != nil {
		return nil, fmt.Errorf("store: catalog.list_notes_for_subject: %w", err)
	}
	defer rows.Close()
	out := []Note{}
	for rows.Next() {
		var n Note
		var created, updated int64
		if err := rows.Scan(&n.ID, &n.SubjectType, &n.SubjectID, &n.Author,
			&n.Body, &created, &updated); err != nil {
			return nil, fmt.Errorf("store: catalog.list_notes_for_subject: %w", err)
		}
		n.CreatedAt = time.Unix(created, 0).UTC()
		n.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

func (v catalogView) SetModelLogo(ctx context.Context, modelID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE models SET logo = ? WHERE id = ?`, logo, modelID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_model_logo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: model %d", ErrNotFound, modelID)
	}
	return nil
}

// SetFamilyLogo/SetGenealogyLogo/SetConfigLogo mirror SetModelLogo (Sprint I
// — icon inheritance hierarchy, docs/v5-prerelease-readiness.md). Each is a
// single-column write so the icon-upload endpoint doesn't need the caller to
// round-trip the rest of the row, unlike Update{Family,Genealogy,Config}
// which are full-replace.

func (v catalogView) SetFamilyLogo(ctx context.Context, familyID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE families SET logo = ? WHERE id = ?`, logo, familyID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_family_logo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: family %d", ErrNotFound, familyID)
	}
	return nil
}

func (v catalogView) SetGenealogyLogo(ctx context.Context, genealogyID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE genealogies SET logo = ? WHERE id = ?`, logo, genealogyID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_genealogy_logo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: genealogy %d", ErrNotFound, genealogyID)
	}
	return nil
}

func (v catalogView) SetConfigLogo(ctx context.Context, configID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE configs SET logo = ? WHERE id = ?`, logo, configID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_config_logo: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: config %d", ErrNotFound, configID)
	}
	return nil
}

// Set*LogoDark (Phase 3 icon-variant work) mirror the Set*Logo group above
// exactly, writing logo_dark instead of logo.

func (v catalogView) SetModelLogoDark(ctx context.Context, modelID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE models SET logo_dark = ? WHERE id = ?`, logo, modelID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_model_logo_dark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: model %d", ErrNotFound, modelID)
	}
	return nil
}

func (v catalogView) SetFamilyLogoDark(ctx context.Context, familyID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE families SET logo_dark = ? WHERE id = ?`, logo, familyID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_family_logo_dark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: family %d", ErrNotFound, familyID)
	}
	return nil
}

func (v catalogView) SetGenealogyLogoDark(ctx context.Context, genealogyID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE genealogies SET logo_dark = ? WHERE id = ?`, logo, genealogyID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_genealogy_logo_dark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: genealogy %d", ErrNotFound, genealogyID)
	}
	return nil
}

func (v catalogView) SetConfigLogoDark(ctx context.Context, configID int64, logo string) error {
	res, err := v.d.sql.ExecContext(ctx,
		`UPDATE configs SET logo_dark = ? WHERE id = ?`, logo, configID)
	if err != nil {
		return fmt.Errorf("store: catalog.set_config_logo_dark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: config %d", ErrNotFound, configID)
	}
	return nil
}

// RegisterDownloadedModel writes b's full row set inside one transaction.
// See ModelBundle's doc comment for why this exists rather than four
// separate Create* calls. The SQL mirrors CreateModel/CreateVariant/
// CreateArtifact/CreateConfig verbatim (same columns, same defaulting),
// just issued against tx instead of v.d.sql.
func (v catalogView) RegisterDownloadedModel(ctx context.Context, b ModelBundle) (ModelBundleResult, error) {
	tx, err := v.d.sql.BeginTx(ctx, nil)
	if err != nil {
		return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: begin: %w", err)
	}
	defer tx.Rollback()

	m := b.Model
	if m.Visibility == "" {
		m.Visibility = "visible"
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO models (family_id, name, architecture, parameter_count,
		description, creator, license_name, license_url, hf_repo, logo, logo_dark,
		key_features, modalities, visibility)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nullInt64(m.FamilyID), m.Name, m.Architecture, m.ParameterCount,
		m.Description, m.Creator, m.LicenseName, m.LicenseURL, m.HFRepo, m.Logo, m.LogoDark,
		jsonList(m.KeyFeatures), jsonList(m.Modalities), m.Visibility)
	if err != nil {
		return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_model: %w", err)
	}
	var out ModelBundleResult
	out.ModelID, _ = res.LastInsertId()

	vt := b.Variant
	vt.ModelID = out.ModelID
	res, err = tx.ExecContext(ctx,
		`INSERT INTO variants (model_id, name, derivation_type, source_variant_id,
		   trained_ctx, is_abliterated, abliteration_quality)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		vt.ModelID, vt.Name, vt.DerivationType, nullInt64(vt.SourceVariantID),
		vt.TrainedCtx, boolInt(vt.IsAbliterated), vt.AbliterationQuality)
	if err != nil {
		return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_variant: %w", err)
	}
	out.VariantID, _ = res.LastInsertId()

	a := b.Artifact
	a.VariantID = out.VariantID
	if a.ArtifactType == "" {
		a.ArtifactType = "weight"
	}
	res, err = tx.ExecContext(ctx,
		`INSERT INTO artifacts (variant_id, quantization_id, format_id, file_path,
		   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
		   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
		   gguf_quant_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.VariantID, nullInt64(a.QuantizationID), a.FormatID, a.FilePath,
		nullStr(a.ShardSetID), boolInt(a.IsAuxiliary), a.ArtifactType,
		boolInt(a.Missing), nullStr(a.SHA256), a.FileSizeBytes,
		a.GGUFArch, a.GGUFTrainedCtx, a.GGUFParameterCount, a.GGUFQuantType)
	if err != nil {
		return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_artifact: %w", err)
	}
	out.ArtifactID, _ = res.LastInsertId()

	for _, extra := range b.ExtraArtifacts {
		extra.VariantID = out.VariantID
		if extra.ArtifactType == "" {
			extra.ArtifactType = "weight"
		}
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO artifacts (variant_id, quantization_id, format_id, file_path,
			   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
			   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
			   gguf_quant_type)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			extra.VariantID, nullInt64(extra.QuantizationID), extra.FormatID, extra.FilePath,
			nullStr(extra.ShardSetID), boolInt(extra.IsAuxiliary), extra.ArtifactType,
			boolInt(extra.Missing), nullStr(extra.SHA256), extra.FileSizeBytes,
			extra.GGUFArch, extra.GGUFTrainedCtx, extra.GGUFParameterCount, extra.GGUFQuantType); err != nil {
			return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_extra_artifact: %w", err)
		}
	}

	if b.MMProj != nil {
		mp := *b.MMProj
		mp.VariantID = out.VariantID
		mp.IsAuxiliary = true
		if mp.ArtifactType == "" {
			mp.ArtifactType = "mmproj"
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO artifacts (variant_id, quantization_id, format_id, file_path,
			   shard_set_id, is_auxiliary, artifact_type, missing, sha256,
			   file_size_bytes, gguf_arch, gguf_trained_ctx, gguf_parameter_count,
			   gguf_quant_type)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			mp.VariantID, nullInt64(mp.QuantizationID), mp.FormatID, mp.FilePath,
			nullStr(mp.ShardSetID), boolInt(mp.IsAuxiliary), mp.ArtifactType,
			boolInt(mp.Missing), nullStr(mp.SHA256), mp.FileSizeBytes,
			mp.GGUFArch, mp.GGUFTrainedCtx, mp.GGUFParameterCount, mp.GGUFQuantType)
		if err != nil {
			return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_mmproj_artifact: %w", err)
		}
		out.MMProjArtifactID, _ = res.LastInsertId()

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO compatibilities (auxiliary_artifact_id, variant_id) VALUES (?, ?)`,
			out.MMProjArtifactID, out.VariantID); err != nil {
			return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_compatibility: %w", err)
		}
	}

	c := b.Config
	c.VariantID = out.VariantID
	c.WeightArtifactID = out.ArtifactID
	c.MMProjArtifactID = out.MMProjArtifactID
	if c.Status == "" {
		c.Status = "unverified"
	}
	if c.Visibility == "" {
		c.Visibility = "hidden"
	}
	res, err = tx.ExecContext(ctx,
		`INSERT INTO configs (name, variant_id, weight_artifact_id, engine_id,
		   build_id, mmproj_artifact_id, n_ctx, parallel, extra_args, status,
		   visibility, is_default, fingerprint, created_at, logo, logo_dark, modalities)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Name, c.VariantID, c.WeightArtifactID, c.EngineID,
		nullInt64(c.BuildID), nullInt64(c.MMProjArtifactID),
		c.NCtx, c.Parallel, jsonList(c.ExtraArgs), c.Status, c.Visibility,
		boolInt(c.IsDefault), c.Fingerprint, unixOf(orNow(c.CreatedAt)), c.Logo, c.LogoDark,
		nullJSONList(c.Modalities))
	if err != nil {
		return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: create_config: %w", err)
	}
	out.ConfigID, _ = res.LastInsertId()

	if err := tx.Commit(); err != nil {
		return ModelBundleResult{}, fmt.Errorf("store: catalog.register_downloaded_model: commit: %w", err)
	}
	return out, nil
}
