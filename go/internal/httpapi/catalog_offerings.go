// SPDX-License-Identifier: Apache-2.0

package httpapi

// catalog_offerings.go — Offering CRUD (split from catalog_handlers.go,
// Sprint 5 code-quality cleanup, #33). Mutating routes are
// requireRole(admin) + requireAssurance(page.settings); see httpapi.go's
// route table.

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/store"
)

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

func offeringToJSON(o store.Offering) offeringJSON {
	return offeringJSON{
		ID: o.ID, ModelID: o.ModelID, VariantID: o.VariantID, Provider: o.ProviderName,
		WireModel: o.WireModel, PriceInPer1M: o.PriceInPer1M,
		PriceOutPer1M: o.PriceOutPer1M, PriceCachedInPer1M: o.PriceCachedInPer1M,
		Currency: o.Currency, ContextLength: o.ContextLength, Enabled: o.Enabled,
		Priority: o.Priority,
	}
}

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
