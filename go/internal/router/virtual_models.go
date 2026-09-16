// SPDX-License-Identifier: Apache-2.0

package router

// virtual_models.go — resolves a virtual model name (store.VirtualModel,
// migration 0088_virtual_models.sql) to a real target Config at request
// time. A virtual model is for a consumer that doesn't care which specific
// config serves it, only that it has some general characteristic — "give
// me whatever's smart" or "give me whatever's fast" — so unlike a
// ModelAlias (a fixed config_id), the target here is picked fresh on every
// call.
//
// Once resolved, a virtual model request is indistinguishable from a
// direct request for its target config: catalogChain assigns the
// resolution to cfg/loadName exactly where it already handles a ModelAlias
// hit, so trySubstitute/EnsureLoaded/trySubstituteAfterFailure all run
// unchanged — including capability-tier substitution on the resolved
// target, if it has its own CapabilityTierID.

import (
	"context"
	"sort"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// resolveVirtualModel resolves name to a real Config to load/serve, or
// ok=false when name isn't a visible virtual model, or its resolution kind
// has no eligible candidate right now (e.g. a throughput virtual model when
// nothing has ever been profiled).
func (s *Server) resolveVirtualModel(ctx context.Context, name string) (store.Config, bool) {
	sc := s.deps.StoreCatalog
	if sc == nil {
		return store.Config{}, false
	}
	vm, err := sc.VirtualModelByName(ctx, name)
	if err != nil || vm.Visibility == "hidden" {
		return store.Config{}, false
	}

	var candidates []store.Config
	switch vm.Kind {
	case "capability_tier":
		candidates = s.capabilityTierVirtualCandidates(ctx, vm)
	case "throughput":
		candidates = s.throughputVirtualCandidates(ctx)
	default:
		return store.Config{}, false
	}
	if len(candidates) == 0 {
		return store.Config{}, false
	}

	// Prefer whichever candidate is already loaded, in the candidates'
	// existing priority order — avoids an unnecessary evict+load when a
	// good-enough answer is already warm.
	if s.deps.Sched != nil {
		loaded := make(map[string]bool)
		for _, mode := range s.deps.Sched.Status().Slots {
			if mode != "" {
				loaded[mode] = true
			}
		}
		for _, c := range candidates {
			if loaded[c.Name] {
				return c, true
			}
		}
	}
	// Nothing eligible is loaded — the caller loads candidates[0] the
	// normal way, same as any other model request.
	return candidates[0], true
}

// capabilityTierVirtualCandidates returns vm's tier members, visible only,
// ranked most-capable-first (lower CapabilityRank = more capable — same
// convention decideSubstitute already uses).
func (s *Server) capabilityTierVirtualCandidates(ctx context.Context, vm store.VirtualModel) []store.Config {
	if vm.CapabilityTierID == 0 {
		return nil
	}
	sc := s.deps.StoreCatalog
	members, err := sc.ListConfigsForCapabilityTier(ctx, vm.CapabilityTierID)
	if err != nil {
		return nil
	}
	out := make([]store.Config, 0, len(members))
	for _, m := range members {
		if m.Visibility != "hidden" {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CapabilityRank < out[j].CapabilityRank })
	return out
}

// throughputVirtualCandidates ranks every visible config by real measured
// decode_tps (registry.ConfigCard.Performance.MeasuredTS — the same merged
// curated-benchmark+live-profile figure the Settings UI shows), fastest
// first. A config with no measured figure is never a candidate — this kind
// exists specifically so "fast" is never a guess.
func (s *Server) throughputVirtualCandidates(ctx context.Context) []store.Config {
	if s.deps.Registry == nil || s.deps.StoreCatalog == nil {
		return nil
	}
	cards, err := s.deps.Registry.Cards(ctx, time.Time{})
	if err != nil {
		return nil
	}
	type ranked struct {
		cfg store.Config
		tps float64
	}
	var withTPS []ranked
	for _, c := range cards {
		if c.Visibility == "hidden" || c.Performance.MeasuredTS == nil {
			continue
		}
		cfg, err := s.deps.StoreCatalog.GetConfig(ctx, c.ID)
		if err != nil {
			continue
		}
		withTPS = append(withTPS, ranked{cfg: cfg, tps: *c.Performance.MeasuredTS})
	}
	sort.Slice(withTPS, func(i, j int) bool { return withTPS[i].tps > withTPS[j].tps })
	out := make([]store.Config, 0, len(withTPS))
	for _, r := range withTPS {
		out = append(out, r.cfg)
	}
	return out
}
