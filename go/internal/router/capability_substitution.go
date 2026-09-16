// SPDX-License-Identifier: Apache-2.0

package router

// capability_substitution.go — capability-tier substitution (Sprint P3, 2026-09-13): let an
// already-loaded config stand in for a requested one, in two modes —
// "fallback_only" (substitute only when loading the requested config is
// genuinely infeasible right now) and "prefer_smarter" (substitute whenever
// a resident config outranks the requested one, even though loading the
// requested one would have been feasible). Default off; a request that
// resolves no substitute behaves byte-identically to before this sprint.
//
// The decision is made in catalogChain, BEFORE EnsureLoaded(want) —
// fallback_only must not pay a 320s block just to discover infeasibility,
// and prefer_smarter must not load `want` only to immediately discard it.
// Substitution is decided at most twice per request (see catalogChain): a
// first pass before EnsureLoaded(want), and — only for fallback_only, only
// when EnsureLoaded(want) itself still fails — one second pass covering the
// one gap CouldLoad structurally cannot see, the engine's same-weights
// sibling guard (engine/lifecycle.go, below place()). It can never chain to
// a third config.
//
// decideSubstitute itself is pure and I/O-free, deliberately: everything
// that needs a DB read or a scheduler probe happens in capabilityTierCandidates
// (this file) before it's called, so the actual selection logic is a plain
// table-testable function over already-gathered, already-gated data.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jsaigou/the-forge/internal/store"
)

// substitutionPolicy is one of the three settable modes. "" (unset/unknown)
// is treated as subOff everywhere it's read, never as a distinct value.
type substitutionPolicy string

const (
	subOff           substitutionPolicy = "off"
	subFallbackOnly  substitutionPolicy = "fallback_only"
	subPreferSmarter substitutionPolicy = "prefer_smarter"
)

func parseSubstitutionPolicy(raw string) substitutionPolicy {
	switch substitutionPolicy(raw) {
	case subFallbackOnly, subPreferSmarter:
		return substitutionPolicy(raw)
	default:
		return subOff
	}
}

// substitutionPolicy resolves the effective policy for a config: its
// CapabilityTier's own Mode override when set, else the global
// router.capability_substitution setting (store Settings KV — same home as
// router.busy_mode/router.provider_failover), else off.
func (s *Server) substitutionPolicy(ctx context.Context, capabilityTierID int64) substitutionPolicy {
	cat := s.deps.StoreCatalog
	if cat != nil && capabilityTierID != 0 {
		if pc, err := cat.GetCapabilityTier(ctx, capabilityTierID); err == nil && pc.Mode != "" {
			return parseSubstitutionPolicy(pc.Mode)
		}
	}
	if st := s.deps.Settings; st != nil {
		if raw, err := st.Get(ctx, "router.capability_substitution"); err == nil {
			var v string
			if err := json.Unmarshal(raw, &v); err == nil {
				return parseSubstitutionPolicy(v)
			}
		}
	}
	return subOff
}

// peerCandidate is one performance-class peer that is currently loaded on a
// slot and has already passed every hard gate in substitutionAllowed.
type peerCandidate struct {
	config store.Config
	slot   string
}

// hasResponseFormat fail-closes substitution whenever the request asks for
// structured output — no data exists anywhere (catalog or live probe) on
// which configs honor response_format/json_schema the same way, so the
// only safe default is to never guess. Unlike tool-calling (see
// peerSupportsTools, Sprint P4), there is no live-probeable signal for
// this to relax against.
func hasResponseFormat(body map[string]any) bool {
	v, ok := body["response_format"]
	return ok && v != nil
}

// hasToolFields reports whether the request carries tool-calling fields —
// gates a per-peer capability check (peerSupportsTools) rather than a
// blanket refusal, now that /props' chat_template_caps gives a real,
// live-probed signal instead of a guess (Sprint P4; Sprint P3 fail-closed
// unconditionally here since no such data existed yet).
func hasToolFields(body map[string]any) bool {
	for _, key := range []string{"tools", "functions", "tool_choice"} {
		if v, ok := body[key]; ok && v != nil {
			return true
		}
	}
	return false
}

// capabilityTierCandidates gathers every performance-class peer of want that is
// (a) currently loaded on some slot and (b) passes every hard gate,
// most-capable-first (ListConfigsForCapabilityTier's own ordering). Returns nil
// — no I/O beyond the two list reads plus one small per-peer gate check —
// whenever want isn't in a class, the request asks for structured output
// (see hasResponseFormat), or the catalog/scheduler aren't wired.
func (s *Server) capabilityTierCandidates(ctx context.Context, want store.Config, body map[string]any) []peerCandidate {
	if want.CapabilityTierID == 0 || hasResponseFormat(body) {
		return nil
	}
	cat := s.deps.StoreCatalog
	scd := s.deps.Sched
	if cat == nil || scd == nil {
		return nil
	}
	peers, err := cat.ListConfigsForCapabilityTier(ctx, want.CapabilityTierID)
	if err != nil {
		return nil
	}
	needsTools := hasToolFields(body)
	loadedSlot := map[string]string{}
	for slot, mode := range scd.Status().Slots {
		if mode != "" {
			loadedSlot[mode] = slot
		}
	}
	var out []peerCandidate
	for _, p := range peers {
		if p.ID == want.ID {
			continue
		}
		slot, isLoaded := loadedSlot[p.Name]
		if !isLoaded {
			continue
		}
		if !s.substitutionAllowed(ctx, want, p) {
			continue
		}
		if needsTools && !s.peerSupportsTools(slot) {
			continue
		}
		out = append(out, peerCandidate{config: p, slot: slot})
	}
	return out
}

// peerSupportsTools live-probes the peer's own slot for tool-calling
// capability (SlotProbe.SupportsTools, ttlCatalog reading /props'
// chat_template_caps — the same fetch already used for health/NCtx, no
// extra round trip). Since the peer must already be loaded to be a
// candidate at all, this is always a real, current answer, never a
// stored/historical one — no catalog column claims tool-calling support
// anywhere, so a peer with an unresolvable port fails closed.
func (s *Server) peerSupportsTools(slot string) bool {
	port := s.deps.Slots[slot]
	if port == 0 {
		return false
	}
	return s.catalog().Probe(port, s.cfg().healthTTL()).SupportsTools
}

// substitutionAllowed enforces the hard gates — policy-independent
// safety properties a substitution must never violate regardless of how
// the operator ranked the class.
func (s *Server) substitutionAllowed(ctx context.Context, want, peer store.Config) bool {
	if peer.Visibility == "hidden" {
		return false
	}
	if peer.NCtx < want.NCtx {
		return false
	}
	if s.sameWeights(ctx, want, peer) {
		return false
	}
	if !s.modalitiesSuperset(ctx, want, peer) {
		return false
	}
	return true
}

// sameWeights mirrors engine.WeightIdentity's own rule ("row IDs are not
// identity; files are") without importing the engine package: two configs
// pointing at different weight_artifact_id rows can still be the exact same
// GGUF on disk (the gemma4-26b-a4b / -nothink duplicate-artifact-row
// incident this gate exists to prevent repeating — ADR-0006's rejected
// case, ADR-0006 amendment on same-weights siblings). Unresolvable (missing
// artifact row, catalog not wired) disables the check rather than guessing,
// matching WeightIdentity's own "" disables same-weights checks" rule.
func (s *Server) sameWeights(ctx context.Context, a, b store.Config) bool {
	if a.WeightArtifactID == 0 || b.WeightArtifactID == 0 {
		return false
	}
	af, aok := s.artifactFilePath(ctx, a.WeightArtifactID)
	bf, bok := s.artifactFilePath(ctx, b.WeightArtifactID)
	if !aok || !bok || af == "" || bf == "" || af != bf {
		return false
	}
	amf, amok := s.artifactFilePath(ctx, a.MMProjArtifactID)
	bmf, bmok := s.artifactFilePath(ctx, b.MMProjArtifactID)
	return amok && bmok && amf == bmf
}

// artifactFilePath resolves one artifact row's file path. artifactID == 0
// ("no mmproj") resolves to ("", true) — a legitimate, matchable value, not
// a failure — so two configs that both have no mmproj still compare equal.
func (s *Server) artifactFilePath(ctx context.Context, artifactID int64) (path string, ok bool) {
	if artifactID == 0 {
		return "", true
	}
	cat := s.deps.StoreCatalog
	if cat == nil {
		return "", false
	}
	a, err := cat.GetArtifact(ctx, artifactID)
	if err != nil {
		return "", false
	}
	return a.FilePath, true
}

// modalitiesSuperset requires peer to deliver everything want's own
// resolved modalities promise — a vision request must never land on a
// text-only substitute. Unresolvable either side (missing variant/model
// row) fails closed.
func (s *Server) modalitiesSuperset(ctx context.Context, want, peer store.Config) bool {
	wantRes, ok := s.resolveConfigModalities(ctx, want)
	if !ok {
		return false
	}
	peerRes, ok := s.resolveConfigModalities(ctx, peer)
	if !ok {
		return false
	}
	peerSet := make(map[string]bool, len(peerRes.Enabled))
	for _, m := range peerRes.Enabled {
		peerSet[m] = true
	}
	for _, m := range wantRes.Enabled {
		if !peerSet[m] {
			return false
		}
	}
	return true
}

func (s *Server) resolveConfigModalities(ctx context.Context, c store.Config) (store.ModalityResolution, bool) {
	cat := s.deps.StoreCatalog
	if cat == nil {
		return store.ModalityResolution{}, false
	}
	v, err := cat.GetVariant(ctx, c.VariantID)
	if err != nil {
		return store.ModalityResolution{}, false
	}
	mdl, err := cat.GetModel(ctx, v.ModelID)
	if err != nil {
		return store.ModalityResolution{}, false
	}
	mmprojMissing := false
	if c.MMProjArtifactID != 0 {
		a, err := cat.GetArtifact(ctx, c.MMProjArtifactID)
		if err != nil {
			return store.ModalityResolution{}, false
		}
		mmprojMissing = a.Missing
	}
	return store.ResolveModalities(c, mdl, mmprojMissing), true
}

// decideSubstitute picks a substitute from already-gathered, already-gated
// candidates (most-capable-first). Pure and table-testable: no I/O, no
// context, no scheduler/store access. wantTerminal is only consulted under
// fallback_only — it must be sched.Placement.Terminal for `want` (computed
// by the caller via CouldLoad), never derived here.
//
// prefer_smarter substitutes on any candidate STRICTLY more capable than
// want (CapabilityRank <, not <=) — an equally-ranked peer is not "smarter", so
// there is nothing to prefer it for. fallback_only substitutes on the first
// candidate at rank <= want's (equal-or-better; want is already known
// infeasible, so equal capability is an acceptable stand-in).
func decideSubstitute(policy substitutionPolicy, want store.Config, candidates []peerCandidate, wantTerminal bool) (sub store.Config, slot, why string, ok bool) {
	switch policy {
	case subPreferSmarter:
		for _, c := range candidates {
			if c.config.CapabilityRank < want.CapabilityRank {
				return c.config, c.slot, fmt.Sprintf(
					"capability-tier substitution (prefer_smarter): %s (rank %d) already loaded and outranks requested %s (rank %d)",
					c.config.Name, c.config.CapabilityRank, want.Name, want.CapabilityRank), true
			}
		}
	case subFallbackOnly:
		if !wantTerminal {
			return store.Config{}, "", "", false
		}
		for _, c := range candidates {
			if c.config.CapabilityRank <= want.CapabilityRank {
				return c.config, c.slot, fmt.Sprintf(
					"capability-tier substitution (fallback_only): %s substituted for %s — not feasible to load right now",
					c.config.Name, want.Name), true
			}
		}
	}
	return store.Config{}, "", "", false
}
