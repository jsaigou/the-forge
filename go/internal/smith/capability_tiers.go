// SPDX-License-Identifier: Apache-2.0

package smith

// capability_tiers.go — smith-assisted curation for capability-tier substitution's
// capability_tiers/capability_rank data (ADR-0014, migration 0082_perf_classes.sql).
//
// ADR-0014 rejected deriving substitution ranking automatically from
// benchmark scores — they're sparse, inconsistently sourced across models,
// and silently wrong to compare directly. It chose "curated tiers" instead:
// an operator hand-orders configs into named classes. This file gives smith
// a role in producing that curation without reopening ADR-0014's rejection
// of *automatic* derivation: the reasoning tier proposes a grouping,
// grounded in real catalog data (including benchmark scores, which the
// operator confirmed are a legitimate *input signal* alongside everything
// else — e.g. qwen38-flash-next's real GPQA/SWE-bench scores against
// gemma4-26b-a4b's), but every proposed class and every proposed assignment
// is created as an ordinary KindCatalogChange action — pending, unexecuted,
// awaiting the same human approval gate every other catalog write goes
// through (execute.go's dispatchCatalogChange / sourcing.go's
// applyCatalogChange). Nothing here writes to capability_tiers or configs
// directly. This deliberately does NOT add a chat tool the reasoning tier
// can call to create actions itself — tools.go's doc comment records that
// as an explicit, considered exclusion (only download_start is exempted);
// this is a second, narrow, code-driven exception in the same spirit as
// that one, not a chat tool.
//
// Cross-action sequencing (same real problem sourcing.go's own doc comment
// flags for Model/Variant/Artifact chains): a brand-new class's row ID
// doesn't exist until its own "create" action is approved and executed, so
// an assignment naming a brand-new class can't carry a real capability_tier_id
// yet. ProposeCapabilityTiers handles this by creating "create" actions for
// every new class immediately, but only creating "assign" actions for
// configs whose target class already exists in the catalog at proposal
// time (pre-existing classes, or classes from an earlier, now-approved
// call). A config whose target class is still pending comes back in
// Skipped with an explicit reason — re-running ProposeCapabilityTiers after
// approving the pending class actions resolves it (the reasoning tier is
// called again, but real classes now resolve immediately).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	modelregistry "github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/store"
)

// capabilityTierProposeTimeout bounds ProposeCapabilityTiers' single non-streaming
// reasoning call — generous for one completion over a modest prompt, well
// under turnBudget's 480s (this isn't a chat turn, so that budget doesn't
// apply, but the same order of magnitude is the right ceiling).
const capabilityTierProposeTimeout = 150 * time.Second

// CapabilityTierDraft is one named capability tier the reasoning tier proposed.
type CapabilityTierDraft struct {
	Name  string `json:"name"`
	Mode  string `json:"mode"` // "" | off | fallback_only | prefer_smarter
	Notes string `json:"notes"`
}

// CapabilityTierAssignmentDraft places one candidate config into a named class at
// a rank (lower = more capable; equal ranks are freely interchangeable —
// same semantics as store.Config.CapabilityRank). Rationale is carried through to
// the action's title/detail for the operator's benefit; it is never
// validated or acted on.
type CapabilityTierAssignmentDraft struct {
	ConfigID  int64  `json:"config_id"`
	ClassName string `json:"class_name"`
	Rank      int64  `json:"rank"`
	Rationale string `json:"rationale"`
}

// CapabilityTierProposal is the reasoning tier's raw judgment, parsed but not yet
// validated against live catalog state.
type CapabilityTierProposal struct {
	Classes     []CapabilityTierDraft           `json:"classes"`
	Assignments []CapabilityTierAssignmentDraft `json:"assignments"`
}

// SkippedCapabilityTierEntry records one assignment ProposeCapabilityTiers declined
// to turn into an action, and why — never a silent drop.
type SkippedCapabilityTierEntry struct {
	ConfigID int64  `json:"config_id"`
	Reason   string `json:"reason"`
}

// CapabilityTierProposeResult is ProposeCapabilityTiers' return value.
type CapabilityTierProposeResult struct {
	CandidateCount       int                          `json:"candidate_count"`
	Proposal             CapabilityTierProposal       `json:"proposal"`
	CreatedClassActions  []int64                      `json:"created_class_action_ids"`
	CreatedAssignActions []int64                      `json:"created_assignment_action_ids"`
	Skipped              []SkippedCapabilityTierEntry `json:"skipped,omitempty"`
}

// ErrNoCapabilityTierCandidates is returned when every visible config already
// carries a capability tier — nothing for this pass to propose.
var ErrNoCapabilityTierCandidates = errors.New("smith: no unclassed visible configs to propose")

// ProposeCapabilityTiers asks the reasoning tier to group every currently
// unclassed, visible config into named performance-substitution classes
// (capability_tiers), grounded in real catalog data — family, description,
// context length, modalities, measured throughput, memory footprint, and
// whatever curated benchmark scores exist. See this file's package doc
// comment for the full design (ADR-0014 compliance, the propose→approve
// gate, and the cross-action sequencing rule).
func (s *Smith) ProposeCapabilityTiers(ctx context.Context) (*CapabilityTierProposeResult, error) {
	if s.d.Registry == nil {
		return nil, errors.New("smith: registry not wired")
	}
	if s.d.Catalog == nil {
		return nil, ErrCatalogChangeUnwired
	}

	cards, err := s.d.Registry.Cards(ctx, time.Time{})
	if err != nil {
		return nil, fmt.Errorf("smith: capability tier candidates: %w", err)
	}
	var candidates []modelregistry.ConfigCard
	for _, c := range cards {
		if c.Visibility == "hidden" || c.CapabilityTierID != 0 {
			continue
		}
		candidates = append(candidates, c)
	}
	if len(candidates) == 0 {
		return nil, ErrNoCapabilityTierCandidates
	}

	existing, err := s.d.Catalog.ListCapabilityTiers(ctx)
	if err != nil {
		return nil, fmt.Errorf("smith: list capability tiers: %w", err)
	}

	br := s.Brain(ctx)
	if br.Resolution == BrainDeterministicOnly {
		if s.settingModel(ctx) == "" {
			return nil, errors.New("smith: no brain model configured — cannot judge capability tiers")
		}
		br = s.ensureBrainLoaded(ctx)
		if br.Resolution == BrainDeterministicOnly {
			return nil, errors.New("smith: couldn't load a brain model to judge capability tiers")
		}
	}

	prompt := buildCapabilityTierPrompt(candidates, existing)

	cctx, cancel := context.WithTimeout(ctx, capabilityTierProposeTimeout)
	defer cancel()

	baseOverride := ""
	if br.Resolution == BrainLocalSlot && !s.a0Reachable(cctx) {
		if u, uerr := s.directSlotBaseURL(br.Slot); uerr == nil {
			baseOverride = u
		}
	}

	round, err := s.streamChatCompletion(cctx, chatRequest{
		Model: br.Model,
		Messages: []chatWireMessage{
			{Role: "system", Content: capabilityTierSystemPrompt},
			{Role: "user", Content: prompt},
		},
		BaseURLOverride: baseOverride,
	}, func(string) {})
	if err != nil {
		return nil, fmt.Errorf("smith: capability tier reasoning call: %w", err)
	}
	s.logf("capability tier propose: model=%s finish_reason=%s content_chars=%d", br.Model, round.FinishReason, len(round.Content))

	proposal, err := parseCapabilityTierResponse(round.Content)
	if err != nil {
		s.logf("capability tier propose: unparseable response (%v): %s", err, truncateForContext(round.Content, 2000))
		return nil, fmt.Errorf("smith: capability tier proposal: %w", err)
	}

	return s.applyCapabilityTierProposal(ctx, candidates, existing, proposal)
}

// applyCapabilityTierProposal validates proposal against live catalog state and
// turns every valid entry into a pending KindCatalogChange action. Never
// executes anything itself.
func (s *Smith) applyCapabilityTierProposal(
	ctx context.Context,
	candidates []modelregistry.ConfigCard,
	existing []store.CapabilityTier,
	proposal CapabilityTierProposal,
) (*CapabilityTierProposeResult, error) {
	result := &CapabilityTierProposeResult{CandidateCount: len(candidates), Proposal: proposal}

	candByID := make(map[int64]modelregistry.ConfigCard, len(candidates))
	for _, c := range candidates {
		candByID[c.ID] = c
	}
	existingByName := make(map[string]store.CapabilityTier, len(existing))
	for _, p := range existing {
		existingByName[p.Name] = p
	}

	// Phase 1: propose new classes. Only names the LLM actually proposed
	// and that pass validation; a class the operator already created is
	// left alone (never re-proposed).
	seenClass := map[string]bool{}
	for _, cl := range proposal.Classes {
		name := strings.TrimSpace(cl.Name)
		if name == "" || len(name) > 256 || seenClass[name] {
			continue
		}
		seenClass[name] = true
		switch cl.Mode {
		case "", "off", "fallback_only", "prefer_smarter":
		default:
			continue
		}
		if _, ok := existingByName[name]; ok {
			continue
		}
		row, err := json.Marshal(store.CapabilityTier{Name: name, Mode: cl.Mode, Notes: cl.Notes})
		if err != nil {
			continue
		}
		detail, err := json.Marshal(catalogChangeDetail{Op: "create", Table: "capability_tier", Row: row})
		if err != nil {
			continue
		}
		id, _, err := s.createOrReuseProposal(ctx, ActionDraft{
			Kind:      KindCatalogChange,
			Title:     fmt.Sprintf("Create capability tier %q", name),
			Risk:      RiskLow,
			Detail:    detail,
			DedupeKey: KindCatalogChange + ":capability_tier:create:" + name,
			CreatedBy: "smith",
		})
		if err != nil {
			s.logf("capability tier propose: create class %q: %v", name, err)
			continue
		}
		result.CreatedClassActions = append(result.CreatedClassActions, id)
	}

	// Phase 2: assign candidates into classes that already exist right now
	// (pre-existing, or approved from an earlier call — never the ones just
	// proposed in phase 1 above, whose real ID isn't known until approved).
	assignedConfig := map[int64]bool{}
	for _, asg := range proposal.Assignments {
		cand, ok := candByID[asg.ConfigID]
		if !ok {
			result.Skipped = append(result.Skipped, SkippedCapabilityTierEntry{
				ConfigID: asg.ConfigID, Reason: "not a current unclassed-visible candidate",
			})
			continue
		}
		if assignedConfig[asg.ConfigID] {
			result.Skipped = append(result.Skipped, SkippedCapabilityTierEntry{
				ConfigID: asg.ConfigID, Reason: "duplicate assignment for this config in the proposal",
			})
			continue
		}
		if asg.Rank <= 0 {
			result.Skipped = append(result.Skipped, SkippedCapabilityTierEntry{
				ConfigID: asg.ConfigID, Reason: "rank must be a positive integer",
			})
			continue
		}
		name := strings.TrimSpace(asg.ClassName)
		pc, ok := existingByName[name]
		if !ok {
			result.Skipped = append(result.Skipped, SkippedCapabilityTierEntry{
				ConfigID: asg.ConfigID,
				Reason:   fmt.Sprintf("class %q not yet created — approve its create action first, then re-run", name),
			})
			continue
		}
		assignedConfig[asg.ConfigID] = true
		row, err := json.Marshal(configCapabilityRow{ConfigID: asg.ConfigID, CapabilityTierID: pc.ID, CapabilityRank: asg.Rank})
		if err != nil {
			continue
		}
		detail, err := json.Marshal(catalogChangeDetail{Op: "update", Table: "config_capability", Row: row})
		if err != nil {
			continue
		}
		title := fmt.Sprintf("Assign %s to capability tier %q (rank %d)", cand.Name, name, asg.Rank)
		id, _, err := s.createOrReuseProposal(ctx, ActionDraft{
			Kind:      KindCatalogChange,
			Title:     title,
			Risk:      RiskLow,
			Detail:    detail,
			DedupeKey: fmt.Sprintf("%s:config_capability:%d:%s", KindCatalogChange, asg.ConfigID, name),
			CreatedBy: "smith",
		})
		if err != nil {
			s.logf("capability tier propose: assign config %d: %v", asg.ConfigID, err)
			continue
		}
		result.CreatedAssignActions = append(result.CreatedAssignActions, id)
	}

	sort.Slice(result.CreatedClassActions, func(i, j int) bool { return result.CreatedClassActions[i] < result.CreatedClassActions[j] })
	sort.Slice(result.CreatedAssignActions, func(i, j int) bool { return result.CreatedAssignActions[i] < result.CreatedAssignActions[j] })
	return result, nil
}

// capabilityTierPromptCard is the trimmed, prompt-facing projection of a
// modelregistry.ConfigCard — only the fields relevant to a performance-tier
// judgment, so the prompt doesn't burn budget on icons/badges/license text.
type capabilityTierPromptCard struct {
	ConfigID       int64                      `json:"config_id"`
	ConfigName     string                     `json:"config_name"`
	ModelName      string                     `json:"model_name"`
	Family         string                     `json:"family"`
	Description    string                     `json:"description"`
	KeyFeatures    []string                   `json:"key_features"`
	NCtx           int                        `json:"n_ctx"`
	Modalities     []string                   `json:"modalities"`
	Capabilities   []modelregistry.Capability `json:"capabilities,omitempty"` // real curated benchmark scores when present — sparse and NOT cross-comparable across different benchmark names, see system prompt
	MeasuredTPS    *float64                   `json:"measured_decode_tps,omitempty"`
	MemoryReqBytes *int64                     `json:"memory_req_bytes,omitempty"`
	ToolCalling    *bool                      `json:"supports_tool_calls,omitempty"`
	Status         string                     `json:"status"`
}

// capabilityTierSystemPrompt fixes the output contract and the judgment rules —
// most importantly, that benchmark scores are one input among several, not
// a formula (ADR-0014's own rejected option), and that leaving a config
// unassigned is the correct call when there's no real peer for it.
const capabilityTierSystemPrompt = `You are curating performance-level substitution classes for a local model-serving system (Forge). A "capability tier" is a named group of interchangeable model configs: when one member is already loaded, a request for another member can be served from it instead (capability-tier substitution), so membership is a real production safety decision, not just a leaderboard.

Rules:
1. Only group configs that are genuinely acceptable substitutes for real traffic aimed at each other — similar purpose, similar output quality tier, same rough use case (e.g. general chat/reasoning vs. code-focused vs. lightweight/fast). It is fine to leave a genuine outlier unassigned, but that should be rare — most candidates you're given DO belong in one of your classes, and the whole point of this task is to actually place them, not just name classes.
2. Rank within a class: lower number = more capable/preferred; equal rank = freely interchangeable. Ranks only compare within the same class — never across classes.
3. Use every signal given: family/architecture lineage, description and key features, context length, modalities, measured decode throughput, memory footprint, and curated benchmark scores when present. Benchmark scores ARE a legitimate signal (e.g. a config with clearly higher GPQA/SWE-bench scores than another IS more capable) — use them as real evidence. But they are sparse (many configs have none) and inconsistently sourced (two configs' scores on the "same" capability label routinely come from different underlying benchmarks) — never treat a missing score as "worst", and never let a benchmark comparison override an obvious mismatch in modality, context length, or use case.
4. Class "mode" should almost always be "" (inherit the global routing policy) unless you have a specific reason to force a class's substitution behavior; if unsure, use "".
5. Propose new class names only when needed; reuse an existing class name exactly (byte-for-byte) when a candidate genuinely belongs in it.
6. The "assignments" array is the actual deliverable — a response with classes but no assignments (or with only a couple of candidates placed) has NOT done the job and will be discarded. For every class you name, include every candidate config_id from the list you were given that genuinely belongs in it, each with its own rank and a one-sentence rationale citing the real evidence you used.
7. Respond with ONLY one JSON object, no markdown code fence, no prose before or after it, matching exactly:
{"classes":[{"name":"...","mode":"","notes":"..."}],"assignments":[{"config_id":123,"class_name":"...","rank":1,"rationale":"one sentence, cite the real evidence you used"}]}
class_name in every assignment must exactly match a name in classes (new) or in the existing_classes list you were given. rank is a positive integer. Only omit a config from assignments when it truly has no fair substitute among the others given.`

// buildCapabilityTierPrompt renders the grounded, real-data user message —
// candidates plus whatever classes already exist (so the model can extend
// rather than always invent fresh ones).
func buildCapabilityTierPrompt(candidates []modelregistry.ConfigCard, existing []store.CapabilityTier) string {
	promptCards := make([]capabilityTierPromptCard, 0, len(candidates))
	for _, c := range candidates {
		pc := capabilityTierPromptCard{
			ConfigID: c.ID, ConfigName: c.Name, ModelName: c.ModelName, Family: c.Family,
			Description: c.Description, KeyFeatures: c.KeyFeatures, NCtx: c.NCtx,
			Modalities: c.Modalities, Capabilities: c.Capabilities, Status: c.Status,
		}
		pc.MeasuredTPS = c.Performance.MeasuredTS
		pc.MemoryReqBytes = c.Derived.MemoryReqBytes
		if v, ok := c.ChatTemplateCaps["supports_tool_calls"]; ok {
			pc.ToolCalling = &v
		}
		promptCards = append(promptCards, pc)
	}
	existingNames := make([]string, 0, len(existing))
	for _, p := range existing {
		existingNames = append(existingNames, p.Name)
	}

	candJSON, _ := json.MarshalIndent(promptCards, "", "  ")
	existJSON, _ := json.MarshalIndent(existingNames, "", "  ")
	var b strings.Builder
	b.WriteString("existing_classes (reuse these names exactly when a candidate belongs in one; otherwise propose new ones):\n")
	b.Write(existJSON)
	b.WriteString("\n\ncandidates (every currently unclassed, visible config):\n")
	b.Write(candJSON)
	return b.String()
}

// parseCapabilityTierResponse extracts the JSON object from the reasoning tier's
// reply. Tolerates a stray markdown fence despite the system prompt asking
// for none — cheap, and every other smith JSON-parsing path in this
// package does the same defensive trim. Extraction is a real balanced-brace
// scan (extractFirstJSONObject), not a naive first-'{'/last-'}' slice — the
// latter breaks the instant a model's reply contains a second, unrelated
// '{' or '}' anywhere (stray prose, an example inside an explanation, a
// brace inside a quoted string) after the real object. Found live against
// gemma4-e4b-qat on the first real run of this feature (2026-09-14): the
// naive slice produced a parse error ("invalid character '[' looking for
// beginning of object key string"), i.e. it grabbed text past the real
// object's closing brace.
func parseCapabilityTierResponse(content string) (CapabilityTierProposal, error) {
	trimmed := strings.TrimSpace(content)
	trimmed = strings.TrimPrefix(trimmed, "```json")
	trimmed = strings.TrimPrefix(trimmed, "```")
	trimmed = strings.TrimSuffix(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)
	obj, err := extractFirstJSONObject(trimmed)
	if err != nil {
		return CapabilityTierProposal{}, err
	}
	var p CapabilityTierProposal
	if err := json.Unmarshal([]byte(obj), &p); err != nil {
		return CapabilityTierProposal{}, fmt.Errorf("parse: %w", err)
	}
	return p, nil
}

// extractFirstJSONObject returns the first complete, balanced {...} object
// in s, scanning past string contents (including escaped quotes) so a
// brace character inside a JSON string value never miscounts depth.
func extractFirstJSONObject(s string) (string, error) {
	start := strings.Index(s, "{")
	if start < 0 {
		return "", fmt.Errorf("no JSON object found in response")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unbalanced JSON object in response")
}
