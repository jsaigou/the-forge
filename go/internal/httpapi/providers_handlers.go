// SPDX-License-Identifier: Apache-2.0

package httpapi

// providers_handlers.go — provider taxonomy + credits + status read
// (Sprint 0 §0.3). BE-3 owns this file; the frozen response shape
// (providersResponse / providerJSON et al.) lives in shapes.go and is
// pinned by shapes_freeze_test.go.
//
// The GET handler delegates to internal/providers.Service, which reads
// the catalog (router_providers, plus offerings for the provided-models
// list — Phase 7) from the store and refreshes+serves the cached
// (health, credits) pair per provider.
// "idle" is intentionally absent from the health vocabulary (user: "I
// don't care about idle"); the states are reachable|degraded|down|unknown.
//
// Provider-key CRUD (POST/PUT/PUT-key/DELETE) was relocated to Settings
// (§0.9) and lives in settings_handlers.go (BE-5). This file keeps only
// the GET read endpoint and the shared notImplemented helper.

import (
	"context"
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/providers"
)

// notImplemented writes a uniform 501 documenting the frozen response shape
// and the owning track, so the stub is self-describing until the feature
// lands. Lives here (the providers file) because the providers + metrics +
// billing handlers all share it; the §0.1 split left this helper where the
// first 501 stub was written (providers).
func notImplemented(w http.ResponseWriter, r *http.Request, shape, track string) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{
		"error": "not implemented yet — frozen shape only (see docs/v5-sprint0-contract-freeze.md)",
		"shape": shape,
		"track": track,
		"path":  r.URL.Path,
	})
}

// handleProvidersList — GET /api/v1/providers (§0.3, operator). Returns
// the frozen providersResponse shape: one entry per router_providers row,
// each with models[] (offerings-derived, Phase 7), health (status-page
// or live-probe, cached), and credits (supported:false when the provider
// has no balance API — e.g. AI& today).
//
// Nil deps.Providers (Phase 4 stub environment, or forge not yet wired)
// returns an empty providers list, not a 501 — the frozen shape requires
// an array, and the FE renders an empty state cleanly.
func (s *Server) handleProvidersList(w http.ResponseWriter, r *http.Request) {
	resp := providersResponse{Providers: []providerJSON{}}
	if s.deps.Providers == nil {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	list, err := s.deps.Providers.List(ctx)
	if err != nil {
		// Catalog read failure (DB error) is a real 500 — distinct from
		// "no providers configured" (an empty list). Per-provider fetch
		// failures never reach here; they degrade to unknown/supported:false.
		writeError(w, http.StatusInternalServerError, "providers query failed")
		return
	}
	for _, p := range list {
		resp.Providers = append(resp.Providers, providerJSON{
			ID:                 p.ID,
			Name:               p.Name,
			APIKeyMasked:       p.APIKeyMasked,
			BillCurrency:       p.BillCurrency,
			Health:             toProviderHealthJSON(p.Health),
			Credits:            toProviderCreditsJSON(p.Credits),
			Models:             toProviderModelsJSON(p.Models),
			BillingEnabled:     p.BillingEnabled,
			BillingConsoleURL:  p.BillingConsoleURL,
			CreditsURL:         p.CreditsURL,
			TargetURL:          p.TargetURL,
			StatusURL:          p.StatusURL,
			OrgID:              p.OrgID,
			Enabled:            p.Enabled,
			Country:            p.Country,
			DataResidencyGroup: p.DataResidencyGroup,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

// toProviderHealthJSON converts the providers.Health domain type to the
// frozen wire shape. zero-time AsOf emits null; "" Detail emits null.
func toProviderHealthJSON(h providers.Health) providerHealthJSON {
	out := providerHealthJSON{
		State:  h.State,
		Source: h.Source,
	}
	if !h.AsOf.IsZero() {
		v := float64(h.AsOf.UnixNano()) / 1e9
		out.AsOf = &v
	}
	if h.Detail != "" {
		out.Detail = &h.Detail
	}
	return out
}

// toProviderCreditsJSON converts the providers.Credits domain type to the
// frozen wire shape. nil BalanceNative / "" Currency emit null.
func toProviderCreditsJSON(c providers.Credits) providerCreditsJSON {
	out := providerCreditsJSON{
		BalanceNative: c.BalanceNative,
		SpendPeriod:   c.SpendPeriod,
		Supported:     c.Supported,
	}
	if c.Currency != "" {
		out.Currency = &c.Currency
	}
	if !c.AsOf.IsZero() {
		v := float64(c.AsOf.UnixNano()) / 1e9
		out.AsOf = &v
	}
	if c.SpendPeriodLabel != "" {
		out.SpendPeriodLabel = &c.SpendPeriodLabel
	}
	return out
}

// toProviderModelsJSON converts the providers.Model domain type (Phase 7:
// offerings-derived, see providers.go's offeringsByProvider) to the wire
// shape. Empty CompressorProxy / false Passthrough emit null.
func toProviderModelsJSON(models []providers.Model) []providerModelJSON {
	out := make([]providerModelJSON, 0, len(models))
	for _, m := range models {
		mj := providerModelJSON{
			ModelID:        m.ModelID,
			CatalogModelID: m.CatalogModelID,
			DisplayName:    m.DisplayName,
			Logo:           m.Logo,
			PriceInPer1M:   m.PriceInPer1M,
			PriceOutPer1M:  m.PriceOutPer1M,
			Currency:       m.Currency,
			Priority:       m.Priority,
			Enabled:        m.Enabled,
		}
		if m.CompressorProxy != "" {
			mj.CompressorProxy = &m.CompressorProxy
			b := m.Passthrough
			mj.Passthrough = &b
		}
		out = append(out, mj)
	}
	return out
}
