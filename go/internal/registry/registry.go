// SPDX-License-Identifier: Apache-2.0

// Package registry is the model-selection UI's card data source (Phase B:
// models.toml → DB migration). The registry reads exclusively from the
// catalog DB (store.Catalog) — models.toml is retired and its archive
// (historical/models.toml) was removed in 2026-08-15.
//
// Two card scopes exist (design decision 4: Config card ≠ Model card):
//
//   - ConfigCard (B1): one card per Config. Backs GET /api/v1/configs/cards.
//     The console "Choose a config" gallery consumes this. Each card carries
//     config launch data (n_ctx, status, visibility) alongside denormalized
//     model identity, variant quality, weight-artifact file size, performance
//     benchmarks, and live-derived history/reliability.
//
//   - Card (model-scoped): one card per Model. Backs GET /api/v1/models/cards.
//     The model gallery modal (opened by the "MODEL" button on a config card)
//     consumes this. Aggregates identity + capabilities + quality; performance
//     and derived data come from the model's first config.
//
// Card assembly merges catalog data with data derived live at read time:
// GGUF header metadata (internal/gguf — header+KV only), on-disk weight-set
// size (internal/collector.WeightSetSizeBytes), per-config load history, and
// usage/reliability counters (both from store.Usage).
package registry

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/gguf"
	"github.com/jsaigou/the-forge/internal/store"
)

// Registry is the read surface the dashboard API and engine memory-budget
// lookups need. Phase B: methods take configID int64 (not mode string) —
// the engine resolves name→ID at the merged seam (config.Mode.ConfigID)
// and passes the ID downstream. No string lookups in the hot path.
type Registry interface {
	// Cards assembles one ConfigCard per catalog Config (B1: config-scoped).
	// since bounds the usage/reliability window; a zero Time means "all
	// time" (caller resolves the window string to a duration).
	Cards(ctx context.Context, since time.Time) ([]ConfigCard, error)

	// ModelCards assembles one Card per catalog Model (model-scoped, for
	// the model gallery modal). GET /api/v1/models/cards consumes this.
	ModelCards(ctx context.Context, since time.Time) ([]Card, error)

	// WeightEstimateBytes is the curated safe_memory_bytes benchmark for
	// the config's variant (was registry.MemoryReqBytes in V4, reading
	// models.toml's performance.memory_req_gb). ok=false when no benchmark
	// exists (the engine falls back to on-disk weight size). Consumed by
	// engine.can_fit's memory-budget fallback chain.
	WeightEstimateBytes(configID int64) (int64, bool)

	// CostPer1k is deprecated (power_cost_per_1k is not stored in the
	// catalog). Always returns 0. Kept on the interface for back-compat
	// (the card wire shape still emits power_cost_per_1k: 0).
	CostPer1k(configID int64) float64

	// PowerEstPer1m is the per-1M-token USD electricity cost estimate
	// (Sprint 0 §0.2) for the config's variant: computed from first
	// principles — power_kW × (1e6 / (tps × 3600)) × rate_per_kWh, using
	// the variant's decode_tps benchmark as tps and config.Cost for
	// power/rate. ok=false when no usable T/s figure exists.
	PowerEstPer1m(configID int64) (float64, bool)
}

// New builds a Registry reading from the catalog DB. cat is the store's
// catalog surface; cfg provides ModelsDir (for live GGUF/disk reads) and
// Cost config; usage provides history/reliability (may be nil — those
// fields are simply omitted).
func New(cat store.Catalog, cfg func() *config.Config, usage store.Usage, opts ...Option) Registry {
	r := &catalogRegistry{cat: cat, cfg: cfg, usage: usage}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Option configures a catalogRegistry (option pattern, same convention as
// authz.WithHasher / fx.WithFetch).
type Option func(*catalogRegistry)

// WithProfileDecodeTPS wires the profile-aware pricing seam: fn resolves a
// mode's FRESH measured decode tok/s (the profile.Runner's DecodeTPS), used
// by costPerMillion as the preferred T/s over the curated decode_tps
// benchmark. See catalogRegistry.ProfileDecodeTPS.
func WithProfileDecodeTPS(fn func(mode string) (tps float64, ok bool)) Option {
	return func(r *catalogRegistry) { r.ProfileDecodeTPS = fn }
}

// ── Output shapes (Contract 1 §3 ModelCard, web/src/lib/types.ts) ───────────

// ConfigCard is a config-scoped card: one card per Config (B1). Backs
// GET /api/v1/configs/cards.
type ConfigCard struct {
	ID         int64   `json:"id"`       // configs.id
	Name       string  `json:"name"`     // config name (= mode name)
	ModelID    string  `json:"model_id"` // model ID as string (for modeToModelID)
	NCtx       int     `json:"n_ctx"`
	Status     string  `json:"status"`     // unverified | verified
	Visibility string  `json:"visibility"` // visible | hidden
	IsDefault  bool    `json:"is_default"`
	CreatedAt  float64 `json:"created_at"` // unix seconds

	// Model identity (denormalized for display).
	ModelName    string       `json:"model_name"`
	Creator      string       `json:"creator"`
	LicenseName  string       `json:"license_name"`
	LicenseURL   string       `json:"license_url"`
	Description  string       `json:"description"`
	KeyFeatures  []string     `json:"key_features"`
	Badges       []Badge      `json:"badges"`
	Logo         string       `json:"logo"`
	LogoDark     string       `json:"logo_dark"` // dark-theme variant; "" = same as Logo
	HFRepo       string       `json:"hf_repo"`
	Family       string       `json:"family"`
	Capabilities []Capability `json:"capabilities"`
	// Modalities (Sprint J1) are what THIS config can actually deliver —
	// the model's architectural modalities, narrowed by mmproj presence and
	// any explicit config-level override. ModalitiesUnavailable lists
	// modalities the model supports that this config can't, each with a
	// human reason (missing mmproj, mmproj file missing on disk, or an
	// explicit override) — shown as a visible, fixable gap rather than
	// silently omitted. See registry.resolveModalities.
	Modalities            []string      `json:"modalities"`
	ModalitiesUnavailable []ModalityGap `json:"modalities_unavailable"`

	// Sprint B: the load recipe itself, for the config expanded view's
	// "load options" list. ExtraArgs is the flat llama.cpp argv token array
	// (one element per launcher-file line, see
	// go/internal/engine/sysconfig.go's writeServiceFiles) — the only
	// source of truth for --parallel and friends; store.Config.Parallel is
	// a dead column (validated/persisted but never read by configToMode)
	// and is deliberately NOT exposed here to avoid implying it does
	// anything. Backend is rocm|vulkan|vllm from the linked Build — the
	// single most load-bearing fact about a config on this hardware (see
	// CLAUDE.md's Vulkan ceiling note). VariantName disambiguates configs
	// that share a model (e.g. different quants of the same weights).
	ExtraArgs   []string `json:"extra_args"`
	Backend     string   `json:"backend"`
	VariantName string   `json:"variant_name"`

	// Variant quality.
	Quality Quality `json:"quality"`

	// Performance (from benchmarks: decode_tps, safe_memory_bytes).
	Performance Performance `json:"performance"`

	// Derived (live: GGUF metadata, file size, history, reliability).
	Derived Derived `json:"derived"`
}

// Card is a model-scoped card: one card per Model. Backs
// GET /api/v1/models/cards (the Models tab — family-grouped model gallery).
type Card struct {
	ID          string   `json:"id"`   // model ID as string
	Name        string   `json:"name"` // model display name
	Creator     string   `json:"creator"`
	LicenseName string   `json:"license_name"`
	LicenseURL  string   `json:"license_url"`
	Description string   `json:"description"`
	KeyFeatures []string `json:"key_features"`
	Badges      []Badge  `json:"badges"`
	// Modalities (Sprint J1) are what this model's architecture supports
	// (text/vision/audio) — always the model-level fact, independent of any
	// one config's ability to deliver it (see ConfigCard.Modalities for the
	// config-scoped, narrowed view).
	Modalities []string `json:"modalities"`
	Logo       string   `json:"logo"`
	LogoDark   string   `json:"logo_dark"` // dark-theme variant; "" = same as Logo
	HFRepo     string   `json:"hf_repo"`
	Family     string   `json:"family"`
	// Genealogy is the level above Family (product/QA sprint, 2026-07-29 —
	// see store.Genealogy's doc comment): a vendor's own release lineage
	// (Nemotron, Gemma, Qwen, …), of which Family is one generation. "" when
	// the model's family has none set.
	Genealogy    string       `json:"genealogy"`
	Modes        []string     `json:"modes"` // config names that load this model
	Capabilities []Capability `json:"capabilities"`
	Performance  Performance  `json:"performance"`
	Quality      Quality      `json:"quality"`
	Derived      Derived      `json:"derived"`
}

type Capability struct {
	ID        string  `json:"id"`
	Label     string  `json:"label"`
	Score     float64 `json:"score"`
	Benchmark string  `json:"benchmark"`
}

// Badge is a normalized model attribute rendered as an inline icon + tooltip
// (Sprint 0 §0.7). icon is an icon-system slug (§0.8); "" ⇒ a generic text
// badge (unknown feature fell through the canonical vocabulary).
type Badge struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

// ModalityGap (Sprint J1) names one modality a model architecturally
// supports that a specific config can't currently deliver, and why —
// missing mmproj link, an mmproj file no longer on disk, or an explicit
// config-level override that narrows the model's default. See
// registry.resolveModalities.
type ModalityGap struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Performance is the wire-facing card performance block. A1 bytes retrofit:
// memory is bytes (the safe_memory_bytes benchmark, already in bytes).
type Performance struct {
	MeasuredTS     *float64 `json:"measured_ts"`
	MemoryReqBytes *int64   `json:"memory_req_bytes"`
	// PowerEstPer1m is the USD electricity cost estimate per 1M tokens
	// (Sprint 0 §0.2), computed from first principles — see
	// costPerMillion — unless a curated override exists (the catalog
	// does not store overrides; the TOML power_est_per_1m manual
	// override is retired with the file). PowerCostPer1k stays emitted
	// for back-compat but is always 0 (the catalog doesn't store it).
	PowerEstPer1m  *float64 `json:"power_est_per_1m"`
	PowerCostPer1k float64  `json:"power_cost_per_1k"`
	// PrefillTS is the curated catalog prefill_tps benchmark, when present
	// (Compressor local-savings prefill sprint, 2026-08-06). Distinct from
	// MeasuredTS (decode_tps) — the two are not reliably ordered on this
	// hardware (a real profiled example has prefill SLOWER than decode), so
	// never substitute one for the other.
	PrefillTS *float64 `json:"prefill_ts"`
}

// Quality drops stability_score from card output (Sprint 0 §0.7 / decision 3).
type Quality struct {
	IsAbliterated       *bool  `json:"is_abliterated"`
	AbliterationQuality string `json:"abliteration_quality"`
}

// Derived carries live-derived card data. A1 bytes retrofit: file_size and
// memory_req are bytes.
type Derived struct {
	Arch           *string         `json:"arch"`
	TrainedCtx     *int            `json:"trained_ctx"`
	FileSizeBytes  *int64          `json:"file_size_bytes"`
	MemoryReqBytes *int64          `json:"memory_req_bytes"`
	History        *HistorySummary `json:"history"`
	Reliability    *Reliability    `json:"reliability"`
}

type HistorySummary struct {
	LastResult       *string  `json:"last_result"`
	CtxReductionRate float64  `json:"ctx_reduction_rate"`
	AvgLoadTimeS     *float64 `json:"avg_load_time_s"`
	TrainedCtx       *int     `json:"trained_ctx"`
}

type Reliability struct {
	LoadsOK      int `json:"loads_ok"`
	LoadFailures int `json:"load_failures"`
	// InferenceHangs is now real (product/QA sprint, 2026-07-29):
	// httpapi.syncNotificationsOnce emits an "inference_hang" usage event
	// the first time collector.hangDetector reports INFERENCE_HANG active
	// for a mode's slot. Before this it was read-only dead code — no writer
	// ever emitted the kind, so this field was always 0.
	InferenceHangs int `json:"inference_hangs"`
	// KFDEvictions is NOT currently detected — always 0. There is no
	// signal distinct from a hang that specifically identifies "the KFD
	// queue was evicted" (the hang alert's message merely *suggests*
	// checking dmesg for that as a possible cause). Real detection would
	// need dmesg/kernel log access, which is deliberately out of scope
	// here (same tradeoff as kernel-panic detection — see the
	// notifications sprint decision log). The field stays on the wire for
	// FE compatibility rather than a breaking removal.
	KFDEvictions int `json:"kfd_evictions"`
}

// ── Implementation ───────────────────────────────────────────────────────────

// catalogRegistry reads exclusively from the catalog DB (B4: no fallback).
// If the catalog is empty, cards are empty (same as models.toml being missing).
type catalogRegistry struct {
	cat   store.Catalog
	cfg   func() *config.Config
	usage store.Usage

	// ProfileDecodeTPS resolves a FRESH profile's measured decode tok/s for
	// a mode (config name). Set by the wiring (forward-ref closure to the
	// profile.Runner, same pattern as engine.Deps.ProfileBytes) so pricing
	// (BE-COST) prefers the real measured figure over the curated decode_tps
	// benchmark whenever one exists. nil disables the profile preference —
	// tests and unwired consumers fall back to the curated benchmark exactly
	// as before. A mode's profile being stale (fingerprint mismatch) also
	// yields ok=false, so the fallback stays correct after a config edit.
	ProfileDecodeTPS func(mode string) (tps float64, ok bool)

	// snapshot cache: avoids re-reading all catalog tables on every
	// Cards()/ModelCards() call. TTL is short so CRUD edits appear on
	// the next cycle (same pattern as the merged config provider).
	mu           sync.Mutex
	snap         *catalogSnapshot
	snapLoadedAt time.Time
}

// catalogSnapshot holds all catalog tables loaded at once, plus lookup maps.
type catalogSnapshot struct {
	configs     []store.Config
	variants    []store.Variant
	models      []store.Model
	families    []store.Family
	genealogies []store.Genealogy
	artifacts   []store.Artifact
	benchmarks  []store.Benchmark
	builds      []store.Build

	variantByID         map[int64]store.Variant
	modelByID           map[int64]store.Model
	familyByID          map[int64]store.Family
	genealogyByID       map[int64]store.Genealogy
	artifactByID        map[int64]store.Artifact
	benchByVariant      map[int64][]store.Benchmark
	benchByModel        map[int64][]store.Benchmark
	benchByConfig       map[int64][]store.Benchmark
	configsByVariant    map[int64][]store.Config
	configByID          map[int64]store.Config
	variantsByModel     map[int64][]store.Variant
	weightArtifactByCfg map[int64]store.Artifact
	buildByID           map[int64]store.Build
}

// genealogyName returns the genealogy name for a family (product/QA
// sprint, 2026-07-29 — the level above Family; see store.Genealogy's doc
// comment), "" when the family has none.
func (s *catalogSnapshot) genealogyName(fam store.Family) string {
	if fam.GenealogyID == 0 {
		return ""
	}
	return s.genealogyByID[fam.GenealogyID].Name
}

// resolveLogos implements the icon inheritance chain (Sprint I,
// docs/v5-prerelease-readiness.md; extended Phase 3 for a dark-theme
// variant): walk config -> model -> family -> genealogy and stop at the
// FIRST level that has either its light or dark field set, returning that
// level's pair. Level-first, not per-field: resolving light and dark
// independently could pair a model's light mark with a genealogy's dark
// mark — two different brands rendered as one icon. dark falls back to the
// resolved level's own light when only that level's light is set, so a
// level that never bothered with a dark variant still renders correctly in
// both themes rather than going blank. "" for both falls through to the FE
// letter-badge fallback (Icon.tsx), identical to the pre-Sprint-I behavior
// when nothing is set. Replaces the three duplicated `Logo: mdl.Logo`
// assignments this registry used to make independently — one source of
// truth for all of ConfigCard/Card.
func (s *catalogSnapshot) resolveLogos(cfgLogo, cfgLogoDark string, mdl store.Model, fam store.Family) (light, dark string) {
	if cfgLogo != "" || cfgLogoDark != "" {
		return cfgLogo, orDefault(cfgLogoDark, cfgLogo)
	}
	if mdl.Logo != "" || mdl.LogoDark != "" {
		return mdl.Logo, orDefault(mdl.LogoDark, mdl.Logo)
	}
	if fam.Logo != "" || fam.LogoDark != "" {
		return fam.Logo, orDefault(fam.LogoDark, fam.Logo)
	}
	if fam.GenealogyID != 0 {
		gen := s.genealogyByID[fam.GenealogyID]
		if gen.Logo != "" || gen.LogoDark != "" {
			return gen.Logo, orDefault(gen.LogoDark, gen.Logo)
		}
	}
	return "", ""
}

// resolveModalities (Sprint J1) narrows a model's architectural modalities
// to what one specific config can actually deliver:
//
//  1. "text" is always enabled — every config can do plain text.
//  2. An explicit cfg.Modalities override wins verbatim, even an empty one
//     (an operator asserting "text only" despite a capable model/mmproj).
//  3. No mmproj linked (MMProjArtifactID == 0) → only "text"; every other
//     model-level modality is unavailable ("no mmproj linked").
//  4. Otherwise inherit the model's modalities; if the linked mmproj
//     artifact is marked Missing (no longer on disk), those modalities are
//     unavailable too ("mmproj file missing on disk") rather than silently
//     claiming a capability the config can't currently serve.
func (s *catalogSnapshot) resolveModalities(c store.Config, mdl store.Model) (enabled []string, unavailable []ModalityGap) {
	nonText := func(mods []string) []string {
		out := make([]string, 0, len(mods))
		for _, m := range mods {
			if m != "text" {
				out = append(out, m)
			}
		}
		return out
	}

	if c.Modalities != nil {
		enabled = append([]string{"text"}, nonText(*c.Modalities)...)
		return enabled, nil
	}

	if c.MMProjArtifactID == 0 {
		for _, m := range nonText(mdl.Modalities) {
			unavailable = append(unavailable, ModalityGap{ID: m, Reason: "no mmproj linked"})
		}
		return []string{"text"}, unavailable
	}

	if a, ok := s.artifactByID[c.MMProjArtifactID]; ok && a.Missing {
		for _, m := range nonText(mdl.Modalities) {
			unavailable = append(unavailable, ModalityGap{ID: m, Reason: "mmproj file missing on disk"})
		}
		return []string{"text"}, unavailable
	}

	enabled = append([]string{"text"}, nonText(mdl.Modalities)...)
	return enabled, nil
}

// benchesFor unions a model's benchmarks (capability scores — intrinsic to
// the weights, e.g. GPQA Diamond) with one of its variants' benchmarks
// (performance metrics — quant/build-specific, e.g. decode_tps). Union, not
// fallback: a card legitimately shows both at once (Sprint D — the
// subject_type trap, docs/v5-prerelease-readiness.md).
func (s *catalogSnapshot) benchesFor(modelID, variantID int64) []store.Benchmark {
	mb := s.benchByModel[modelID]
	vb := s.benchByVariant[variantID]
	if len(mb) == 0 {
		return vb
	}
	if len(vb) == 0 {
		return mb
	}
	out := make([]store.Benchmark, 0, len(mb)+len(vb))
	out = append(out, mb...)
	out = append(out, vb...)
	return out
}

// benchesForConfig widens benchesFor for CONFIG cards only (Phase 8,
// pre-release feedback sprint) — model cards keep calling benchesFor
// unchanged, so a config-specific benchmark never leaks onto the model
// card it doesn't describe. Unions config ∪ variant ∪ model, most-specific
// first, deduped so precedence can't disagree between the two real
// consumers of this list: performanceFromBenchmarks (which used to assign
// on every match, i.e. last-wins) and WeightEstimateBytes/PowerEstPer1m
// (which return on first match, i.e. first-wins) — before this dedupe, a
// model- and a variant-scoped safe_memory_bytes could genuinely disagree
// about which one an OOM check saw vs. which one a card displayed. Rather
// than picking a winner between those two call sites, this makes them
// unable to disagree: a performance metric (performanceMetrics) collapses
// to exactly one row regardless of scope, so first-wins and last-wins
// become the same answer. Capability rows key on (metric, notes) instead,
// since two distinct benchmarks legitimately back one capability id (e.g.
// "reasoning" from GPQA and from AIME) and both must survive — only a
// config re-measuring the exact same benchmark as its model/variant should
// shadow the inherited one.
func (s *catalogSnapshot) benchesForConfig(modelID, variantID, configID int64) []store.Benchmark {
	cb := s.benchByConfig[configID]
	vb := s.benchByVariant[variantID]
	mb := s.benchByModel[modelID]
	if len(cb) == 0 && len(vb) == 0 && len(mb) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(cb)+len(vb)+len(mb))
	out := make([]store.Benchmark, 0, len(cb)+len(vb)+len(mb))
	add := func(list []store.Benchmark) {
		for _, b := range list {
			key := b.Metric
			if !performanceMetrics[b.Metric] {
				key = b.Metric + "\x00" + b.Notes
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, b)
		}
	}
	add(cb)
	add(vb)
	add(mb)
	return out
}

const catalogSnapTTL = 5 * time.Second

// loadSnapshot reads all catalog tables and builds lookup maps. On any
// error, returns an empty snapshot (cards are empty — never fatal, mirrors
// V4 registry.py's _load_registry()).
func (r *catalogRegistry) loadSnapshot(ctx context.Context) *catalogSnapshot {
	r.mu.Lock()
	if r.snap != nil && time.Since(r.snapLoadedAt) < catalogSnapTTL {
		s := r.snap
		r.mu.Unlock()
		return s
	}
	r.mu.Unlock()

	s := &catalogSnapshot{
		variantByID:         map[int64]store.Variant{},
		modelByID:           map[int64]store.Model{},
		familyByID:          map[int64]store.Family{},
		genealogyByID:       map[int64]store.Genealogy{},
		artifactByID:        map[int64]store.Artifact{},
		benchByVariant:      map[int64][]store.Benchmark{},
		benchByModel:        map[int64][]store.Benchmark{},
		benchByConfig:       map[int64][]store.Benchmark{},
		configsByVariant:    map[int64][]store.Config{},
		configByID:          map[int64]store.Config{},
		variantsByModel:     map[int64][]store.Variant{},
		weightArtifactByCfg: map[int64]store.Artifact{},
		buildByID:           map[int64]store.Build{},
	}

	if r.cat == nil {
		return s
	}

	var err error
	s.configs, err = r.cat.ListConfigs(ctx)
	if err != nil {
		log.Printf("registry: list configs: %v", err)
		return s
	}
	s.variants, err = r.cat.ListVariants(ctx)
	if err != nil {
		log.Printf("registry: list variants: %v", err)
		return s
	}
	s.models, err = r.cat.ListModels(ctx)
	if err != nil {
		log.Printf("registry: list models: %v", err)
		return s
	}
	s.families, err = r.cat.ListFamilies(ctx)
	if err != nil {
		log.Printf("registry: list families: %v", err)
		return s
	}
	s.genealogies, err = r.cat.ListGenealogies(ctx)
	if err != nil {
		log.Printf("registry: list genealogies: %v", err)
		return s
	}
	s.artifacts, err = r.cat.ListArtifacts(ctx)
	if err != nil {
		log.Printf("registry: list artifacts: %v", err)
		return s
	}
	s.benchmarks, err = r.cat.ListBenchmarks(ctx)
	if err != nil {
		log.Printf("registry: list benchmarks: %v", err)
		return s
	}
	s.builds, err = r.cat.ListBuilds(ctx)
	if err != nil {
		log.Printf("registry: list builds: %v", err)
		return s
	}

	// Build lookup maps.
	for _, v := range s.variants {
		s.variantByID[v.ID] = v
		s.variantsByModel[v.ModelID] = append(s.variantsByModel[v.ModelID], v)
	}
	for _, m := range s.models {
		s.modelByID[m.ID] = m
	}
	for _, f := range s.families {
		s.familyByID[f.ID] = f
	}
	for _, g := range s.genealogies {
		s.genealogyByID[g.ID] = g
	}
	for _, a := range s.artifacts {
		s.artifactByID[a.ID] = a
	}
	for _, c := range s.configs {
		s.configByID[c.ID] = c
		s.configsByVariant[c.VariantID] = append(s.configsByVariant[c.VariantID], c)
		if c.WeightArtifactID != 0 {
			if a, ok := s.artifactByID[c.WeightArtifactID]; ok {
				s.weightArtifactByCfg[c.ID] = a
			}
		}
	}
	for _, b := range s.benchmarks {
		switch b.SubjectType {
		case "variant":
			s.benchByVariant[b.SubjectID] = append(s.benchByVariant[b.SubjectID], b)
		case "model":
			s.benchByModel[b.SubjectID] = append(s.benchByModel[b.SubjectID], b)
		case "config":
			s.benchByConfig[b.SubjectID] = append(s.benchByConfig[b.SubjectID], b)
		// "offering" is deliberately left unindexed — no card of any kind
		// (model, config, or otherwise) reads offering-scoped benchmarks,
		// and none will after Phase 8 either (pre-release feedback sprint).
		// Do not "complete" this switch; see BenchmarkForm's legacy-offering
		// handling on the FE for where those rows are actually surfaced.
		}
	}
	for _, b := range s.builds {
		s.buildByID[b.ID] = b
	}

	r.mu.Lock()
	r.snap = s
	r.snapLoadedAt = time.Now()
	r.mu.Unlock()
	return s
}

// ── Config-scoped cards (B1) ─────────────────────────────────────────────────

func (r *catalogRegistry) Cards(ctx context.Context, since time.Time) ([]ConfigCard, error) {
	snap := r.loadSnapshot(ctx)

	usageByMode, err := r.usageByMode(ctx, since)
	if err != nil {
		log.Printf("registry: %v", err)
		usageByMode = map[string]Reliability{}
	}

	cards := make([]ConfigCard, 0, len(snap.configs))
	for _, c := range snap.configs {
		if c.Visibility == "hidden" {
			continue
		}
		vt := snap.variantByID[c.VariantID]
		mdl := snap.modelByID[vt.ModelID]
		if mdl.Visibility == "hidden" {
			// Config under a decommissioned (hidden) model: also keep its
			// config card out of the gallery (0062).
			continue
		}
		fam := snap.familyByID[mdl.FamilyID]
		benches := snap.benchesForConfig(mdl.ID, vt.ID, c.ID)
		modelPath, mmprojPath := snap.resolveModelAndMmprojPaths(c)
		build := snap.buildByID[c.BuildID]
		logo, logoDark := snap.resolveLogos(c.Logo, c.LogoDark, mdl, fam)
		modalities, modalitiesUnavailable := snap.resolveModalities(c, mdl)

		card := r.assembleConfigCard(c, vt, mdl, fam, build, modelPath, mmprojPath, benches, usageByMode, logo, logoDark, modalities, modalitiesUnavailable, ctx)
		cards = append(cards, card)
	}

	// Config name alphabetical — stable UI ordering.
	sort.SliceStable(cards, func(i, j int) bool {
		return strings.ToLower(cards[i].Name) < strings.ToLower(cards[j].Name)
	})
	return cards, nil
}

// assembleConfigCard builds one ConfigCard from catalog entities + live data.
func (r *catalogRegistry) assembleConfigCard(
	c store.Config, vt store.Variant, mdl store.Model, fam store.Family, build store.Build,
	modelPath, mmprojPath string, benches []store.Benchmark,
	usageByMode map[string]Reliability, logo, logoDark string,
	modalities []string, modalitiesUnavailable []ModalityGap, ctx context.Context,
) ConfigCard {
	cfg := r.safeConfig()
	modelPath = resolveArtifactPath(modelPath, cfg)
	mmprojPath = resolveArtifactPath(mmprojPath, cfg)
	arch, ggufTrainedCtx := deriveGGUF(modelPath)
	fileSizeBytes := deriveFileSizeBytes(modelPath, mmprojPath, cfg)
	history := r.deriveHistory(ctx, c.Name)
	reliability := deriveReliability(c.Name, usageByMode)

	perf := performanceFromBenchmarks(benches)
	registryMemBytes := perf.MemoryReqBytes

	var trainedCtx *int
	if ggufTrainedCtx > 0 {
		v := ggufTrainedCtx
		trainedCtx = &v
	} else if vt.TrainedCtx > 0 {
		v := vt.TrainedCtx
		trainedCtx = &v
	} else if history != nil && history.TrainedCtx != nil {
		trainedCtx = history.TrainedCtx
	}

	var archPtr *string
	if arch != "" {
		archPtr = &arch
	}

	// Curated benchmark figure wins; live on-disk size is the fallback
	// (mirrors engine.CanFit's precedence).
	derivedMemBytes := registryMemBytes
	if derivedMemBytes == nil {
		derivedMemBytes = fileSizeBytes
	}

	var powerEstPer1m *float64
	if v, ok := r.costPerMillion(c.Name, benches); ok {
		powerEstPer1m = &v
	}
	perf.PowerEstPer1m = powerEstPer1m

	var isAbliterated *bool
	if vt.IsAbliterated {
		b := true
		isAbliterated = &b
	}

	return ConfigCard{
		ID:         c.ID,
		Name:       c.Name,
		ModelID:    strconv.FormatInt(mdl.ID, 10),
		NCtx:       c.NCtx,
		Status:     c.Status,
		Visibility: c.Visibility,
		IsDefault:  c.IsDefault,
		CreatedAt:  unixSeconds(c.CreatedAt),

		ModelName:             orDefault(mdl.Name, c.Name),
		Creator:               mdl.Creator,
		LicenseName:           mdl.LicenseName,
		LicenseURL:            mdl.LicenseURL,
		Description:           mdl.Description,
		KeyFeatures:           orEmptySlice(mdl.KeyFeatures),
		Badges:                deriveBadges(mdl.KeyFeatures, isAbliterated, modalities),
		Logo:                  logo,
		LogoDark:              logoDark,
		HFRepo:                mdl.HFRepo,
		Family:                fam.Name,
		Capabilities:          capabilitiesFromBenchmarks(benches),
		Modalities:            modalities,
		ModalitiesUnavailable: orEmptyModalityGaps(modalitiesUnavailable),

		ExtraArgs:   orEmptySlice(c.ExtraArgs),
		Backend:     build.Backend,
		VariantName: vt.Name,

		Quality: Quality{
			IsAbliterated:       isAbliterated,
			AbliterationQuality: vt.AbliterationQuality,
		},

		Performance: perf,

		Derived: Derived{
			Arch:           archPtr,
			TrainedCtx:     trainedCtx,
			FileSizeBytes:  fileSizeBytes,
			MemoryReqBytes: derivedMemBytes,
			History:        history,
			Reliability:    reliability,
		},
	}
}

// ── Model-scoped cards ───────────────────────────────────────────────────────

func (r *catalogRegistry) ModelCards(ctx context.Context, since time.Time) ([]Card, error) {
	snap := r.loadSnapshot(ctx)

	usageByMode, err := r.usageByMode(ctx, since)
	if err != nil {
		log.Printf("registry: %v", err)
		usageByMode = map[string]Reliability{}
	}

	cards := make([]Card, 0, len(snap.models))
	for _, mdl := range snap.models {
		if mdl.Visibility == "hidden" {
			// Decommissioned model (0062 model-level visibility): card stays
			// out of the gallery; rows remain visible in Settings → Catalog.
			continue
		}
		fam := snap.familyByID[mdl.FamilyID]
		variants := snap.variantsByModel[mdl.ID]
		if len(variants) == 0 {
			// Model with no variants — still show identity, and still surface
			// its model-scoped published benchmarks (e.g. DeepSeek V4) so a
			// remote-only model isn't a blank card.
			logo, logoDark := snap.resolveLogos("", "", mdl, fam)
			benches := snap.benchesFor(mdl.ID, 0)
			cards = append(cards, Card{
				ID:           strconv.FormatInt(mdl.ID, 10),
				Name:         orDefault(mdl.Name, strconv.FormatInt(mdl.ID, 10)),
				Creator:      mdl.Creator,
				LicenseName:  mdl.LicenseName,
				LicenseURL:   mdl.LicenseURL,
				Description:  mdl.Description,
				KeyFeatures:  orEmptySlice(mdl.KeyFeatures),
				Badges:       deriveBadges(mdl.KeyFeatures, nil, mdl.Modalities),
				Modalities:   orEmptySlice(mdl.Modalities),
				Logo:         logo,
				LogoDark:     logoDark,
				HFRepo:       mdl.HFRepo,
				Family:       fam.Name,
				Genealogy:    snap.genealogyName(fam),
				Modes:        []string{},
				Capabilities: capabilitiesFromBenchmarks(benches),
				Performance:  Performance{},
				Quality:      Quality{},
				Derived:      Derived{},
			})
			continue
		}

		// Gather all config names across this model's variants.
		var modes []string
		var firstConfig store.Config
		var firstVariant store.Variant
		for _, vt := range variants {
			for _, c := range snap.configsByVariant[vt.ID] {
				modes = append(modes, c.Name)
				if firstConfig.ID == 0 || c.IsDefault {
					firstConfig = c
					firstVariant = vt
					if c.IsDefault {
						break
					}
				}
			}
		}
		sort.Strings(modes)

		// Known limitation, not fixed by Phase 8's config-scoped union below:
		// this only ever unions the DEFAULT config's variant. A benchmark
		// scoped to a non-default variant (e.g. a Q4 vs. a Q8 build of the
		// same model) never reaches the model card — it does still reach
		// that variant's own config cards via benchesForConfig. Widening
		// this to union every variant isn't a safe fix on its own: two
		// variants could report conflicting decode_tps for one card slot
		// with no principled way to pick a winner.
		benches := snap.benchesFor(mdl.ID, firstVariant.ID)
		var modelPath, mmprojPath string
		if firstConfig.ID != 0 {
			modelPath, mmprojPath = snap.resolveModelAndMmprojPaths(firstConfig)
		}

		logo, logoDark := snap.resolveLogos("", "", mdl, fam)
		card := r.assembleModelCard(mdl, fam, firstVariant, firstConfig, modelPath, mmprojPath, benches, modes, usageByMode, logo, logoDark, ctx)
		card.Genealogy = snap.genealogyName(fam)
		cards = append(cards, card)
	}

	sort.SliceStable(cards, func(i, j int) bool {
		return strings.ToLower(cards[i].Name) < strings.ToLower(cards[j].Name)
	})
	return cards, nil
}

func (r *catalogRegistry) assembleModelCard(
	mdl store.Model, fam store.Family, vt store.Variant,
	c store.Config, modelPath, mmprojPath string, benches []store.Benchmark,
	modes []string, usageByMode map[string]Reliability, logo, logoDark string, ctx context.Context,
) Card {
	cfg := r.safeConfig()
	modelPath = resolveArtifactPath(modelPath, cfg)
	mmprojPath = resolveArtifactPath(mmprojPath, cfg)
	arch, ggufTrainedCtx := deriveGGUF(modelPath)
	fileSizeBytes := deriveFileSizeBytes(modelPath, mmprojPath, cfg)

	history := r.deriveHistory(ctx, c.Name)
	reliability := deriveReliability(c.Name, usageByMode)

	perf := performanceFromBenchmarks(benches)
	registryMemBytes := perf.MemoryReqBytes

	var trainedCtx *int
	if ggufTrainedCtx > 0 {
		v := ggufTrainedCtx
		trainedCtx = &v
	} else if vt.TrainedCtx > 0 {
		v := vt.TrainedCtx
		trainedCtx = &v
	} else if history != nil && history.TrainedCtx != nil {
		trainedCtx = history.TrainedCtx
	}

	var archPtr *string
	if arch != "" {
		archPtr = &arch
	}

	derivedMemBytes := registryMemBytes
	if derivedMemBytes == nil {
		derivedMemBytes = fileSizeBytes
	}

	var powerEstPer1m *float64
	if v, ok := r.costPerMillion(c.Name, benches); ok {
		powerEstPer1m = &v
	}

	var isAbliterated *bool
	if vt.IsAbliterated {
		b := true
		isAbliterated = &b
	}

	return Card{
		ID:           strconv.FormatInt(mdl.ID, 10),
		Name:         orDefault(mdl.Name, strconv.FormatInt(mdl.ID, 10)),
		Creator:      mdl.Creator,
		LicenseName:  mdl.LicenseName,
		LicenseURL:   mdl.LicenseURL,
		Description:  mdl.Description,
		KeyFeatures:  orEmptySlice(mdl.KeyFeatures),
		Badges:       deriveBadges(mdl.KeyFeatures, isAbliterated, mdl.Modalities),
		Modalities:   orEmptySlice(mdl.Modalities),
		Logo:         logo,
		LogoDark:     logoDark,
		HFRepo:       mdl.HFRepo,
		Family:       fam.Name,
		Modes:        orEmptySlice(modes),
		Capabilities: capabilitiesFromBenchmarks(benches),
		Performance: Performance{
			MeasuredTS:     perf.MeasuredTS,
			MemoryReqBytes: registryMemBytes,
			PowerEstPer1m:  powerEstPer1m,
			PowerCostPer1k: 0,
		},
		Quality: Quality{
			IsAbliterated:       isAbliterated,
			AbliterationQuality: vt.AbliterationQuality,
		},
		Derived: Derived{
			Arch:           archPtr,
			TrainedCtx:     trainedCtx,
			FileSizeBytes:  fileSizeBytes,
			MemoryReqBytes: derivedMemBytes,
			History:        history,
			Reliability:    reliability,
		},
	}
}

// ── Per-config lookups (B2: configID-based) ──────────────────────────────────

func (r *catalogRegistry) WeightEstimateBytes(configID int64) (int64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snap := r.loadSnapshot(ctx)

	c, ok := snap.configByID[configID]
	if !ok {
		return 0, false
	}
	vt := snap.variantByID[c.VariantID]
	benches := snap.benchesForConfig(vt.ModelID, c.VariantID, c.ID)
	for _, b := range benches {
		if b.Metric == "safe_memory_bytes" {
			if v, err := strconv.ParseInt(b.Value, 10, 64); err == nil && v > 0 {
				return v, true
			}
		}
	}
	return 0, false
}

func (r *catalogRegistry) CostPer1k(configID int64) float64 {
	return 0 // deprecated — catalog does not store power_cost_per_1k
}

func (r *catalogRegistry) PowerEstPer1m(configID int64) (float64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	snap := r.loadSnapshot(ctx)

	c, ok := snap.configByID[configID]
	if !ok {
		return 0, false
	}
	vt := snap.variantByID[c.VariantID]
	benches := snap.benchesForConfig(vt.ModelID, c.VariantID, c.ID)
	return r.costPerMillion(c.Name, benches)
}

// ── Derived data ─────────────────────────────────────────────────────────────

// deriveGGUF reads GGUF header metadata for modelPath. "" for non-GGUF
// paths (vLLM repo ids, unlinked entries) or unreadable files — never
// fatal, mirrors V4's _derive_gguf().
func deriveGGUF(modelPath string) (arch string, trainedCtx int) {
	if modelPath == "" || !strings.HasSuffix(modelPath, ".gguf") {
		return "", 0
	}
	md, err := gguf.ReadMetadata(modelPath)
	if err != nil {
		return "", 0
	}
	return md.Architecture, md.TrainedCtx
}

// deriveFileSizeBytes is the on-disk size (bytes) of the model(+mmproj)
// weight set, shard-aware via collector.WeightSetSizeBytes. nil when nothing
// is on disk or the model path isn't a GGUF file (vLLM repo ids / local
// safetensors dirs stat() to meaningless directory sizes).
func deriveFileSizeBytes(modelPath, mmprojPath string, cfg *config.Config) *int64 {
	if cfg == nil {
		return nil
	}
	if modelPath != "" && !strings.HasSuffix(modelPath, ".gguf") {
		modelPath = ""
	}
	sizeBytes := collector.WeightSetSizeBytes(modelPath, mmprojPath, cfg.Paths.ModelsDir)
	if sizeBytes <= 0 {
		return nil
	}
	return &sizeBytes
}

// modeSummary mirrors one call to V4 history.get_summary() for a single
// mode: the last RING_SIZE(10) load records, summarized.
type modeSummary struct {
	lastResult       string
	ctxReductionRate float64
	avgLoadTimeS     *float64
	trainedCtx       *int
	hasData          bool
}

func (r *catalogRegistry) modeHistorySummary(ctx context.Context, mode string) modeSummary {
	if r.usage == nil {
		return modeSummary{}
	}
	entries, err := r.usage.History(ctx, mode, 10) // V4 RING_SIZE
	if err != nil || len(entries) == 0 {
		return modeSummary{}
	}
	last := entries[0] // History() returns newest-first
	reduced := 0
	var loadTimes []float64
	for _, e := range entries {
		if e.Result != "failed" {
			loadTimes = append(loadTimes, e.LoadTimeS)
		}
		if e.ActualCtx > 0 && e.ConfiguredCtx > 0 && e.ActualCtx < e.ConfiguredCtx {
			reduced++
		}
	}
	var avg *float64
	if len(loadTimes) > 0 {
		sum := 0.0
		for _, v := range loadTimes {
			sum += v
		}
		v := math.Round(sum/float64(len(loadTimes))*10) / 10
		avg = &v
	}
	var trained *int
	if last.TrainedCtx > 0 {
		t := last.TrainedCtx
		trained = &t
	}
	return modeSummary{
		lastResult:       last.Result,
		ctxReductionRate: float64(reduced) / float64(len(entries)),
		avgLoadTimeS:     avg,
		trainedCtx:       trained,
		hasData:          last.Result != "",
	}
}

// deriveHistory merges modeHistorySummary across a config's name. nil when
// no load history exists.
func (r *catalogRegistry) deriveHistory(ctx context.Context, configName string) *HistorySummary {
	s := r.modeHistorySummary(ctx, configName)
	if !s.hasData {
		return nil
	}
	lastResult := s.lastResult
	return &HistorySummary{
		LastResult:       &lastResult,
		CtxReductionRate: s.ctxReductionRate,
		AvgLoadTimeS:     s.avgLoadTimeS,
		TrainedCtx:       s.trainedCtx,
	}
}

// usageByMode aggregates loads_ok/load_failures/inference_hangs/
// kfd_evictions per mode over [since, now).
func (r *catalogRegistry) usageByMode(ctx context.Context, since time.Time) (map[string]Reliability, error) {
	out := map[string]Reliability{}
	if r.usage == nil {
		return out, nil
	}
	events, err := r.usage.Events(ctx, since, 0)
	if err != nil {
		return nil, fmt.Errorf("registry: usage events: %w", err)
	}
	for _, ev := range events {
		rel := out[ev.Model]
		switch ev.Kind {
		case "inference":
			// seed presence only
		case "load_ok":
			rel.LoadsOK++
		case "load_failed", "load_failure":
			rel.LoadFailures++
		case "inference_hang":
			rel.InferenceHangs++
		// "kfd_eviction"/"kfd_evict" intentionally not read: no writer ever
		// emits either kind (see Reliability.KFDEvictions' doc comment) —
		// a case here would be dead code pretending to aggregate data that
		// doesn't exist.
		default:
			continue
		}
		out[ev.Model] = rel
	}
	return out, nil
}

// deriveReliability returns the reliability row for a config name. nil when
// no recorded activity in the window (the usageByMode map has no entry).
// A zero-valued struct (not nil) means "activity, no failures" — the
// usageByMode function seeds an entry for any mode with inference events.
func deriveReliability(configName string, usageByMode map[string]Reliability) *Reliability {
	row, ok := usageByMode[configName]
	if !ok {
		return nil
	}
	return &row
}

// ── Badges (Sprint 0 §0.7, vocabulary overhauled Sprint J2) ──────────────────
//
// J2 dropped `fast`/`multimodal`/`moe`/`mtp`/`dense` (architecture trivia,
// not something an operator picking a model needs a glyph for) and retired
// the `abliterated` slug/label in favor of `uncensored` (still keyed by the
// `is_abliterated` flag and the `abliterated` key_feature string — just a
// different rendered badge). `vision`/`hearing` are no longer key_features
// strings at all: they're synthesized directly from a card's resolved
// modalities (model-level for Card, config-level/narrowed for ConfigCard —
// see resolveModalities), so a config with no mmproj shows no vision badge
// even when its model architecturally supports it.

var canonicalBadge = map[string]Badge{
	"reasoning":      {ID: "reasoning", Label: "Reasoning", Icon: "reasoning"},
	"deep reasoning": {ID: "reasoning", Label: "Reasoning", Icon: "reasoning"},
	"thinking":       {ID: "reasoning", Label: "Reasoning", Icon: "reasoning"},
	"coding":         {ID: "coding", Label: "Coding", Icon: "coding"},
	"agentic coding": {ID: "coding", Label: "Coding", Icon: "coding"},
	"uncensored":     {ID: "uncensored", Label: "Uncensored", Icon: "uncensored"},
	"abliterated":    {ID: "uncensored", Label: "Uncensored", Icon: "uncensored"},
	"no guardrails":  {ID: "uncensored", Label: "Uncensored", Icon: "uncensored"},
	"long context":   {ID: "long-context", Label: "Long Context", Icon: "long-context"},
	"long-context":   {ID: "long-context", Label: "Long Context", Icon: "long-context"},
	"1m context":     {ID: "long-context", Label: "Long Context", Icon: "long-context"},
}

// suppressedBadgeKey are key_features that must vanish entirely from a
// card's Badges — no glyph, and critically no generic text-pill fallback
// either (unlike an unrecognized feature, which still gets a "text:" pill).
// "vision"/"audio"/"hearing" join the plan's original five here: live-audit
// after deploy found Nemotron 3 Nano Omni still carries a literal "Vision"
// key_feature string predating the J1 typed modalities column — without
// suppression it fell through as an unrecognized feature and rendered a
// redundant "text:vision" pill next to the real modality-derived vision
// glyph. Same category as "multimodal": a stale free-text descriptor now
// superseded by a typed field, not a fact that needs its own separate badge.
var suppressedBadgeKey = map[string]bool{
	"fast": true, "multimodal": true, "moe": true, "mtp": true, "dense": true,
	"vision": true, "audio": true, "hearing": true,
}

// badgePriority orders the glyph-bearing badges for both display (glyphs
// nearer the front of the cluster read as more important) and the 4-badge
// cap below (excess low-priority glyphs are dropped, not the high-priority
// ones). Text-pill badges (unrecognized key_features) have no priority tier
// and are never capped — they're appended after glyphs, in discovery order.
var badgePriority = map[string]int{
	"reasoning":    0,
	"vision":       1,
	"hearing":      2,
	"coding":       3,
	"uncensored":   4,
	"long-context": 5,
}

const maxGlyphBadges = 4

// deriveBadges maps key_features + is_abliterated + a card's resolved
// modalities to the canonical badge vocabulary, deduped, glyph badges sorted
// by badgePriority and capped at maxGlyphBadges, followed by any
// unrecognized-feature text pills (uncapped — those render only in plain-list
// contexts, never the capped glyph cluster on a card face).
func deriveBadges(keyFeatures []string, isAbliterated *bool, modalities []string) []Badge {
	glyphs := []Badge{}
	pills := []Badge{}
	seen := map[string]bool{}
	addGlyph := func(b Badge) {
		if seen[b.ID] {
			return
		}
		seen[b.ID] = true
		glyphs = append(glyphs, b)
	}
	addPill := func(b Badge) {
		if seen[b.ID] {
			return
		}
		seen[b.ID] = true
		pills = append(pills, b)
	}

	if isAbliterated != nil && *isAbliterated {
		addGlyph(canonicalBadge["abliterated"])
	}
	for _, m := range modalities {
		switch m {
		case "vision":
			addGlyph(Badge{ID: "vision", Label: "Vision", Icon: "vision"})
		case "audio":
			addGlyph(Badge{ID: "hearing", Label: "Hearing", Icon: "hearing"})
		}
	}
	for _, f := range keyFeatures {
		key := strings.ToLower(strings.TrimSpace(f))
		if suppressedBadgeKey[key] {
			continue
		}
		if b, ok := canonicalBadge[key]; ok {
			addGlyph(b)
			continue
		}
		addPill(Badge{ID: "text:" + key, Label: f, Icon: ""})
	}

	sort.SliceStable(glyphs, func(i, j int) bool {
		return badgePriority[glyphs[i].ID] < badgePriority[glyphs[j].ID]
	})
	if len(glyphs) > maxGlyphBadges {
		glyphs = glyphs[:maxGlyphBadges]
	}
	return append(glyphs, pills...)
}

// ── Capabilities + performance from benchmarks ───────────────────────────────

// humanizeCapability: "agentic_logic" -> "Agentic Logic" — the capability
// table's key is the humanized label; the raw benchmark name stays as
// secondary detail (V4's _humanize_capability()).
func humanizeCapability(key string) string {
	words := strings.Split(strings.ReplaceAll(key, "_", " "), " ")
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

// performanceMetrics is the single source of truth for which benchmark
// metric names are PERFORMANCE figures (consumed by performanceFromBenchmarks
// below) rather than capability scores. Every metric here must be excluded
// from capabilitiesFromBenchmarks — a real bug found while building the
// Compressor local-savings prefill sprint (2026-08-06): the old two-entry
// denylist (decode_tps, safe_memory_bytes) let a variant-scoped prefill_tps
// row — which the compressor summary handler's catalog fallback step
// explicitly invites operators to create — fall through and render on the
// model/config card as a capability literally labelled "Prefill Tps" at a
// four-digit percentage, sorted to the very top since capabilities sort by
// score descending.
var performanceMetrics = map[string]bool{
	"decode_tps":        true,
	"prefill_tps":       true,
	"safe_memory_bytes": true,
}

// capabilitiesFromBenchmarks extracts capability scores from benchmark rows.
// Any benchmark whose metric is NOT in performanceMetrics is treated as a
// capability (metric = capability ID, value = score, notes = benchmark name).
func capabilitiesFromBenchmarks(benches []store.Benchmark) []Capability {
	out := make([]Capability, 0, len(benches))
	for _, b := range benches {
		if performanceMetrics[b.Metric] {
			continue
		}
		score, _ := strconv.ParseFloat(b.Value, 64)
		out = append(out, Capability{
			ID:        b.Metric,
			Label:     humanizeCapability(b.Metric),
			Score:     score,
			Benchmark: b.Notes,
		})
	}
	// Highest-scoring capability first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

// performanceFromBenchmarks extracts the performance block (decode_tps,
// prefill_tps, safe_memory_bytes) from benchmark rows — see performanceMetrics.
func performanceFromBenchmarks(benches []store.Benchmark) Performance {
	var perf Performance
	for _, b := range benches {
		switch b.Metric {
		case "decode_tps":
			tps, _ := strconv.ParseFloat(b.Value, 64)
			if tps > 0 {
				perf.MeasuredTS = &tps
			}
		case "prefill_tps":
			tps, _ := strconv.ParseFloat(b.Value, 64)
			if tps > 0 {
				perf.PrefillTS = &tps
			}
		case "safe_memory_bytes":
			if v, err := strconv.ParseInt(b.Value, 10, 64); err == nil && v > 0 {
				perf.MemoryReqBytes = &v
			}
		}
	}
	perf.PowerCostPer1k = 0 // deprecated — catalog does not store it
	return perf
}

// ── Cost derivation (BE-COST, docs/v5-review-fixes.md F5) ──────────────────

// costPerMillionTokens computes the local-model electricity cost per 1M
// generated tokens from first principles:
//
//	energy (kWh) to generate 1M tokens = power_kW × time_hours
//	time_hours = (1,000,000 tokens / tps) / 3600 seconds-per-hour
//	cost       = energy_kWh × rate_per_kWh
func costPerMillionTokens(tps, powerKW, ratePerKWh float64) float64 {
	if tps <= 0 {
		return 0
	}
	hours := (1_000_000.0 / tps) / 3600.0
	return powerKW * hours * ratePerKWh
}

// powerRate resolves the power/rate inputs from r.cfg, falling back to
// config.Default* when cfg is nil or unset.
//
// powerRate returns WALL power, not the raw configured power_kw. power_kw
// is documented as an approximate board/package power draw (the same
// quantity the amdgpu PPT sensor measures) — running it through
// config.Cost.WallWatts (+overhead_w, /psu_efficiency) before pricing keeps
// every displayed cost figure (this card estimate and the whole-server
// /api/v1/cost/summary total) on the same basis, rather than one raw and
// one wall-adjusted. This shifts every card's power_est_per_1m ~28-31%
// higher at the defaults (140W → (140+25)/0.9 ≈ 183W) — a deliberate,
// signed-off change (cost/savings sprint, 2026-07-30), not a regression.
func (r *catalogRegistry) powerRate() (powerKW, ratePerKWh float64) {
	cfg := r.safeConfig()
	rawPowerKW, overheadW, psuEfficiency := config.DefaultPowerKW, config.DefaultOverheadW, config.DefaultPSUEfficiency
	ratePerKWh = config.DefaultRatePerKWh
	if cfg != nil {
		// Resolved independently rather than trusting cfg.Cost to already
		// be defaulted (New/LoadFromStore's applyDefaults does, but a
		// directly-constructed config.Config{} literal — as in tests, and
		// any future caller — would otherwise divide by a zero
		// PSUEfficiency in WallWatts).
		if cfg.Cost.PowerKW > 0 {
			rawPowerKW = cfg.Cost.PowerKW
		}
		if cfg.Cost.RatePerKWh > 0 {
			ratePerKWh = cfg.Cost.RatePerKWh
		}
		if cfg.Cost.OverheadW > 0 {
			overheadW = cfg.Cost.OverheadW
		}
		if cfg.Cost.PSUEfficiency > 0 {
			psuEfficiency = cfg.Cost.PSUEfficiency
		}
	}
	wall := config.Cost{OverheadW: overheadW, PSUEfficiency: psuEfficiency}
	powerKW = wall.WallWatts(rawPowerKW*1000) / 1000
	return powerKW, ratePerKWh
}

// costPerMillion resolves the per-1M-token cost from decode_tps + powerRate.
// mode is the config/mode name: a FRESH profile's measured decode_tps wins
// (BE-COST per docs/v5-profiling-benchmarks.md §5), falling back to the
// curated decode_tps benchmark. ok=false when no usable T/s figure exists
// from either source.
func (r *catalogRegistry) costPerMillion(mode string, benches []store.Benchmark) (float64, bool) {
	tps := r.profileDecodeTPS(mode)
	if tps <= 0 {
		tps = curatedDecodeTPS(benches)
	}
	if tps <= 0 {
		return 0, false
	}
	powerKW, ratePerKWh := r.powerRate()
	return costPerMillionTokens(tps, powerKW, ratePerKWh), true
}

// profileDecodeTPS returns the fresh profile's measured decode tok/s for a
// mode when one exists (ProfileDecodeTPS wired and non-stale). 0 otherwise.
func (r *catalogRegistry) profileDecodeTPS(mode string) float64 {
	if r.ProfileDecodeTPS == nil || mode == "" {
		return 0
	}
	if tps, ok := r.ProfileDecodeTPS(mode); ok && tps > 0 {
		return tps
	}
	return 0
}

// curatedDecodeTPS extracts the curated decode_tps benchmark (the catalog
// fallback source when no fresh profile exists).
func curatedDecodeTPS(benches []store.Benchmark) float64 {
	for _, b := range benches {
		if b.Metric != "decode_tps" {
			continue
		}
		if v, err := strconv.ParseFloat(b.Value, 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (r *catalogRegistry) safeConfig() *config.Config {
	if r.cfg == nil {
		return nil
	}
	return r.cfg()
}

// resolveArtifactPath joins a relative artifact file_path with ModelsDir.
// Absolute paths and empty strings pass through unchanged.
//
// Thin wrapper around config.Paths.ResolveModelPath so the registry and the
// engine share one resolution rule (a load bug found 2026-07-25: the engine
// had its own bare filepath.Join here, which broke for absolute file_paths
// and refused loads the registry's own card path showed as fitting).
func resolveArtifactPath(path string, cfg *config.Config) string {
	if cfg == nil {
		return path
	}
	return cfg.Paths.ResolveModelPath(path)
}

// unixSeconds returns the float epoch for t (0 for zero time), matching the
// httpapi package's wire convention for *_at timestamp fields.
func unixSeconds(t time.Time) float64 {
	if t.IsZero() {
		return 0
	}
	return float64(t.UnixNano()) / 1e9
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orEmptySlice(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptyModalityGaps(s []ModalityGap) []ModalityGap {
	if s == nil {
		return []ModalityGap{}
	}
	return s
}

// resolveMmprojPath looks up the mmproj artifact's file_path from the
// snapshot (B3: weight path from artifacts).
func (s *catalogSnapshot) resolveMmprojPath(c store.Config) string {
	if c.MMProjArtifactID == 0 {
		return ""
	}
	if a, ok := s.artifactByID[c.MMProjArtifactID]; ok {
		return a.FilePath
	}
	return ""
}

// resolveModelAndMmprojPaths resolves both the weight model path and mmproj
// path from the snapshot's artifacts (B3).
func (s *catalogSnapshot) resolveModelAndMmprojPaths(c store.Config) (string, string) {
	weight := s.weightArtifactByCfg[c.ID]
	modelPath := weight.FilePath
	mmprojPath := s.resolveMmprojPath(c)
	return modelPath, mmprojPath
}
