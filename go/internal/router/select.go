// SPDX-License-Identifier: Apache-2.0

package router

import (
	"sort"

	"github.com/jsaigou/the-forge/internal/store"
)

// select.go — Phase 7 (Settings: Providers + Routing/Compressor) extraction.
// The offering-group primary-selection rule used to be reimplemented
// independently in offeringChain (routing.go), BuildModelsResponse
// (catalog.go), and CatalogPanel.tsx's "preferred" badge — three
// implementations that could each drift from the others (and already had:
// offeringChain's own doc comment claimed the tie-break was offering id
// while the SQL it relied on, store.catalogView.ListOfferings, actually
// broke ties on wire_model). This file is now the one place the rule lives
// on the Go side; the routing-preview endpoint (httpapi) shares it too.

// GroupOfferingsByModel groups offerings by their catalog Model, sorting
// each group explicitly rather than trusting the caller's row order:
// priority ascending, then provider name, then offering id. This is the
// tie-break offeringChain's doc comment always described — reconciled here
// since callers no longer need to rely on ListOfferings' own ORDER BY.
func GroupOfferingsByModel(offerings []store.Offering) map[int64][]store.Offering {
	groups := map[int64][]store.Offering{}
	for _, o := range offerings {
		groups[o.ModelID] = append(groups[o.ModelID], o)
	}
	for id, g := range groups {
		sort.SliceStable(g, func(i, j int) bool {
			if g[i].Priority != g[j].Priority {
				return g[i].Priority < g[j].Priority
			}
			if g[i].ProviderName != g[j].ProviderName {
				return g[i].ProviderName < g[j].ProviderName
			}
			return g[i].ID < g[j].ID
		})
		groups[id] = g
	}
	return groups
}

// SelectOfferingChain narrows one model's priority-ordered offering group
// (as returned by GroupOfferingsByModel) to what's actually routable right
// now: enabled offerings of enabled providers. enabledProviders is keyed by
// provider id and must be built explicitly by the caller — there is no
// "nil means permissive" default here, because the two existing callers
// want opposite defaults for their own "provider store unwired" case
// (offeringChain must fail closed for safety; BuildModelsResponse's
// skeleton-mode listing intentionally does not filter — see its own
// construction of the map).
//
// failover=false (the default, "no fallback chains" posture) returns only
// the primary — the first routable entry. failover=true returns the whole
// routable chain in priority order, for tryBackends' transport-error/5xx
// failover to walk. A nil result means the group has nothing routable at
// all (every offering disabled, or every provider disabled/missing).
func SelectOfferingChain(group []store.Offering, enabledProviders map[int64]bool, failover bool) []store.Offering {
	var routable []store.Offering
	for _, o := range group {
		if !o.Enabled || !enabledProviders[o.ProviderID] {
			continue
		}
		routable = append(routable, o)
	}
	if len(routable) == 0 {
		return nil
	}
	if !failover {
		return routable[:1]
	}
	return routable
}
