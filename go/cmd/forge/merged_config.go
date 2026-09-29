// SPDX-License-Identifier: Apache-2.0

package main

// merged_config.go — Phase 2 of MODEL CATALOG (docs/v5-modes-config-editable.md
// §Phase 2). The merged-config seam: overlays store-backed catalog data
// (configs, services, artifacts, variants, models, builds) on top of the
// store-backed infra config (Server, Paths, Slots, Ports, Scheduler,
// Monitor, Tailscale, Cost — config.LoadFromStore, TOML decommission
// Phase 1/3).
//
// The `func() *config.Config` seam in main.go is the single point where all
// engine/collector/registry/profile read sites get their config. By making
// this seam return a merged view, all read sites continue to work
// unchanged — they still call `cfg()` and get a `*config.Config` with
// `.Modes` populated, sourced entirely from the catalog (an empty `configs`
// table just means zero Modes — the pre-MODEL-CATALOG file-fallback feature
// flag was removed once the catalog became the sole source, TOML
// decommission Phase 3, docs/v5-toml-decommission.md §6).
//
// Caching: the merged view is cached with a short TTL (5 seconds) so hot
// paths (CurrentMode, inferSlotMode — called on every scheduling decision
// and collector cycle) don't hit the DB every call.

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/store"
)

// mergedConfigTTL is how long the merged view is cached before rebuilding
// from the store. Short enough that CRUD edits (Phase 3) appear on the next
// cycle; long enough that hot paths don't DB-thrash.
const mergedConfigTTL = 5 * time.Second

// mergedConfigProvider implements `func() *config.Config` by overlaying
// store-backed catalog data on top of the file-owned config. It replaces the
// raw `cfgHolder.Load()` closure in main.go.
type mergedConfigProvider struct {
	fileCfg func() *config.Config // the atomic.Pointer loader (SIGHUP-reloadable)
	catalog store.Catalog         // the store's catalog surface (nil → file-only)

	mu       sync.Mutex
	cached   *config.Config
	cachedAt time.Time
}

// newMergedConfigProvider creates a merged-config provider. If catalog is
// nil, it degrades to a thin wrapper around fileCfg (no store reads — used
// in tests and any environment where the store isn't wired).
func newMergedConfigProvider(fileCfg func() *config.Config, catalog store.Catalog) *mergedConfigProvider {
	return &mergedConfigProvider{fileCfg: fileCfg, catalog: catalog}
}

// Get returns the merged config, cached for mergedConfigTTL. On any store
// error, it falls back to the base config with Modes untouched (never
// crashes the daemon on a DB issue — the base config is always usable, just
// stale on Modes until the next successful rebuild).
func (p *mergedConfigProvider) Get() *config.Config {
	// Fast path: cache hit.
	p.mu.Lock()
	if p.cached != nil && time.Since(p.cachedAt) < mergedConfigTTL {
		result := p.cached
		p.mu.Unlock()
		return result
	}
	p.mu.Unlock()

	// Slow path: rebuild. The lock is held only during the cache check/
	// store, not during the DB reads (which may be slow).
	fileCfg := p.fileCfg()
	if p.catalog == nil {
		return fileCfg
	}

	merged, err := p.buildMerged(fileCfg)
	if err != nil {
		// Store read failed — fall back to the base config. Log so the
		// operator sees the degradation, but don't crash.
		log.Printf("forge: merged config build failed, using base config: %v", err)
		return fileCfg
	}

	p.mu.Lock()
	p.cached = merged
	p.cachedAt = time.Now()
	p.mu.Unlock()
	return merged
}

// buildMerged reads the store catalog and overlays store-backed Modes on
// top of the base config. The base config's infra fields (Server, Paths,
// Slots, Ports, Scheduler, Monitor, Tailscale, Cost) are preserved; only
// Modes is replaced, entirely from the catalog (zero configs → zero Modes,
// no fallback — see the package doc comment).
func (p *mergedConfigProvider) buildMerged(fileCfg *config.Config) (*config.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	configs, err := p.catalog.ListConfigs(ctx)
	if err != nil {
		return nil, err
	}

	// Read all supporting tables in parallel-ish (sequential is fine — these
	// are small tables and we're on a single SQLite connection anyway).
	variants, err := p.catalog.ListVariants(ctx)
	if err != nil {
		return nil, err
	}
	models, err := p.catalog.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	families, err := p.catalog.ListFamilies(ctx)
	if err != nil {
		return nil, err
	}
	artifacts, err := p.catalog.ListArtifacts(ctx)
	if err != nil {
		return nil, err
	}
	builds, err := p.catalog.ListBuilds(ctx)
	if err != nil {
		return nil, err
	}
	services, err := p.catalog.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	engines, err := p.catalog.ListEngines(ctx)
	if err != nil {
		return nil, err
	}

	// Build lookup maps.
	modelByID := make(map[int64]store.Model, len(models))
	for _, m := range models {
		modelByID[m.ID] = m
	}
	variantByID := make(map[int64]store.Variant, len(variants))
	for _, v := range variants {
		variantByID[v.ID] = v
	}
	familyByID := make(map[int64]store.Family, len(families))
	for _, f := range families {
		familyByID[f.ID] = f
	}
	artifactByID := make(map[int64]store.Artifact, len(artifacts))
	for _, a := range artifacts {
		artifactByID[a.ID] = a
	}
	buildByID := make(map[int64]store.Build, len(builds))
	for _, b := range builds {
		buildByID[b.ID] = b
	}
	engineByID := make(map[int64]store.Engine, len(engines))
	for _, e := range engines {
		engineByID[e.ID] = e
	}

	// Build Modes from configs. A Config that fails to convert (no resolvable
	// backend — see configFromCatalog) is logged and skipped, not defaulted:
	// it becomes an "unknown mode" for the engine, a safe and loud failure,
	// rather than risk launching the wrong binary for a model. This must not
	// abort the whole merge — every other correctly-configured mode should
	// stay reachable.
	modes := make(map[string]config.Mode, len(configs)+len(services))
	for _, c := range configs {
		mode, err := configFromCatalog(c, variantByID, modelByID, familyByID,
			artifactByID, buildByID, engineByID)
		if err != nil {
			log.Printf("forge: merged config: skipping config %q: %v", c.Name, err)
			continue
		}
		modes[c.Name] = mode
	}

	// Build Modes from services (Type="service").
	for _, s := range services {
		modes[s.Name] = config.Mode{
			Type:        "service",
			Label:       s.Label,
			Description: s.Description,
			Icon:        s.Icon,
			Color:       s.Color,
			Unit:        s.Unit,
		}
	}

	// Shallow-copy the file config and replace Modes. The infra fields
	// (Server, Paths, Slots, Ports, Scheduler, Monitor, Tailscale, Cost)
	// stay file-owned; only Modes is store-backed.
	merged := *fileCfg
	merged.Modes = modes
	return &merged, nil
}

// configFromCatalog converts a store.Config (plus its related entities) back
// into a config.Mode that the engine/collector/registry/profile can consume
// unchanged. This is the inverse of the migrate-v4 seed: seed reads
// config.Mode → writes catalog rows; this reads catalog rows → config.Mode.
//
// Returns an error if the Config has no Build with an explicit Backend set —
// this is a hard failure, never a guess. Found live (2026-07-28): the prior
// version derived backend by string-matching "ROCM" in the Build's name and
// fell back to "vulkan" whenever there was no Build at all (the common case —
// migrate-v4 only created a Build for modes with a custom llama_bin
// override). nemotron (backend 'rocm' in forge.toml, no custom llama_bin)
// silently got the vulkan binary at runtime, OOM'd past Vulkan's ~63GB
// ceiling, and took the host down with a kernel panic. A Config with no
// resolvable backend is now surfaced as an error by the caller (buildMerged
// logs it and omits the mode — "unknown mode" is a safe, loud failure; a
// wrong backend guess is not) instead of ever being guessed.
func configFromCatalog(
	c store.Config,
	variantByID map[int64]store.Variant,
	modelByID map[int64]store.Model,
	familyByID map[int64]store.Family,
	artifactByID map[int64]store.Artifact,
	buildByID map[int64]store.Build,
	engineByID map[int64]store.Engine,
) (config.Mode, error) {
	vt := variantByID[c.VariantID]
	mdl := modelByID[vt.ModelID]
	fam := familyByID[mdl.FamilyID]

	// Weight artifact → the model file path.
	weight := artifactByID[c.WeightArtifactID]
	modelPath := weight.FilePath

	// MMProj artifact → the mmproj path (empty if none).
	var mmprojPath string
	if c.MMProjArtifactID != 0 {
		mmprojPath = artifactByID[c.MMProjArtifactID].FilePath
	}

	// Build → binary path + backend. Both come directly from the Build row —
	// no derivation, no fallback. A missing or unclassified Build is a hard
	// error; see `forge repair-catalog-backend` for fixing already-seeded
	// Configs that predate the Backend column.
	build, ok := buildByID[c.BuildID]
	if c.BuildID == 0 || !ok || build.Backend == "" {
		return config.Mode{}, fmt.Errorf(
			"config %q (id=%d): no backend defined (build_id=%d) — refusing to guess; "+
				"run `forge repair-catalog-backend`", c.Name, c.ID, c.BuildID)
	}

	// Build the Mode. The Services[] slice carries the launch recipe (the
	// engine reads Services[0] for the model path, context, backend, etc.).
	// Display fields (Label, Family, Description, Icon, Tags) come from the
	// Model entity, not the Config — the V4 Mode conflated them, the catalog
	// separates them, and this conversion rejoins them for the V4 read sites.
	//
	// Alias: set to the Config name (c.Name), NOT the Variant name. The alias
	// becomes the --alias flag passed to llama-server, which sets the model
	// name in the OpenAI-compatible API. a0 routes by config/mode name, so
	// the alias must match c.Name for routing to work. Using the Variant
	// name here (e.g. "Stock", "Q4_K_M") would break model resolution.
	return config.Mode{
		ConfigID:    c.ID, // B2: populated from configs.id for the engine → registry path
		Label:       mdl.Name,
		Family:      fam.Name,
		Description: mdl.Description,
		Icon:        mdl.Logo,
		Tags:        mdl.KeyFeatures,
		Default:     c.IsDefault,
		Type:        "", // inference (services are separate)
		Services: []config.Service{{
			Model:     modelPath,
			Alias:     c.Name,
			Context:   c.NCtx,
			PortRole:  "a1", // slot is runtime now, but V4 read sites (SwitchMode) expect this
			Backend:   build.Backend,
			LlamaBin:  build.BinaryPath,
			MMProj:    mmprojPath,
			ExtraArgs: c.ExtraArgs,
			// 120s was too tight for laguna-s-21 (118B-A8B + DFlash draft head,
			// 262144 ctx): a still-draining GTT pool from the prior slot's
			// unload (waitGTTDrain only waits 20s before proceeding anyway,
			// see engine/lifecycle.go) left the process fighting for GPU
			// memory past the old ceiling on ~half its load attempts, each
			// failure burning another cycle before a clean retry finally
			// landed. 300s covers that without cost to fast-loading models,
			// which finish in 20-40s either way. Found live 2026-07-29/30 —
			// see progress.md.
			StartupTimeoutS: 300,
		}},
	}, nil
}

// Invalidate clears the cache so the next Get() rebuilds from the store.
// Called after SIGHUP (file config changed → re-merge) and after CRUD
// operations in Phase 3 (store data changed → re-merge).
func (p *mergedConfigProvider) Invalidate() {
	p.mu.Lock()
	p.cached = nil
	p.mu.Unlock()
}
