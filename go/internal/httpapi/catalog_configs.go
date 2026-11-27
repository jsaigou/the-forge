// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_configs.go — Config CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

type configJSON struct {
	ID               int64    `json:"id"`
	Name             string   `json:"name"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	EngineID         int64    `json:"engine_id"`
	BuildID          int64    `json:"build_id"`
	MMProjArtifactID int64    `json:"mmproj_artifact_id"`
	NCtx             int      `json:"n_ctx"`
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
	// OutputModalities overrides the model's output set (ADR-0021). nil = derive via store.ResolveOutputModalities
	// (the model's outputs; "audio" additionally needs a linked mmproj); omitzero keeps derive-mode responses clean.
	OutputModalities *[]string `json:"output_modalities,omitzero"`
	// CapabilityTierID groups this config with others that may substitute for
	// each other under capability-tier substitution (Sprint P1, 2026-09-13);
	// 0 = no class, never substitutes or is substituted for. CapabilityRank is
	// meaningless when CapabilityTierID is 0. See store.CapabilityTier's doc comment.
	CapabilityTierID int64 `json:"capability_tier_id"`
	CapabilityRank   int   `json:"capability_rank"`
	// ChatTemplateCaps is the last live /props probe result (T1,
	// per-request thinking control), read-only here — write
	// ChatTemplateCapsOverride instead. ChatTemplateCapsProbedAt is unix
	// seconds, 0 = never probed.
	ChatTemplateCaps         map[string]bool `json:"chat_template_caps"`
	ChatTemplateCapsProbedAt int64           `json:"chat_template_caps_probed_at"`
	// ChatTemplateCapsOverride is an operator-curated correction merged
	// over ChatTemplateCaps per key (store.Config.EffectiveChatTemplateCaps)
	// — for the cases the live probe gets wrong. nil = no override.
	ChatTemplateCapsOverride map[string]bool `json:"chat_template_caps_override,omitzero"`
	// ReasoningEffortDefault is the per-config default reasoning_effort
	// level (T2, per-request thinking control) — "" | none | low | medium |
	// high. Applied by the router whenever a request sends none of its own.
	ReasoningEffortDefault string `json:"reasoning_effort_default"`
}

func configToJSON(c store.Config) configJSON {
	var probedAt int64
	if !c.ChatTemplateCapsProbedAt.IsZero() {
		probedAt = c.ChatTemplateCapsProbedAt.Unix()
	}
	return configJSON{
		ID: c.ID, Name: c.Name, VariantID: c.VariantID,
		WeightArtifactID: c.WeightArtifactID, EngineID: c.EngineID, BuildID: c.BuildID,
		MMProjArtifactID: c.MMProjArtifactID, NCtx: c.NCtx,
		ExtraArgs: c.ExtraArgs, Status: c.Status, Visibility: c.Visibility,
		IsDefault: c.IsDefault, Fingerprint: c.Fingerprint, Logo: c.Logo, LogoDark: c.LogoDark,
		Modalities: c.Modalities, OutputModalities: c.OutputModalities,
		CapabilityTierID: c.CapabilityTierID, CapabilityRank: c.CapabilityRank,
		ChatTemplateCaps: c.ChatTemplateCaps, ChatTemplateCapsProbedAt: probedAt,
		ChatTemplateCapsOverride: c.ChatTemplateCapsOverride,
		ReasoningEffortDefault:   c.ReasoningEffortDefault,
	}
}

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
			writeValidationErrorCodes(w, map[string]string{"variant_id": "must be an integer"}, map[string]string{"variant_id": "must_be_integer"})
			return
		}
		list, err = cat.ListConfigsForVariant(ctx, id)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "configs query failed")
			return
		}
	} else {
		var err error
		list, err = cat.ListConfigs(ctx)
		if err != nil {
			writeErrorCode(w, http.StatusInternalServerError, "operation_failed", nil, "configs query failed")
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
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	c, err := cat.GetConfig(ctx, id)
	if err != nil {
		if isNotFound(err) {
			writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "config"}, "config not found")
			return
		}
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, configToJSON(c))
}

func (s *Server) handleCatalogConfigCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b configBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	c, err := s.configWrites().Create(ctx, b.toStore(0))
	if err != nil {
		writeConfigError(w, err, c.Name)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_create", strconv.FormatInt(c.ID, 10), withReason(b.Name, b.Reason))
	writeJSON(w, http.StatusCreated, configToJSON(c))
}

func (s *Server) handleCatalogConfigUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	var b configBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	c, err := s.configWrites().Update(ctx, b.toStore(id))
	if err != nil {
		writeConfigError(w, err, b.Name)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_update", strconv.FormatInt(id, 10), withReason(b.Name, b.Reason))
	writeJSON(w, http.StatusOK, configToJSON(c))
}

func (s *Server) handleCatalogConfigDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	id, ok := parseID(r)
	if !ok {
		writeValidationErrorCodes(w, map[string]string{"id": "must be an integer"}, map[string]string{"id": "must_be_integer"})
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	// Refuse to delete a Config currently loaded unless force is truthy: ?force=false (or an unparseable value) must not force.
	force := queryFlag(r, "force")
	c, err := s.configWrites().Delete(ctx, id, force)
	if err != nil {
		writeConfigError(w, err, c.Name)
		return
	}
	s.audit(r, identity(r).Name, "catalog_config_delete", strconv.FormatInt(id, 10), withReason(c.Name, r.URL.Query().Get("reason")))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleCatalogConfigValidate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Catalog == nil {
		writeErrorCode(w, http.StatusServiceUnavailable, "not_wired", map[string]any{"resource": "catalog"}, "catalog not wired")
		return
	}
	var b configBody
	if fields := decodeCatalogBody(r, &b); fields != nil {
		writeValidationError(w, fields)
		return
	}
	ctx, cancel := catalogCtx(r)
	defer cancel()
	inv, err := s.configWrites().Validate(ctx, b.toStore(0), validateExcludeID(r))
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if inv != nil {
		writeValidationErrorCodes(w, inv.Fields, inv.Codes)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"valid": true})
}

// configWrites builds the Config write module from the live dependencies: every create, update and delete of a Config goes through it.
func (s *Server) configWrites() store.ConfigWrites {
	return store.ConfigWrites{Cat: s.deps.Catalog, Changed: s.invalidateCfg, Loaded: s.isConfigLoaded}
}

// writeConfigError maps the Config write module's errors onto the API's codes. name is the Config's name, for the "currently loaded" answer.
func writeConfigError(w http.ResponseWriter, err error, name string) {
	var inv *store.ConfigInvalid
	switch {
	case errors.As(err, &inv):
		writeValidationErrorCodes(w, inv.Fields, inv.Codes)
	case errors.Is(err, store.ErrConfigLoaded):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "config is currently loaded", "config": name})
	case isNotFound(err):
		writeErrorCode(w, http.StatusNotFound, "not_found", map[string]any{"resource": "config"}, "config not found")
	default:
		writeInternalError(w, err)
	}
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

type configBody struct {
	Name             string   `json:"name"`
	VariantID        int64    `json:"variant_id"`
	WeightArtifactID int64    `json:"weight_artifact_id"`
	EngineID         int64    `json:"engine_id"`
	BuildID          int64    `json:"build_id"`
	MMProjArtifactID int64    `json:"mmproj_artifact_id"`
	NCtx             int      `json:"n_ctx"`
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
	// OutputModalities overrides the model's output set; nil = derive. See configJSON.OutputModalities.
	OutputModalities *[]string `json:"output_modalities,omitzero"`
	// CapabilityTierID/CapabilityRank — see configJSON's doc comment.
	CapabilityTierID int64 `json:"capability_tier_id"`
	CapabilityRank   int   `json:"capability_rank"`
	// ChatTemplateCapsOverride — see configJSON's doc comment. The probed
	// fields are deliberately absent from configBody: they are read-only,
	// written only by the engine's post-load probe.
	ChatTemplateCapsOverride map[string]bool `json:"chat_template_caps_override,omitzero"`
	// ReasoningEffortDefault — see configJSON's doc comment.
	ReasoningEffortDefault string `json:"reasoning_effort_default"`
	// Reason is an optional operator note on WHY this change was made —
	// Sprint C. See modelBody.Reason's doc comment; same treatment here.
	Reason string `json:"reason"`
}

// toStore is the one place a request body becomes a store.Config (id is 0 for a create).
func (b configBody) toStore(id int64) store.Config {
	return store.Config{
		ID: id, Name: b.Name, VariantID: b.VariantID, WeightArtifactID: b.WeightArtifactID,
		EngineID: b.EngineID, BuildID: b.BuildID, MMProjArtifactID: b.MMProjArtifactID,
		NCtx: b.NCtx, ExtraArgs: b.ExtraArgs,
		Status: b.Status, Visibility: b.Visibility, IsDefault: b.IsDefault,
		Fingerprint: b.Fingerprint, Logo: b.Logo, LogoDark: b.LogoDark, Modalities: b.Modalities,
		OutputModalities: b.OutputModalities, CapabilityTierID: b.CapabilityTierID, CapabilityRank: b.CapabilityRank,
		ChatTemplateCapsOverride: b.ChatTemplateCapsOverride,
		ReasoningEffortDefault:   b.ReasoningEffortDefault,
	}
}

// queryFlag reports whether the query parameter is present AND not explicitly off. A bare "?force" and "?force=yes/on/true/1" mean on, as they
// did when presence alone counted; "?force=false/0/no/off" and anything unrecognised mean off, so ?force=false can never force a delete.
func queryFlag(r *http.Request, key string) bool {
	q := r.URL.Query()
	if !q.Has(key) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(q.Get(key))) {
	case "", "1", "t", "true", "y", "yes", "on":
		return true
	}
	return false
}
