// SPDX-License-Identifier: Apache-2.0

package httpapi

// registry_handlers.go — model registry card lists (Contract 1 §2 #9).
// Phase B: the registry now reads from the catalog DB. Two endpoints:
//
//   - GET /api/v1/configs/cards — ConfigCard[] (config-scoped, B1). The
//     console "Choose a config" gallery consumes this.
//   - GET /api/v1/models/cards — Card[] (model-scoped). The model gallery
//     modal consumes this.
//
// Split out of handlers.go by Sprint 0; Phase B added the config-scoped
// endpoint. Owner track: BE-2.

import (
	"context"
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/registry"
)

// handleConfigCards returns config-scoped cards (B1).
func (s *Server) handleConfigCards(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if !usageWindowRE.MatchString(window) {
		window = "7d"
	}

	cards := []registry.ConfigCard{}
	if s.deps.Registry != nil {
		dur, ok := parseUsageWindow(window)
		if !ok {
			dur = 7 * 24 * time.Hour
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		got, err := s.deps.Registry.Cards(ctx, time.Now().Add(-dur))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "registry query failed")
			return
		}
		cards = got
	}

	display := s.cardDisplayCurrency(r)
	for i := range cards {
		s.convertCardPowerEst(r, &cards[i].Performance, display)
	}

	writeJSON(w, http.StatusOK, configCardsResponse{
		Cards:           cards,
		Window:          window,
		DisplayCurrency: display,
	})
}

// handleModelCards returns model-scoped cards (the model gallery modal).
func (s *Server) handleModelCards(w http.ResponseWriter, r *http.Request) {
	window := r.URL.Query().Get("window")
	if !usageWindowRE.MatchString(window) {
		window = "7d"
	}

	cards := []registry.Card{}
	if s.deps.Registry != nil {
		dur, ok := parseUsageWindow(window)
		if !ok {
			dur = 7 * 24 * time.Hour
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		got, err := s.deps.Registry.ModelCards(ctx, time.Now().Add(-dur))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "registry query failed")
			return
		}
		cards = got
	}

	display := s.cardDisplayCurrency(r)
	for i := range cards {
		s.convertCardPowerEst(r, &cards[i].Performance, display)
	}

	writeJSON(w, http.StatusOK, modelCardsResponse{
		Cards:           cards,
		Window:          window,
		DisplayCurrency: display,
	})
}

// cardDisplayCurrency resolves the display currency for card responses:
// billing.display_currency via the FX source (mirroring usage_handlers), with
// "USD" as the fallback when FX is nil/unset. Cards are viewer-accessible
// (bare HandleFunc), so this deliberately does NOT read the operator-gated
// billing settings endpoint server-side — it rides the same FX source the
// usage handler uses.
func (s *Server) cardDisplayCurrency(r *http.Request) string {
	return s.displayCurrency(r.Context())
}

// convertCardPowerEst FX-converts a card's power_est_per_1m from the
// electricity rate currency (config.Cost.RateCurrency) to the display
// currency, matching how the usage handler prices local cost. The registry
// computes power_est_per_1m in rate currency; without this the card would
// render a JPY-denominated figure as if it were USD. nil/absent value is left
// untouched (no price on the card). 1:1 when currencies match or FX is nil.
func (s *Server) convertCardPowerEst(r *http.Request, perf *registry.Performance, display string) {
	if perf == nil || perf.PowerEstPer1m == nil {
		return
	}
	rateCurrency := "USD"
	if s.deps.Config != nil {
		if cfg := s.deps.Config(); cfg != nil && cfg.Cost.RateCurrency != "" {
			rateCurrency = cfg.Cost.RateCurrency
		}
	}
	converted, _ := s.convert(r.Context(), *perf.PowerEstPer1m, rateCurrency, display)
	perf.PowerEstPer1m = &converted
}
