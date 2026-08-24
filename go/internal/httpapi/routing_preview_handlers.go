// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/router"
	"github.com/jsaigou/the-forge/internal/store"
)

// routing_preview_handlers.go — Phase 7 (Settings: Providers + Routing).
// GET /api/v1/routing/preview simulates what a0 would resolve a model to,
// without ever resolving it for real: the remote path shares
// router.SelectOfferingChain (select.go) with offeringChain and
// BuildModelsResponse, so this can never show a chain live routing
// wouldn't actually pick; the local path stops at "config exists and is
// visible" and never calls sched.EnsureLoaded — a preview that loads a
// model is not a preview.
//
// assume_down / assume_disabled let the operator see what a failover would
// pick, but are honest about what they can and can't demonstrate: live
// routing does not consult provider health at all today — a "down"
// provider is only skipped when router.provider_failover is on AND it
// actually returns a transport error/5xx. assume_disabled always changes
// the result (it's a faithful simulation of the real Enabled toggle);
// assume_down only changes the result when provider_failover is currently
// on, and says so explicitly on every affected candidate.

type routingPreviewCandidate struct {
	Provider        string `json:"provider"`
	WireModel       string `json:"wire_model"`
	Priority        int    `json:"priority"`
	OfferingEnabled bool   `json:"offering_enabled"`
	ProviderEnabled bool   `json:"provider_enabled"` // real state, unaffected by assume_disabled
	AssumedDisabled bool   `json:"assumed_disabled"`
	AssumedDown     bool   `json:"assumed_down"`
	Selected        bool   `json:"selected"` // would this candidate actually be used
	Reason          string `json:"reason"`   // why skipped; "" when selected
}

type routingPreviewResponse struct {
	Model            string                    `json:"model"`
	Kind             string                    `json:"kind"` // "local" | "remote" | "not_found"
	HealthConsulted  bool                      `json:"health_consulted"`
	ProviderFailover bool                      `json:"provider_failover"`
	Candidates       []routingPreviewCandidate `json:"candidates"`
	Note             string                    `json:"note,omitempty"`
}

func (s *Server) handleRoutingPreview(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		writeValidationError(w, map[string]string{"model": "is required"})
		return
	}
	if s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, "catalog not wired")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	failover := s.resolvedRouterSettings(ctx).ProviderFailover

	// Local path (mirrors catalogChain's existence+visibility check only —
	// never EnsureLoaded).
	cfg, err := s.deps.Catalog.ConfigByName(ctx, model)
	if err == nil {
		if cfg.Visibility == "hidden" {
			writeJSON(w, http.StatusOK, routingPreviewResponse{
				Model: model, Kind: "not_found", HealthConsulted: false,
				ProviderFailover: failover, Candidates: []routingPreviewCandidate{},
				Note: "a catalog config with this name exists but is hidden — a0 will not route to it",
			})
			return
		}
		writeJSON(w, http.StatusOK, routingPreviewResponse{
			Model: model, Kind: "local", HealthConsulted: false,
			ProviderFailover: failover, Candidates: []routingPreviewCandidate{},
			Note: "local catalog config — served on-demand by the scheduler on any free slot, not through a provider offering chain",
		})
		return
	}
	if !errors.Is(err, store.ErrNotFound) {
		writeInternalError(w, err)
		return
	}

	// Remote path: same selection rule as offeringChain/BuildModelsResponse.
	offerings, err := s.deps.Catalog.ListOfferings(ctx)
	if err != nil {
		writeInternalError(w, fmt.Errorf("catalog lookup failed: %w", err))
		return
	}
	var groupID int64
	for _, o := range offerings {
		if o.WireModel == model {
			groupID = o.ModelID
			break
		}
	}
	if groupID == 0 {
		writeJSON(w, http.StatusOK, routingPreviewResponse{
			Model: model, Kind: "not_found", HealthConsulted: false,
			ProviderFailover: failover, Candidates: []routingPreviewCandidate{},
			Note: "no local config and no offering carries this wire_model",
		})
		return
	}

	var providers []store.ProviderRow
	if s.deps.Routing != nil {
		providers, _ = s.deps.Routing.Providers(ctx)
	}
	realEnabled := map[int64]bool{}
	idByName := map[string]int64{}
	for _, p := range providers {
		idByName[p.Name] = p.ID
		if p.Enabled {
			realEnabled[p.ID] = true
		}
	}

	assumeDisabled := splitCSV(r.URL.Query().Get("assume_disabled"))
	assumeDown := splitCSV(r.URL.Query().Get("assume_down"))
	assumeDisabledSet := map[string]bool{}
	for _, name := range assumeDisabled {
		assumeDisabledSet[name] = true
	}
	assumeDownSet := map[string]bool{}
	for _, name := range assumeDown {
		assumeDownSet[name] = true
	}

	// effectiveEnabled is what SelectOfferingChain actually sees: real
	// enablement, minus assume_disabled unconditionally, minus assume_down
	// only when failover is on (matching what live routing would really do
	// if that provider started 5xxing).
	effectiveEnabled := map[int64]bool{}
	for id, on := range realEnabled {
		effectiveEnabled[id] = on
	}
	for name := range assumeDisabledSet {
		if id, ok := idByName[name]; ok {
			delete(effectiveEnabled, id)
		}
	}
	if failover {
		for name := range assumeDownSet {
			if id, ok := idByName[name]; ok {
				delete(effectiveEnabled, id)
			}
		}
	}

	group := router.GroupOfferingsByModel(offerings)[groupID]
	chain := router.SelectOfferingChain(group, effectiveEnabled, failover)
	selected := map[int64]bool{}
	for _, o := range chain {
		selected[o.ID] = true
	}

	candidates := make([]routingPreviewCandidate, 0, len(group))
	for _, o := range group {
		provEnabled := realEnabled[o.ProviderID]
		down := assumeDownSet[o.ProviderName]
		disabledOverride := assumeDisabledSet[o.ProviderName]
		reason := ""
		switch {
		case selected[o.ID]:
			// no reason — this one is (or would be) used
		case !o.Enabled:
			reason = "offering disabled"
		case disabledOverride:
			reason = "assumed disabled (hypothetical — the real Enabled toggle is unchanged)"
		case !provEnabled:
			reason = "provider disabled"
		case down && failover:
			reason = "assumed down (hypothetical) — provider_failover is on, so a real transport error/5xx would skip this"
		case down && !failover:
			reason = "assume_down has no effect here — provider_failover is off, live routing never consults health"
		case failover:
			reason = "lower priority than the selected offering(s)"
		default:
			reason = "not the primary — provider_failover is off, so only the lowest-priority offering is ever used"
		}
		candidates = append(candidates, routingPreviewCandidate{
			Provider:        o.ProviderName,
			WireModel:       o.WireModel,
			Priority:        o.Priority,
			OfferingEnabled: o.Enabled,
			ProviderEnabled: provEnabled,
			AssumedDisabled: disabledOverride,
			AssumedDown:     down,
			Selected:        selected[o.ID],
			Reason:          reason,
		})
	}

	writeJSON(w, http.StatusOK, routingPreviewResponse{
		Model: model, Kind: "remote", HealthConsulted: false,
		ProviderFailover: failover, Candidates: candidates,
	})
}

func splitCSV(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
