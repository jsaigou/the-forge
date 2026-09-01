// SPDX-License-Identifier: Apache-2.0

package smith

// intents.go implements the smith fast-path intent classifier
// (docs/v5-smith-experience.md §2.2 UNDERSTAND step, §2.7, §3.1). It is a
// 100% deterministic pattern matcher over entity mentions — NO LLM, no
// probabilistic matching, no learning. A user's question is classified into
// one intent FAMILY (health, version, quantity, reachability, listing,
// history, logs, kb, action) plus the specific ENTITY it asks about, or
// FamilyNoMatch when the classifier is not confident (which routes the turn
// to the existing reasoning tier).
//
// Entities are derived from live config/catalog at classify time (§2.7: "the
// catalog is derived, not enumerated"): check IDs from the registry, service
// names from cfg.Ports + servicePortUnit, slot labels from sched.Status,
// tracked binaries from smith.binaries.tracked, mesh service names from
// smith.mesh.services (migration 0060 — deployment data, operator-edited;
// the KB mesh table is no longer the classifier's entity source), and
// curated aliases ("tailnet"/"tailscale", "comfy"/"comfyui", …). A newly
// tracked binary or mesh service becomes askable with zero classifier
// changes — knownEntities reads it live.
//
// Matching is conservative (§2.2: "A wrong fast answer is worse than a slow
// one"): a family matches only when the text carries that family's cue words
// AND mentions a known entity for it. Shared aliases (e.g. "comfyui" is both
// a health entity and, via the deployment's local ComfyUI mesh entry, a
// reachability entity) are
// disambiguated by cue strength — a bare "up" resolves to health for a shared
// alias, to reachability for a reachability-only alias. Anything genuinely
// ambiguous falls through to FamilyNoMatch, which triggers THINK.

import (
	"context"
	"strings"

	"github.com/jsaigou/the-forge/internal/config"
)

// IntentFamily is the family of a classified question (§2.7).
type IntentFamily string

const (
	FamilyHealth          IntentFamily = "health"
	FamilyVersion         IntentFamily = "version"
	FamilyQuantity        IntentFamily = "quantity"
	FamilyReachability    IntentFamily = "reachability"
	FamilyListing         IntentFamily = "listing"
	FamilyHistory         IntentFamily = "history"
	FamilyLogs            IntentFamily = "logs"
	FamilyKB              IntentFamily = "kb"
	FamilyAction          IntentFamily = "action"
	FamilySweepRequest    IntentFamily = "sweep_request"
	FamilyContextFollowup IntentFamily = "context_followup"
	FamilyNoMatch         IntentFamily = "no_match"
)

// Intent is the result of classifying one user turn.
type Intent struct {
	Family   IntentFamily
	Entity   string // the matched entity (e.g. "comfyui", "a0", "ram")
	Phrasing string // the cue phrase that matched (for diagnostics)
	// ConversationID is set by Chat() before calling Answer() so action
	// intents can link their CreateAction drafts to the active conversation
	// (§2.4.2). nil when Answer() is called outside a chat turn (tests).
	ConversationID *int64
}

// entityAlias pairs a canonical entity name with the surface aliases that
// mention it in user text. Aliases are matched as whole-word, case-insensitive
// substrings of the normalized text.
type entityAlias struct {
	entity  string
	aliases []string
}

// entitySet is the per-family collection of askable entities, built fresh from
// live config on every Classify call (§3.1).
type entitySet struct {
	health       []entityAlias
	version      []entityAlias
	quantity     []entityAlias
	reachability []entityAlias
	listing      []entityAlias
	history      []entityAlias
	logs         []entityAlias
	kb           []entityAlias
	action       []entityAlias
}

// healthAliasSet is the set of all surface aliases that resolve to a health
// entity, used to decide whether a reachability alias is "reachability-only"
// (a bare "up" should route to health, not reachability, for a shared alias).
// Built once per Classify from the health entity list.
func (es entitySet) healthAliasSet() map[string]bool {
	m := map[string]bool{}
	for _, ea := range es.health {
		for _, a := range ea.aliases {
			m[a] = true
		}
	}
	return m
}

// Classify examines user text and returns the best-matching intent, or
// FamilyNoMatch. Conservative: ambiguous → no_match (which triggers THINK).
// The classifier is 100% deterministic — no LLM, no probabilistic matching.
func (s *Smith) Classify(ctx context.Context, text string) Intent {
	return s.classifyWithContext(ctx, text, nil)
}

// classifyWithContext is the entry point for classification. When context
// items are attached (§3.4, R5), it tries to resolve the context's source/
// code against known check IDs first — the composed seed message text may
// not naturally classify to the right family (e.g. "alert" in the text
// triggers the logs family). If a context code/source matches a known check,
// it returns a health intent for that check; otherwise it falls through to
// the normal text-based Classify.
func (s *Smith) classifyWithContext(ctx context.Context, text string, contextItems []ChatContext) Intent {
	if len(contextItems) > 0 {
		if intent, ok := s.classifyContextItems(ctx, contextItems); ok {
			return intent
		}
	}
	norm := normalizeClassifyText(text)
	if norm == "" {
		return Intent{Family: FamilyNoMatch}
	}
	es := s.knownEntities(ctx)
	healthAliases := es.healthAliasSet()

	// Families are checked in precedence order. The first family that
	// confidently matches (cue + entity) wins. The ordering resolves the
	// shared-alias cases: action and the "how/why" question families are
	// checked before health/reachability so an imperative or diagnostic
	// phrasing is never misread as a bare health check.
	for _, m := range []match{
		s.matchAction(norm, es),
		s.matchHistory(norm, es),
		s.matchLogs(norm, es),
		s.matchVersion(norm, es),
		s.matchKB(norm, es),
		s.matchQuantity(norm, es),
		s.matchReachability(norm, es, healthAliases),
		s.matchListing(norm, es),
		s.matchHealth(norm, es),
	} {
		if m.family != "" && m.entity != "" {
			return Intent{Family: m.family, Entity: m.entity, Phrasing: m.phrase}
		}
	}
	return Intent{Family: FamilyNoMatch}
}

// classifyContextItems tries to resolve attached error context (§3.4) to a
// health intent. The context's Source field is often a check ID (e.g.
// "gpu_hang"), and the Code field is an alert/notification code (e.g.
// "KFD_EVICTION", "INFERENCE_HANG"). We check each against the registered
// check IDs; a match returns a health intent for that check's owning entity.
// Alert-code-to-check mappings are curated for the known set (§8 item 16's
// notification codes); unknown codes fall through to normal text Classify.
func (s *Smith) classifyContextItems(ctx context.Context, items []ChatContext) (Intent, bool) {
	checkIDs := make(map[string]bool, len(registry))
	for _, c := range registry {
		checkIDs[c.ID] = true
	}
	for _, item := range items {
		// Source is often the check ID directly (e.g. "gpu_hang").
		if item.Source != "" && checkIDs[item.Source] {
			return Intent{Family: FamilyHealth, Entity: checkIDToHealthEntity(item.Source), Phrasing: "context:" + item.Source}, true
		}
		// Code might be a check ID too.
		if item.Code != "" && checkIDs[item.Code] {
			return Intent{Family: FamilyHealth, Entity: checkIDToHealthEntity(item.Code), Phrasing: "context:" + item.Code}, true
		}
		// Unit-scoped alert codes: route to the CRASHED UNIT's own health
		// entity when the unit is known, before falling back to
		// alertCodeToEntity's generic per-code mapping. Without this, every
		// UNIT_CRASH/UNIT_OOM — ComfyUI, a slot, a service, a compressor
		// proxy — answered about the same generic "forge" entity regardless
		// of which unit actually failed (found live 2026-09-01: a
		// forge-comfyui crash was diagnosed via forge_self's unrelated DB-
		// integrity check).
		if item.Unit != "" && unitScopedAlertCode(item.Code) {
			if entity := s.unitToHealthEntity(item.Unit); entity != "" {
				return Intent{Family: FamilyHealth, Entity: entity, Phrasing: "context:" + item.Code}, true
			}
		}
		// Known alert/notification codes that map to a check.
		if entity, ok := alertCodeToEntity(item.Code); ok {
			return Intent{Family: FamilyHealth, Entity: entity, Phrasing: "context:" + item.Code}, true
		}
	}
	return Intent{}, false
}

// unitScopedAlertCode reports whether code identifies a specific systemd
// unit (mirrors collector.unitAlerts' three codes — run.go) rather than a
// port/GTT condition with no single owning unit.
func unitScopedAlertCode(code string) bool {
	switch code {
	case "UNIT_CRASH", "UNIT_OOM", "UNIT_RESTARTED":
		return true
	}
	return false
}

// unitToHealthEntity maps a real systemd unit name to the health entity
// entityCheck (answers.go) already knows how to check — the missing half of
// classifyContextItems' unit-scoped routing above. Returns "" for a unit
// with no dedicated health entity (e.g. forge-a5 doesn't exist, an unknown
// compressor instance name) rather than guessing.
func (s *Smith) unitToHealthEntity(unit string) string {
	if cfg := s.cfg(); cfg != nil && cfg.Server.TTSUnit != "" && unit == cfg.Server.TTSUnit {
		return "tts"
	}
	switch unit {
	case "forge-comfyui":
		return "comfyui"
	case "forge-daemon":
		return "forge"
	case "forge-embedding":
		return "embedding"
	case "forge-stt":
		return "stt"
	case "forge-aligner":
		return "aligner"
	case "forge-tts":
		return "tts"
	case "forge-a1", "forge-a2", "forge-a3", "forge-a4":
		return strings.TrimPrefix(unit, "forge-")
	}
	if compressorUnitPattern.MatchString(unit) {
		return "compressor"
	}
	return ""
}

// checkIDToHealthEntity maps a check ID to the health-family entity the
// classifier uses (entityCheck in answers.go). Returns the check ID itself
// when no direct mapping exists — answerHealth falls through to runOneCheck
// with it, which is still the right answer for a context-seeded turn.
func checkIDToHealthEntity(checkID string) string {
	switch checkID {
	case "comfyui_health":
		return "comfyui"
	case "compressor_reachability":
		return "compressor"
	case "a0_reachability":
		return "a0"
	case "forge_self":
		return "forge"
	case "brain_resolvable":
		return "brain"
	case "gpu_hang":
		return "gpu"
	case "always_on_ports":
		return "embedding" // ambiguous — embedding/stt/tts/aligner all map here
	case "slot_agreement":
		return "a1" // ambiguous — a1–a4 all map here
	}
	return checkID
}

// alertCodeToEntity maps known alert/notification codes to health entities.
// Mirrors the crit notification codes in investigations.go and the collector's
// alert set. Unknown codes return ok=false (fall through to text Classify).
func alertCodeToEntity(code string) (string, bool) {
	switch code {
	case "INFERENCE_HANG":
		return "gpu", true
	case "KFD_EVICTION":
		return "gpu", true
	case "UNIT_OOM", "UNIT_CRASH":
		return "forge", true
	case "GTT_HIGH":
		return "gtt", true
	}
	return "", false
}

// match is a single family's result: the matched entity ("" = no match) plus
// the cue phrase that fired. Using a struct lets each matcher return both
// without a growing parameter list.
type match struct {
	family IntentFamily
	entity string
	phrase string
}

func noMatch() match { return match{} }

func (s *Smith) matchAction(norm string, es entitySet) match {
	// "why won't smith prune..." is a kb question, not an action command —
	// skip action when a "why" cue is present so kb can claim it.
	if hasAny(norm, "why ", "why won", "why can", "why does", "why is") {
		return noMatch()
	}
	// Action cues are imperative verbs. "run a check-up"/"run a diagnostic"
	// map to run_check_up; "restart X" maps to a restart entity; "unload"/
	// "prune"/"delete" map to their entities.
	if hasAny(norm, "restart", "start ", "stop ", "unload", "prune", "delete", "clean up", "free up", "run a diagnostic", "run a check-up", "run the quick sweep", "check everything", "run a check", "run diagnostic") {
		// Sweep/check-up phrasings → run_check_up.
		if hasAny(norm, "check-up", "diagnostic", "check everything", "quick sweep", "run a check") {
			return match{FamilyAction, "run_check_up", "run check-up"}
		}
		// Restart targets: resolve the unit/service/backend mentioned.
		for _, ea := range es.action {
			if mentionsAlias(norm, ea.aliases) {
				if hasAny(norm, "restart", "start ", "stop ") {
					return match{FamilyAction, ea.entity, "restart"}
				}
				if hasAny(norm, "unload", "free up") && strings.HasPrefix(ea.entity, "unload") {
					return match{FamilyAction, ea.entity, "unload"}
				}
				if hasAny(norm, "prune", "delete", "clean up") && strings.HasPrefix(ea.entity, "delete") {
					return match{FamilyAction, ea.entity, "delete"}
				}
			}
		}
		// "restart llama.cpp" / "restart the model server" → restart_llama.cpp
		if hasAny(norm, "llama.cpp", "llama server", "llama server", "model server", "inference server", "the llama") && hasAny(norm, "restart", "start ") {
			return match{FamilyAction, "restart_llama.cpp", "restart llama.cpp"}
		}
	}
	return noMatch()
}

func (s *Smith) matchHistory(norm string, es entitySet) match {
	if !hasAny(norm, "how long", "how often", "been happening", "come up", "fire", "firing") {
		return noMatch()
	}
	// Resolve to a known check_id entity, or the free-text fallback.
	for _, ea := range es.history {
		if ea.entity == "any_finding_by_text" {
			continue
		}
		if mentionsAlias(norm, ea.aliases) {
			return match{FamilyHistory, ea.entity, "history"}
		}
	}
	// "problem X" / "this error" → free-text history.
	if hasAny(norm, "problem", "this error", "this come up", "been happening") {
		return match{FamilyHistory, "any_finding_by_text", "free-text history"}
	}
	// "how often does this error come up?" → free-text.
	if hasAny(norm, "how often", "how long") && hasAny(norm, "this", "problem", "error") {
		return match{FamilyHistory, "any_finding_by_text", "free-text history"}
	}
	return noMatch()
}

func (s *Smith) matchLogs(norm string, es entitySet) match {
	if !hasAny(norm, "error log", "the log", "logs", "journal", "notifications", "notification", "alerts", "alert", "active alert") {
		return noMatch()
	}
	if hasAny(norm, "notification", "notifications", "alerts", "alert") && !hasAny(norm, "error log", "journal") {
		return match{FamilyLogs, "notifications", "notifications"}
	}
	return match{FamilyLogs, "forge_unit_error_digest", "error log"}
}

func (s *Smith) matchVersion(norm string, es entitySet) match {
	if !hasAny(norm, "version", "up to date", "lag its tree", "lags", "lag ") {
		return noMatch()
	}
	for _, ea := range es.version {
		if mentionsAlias(norm, ea.aliases) {
			return match{FamilyVersion, ea.entity, "version"}
		}
	}
	return noMatch()
}

func (s *Smith) matchQuantity(norm string, es entitySet) match {
	if !hasAny(norm, "how much", "how big", "free", "used", "full", "size", "available", "what the context", "what was configured", "is configured") {
		return noMatch()
	}
	// Pick the entity whose matched alias is longest, so "gpu memory" (gtt)
	// wins over bare "memory" (ram) when both appear.
	bestLen := 0
	bestEntity := ""
	bestPhrase := ""
	for _, ea := range es.quantity {
		alias := matchedAlias(norm, ea.aliases)
		if alias == "" {
			continue
		}
		if len(alias) > bestLen {
			bestLen = len(alias)
			bestEntity = ea.entity
			bestPhrase = alias
		}
	}
	if bestEntity != "" {
		return match{FamilyQuantity, bestEntity, bestPhrase}
	}
	return noMatch()
}

func (s *Smith) matchReachability(norm string, es entitySet, healthAliases map[string]bool) match {
	strong := hasAny(norm, "reachable", "address", "available via", "on the tailnet", "on tailnet", "tailnet", "able to connect", "mesh connected", "mesh", "tailscale up", "tailnet up", "reach")
	weak := hasAny(norm, "up", "connected")
	if !strong && !weak {
		return noMatch()
	}
	for _, ea := range es.reachability {
		alias := matchedAlias(norm, ea.aliases)
		if alias == "" {
			continue
		}
		// "internet"/"web" special-case (§fixture health.internet vs
		// reachability.internet): "are you able to"/"do you have"/"smith
		// reach the web" → health (handled by matchHealth); everything else
		// internet/web → reachability.
		if ea.entity == "internet" {
			if hasAny(norm, "are you able to", "do you have", "smith reach the web") {
				continue // let matchHealth claim it
			}
			return match{FamilyReachability, "internet", "reachability"}
		}
		if !strong {
			// Weak cue only: only reachability-only aliases qualify (a
			// bare "up" on a shared alias like "comfyui"/"a1" routes to
			// health, not reachability).
			if healthAliases[alias] {
				continue
			}
		}
		return match{FamilyReachability, ea.entity, "reachability"}
	}
	return noMatch()
}

func (s *Smith) matchListing(norm string, es entitySet) match {
	if !hasAny(norm, "pending", "degraded", "backlog", "queued", "queue", "blocked", "investigations", "investigation", "tasks", "what down", "what pending", "what in the", "what services", "being investigated", "proposal", "proposals", "open", "not listening", "scheduler queue") {
		return noMatch()
	}
	switch {
	case hasAny(norm, "backlog", "blocked", "externally blocked"):
		return match{FamilyListing, "backlog", "backlog"}
	case hasAny(norm, "pending task", "pending action", "proposal", "proposals", "scheduler queue", "queue", "queued", "pending smith", "what queued", "what pending", "what in the"):
		if hasAny(norm, "action", "proposal", "proposals", "smith action") {
			return match{FamilyListing, "pending_actions", "pending actions"}
		}
		return match{FamilyListing, "pending_tasks", "pending tasks"}
	case hasAny(norm, "investigation", "investigations", "being investigated"):
		return match{FamilyListing, "open_investigations", "investigations"}
	case hasAny(norm, "degraded", "what down", "what services", "not listening", "services are", "what degraded", "down"):
		return match{FamilyListing, "degraded_services", "degraded services"}
	}
	// Bare "what's pending" → pending_tasks.
	if hasAny(norm, "pending", "tasks") {
		return match{FamilyListing, "pending_tasks", "pending tasks"}
	}
	return noMatch()
}

func (s *Smith) matchKB(norm string, es entitySet) match {
	if !hasAny(norm, kbCuePhrases...) {
		return noMatch()
	}
	// Require a KB topic keyword so generic "why is the box slow?" doesn't
	// misclassify as kb (it has no KB-corpus anchor). The keywords come from
	// the fixture's kb entity set (each phrasing references one).
	if !hasAny(norm, kbTopicKeywords...) {
		return noMatch()
	}
	// Resolve the best KB chunk by keyword search (deterministic, no LLM —
	// kb.go's searchCorpus scores the embedded corpus by token overlap).
	results, err := s.KBSearch(context.Background(), norm, 1)
	if err == nil && len(results) > 0 {
		slug := kbSlugFromRef(results[0].Ref)
		if slug != "" {
			return match{FamilyKB, slug, "kb"}
		}
	}
	// Fallback: a "why" question with a topic keyword but no KB hit still
	// classifies as kb with an empty entity — Answer reports it can't find a
	// matching entry.
	if hasAny(norm, "why ", "what's the recipe", "recipe for") {
		return match{FamilyKB, "", "kb-nohit"}
	}
	return noMatch()
}

// kbCuePhrases are the question-shape cues that indicate a KB lookup (vs an
// imperative action or a status check). Curated from Sprint R's fixture
// phrasings — each kb entry's phrasing carries one of these.
var kbCuePhrases = []string{
	"why ", "what the recipe", "recipe for", "should i", "does mtp",
	"when does mtp", "is it safe", "which rocm", "why can t", "why can not",
	"why does", "why won", "why is", "what should", "what does", "does ",
	"how is", "how do", "what happened", "what local port", "what are the",
	"what the 1.2x", "what the target", "what quant", "what --parallel",
	"what llama", "vulkan or rocm", "is vulkan faster", "is x-compress",
	"is librechat a forgehost", "smith says", "is compressor a black hole",
	"what tailscale",
	// 2026-08-17 build-refresh additions: "how do I rebuild", "what's the
	// procedure", "does this model need a fork", "is X supported".
	"how do i", "how do you", "what the procedure", "what's the procedure",
	"does this model", "does this need", "is it required", "is a fork",
	"supported by", "is .* mainline", "what's the recipe", "the recipe",
}

// kbTopicKeywords anchor a "why" question to the KB corpus — drawn from the
// fixture's kb entity phrasings (context, routing/gtt, compressor, slot, hang,
// mtp, quant, vulkan/rocm/vllm, mesh, prune, etc.). A "why" question without
// one of these is treated as no_match (it's open-ended, not a KB lookup).
var kbTopicKeywords = []string{
	"context", "n_ctx", "n ctx", "routing", "black hole", "black-hole", "502",
	"gtt", "rocm", "vllm", "hipmalloc", "mtp", "quant", "vram", "1.2x",
	"compressor", "orphaned", "gpu pid", "hang", "timeoutstopsec", "restart-loop",
	"vulkan", "nemotron", "backend", "reprocess", "ornith", "qwen36",
	"curl", "localhost:5000", "200ms", "dashboard hang", "debugging",
	"tailscale", "mesh", "svc", "librechat", "prune", "guardrail",
	"parallel", "ctx-checkpoints", "mmproj", "runtime flag", "--parallel",
	"ssrf", "compressor topology", "ldd", "systemd", "hsa", "sdma",
	"slot rename", "primary", "secondary", "400", 	"llama.cpp", "llamacpp", "llama cpp",
	"build", "model-selection", "comfortable", "context split", "strix halo",
	"pruning", "guardrails", "quantization", "quantization level", "slot",
	"multiply", "split it",
	// 2026-08-17 build-refresh additions: upstream drift + fork-support.
	"rebuild", "rebuilding", "kintsugi", "puzzle", "fork", "upstream",
	"refresh", "build refresh", "newer build", "rebase", "expert_used_count",
	"mainline",
}

func (s *Smith) matchHealth(norm string, es entitySet) match {
	// "internet"/"web" special-case: "are you able to reach the internet?",
	// "do you have internet?", "can smith reach the web?" → health.internet.
	if hasAny(norm, "internet", "the web") {
		if hasAny(norm, "are you able to", "do you have", "smith reach the web") {
			return match{FamilyHealth, "internet", "internet-health"}
		}
	}
	if !hasAny(norm, "healthy", "up", "responding", "working", "ok", "alright", "fine", "status of", "health", "resolvable", "configured", "hang", "hang indicators", "loaded") {
		return noMatch()
	}
	for _, ea := range es.health {
		if mentionsAlias(norm, ea.aliases) {
			return match{FamilyHealth, ea.entity, "health"}
		}
	}
	return noMatch()
}

// ── entity collection from live config ───────────────────────────────────

// knownEntities builds the per-family entity lists from live config/catalog
// at classify time. Static entities (probe names, aliases) are curated here;
// dynamic entities (check IDs, tracked binaries, mesh services, slots,
// service ports) are read from the wired seams so a newly tracked binary or
// mesh service becomes askable with zero classifier changes (§2.7).
func (s *Smith) knownEntities(ctx context.Context) entitySet {
	es := entitySet{
		health:       defaultHealthEntities(),
		version:      defaultVersionEntities(),
		quantity:     defaultQuantityEntities(),
		reachability: defaultReachabilityEntities(),
		listing:      defaultListingEntities(),
		logs:         defaultLogsEntities(),
		action:       defaultActionEntities(),
	}

	// History entities = every registered check ID (the family instantiates
	// over EVERY check that has ever fired, §2.7) + the free-text fallback.
	hist := make([]entityAlias, 0, len(registry)+1)
	for _, c := range registry {
		hist = append(hist, entityAlias{
			entity:  c.ID,
			aliases: checkIDAliases(c.ID, c.Name),
		})
	}
	hist = append(hist, entityAlias{entity: "any_finding_by_text", aliases: []string{"problem x", "this error", "problem"}})
	es.history = hist

	// Version entities: tracked binaries from settings (dynamic) overlay the
	// curated defaults — a newly tracked binary is askable immediately.
	if tracked := s.TrackedBinaries(ctx); len(tracked) > 0 {
		vs := defaultVersionEntities()
		seen := map[string]bool{}
		for _, ea := range vs {
			for _, a := range ea.aliases {
				seen[a] = true
			}
		}
		for _, b := range tracked {
			name := strings.ToLower(b.Name)
			if name == "" {
				continue
			}
			if !seen[name] {
				vs = append(vs, entityAlias{entity: b.Name, aliases: []string{name}})
			}
		}
		es.version = vs
	}

	// Reachability entities: the mesh inventory from settings comes FIRST,
	// then the code-curated live probes — a newly registered mesh service
	// becomes askable immediately (§2.7 "derived, not enumerated";
	// open-source-readiness finding 1: the mesh map is deployment data,
	// never compiled-in). Ordering matters: the probes' broad aliases
	// ("tailnet", "internet") appear in most reachability questions, so the
	// specific mesh entities must get first match, with the probes as the
	// fallback. Entries without a name or aliases are skipped, and
	// duplicate names keep the first entry.
	seenReach := map[string]bool{}
	reach := make([]entityAlias, 0, len(es.reachability))
	for _, svc := range s.MeshServices(ctx) {
		if svc.Name == "" || len(svc.Aliases) == 0 || seenReach[svc.Name] {
			continue
		}
		seenReach[svc.Name] = true
		reach = append(reach, entityAlias{entity: svc.Name, aliases: svc.Aliases})
	}
	for _, ea := range es.reachability {
		if seenReach[ea.entity] {
			continue
		}
		seenReach[ea.entity] = true
		reach = append(reach, ea)
	}
	es.reachability = reach

	// KB entities: the embedded corpus slugs (each entry implies its
	// questions, §2.7). The matcher resolves via KBSearch at match time, so
	// no static list is needed here — but keep a non-nil slice for clarity.
	es.kb = []entityAlias{{entity: "*", aliases: []string{"why"}}}

	// Slot labels from the scheduler: a1–a4 (and any future slot) are
	// askable for health/reachability. Slots already in the curated lists
	// are not duplicated.
	if s.d.Sched != nil {
		st := s.d.Sched.Status()
		for slot := range st.Slots {
			alias := strings.ToLower(slot)
			if !aliasInList(es.health, alias) {
				es.health = append(es.health, entityAlias{entity: alias, aliases: []string{alias}})
			}
			if !aliasInList(es.reachability, alias) {
				es.reachability = append(es.reachability, entityAlias{entity: alias, aliases: []string{alias}})
			}
		}
	}

	// Service ports from config: each cfg.Ports key (embedding, stt, tts,
	// aligner, …) is an askable health entity. Already-curated ones are not
	// duplicated.
	if cfg := s.liveCfg(); cfg != nil {
		for name := range cfg.Ports {
			alias := strings.ToLower(name)
			if !aliasInList(es.health, alias) {
				es.health = append(es.health, entityAlias{entity: alias, aliases: []string{alias}})
			}
		}
		// TTS unit from cfg.Server.TTSUnit → "tts" health entity already
		// curated; nothing to add unless the unit name differs.
	}

	return es
}

// liveCfg returns the infra config or nil when no Cfg func is wired.
func (s *Smith) liveCfg() *config.Config {
	if s.d.Cfg == nil {
		return nil
	}
	return s.d.Cfg()
}

// aliasInList reports whether alias already appears in the entity list.
func aliasInList(list []entityAlias, alias string) bool {
	for _, ea := range list {
		for _, a := range ea.aliases {
			if a == alias {
				return true
			}
		}
	}
	return false
}

// ── curated default entity lists (static aliases from the KB corpus + ─────
// checks.go registry + propose.go servicePortUnit + execute.go restartAllowed)

func defaultHealthEntities() []entityAlias {
	return []entityAlias{
		{entity: "comfyui", aliases: []string{"comfyui", "comfy"}},
		{entity: "compressor", aliases: []string{"compressor", "compressor proxy", "the proxies"}},
		{entity: "a0", aliases: []string{"a0", "router", "the router"}},
		{entity: "a1", aliases: []string{"a1", "primary", "the primary"}},
		{entity: "a2", aliases: []string{"a2", "secondary", "the secondary"}},
		{entity: "a3", aliases: []string{"a3"}},
		{entity: "a4", aliases: []string{"a4"}},
		{entity: "embedding", aliases: []string{"embedding", "embeddings"}},
		{entity: "stt", aliases: []string{"stt", "speech-to-text", "speech to text", "transcription"}},
		{entity: "tts", aliases: []string{"tts", "text-to-speech", "text to speech"}},
		{entity: "aligner", aliases: []string{"aligner", "the aligner"}},
		{entity: "forge", aliases: []string{"forge", "daemon", "the daemon", "the db", "db ok"}},
		{entity: "brain", aliases: []string{"brain", "smith's brain", "reasoning brain", "a model loaded", "model loaded for smith"}},
		{entity: "gpu", aliases: []string{"gpu", "the gpu", "hang indicators", "gpu hang"}},
		{entity: "search", aliases: []string{"search", "web search"}},
		{entity: "internet", aliases: []string{"internet", "the web"}},
	}
}

func defaultVersionEntities() []entityAlias {
	return []entityAlias{
		{entity: "llama.cpp-vulkan", aliases: []string{"llama.cpp", "llama cpp", "vulkan build", "vulkan"}},
		{entity: "llama.cpp-puzzle", aliases: []string{"puzzle build", "puzzle"}},
		{entity: "llama.cpp-kintsugi", aliases: []string{"kintsugi build", "kintsugi"}},
		{entity: "llama.cpp-poolside", aliases: []string{"poolside build", "poolside"}},
		{entity: "headroom-ai", aliases: []string{"headroom-ai", "compressor ai", "compressor"}},
		{entity: "tailscale", aliases: []string{"tailscale", "tailnet"}},
		{entity: "comfyui", aliases: []string{"comfyui", "comfy"}},
		{entity: "forge", aliases: []string{"forge", "daemon"}},
	}
}

func defaultQuantityEntities() []entityAlias {
	return []entityAlias{
		{entity: "ram", aliases: []string{"ram", "memory"}},
		{entity: "gtt", aliases: []string{"gtt", "gpu memory"}},
		{entity: "disk", aliases: []string{"disk", "disk space", "storage"}},
		{entity: "n_ctx", aliases: []string{"n_ctx", "n ctx", "context size", "the context", "context"}},
	}
}

// defaultReachabilityEntities are the code-curated reachability entities —
// ONLY the live probes, which behave identically on every deployment. The
// mesh service inventory is deployment data and lives in smith.mesh.services;
// knownEntities overlays it live (open-source-readiness finding 1).
func defaultReachabilityEntities() []entityAlias {
	return []entityAlias{
		{entity: "internet", aliases: []string{"internet", "the web"}},
		{entity: "tailnet", aliases: []string{"tailnet", "tailscale", "mesh"}},
	}
}

func defaultListingEntities() []entityAlias {
	return []entityAlias{
		{entity: "pending_tasks", aliases: []string{"pending task", "scheduler queue", "queued", "what's queued"}},
		{entity: "pending_actions", aliases: []string{"pending smith action", "pending action", "proposal", "proposals"}},
		{entity: "open_investigations", aliases: []string{"investigation", "investigations", "being investigated"}},
		{entity: "backlog", aliases: []string{"backlog", "blocked", "externally blocked"}},
		{entity: "degraded_services", aliases: []string{"degraded", "what's down", "not listening", "services are down"}},
	}
}

func defaultLogsEntities() []entityAlias {
	return []entityAlias{
		{entity: "forge_unit_error_digest", aliases: []string{"error log", "journal", "logs"}},
		{entity: "notifications", aliases: []string{"notification", "notifications", "alert", "alerts"}},
	}
}

func defaultActionEntities() []entityAlias {
	return []entityAlias{
		{entity: "restart_forge-stt", aliases: []string{"stt", "forge-stt", "speech-to-text", "speech to text"}},
		{entity: "restart_forge-embedding", aliases: []string{"embedding", "forge-embedding"}},
		{entity: "restart_forge-aligner", aliases: []string{"aligner", "forge-aligner"}},
		{entity: "restart_comfyui", aliases: []string{"comfyui", "ai-mode-comfyui", "comfy"}},
		{entity: "restart_tts", aliases: []string{"tts", "forge-tts", "text-to-speech", "text to speech"}},
		{entity: "restart_compressor", aliases: []string{"compressor", "compressor proxy", "headroom@local"}},
		{entity: "restart_llama.cpp", aliases: []string{"llama.cpp", "model server", "inference server", "the llama server"}},
		{entity: "unload_slot", aliases: []string{"slot a2", "the secondary", "a slot", "free up a slot"}},
		{entity: "delete_comfyui_files", aliases: []string{"comfyui model", "unreferenced comfyui", "comfyui files"}},
		// Known refusal entities (answerable=false in the fixture) — the
		// classifier matches them so Answer can refuse with the plain-
		// language reason from execute.go's restartAllowed.
		{entity: "restart_forge-daemon", aliases: []string{"forge", "forge-daemon", "the daemon"}},
		{entity: "restart_slot_unit", aliases: []string{"forge-a1", "forge-a2", "forge-a3", "forge-a4", "slot unit"}},
	}
}

// ── text helpers ─────────────────────────────────────────────────────────

// normalizeClassifyText lowercases, trims, and collapses punctuation that
// would otherwise break whole-word alias matching (apostrophes in "a0's",
// question marks, commas).
func normalizeClassifyText(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.ReplaceAll(t, "'s", " ")
	t = strings.ReplaceAll(t, "'", " ")
	t = strings.ReplaceAll(t, "?", " ? ")
	t = strings.ReplaceAll(t, ",", " , ")
	t = strings.ReplaceAll(t, "!", " ! ")
	t = strings.ReplaceAll(t, ":", " ")
	t = strings.ReplaceAll(t, "/", " ")
	t = strings.ReplaceAll(t, "_", " ")
	return strings.Join(strings.Fields(t), " ")
}

// hasAny reports whether norm contains any of the phrases as whole-word-
// aware substrings. A phrase with spaces is matched as-is; a single token is
// matched as a whole word (bounded by non-alphanumeric) to avoid "ram"
// matching "parameter".
func hasAny(norm string, phrases ...string) bool {
	for _, p := range phrases {
		if phraseInText(norm, p) {
			return true
		}
	}
	return false
}

func phraseInText(norm, phrase string) bool {
	phrase = strings.ToLower(strings.TrimSpace(phrase))
	if phrase == "" {
		return false
	}
	if !strings.Contains(phrase, " ") && !strings.ContainsAny(phrase, "-._") {
		// Single token: whole-word match.
		for _, w := range strings.Fields(norm) {
			if w == phrase {
				return true
			}
		}
		return false
	}
	return strings.Contains(norm, phrase)
}

// mentionsAlias reports whether norm contains any of the aliases.
func mentionsAlias(norm string, aliases []string) bool {
	return hasAny(norm, aliases...)
}

// matchedAlias returns the first alias in the list that appears in norm, or "".
func matchedAlias(norm string, aliases []string) string {
	for _, a := range aliases {
		if phraseInText(norm, a) {
			return a
		}
	}
	return ""
}

// checkIDAliases produces surface aliases for a check ID — the ID itself
// (with underscores → spaces) and the check's display name, lowercased.
func checkIDAliases(checkID, name string) []string {
	id := strings.ToLower(checkID)
	aliases := []string{id, strings.ReplaceAll(id, "_", " ")}
	if name != "" {
		aliases = append(aliases, strings.ToLower(name))
	}
	return aliases
}

// kbSlugFromRef extracts the slug portion from a KBRef like
// "pitfalls:silent-context-reduction" → "silent-context-reduction".
func kbSlugFromRef(ref string) string {
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}
