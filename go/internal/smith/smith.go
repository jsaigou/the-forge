// SPDX-License-Identifier: Apache-2.0

// Package smith implements the Forge self-diagnosis & operational agent
// (docs/v5-smith.md). It lives inside forge and gives the daemon
// self-diagnosis, host awareness, and (in later phases) supervised
// operational execution.
//
// This wave ships P0 (contracts: tables, settings keys, nil-tolerant Deps)
// + P1 (the deterministic core): a registry of coded checks that are pure
// reads over the collector Snapshot + scheduler Status + catalog, findings
// persistence, quick/deep sweeps, and the SelfContext behind
// GET /api/v1/smith/status.
//
// P1 is the DETERMINISTIC tier: there is no LLM / a0 chat-completion call
// anywhere in this package. Tier 2 (the reasoning brain, docs/v5-smith.md
// §4.3) lands in a later phase; everything here degrades cleanly when no
// brain model is resolvable (that is the deterministic_only mode, not an
// error).
package smith

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith/comfyui"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/smith/web"
	"github.com/jsaigou/the-forge/internal/store"
)

// Settings keys (docs/v5-smith.md §5). Values are raw JSON in the settings
// KV, same posture as every other settings key.
const (
	// SettingModel is the Tier 2 brain selector: a local Config name or an
	// Offering's wire_model. Seeded once by migration 0033 ("qwen36-mtp");
	// empty/unresolvable ⇒ deterministic-only mode. Never a hardcoded
	// default in code (§4.3, decision-log item 2).
	SettingModel = "smith.model"

	// SettingSchedule is the periodic sweep schedule:
	// {"quick":"60m","deep":"24h","enabled":true}. Missing/invalid values
	// fall back to those defaults (both sweeps disable-able via enabled).
	SettingSchedule = "smith.schedule"
	// SettingTurnBudget is S4's per-tier wall-clock budgets (JSON:
	// smith.TurnBudget). Empty/absent → DefaultTurnBudget; the reader
	// falls back field-by-field so a partial object never zeroes a budget.
	SettingTurnBudget = "smith.turn_budget"
	// SettingBrainChain is S5's ordered fallback list of acceptable local
	// brain configs (JSON array of config names). Empty/absent → behavior
	// unchanged (smith.model alone). When provisioned: a chain member
	// already loaded AND idle answers with zero load wait; otherwise
	// chain[0] is the load target. User-editable per the operator decision
	// recorded 2026-08-21.
	SettingBrainChain = "smith.brain_chain"

	// SettingThresholds tunes operator-facing check thresholds:
	// {"gtt_warn_pct":85,"gtt_crit_pct":95,"disk_warn_pct":85,"disk_crit_pct":95}.
	// Missing/invalid values fall back to those defaults.
	SettingThresholds = "smith.thresholds"

	// SettingHandoffOfferings is the ordered list of Offering IDs the P3
	// remote brain-swap probe will try, most-preferred first
	// (docs/v5-smith.md §4.5). P2 never reads this (no remote probing yet —
	// candidates stays frozen at []); it is on the settings_change
	// allowlist now so the key exists and is editable ahead of P3, per
	// "no hardcoded default anywhere in code" (decision-log item 2).
	SettingHandoffOfferings = "smith.handoff_offerings"

	// SettingTools is the P7 read-only tool-loop config:
	// {"enabled":true,"mode":"auto","max_rounds":6}. Grouped, matching the
	// real precedent (smith.schedule/smith.thresholds/smith.web) rather
	// than a lone scalar — never seeded by migration, code Default*() only,
	// same posture as smith.schedule/smith.thresholds.
	SettingTools = "smith.tools"

	// SettingRetention is the P7 findings+web-cache retention config. Never
	// seeded by migration, code Default*() only.
	SettingRetention = "smith.retention"

	// SettingSelfReview is the periodic self-review sweep's config
	// (self_review.go, Thread C — docs/v5-smith-efficiency.md §4):
	// {"enabled":true,"grace_minutes":30}. The sweep interval itself
	// (selfReviewInterval) is a fixed constant, not settings-driven — only
	// enabled/grace_minutes are operator-tunable, same posture as
	// retentionInterval vs. smith.retention. Never seeded by migration, code
	// Default*() only.
	SettingSelfReview = "smith.self_review"

	// SettingBrainResidency ("smith.brain_residency", {"stay_resident":bool})
	// is the operator's opt-in "always keep the brain loaded" choice
	// (brain_residency.go). Default false — a resolvable-but-unloaded
	// brain is NOT kept loaded by default; it is loaded on demand, only
	// when a turn actually escalates to the reasoning tier
	// (ensureBrainLoaded, called from decideTier), and left to unload per
	// normal idle policy afterward. Never seeded by migration, code
	// Default*() only, same posture as smith.self_review/smith.retention.
	SettingBrainResidency = "smith.brain_residency"

	// SettingAutoRecoverDeviceLost is the auto-recovery kill switch
	// ("smith.auto_recover_device_lost", JSON bool). When true (default),
	// a journal-confirmed GPU device-lost on a loaded slot triggers an
	// automatic unload→reload of the same mode — the recovery is trivial
	// and programmatic; the only "intelligent" part is escalation (propose
	// a different brain/mode) if the model refuses to come back. Reads as a
	// JSON bool via settings KV. Never seeded by migration, code default
	// only, same posture as the other smith.* toggles.
	SettingAutoRecoverDeviceLost = "smith.auto_recover_device_lost"

	// SettingAutoHandoffCloud is the graceful-failover toggle
	// ("smith.auto_handoff_cloud", JSON bool). When true (default), a
	// crash-looping local brain (device-lost that recurs within the
	// auto-recover cooldown, or a reload failure) triggers an automatic
	// swap of smith.model to the first healthy remote offering
	// (smith.handoff_offerings) so the reasoning tier keeps working during
	// the outage. When false, escalation degrades to an operator-runbook
	// proposal only. The swap is audited and hand-reversible.
	SettingAutoHandoffCloud = "smith.auto_handoff_cloud"
)

// Domain module settings keys (docs/v5-smith.md §4.9, P6). Seeded once by
// migration 0038 with ForgeHost's real ground-truthed values (comfyui's two
// model roots, the real llama.cpp/compressor paths, the two infra-critical
// tailnet peers) — never a hardcoded default in code, same convention as
// SettingModel.
const (
	SettingComfyUIEnabled      = "smith.comfyui.enabled"
	SettingComfyUIURL          = "smith.comfyui.url"
	SettingComfyUIUnit         = "smith.comfyui.unit"
	SettingComfyUIModelRoots   = "smith.comfyui.model_roots"
	SettingComfyUIWorkflowDirs = "smith.comfyui.workflow_dirs"
	// SettingComfyUIKeepFiles is the operator-maintained exclusion list
	// (JSON array of full paths) proposeComfyUIDelete filters candidates
	// against before building a delete_files proposal — a direct "don't
	// propose deleting this" that doesn't require gaming ComfyUI's own
	// workflow system the way comfyUIKeepGuidance's advice does (S7-followup
	// smith UX sprint, 2026-08-26: comfyui_prune used to re-propose the same
	// rejected files every sweep with no memory of the rejection).
	SettingComfyUIKeepFiles = "smith.comfyui.keep_files"

	SettingBinariesEnabled = "smith.binaries.enabled"
	SettingBinariesTracked = "smith.binaries.tracked"

	SettingTailscaleWatchPeers = "smith.tailscale.watch_peers"

	// SettingMeshServices is the reachability family's mesh inventory
	// ("smith.mesh.services", JSON array of MeshService). Seeded once by
	// migration 0060 with ForgeHost's real values; operator-edited per
	// deployment thereafter (open-source-readiness finding 1 — the
	// inventory is deployment data, never compiled-in defaults).
	SettingMeshServices = "smith.mesh.services"

	// SettingBuildRefreshForks is the build_refresh procedure's fork
	// registry ("smith.build_refresh.forks", JSON array of
	// buildRefreshFork). Seeded once by migration 0061 with the
	// code-reviewed entries; operator-edited per deployment thereafter
	// (open-source-readiness finding 4 — fork recipes are deployment
	// data; the procedure's fail-closed cross-checks apply to whatever
	// the setting says).
	SettingBuildRefreshForks = "smith.build_refresh.forks"

	// SettingBuildRefreshWatchlist is the operator-editable keyword list
	// ("smith.build_refresh.watchlist", JSON array of strings) that
	// binary_versions matches against fetched upstream commit subjects
	// (Deps.GitBehindLog) — surfaces a drifted build that mentions e.g.
	// "CVE" or "breaking" regardless of whether the raw commit count has
	// crossed BuildRefreshBehindN. Visibility only: a watchlist hit adds a
	// distinct evidence/summary line, it does NOT by itself widen
	// proposeRebuildRunbook's threshold gate (free-form investigation-
	// matching that would auto-propose was already rejected in favor of
	// this narrower, explicit, operator-maintained list — see
	// docs/v5-ops-sprints-2026-08-21.md's cross-sprint notes). Empty/absent
	// → no matching happens, same as before this setting existed. Smith
	// itself may append to this list (auto-populated from failed model-add
	// compatibility failures, per the original design) — see
	// allowedSmithSettingsKeys in execute.go.
	SettingBuildRefreshWatchlist = "smith.build_refresh.watchlist"

	// SettingFailOpenBudgetMS (Sprint D) is the per-proxy fail-open budget
	// override (JSON integer, milliseconds). Written by smith's
	// settings_change apply path; read by compressorctl's writeEnv.
	SettingFailOpenBudgetMS = "smith.failopen_budget_ms"
)

// Brain resolution enum (docs/v5-smith.md §4.1).
const (
	BrainLocalSlot         = "local_slot"         // a local Config, currently loaded on a slot
	BrainRemote            = "remote"             // an Offering wire_model (remote provider)
	BrainDeterministicOnly = "deterministic_only" // nothing resolvable (or not loaded)
)

// Tier is the reasoning tier the daemon is currently operating in.
// TierDeterministic: no LLM anywhere, Tier 1 checks/findings only.
// TierReasoning (P3): a brain is resolvable (Brain().Resolution !=
// BrainDeterministicOnly) and a0 answered the last liveness probe — chat
// escalates to Tier 2. SelfContext.Tier reflects this per-request; a
// conversation's OWN tier (smith_conversations.tier) can lag behind it
// after a mid-stream degrade (docs/v5-smith.md §4.3 "never lose the
// transcript") — the conversation keeps recording deterministic until its
// next successful reasoning turn, rather than snapping back automatically.
const (
	TierDeterministic = "deterministic"
	TierReasoning     = "reasoning"
)

// Severity is a finding's severity. Ordered — Rank() gives the comparable
// value (crit > warn > info > ok).
type Severity string

const (
	SeverityOK   Severity = "ok"
	SeverityInfo Severity = "info"
	SeverityWarn Severity = "warn"
	SeverityCrit Severity = "crit"
)

// Rank orders severities for aggregation (higher = worse).
func (s Severity) Rank() int {
	switch s {
	case SeverityCrit:
		return 3
	case SeverityWarn:
		return 2
	case SeverityInfo:
		return 1
	default:
		return 0
	}
}

// Confidence expresses how complete the evidence behind a Finding is (Tier 1
// Sprint 4) — derived from what the check actually managed to read, never a
// model's self-assessment (there is no LLM in the deterministic check path
// at all). High: every probe the check wanted succeeded. Medium/Low mean a
// probe was unavailable, degraded, or fell back to a weaker source, and
// ConfidenceNote names which one. A check that never sets it defaults to
// High via normalize() — every check written before this sprint always read
// what it needed or errored out entirely (SeverityCrit via runOne's panic
// recovery), so "unset" and "fully confident" describe the same prior
// behavior.
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Rank orders confidence for display/sorting (higher = more confident).
func (c Confidence) Rank() int {
	switch c {
	case ConfidenceHigh:
		return 2
	case ConfidenceMedium:
		return 1
	default: // ConfidenceLow, or unset
		return 0
	}
}

// Finding is one check's outcome (docs/v5-smith.md §4.2). Evidence carries
// structured data for the UI's expandable detail and (from a later phase)
// for Tier 2 context assembly. ProposalIDs is populated by proposeFrom
// (propose.go) after a sweep's checks run — one smith_actions row ID per
// auto-proposal this finding produced, [] when the check has no proposer or
// its gate didn't fire.
//
// []int64, not []string (P2 action-model type fix): P1 shipped this as an
// always-empty []string hook with no real IDs ever assigned, so widening the
// element type to match Action.ID is a clean fix, not a breaking change to
// any real behavior.
type Finding struct {
	CheckID     string         `json:"check_id"`
	Severity    Severity       `json:"severity"`
	Summary     string         `json:"summary"`
	Evidence    map[string]any `json:"evidence"`
	ProposalIDs []int64        `json:"proposal_ids"`
	KBRefs      []string       `json:"kb_refs"`
	// Confidence/ConfidenceNote (Tier 1 Sprint 4) — see Confidence's doc
	// comment. Confidence defaults to High in normalize() when a check
	// leaves it unset.
	Confidence     Confidence `json:"confidence"`
	ConfidenceNote string     `json:"confidence_note,omitempty"`
}

// normalize returns the finding with non-nil slice/map fields so its JSON is
// stable ([] and {} rather than null) — the FE iterates these without
// guarding for null.
func (f Finding) normalize() Finding {
	if f.Evidence == nil {
		f.Evidence = map[string]any{}
	}
	if f.ProposalIDs == nil {
		f.ProposalIDs = []int64{}
	}
	if f.KBRefs == nil {
		f.KBRefs = []string{}
	}
	if f.Confidence == "" {
		f.Confidence = ConfidenceHigh
	}
	return f
}

// StoredFinding is a persisted finding row (smith_findings). KBRefs
// (migration 0036) mirrors Finding.KBRefs — added after the fact because
// the original 0033 schema never persisted it, silently dropping every
// check's kb_refs on the way from a fresh sweep into GET /findings and an
// investigation's findings trail (the only places findings actually
// render).
type StoredFinding struct {
	ID              int64     `json:"id"`
	InvestigationID *int64    `json:"investigation_id"` // null = standalone
	CheckID         string    `json:"check_id"`
	Severity        Severity  `json:"severity"`
	Summary         string    `json:"summary"`
	Evidence        string    `json:"evidence"` // raw JSON text
	SweepKind       string    `json:"sweep_kind"`
	CreatedAt       time.Time `json:"created_at"`
	KBRefs          []string  `json:"kb_refs"`
	RepeatCount     int       `json:"repeat_count"` // >1 when dedup collapsed repeat crits (migration 0045)
	// Confidence/ConfidenceNote (migration <next>, Tier 1 Sprint 4) mirror
	// Finding's fields of the same name.
	Confidence     Confidence `json:"confidence"`
	ConfidenceNote string     `json:"confidence_note,omitempty"`
}

// BrainResolution is where smith's own inference would run (docs/v5-smith.md
// §4.1). Resolution is one of the Brain* constants; Slot is set for
// BrainLocalSlot, Provider for BrainRemote. Detail always carries a human
// explanation (rendered verbatim by the FE's smith-state chip).
type BrainResolution struct {
	Resolution string `json:"resolution"`
	Model      string `json:"model,omitempty"`
	Slot       string `json:"slot,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Detail     string `json:"detail"`
}

// MetricsSummary is the host-telemetry slice of SelfContext — the subset of
// collector.Metrics smith surfaces. Defined here (not an embed of the frozen
// collector type) so the wire keys are explicit snake_case.
type MetricsSummary struct {
	Mode              string   `json:"mode"`
	MemTotalBytes     int64    `json:"mem_total_bytes"`
	MemUsedBytes      int64    `json:"mem_used_bytes"`
	MemAvailBytes     int64    `json:"mem_avail_bytes"`
	MemPct            float64  `json:"mem_pct"`
	GTTUsedBytes      *int64   `json:"gtt_used_bytes"`
	GTTTotalBytes     *int64   `json:"gtt_total_bytes"`
	GPUUsePct         *float64 `json:"gpu_use_pct"`
	TempCelsius       *float64 `json:"temp_celsius"`
	UptimeSeconds     *int64   `json:"uptime_seconds"`
	PackagePowerW     *float64 `json:"package_power_w"`
	DiskTotalBytes    int64    `json:"disk_total_bytes"`
	DiskFreeBytes     int64    `json:"disk_free_bytes"`
	DiskUsedBytes     int64    `json:"disk_used_bytes"`
	DiskPct           float64  `json:"disk_pct"`
	InferenceRSSBytes *int64   `json:"inference_rss_bytes"`
}

// AlertInfo is one active collector alert (hang detection, GTT warning,
// unit OOM/crash). Mirrors collector.Alert with snake_case keys.
type AlertInfo struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Port int    `json:"port,omitempty"`
	Unit string `json:"unit,omitempty"`
}

// BudgetSummary mirrors sched.Budget with snake_case keys.
type BudgetSummary struct {
	TotalBytes int64 `json:"total_bytes"`
	UsedBytes  int64 `json:"used_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
}

// SlotAllocation is one slot's occupancy as the scheduler + collector see it.
type SlotAllocation struct {
	Mode        string   `json:"mode"`
	Label       string   `json:"label"`
	MemoryBytes int64    `json:"memory_bytes"`
	IdleSeconds *float64 `json:"idle_seconds"`
}

// Schedule is the periodic sweep cadence (smith.schedule). Quick/Deep are
// duration strings ("60m", "24h") so they round-trip through the settings KV
// exactly as the operator typed them.
type Schedule struct {
	Quick   string `json:"quick"`
	Deep    string `json:"deep"`
	Enabled bool   `json:"enabled"`
}

// DefaultSchedule is used when smith.schedule is unset or unreadable.
func DefaultSchedule() Schedule {
	return Schedule{Quick: "60m", Deep: "24h", Enabled: true}
}

// TurnBudget is S4's per-tier wall-clock ceilings (smith.turn_budget), the
// settings-backed form of the old hard-coded 480s turn / 360s round
// constants. The operator's bar: a 10-minute silent startup is "a complete
// abject failure" — structurally impossible when a first turn cannot run
// past FirstTurnS even if every stage is slow. An explicitly escalated turn
// (escalate/web/no-match) earns more, still bounded. RoundTimeoutS caps one
// streaming round; it is clamped to the enclosing turn budget at use time,
// so an operator setting it above the tier budget cannot reintroduce a
// runaway.
type TurnBudget struct {
	FirstTurnS    int `json:"first_turn_s"`
	EscalationS   int `json:"escalation_s"`
	RoundTimeoutS int `json:"round_timeout_s"`
}

// DefaultTurnBudget returns the S4 defaults: a first answer is bounded to
// 2.5 minutes wall (a cold brain load ~90s + prefill + one short round fits),
// an explicit escalation gets 5, and no single round may stream past 4
// minutes of its own.
func DefaultTurnBudget() TurnBudget {
	return TurnBudget{FirstTurnS: 150, EscalationS: 300, RoundTimeoutS: 240}
}

// Thresholds are the operator-tunable check thresholds (smith.thresholds).
type Thresholds struct {
	GTTWarnPct  float64 `json:"gtt_warn_pct"`
	GTTCritPct  float64 `json:"gtt_crit_pct"`
	DiskWarnPct float64 `json:"disk_warn_pct"`
	DiskCritPct float64 `json:"disk_crit_pct"`
	// DeviceLostWindowMinutes bounds how far back gpu_device_lost's journal
	// reads reach via --since. Without a window a resolved incident's
	// journal lines read as a fresh crit forever, which is what made
	// autorecover's confirmation gate unable to tell a live device-lost
	// from a stale one (found live 2026-08-18).
	DeviceLostWindowMinutes int `json:"device_lost_window_minutes"`

	// Compressor* (Sprint 4, resource bounding + monitoring) tune
	// ClassifyCompressorHealth — shared by the compressor_health check and
	// the Dashboard's Compressor tile (httpapi.compressorServiceRows), so both
	// read the same judgment rather than two independently-tuned
	// heuristics drifting apart. Deliberately slope-based, not an absolute
	// RSS ceiling: there is no known-good baseline for forge-compress,
	// and the failure this defends against (the 0.35.0 --lossless leak)
	// presented as a healthy, serving process the whole time — see
	// docs/v5-headroom-replacement.md Sprint 4. Starting values, not
	// validated against real long-run data yet.
	CompressorRSSWindowHours      float64 `json:"compressor_rss_window_hours"`
	CompressorRSSGrowthWarnPct    float64 `json:"compressor_rss_growth_warn_pct"`
	CompressorRestartsWarnPerHour float64 `json:"compressor_restarts_warn_per_hour"`

	// BuildRefreshBehindN (S6, feedback F1): how far behind upstream a
	// tracked build must drift before the runbook suggestion fires.
	// Operator feedback: the old any-drift-fires behavior spammed
	// suggestions at single-digit commit distance. Below-threshold drift
	// stays VISIBLE as an info finding — it just doesn't propose a rebuild.
	// Default 500 per operator instruction.
	BuildRefreshBehindN int `json:"build_refresh_behind_n"`

	// CompressorFailOpenWarnPct (Sprint D) is the fail-open rate threshold
	// (%) above which smith warns and proposes raising the fail-open budget.
	// Fail-open rate = fail_open_total / (requests + timeout + canceled)
	// over a 1-hour rolling window. Default 10%.
	CompressorFailOpenWarnPct float64 `json:"compressor_failopen_warn_pct"`
}

// DefaultThresholds is used when smith.thresholds is unset or unreadable.
func DefaultThresholds() Thresholds {
	return Thresholds{
		GTTWarnPct: 85, GTTCritPct: 95, DiskWarnPct: 85, DiskCritPct: 95, DeviceLostWindowMinutes: 15,
		CompressorRSSWindowHours: 6, CompressorRSSGrowthWarnPct: 40, CompressorRestartsWarnPerHour: 3,
		BuildRefreshBehindN:         500,
		CompressorFailOpenWarnPct: 10,
	}
}

// SelfContext is the host+self awareness snapshot behind
// GET /api/v1/smith/status (docs/v5-smith.md §4.1): host telemetry summary,
// slot allocations, memory budget, and the brain resolution. It is the
// deterministic-tier picture — no LLM is involved in assembling it.
type SelfContext struct {
	Hostname        string                    `json:"hostname"`
	SnapshotTakenAt int64                     `json:"snapshot_taken_at"` // unix seconds; 0 = no snapshot
	SnapshotAgeS    float64                   `json:"snapshot_age_s"`
	Metrics         *MetricsSummary           `json:"metrics"` // nil = no snapshot yet
	Alerts          []AlertInfo               `json:"alerts"`
	Slots           map[string]SlotAllocation `json:"slots"`
	MemoryBudget    BudgetSummary             `json:"memory_budget"`
	Brain           BrainResolution           `json:"brain"`
	Tier            string                    `json:"tier"` // always "deterministic" in P1
	CheckCount      int                       `json:"check_count"`
	FastCheckCount  int                       `json:"fast_check_count"`
	Schedule        Schedule                  `json:"schedule"`
	Web             WebStatus                 `json:"web"`
	Tools           ToolsStatus               `json:"tools"`
	Retention       RetentionStatus           `json:"retention"`
	SelfReview      SelfReviewStatus          `json:"self_review"`
	BrainResidency  BrainResidencyStatus      `json:"brain_residency"`
	// MissedPatterns surfaces the capped ledger of questions the fast path
	// missed and the reasoning tier answered (§3.7) on GET /smith/status.
	// omitempty so the key is absent (not null) when no patterns have been
	// recorded yet — additive to the frozen wire shape.
	MissedPatterns []MissedPattern `json:"missed_patterns,omitempty"`
}

// ToolsStatus is the P7 tool-loop summary on SelfContext — the resolved
// verdict (native vs fenced) is what makes "verify per model, deferred"
// (docs/v5-smith.md §9) a one-minute check instead of untestable: run one
// turn on a candidate brain, then read this off GET /smith/status.
type ToolsStatus struct {
	Enabled      bool   `json:"enabled"`
	Mode         string `json:"mode"`          // configured smith.tools.mode
	ResolvedMode string `json:"resolved_mode"` // "" until a turn has run for Model
	Model        string `json:"model"`
	Count        int    `json:"count"` // len(Tools()) currently offered
}

// WebStatus is the P5 web-research summary on SelfContext (docs/v5-smith.md
// §5: "status … web providers"). Providers reads the in-memory reachability
// map (web_research.go's WebProviders) — no network round-trip, keeping
// this frequently-polled assembly free of one, same posture as Tier above.
type WebStatus struct {
	Enabled   bool                 `json:"enabled"`
	Providers []web.ProviderStatus `json:"providers"`
}

// Deps wires a Smith. Every field is nil-tolerant per house convention —
// each capability degrades instead of panicking when a dependency is absent
// (tests wire only what the case exercises).
type Deps struct {
	// Store backs findings persistence (smith_findings, migration 0033).
	// nil = sweeps still run and return findings; nothing is persisted and
	// the findings list reads empty.
	Store *store.DB

	// Catalog is the model catalog (ConfigByName/ListOfferings for brain
	// resolution, configured n_ctx for the n_ctx check). nil = the checks
	// that need it skip themselves; the brain resolves deterministic_only.
	Catalog store.Catalog

	// Settings is the settings KV (smith.* keys). nil = defaults.
	Settings store.Settings

	// Sched is the scheduler (Status for slot allocations + the slot-vs-
	// scheduler agreement check). nil = the checks that need it skip.
	Sched sched.Scheduler

	// Engine is P1-read-only: Slots() for the slot list fallback in
	// SelfContext when Sched is nil. Load/Unload are only called via an
	// approved Action's executor, through the Placer seam below — never
	// directly off this field (docs/v5-smith.md §3).
	Engine engine.Engine

	// Placer is the action-model execution seam for load_config/unload_slot
	// (execute.go): the eviction-aware fit check plus the slot lifecycle
	// operations. A named, explicitly-wired interface rather than a type
	// assertion on Engine — see execute.go's Placer doc comment for why a
	// silently-unavailable FitPlan is the one failure mode this feature
	// exists to prevent. nil = load_config/unload_slot actions fail with
	// ErrPlacerUnwired at proposal-stamping time and at execution time.
	Placer Placer

	// RestartUnit issues a real systemd restart of an allowlisted unit
	// (production: (*engine.DBus).Restart) for KindRestartForgeUnit
	// actions — never a raw systemctl shell-out. nil = restart_forge_unit
	// actions fail with ErrRestartUnwired.
	RestartUnit func(ctx context.Context, unit string) error

	// Source is the collector snapshot source — all host telemetry reads go
	// through it. The only probes smith itself performs are the wired
	// loopback checks (HTTPClient / DialPort). nil = checks that need a
	// snapshot skip.
	Source collector.Source

	// Publisher emits smith:* SSE events (docs/v5-smith.md §5). nil = no
	// events.
	Publisher bus.Publisher

	// Subscriber consumes bus events for the anomaly hook (docs/v5-smith.md
	// §4.2 "Anomaly-driven"). smith subscribes to notification:new events
	// and auto-opens investigations for crit-class notifications. nil = no
	// anomaly-triggered investigations.
	Subscriber bus.Subscriber

	// Audit records smith mutations. P1 audits sweep runs triggered via the
	// API from the httpapi layer, not here; the field is wired for the
	// action-model phase.
	Audit store.Audit

	// Cfg returns the infra config (Ports map for the always-on services
	// check, Server.RouterListen for the a0 health probe). Same func()
	// pattern as profile.Deps.Cfg. nil = config-driven checks skip.
	Cfg func() *config.Config

	// HTTPClient is the loopback HTTP client for the a0 reachability +
	// compressor health checks. nil = those checks skip. Never used for
	// anything requiring auth.
	HTTPClient *http.Client

	// ProviderHealth reports remote-provider reachability for handoff
	// candidate probing (handoff.go's probeHandoffCandidates, P3). A narrow
	// seam rather than wiring internal/providers.Service wholesale — smith
	// only ever needs "is this one provider reachable right now". nil ⇒
	// every candidate probes Healthy:false (never assume safe — the same
	// convention as Placer-nil forcing high-risk/required in stampSelfEviction).
	ProviderHealth func(ctx context.Context, provider string) (state string, err error)

	// DialPort probes a loopback TCP port for the compressor health check —
	// proxy ports are dynamic compressor_proxies rows the collector's
	// configured-ports map never dials. nil = real loopback dial.
	DialPort func(port int) bool

	// Web is the P5 web-research provider (search + fetch, docs/v5-smith.md
	// §4.8). nil ⇒ every web path degrades: an explicit web:true chat
	// request gets a "web research unavailable" notice instead of failing
	// the turn, the blocked_work_recheck check reports itself skipped with
	// zero network calls, and no web-sources context block is assembled.
	// Never reuse Deps.HTTPClient for this — its blanket 3s timeout is
	// sized for loopback probes only (see its own doc comment) and would
	// truncate every real internet fetch, the same trap that broke every
	// P3 chat generation before it was found live.
	Web web.Service

	// HF is the HF Hub API client (go/internal/hf) backing the hf_search
	// and hf_preflight-adjacent tree lookups tools.go exposes read-only.
	// nil ⇒ hf_search/hf_preflight report themselves unavailable rather
	// than failing the turn, the same degrade posture as Web above.
	HF *hf.Client

	// HFDownload is the model-acquisition engine (go/internal/hfdownload)
	// backing download_status (read) and download_start (the one
	// deliberate exception to tools.go's "never write" guarantee —
	// download_start only ever creates a pending_approval row; nothing
	// downloads until an operator approves it through the ordinary UI,
	// same posture as every other smith-proposed action). nil ⇒ both
	// tools report themselves unavailable.
	HFDownload *hfdownload.Service

	// TailscalePeers reports the tailnet peer list (P6 FR8 —
	// tailscale_peers check; production: (*collector.TailscaleLocalAPI).Peers
	// adapted to this signature). nil ⇒ the check skips itself, zero
	// LocalAPI calls.
	TailscalePeers func(ctx context.Context) ([]collector.Peer, bool)

	// BinaryVersion execs "<path> --version" (fixed argv, no shell, a
	// short bounded timeout) and returns its trimmed stdout — the P6 FR6
	// binary_versions check's "installed version" probe. Production wiring
	// validates path against binaryPathAllowed before exec'ing anything.
	// nil ⇒ every tracked binary's installed_version reads "unknown"
	// rather than the check failing outright (the source-tree probe, a
	// pure file read, still runs).
	BinaryVersion func(ctx context.Context, path string) (string, error)

	// GitAhead counts how many commits the source tree is AHEAD of a given
	// ref (e.g. "origin/master") — binary_versions' upstream-drift probe
	// (2026-08-17 build-refresh addition). Production wires
	// exec.CommandContext("git", "-C", root, "rev-list", "--count",
	// "HEAD.."+ref) with a bounded timeout — read-only, no shell. The count
	// tells the check "the installed build lags upstream by N commits" (a
	// "rebuild recommended" signal distinct from installed-vs-source-tree,
	// which only measures against the local checkout). nil ⇒ upstream drift
	// is not measured; the check reports source-vs-installed only.
	GitAhead func(ctx context.Context, root, ref string) (int, error)

	// GitBehindLog fetches up to maxN commit SUBJECT lines for the same
	// HEAD..ref range GitAhead counts ("git log --format=%s -n <maxN>
	// HEAD..<ref>", read-only, no shell, bounded timeout) — feeds
	// binary_versions' watchlist match (S6 phase 2, feedback F1's
	// commit-subject-fetching half). nil ⇒ upstream drift is measured
	// (GitAhead) but subjects are never fetched, so the watchlist
	// contributes nothing — same "bonus signal, never a failure" posture
	// as GitAhead/GitLsRemote.
	GitBehindLog func(ctx context.Context, root, ref string, maxN int) ([]string, error)

	// GitLsRemote resolves a remote git URL's HEAD commit ("git ls-remote
	// <url> HEAD", read-only, no shell, caller-bounded timeout) — the
	// upstream-nightly tracking probe (P3smith): binary_versions compares
	// a tracked fork's remote HEAD against last_built_upstream_sha, and
	// build_refresh's build_record_upstream_sha step records it mid-run.
	// The URL comes from operator-editable settings/DB data; production
	// wiring re-validates it against smith.upstreamURLAllowed before
	// exec'ing anything. nil ⇒ tracking-enabled forks fail closed (the op)
	// or report themselves unmeasurable (the check) — never silently OK.
	GitLsRemote func(ctx context.Context, url string) (string, error)

	// ComfyUI is the P6 FR7 client (comfyui.HTTPClient in production,
	// bound to smith.comfyui.url) — comfyui_health/comfyui_prune's only
	// route to ComfyUI's own API. nil ⇒ both checks skip; comfyui_prune
	// can never build a map, so no delete_files proposal is ever possible
	// (guardrail (a) via a different route: no client at all, not just an
	// unreachable one).
	ComfyUI comfyui.Client

	// DeleteFile performs one real file deletion (production:
	// os.Remove, called ONLY after deleteAllowed re-validates the path
	// against the configured ComfyUI model roots at dispatch time — see
	// execute.go's dispatchDeleteFiles). nil ⇒ delete_files actions fail
	// with ErrDeleteUnwired; nothing is ever deleted by accident from an
	// unwired daemon.
	DeleteFile func(ctx context.Context, path string) error

	// RunStep executes one procedure step's fixed argv (production:
	// exec.CommandContext, bounded by spec.Timeout, no shell — see
	// procedure.go's runProcedureSteps, which re-validates every argv
	// against procedures.ArgvAllowed immediately before calling this, same
	// belt-and-braces posture as DeleteFile/dispatchDeleteFiles). nil ⇒
	// every KindProcedure action fails with ErrProcedureUnwired at
	// dispatch — an unwired daemon can never run a command by accident.
	RunStep func(ctx context.Context, spec procedures.StepSpec) (procedures.StepResult, error)

	// Maintenance is the Sprint 1 quiet-host gate (internal/maintenance).
	// A procedure whose Impact.NeedsMaintenance is true holds a window for
	// its whole run via this field (procedure.go's runProcedureSteps). nil
	// ⇒ such a procedure fails closed with ErrMaintenanceUnwired rather
	// than running without the quiet-host guarantee it declared it needs;
	// procedures with NeedsMaintenance=false never touch this field at all.
	Maintenance *maintenance.Gate

	// Activity is the per-slot consumer attribution registry (shared with
	// the router + httpapi — one instance wired in cmd/forge). Reasoning
	// turns and brain-residency loads Mark their slot as "SMITH" so the
	// dashboard's status.slot_consumers shows who is generating. nil → no
	// attribution.
	Activity *activity.Registry

	// CompressorProvisioner (Sprint D) writes a proxy's env file and
	// restarts its systemd unit — the apply path for settings that forge-
	// compress reads at startup (currently only FailOpenBudgetMS). nil ⇒
	// KindSettingsChange proposals for those keys still write the settings
	// store but skip the restart; the change takes effect on the next
	// manual or reboot-driven restart. Narrow interface rather than the
	// full *compressorctl.Provisioner — smith only needs Reconcile.
	CompressorProvisioner interface {
		Reconcile(ctx context.Context, row store.ProxyRow) error
	}

	// CmdlinePath overrides the kernel boot-params file (default
	// /proc/cmdline). Tests point it at a fixture.
	CmdlinePath string

	// BlockedWorkPath is the operator-local blocked-work tracker file
	// (kb_investigations.go, docs/v5-smith.md §4.7) — layer-2 deployment
	// data, never embedded, read live on every ListBlockedItems call.
	// "" or an absent file ⇒ the blocked-work KB is honestly empty (a
	// fresh install has no externally-blocked items yet).
	BlockedWorkPath string

	// JournalErrors reads the last n error lines from forge-* unit
	// journals (the smith fast-path "what does the error log show?" answer,
	// docs/v5-smith-experience.md §8 item 16). Production wires exec.Command
	// ("journalctl", "-u", "forge-*", ...); nil ⇒ the logs answer degrades
	// to the notifications list (strictly less useful but always safe). The
	// read is bounded — never a log viewer, just a last-N digest. since, when
	// non-zero, is passed through as journalctl's --since — a zero Time means
	// unbounded (the historical behavior, used by the log-digest answer).
	JournalErrors func(ctx context.Context, n int, since time.Time) ([]string, error)

	// KernelJournal reads the last n lines from the kernel journal
	// ("journalctl -k"). This is the device-lost detection seam: the
	// amdgpu driver logs "ring comp_X.Y timeout" / "device wedged" and the
	// llama-server unit logs "ErrorDeviceLost" there — the definitive
	// signature of a GPU hang that leaves /health green (the 2026-08-16
	// qwen38-27b incident). nil ⇒ the gpu_device_lost check reports itself
	// skipped. Bounded like JournalErrors; never a log viewer. since is the
	// same --since seam as JournalErrors — the gpu_device_lost check passes
	// a real window so a resolved incident's journal lines stop reading as
	// a fresh crit indefinitely (found live 2026-08-18: the unwindowed read
	// made autorecover's confirmation gate indistinguishable from a stale
	// signature).
	KernelJournal func(ctx context.Context, n int, since time.Time) ([]string, error)

	// Now and Logf default to time.Now and a no-op logger.
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// Smith is the self-diagnosis agent.
type Smith struct {
	d Deps // resolved (defaults filled)

	mu                sync.Mutex
	sweeping          bool
	lastSweepAt       time.Time
	lastSweepKind     string
	lastSweepFindings int

	// openAnomaly maps an anomaly code to the ID of its currently-open
	// investigation (debounce: one open investigation per code — a repeat
	// notification while open attaches findings to it, never a duplicate).
	openAnomaly map[string]int64

	// autoRecoveredAt tracks the last auto-recovery per slot (autorecover.go)
	// for cooldown: a device-lost that recurs within autoRecoverCooldown
	// escalates instead of reloading again.
	autoRecoveredAt map[string]time.Time

	// autonomyRunsAt tracks recent standing-autonomy runs per procedure ID
	// (autonomy.go), oldest-first, trimmed to the last 24h on each check —
	// backs both the cooldown and the rolling daily rate cap. In-memory
	// only, same posture as autoRecoveredAt: resets on daemon restart,
	// which only ever makes the system MORE conservative (a lost window
	// means fewer autonomous runs allowed, never more).
	autonomyRunsAt map[string][]time.Time

	// chatFailures is the per-conversation Tier 2 retry budget (reasoning.go
	// §10 risk #1: bounds load loops from an evicted/unreachable brain).
	// Reset to 0 (deleted) on any successful turn.
	chatFailures map[int64]int

	// pendingMissed carries the redacted user text for a no-match turn that
	// went to the reasoning tier, keyed by assistant message ID. On a
	// successful reasoning turn, runReasoningTurn records it (with the tools
	// it used) via RecordMissedPattern (§3.7), then clears it. Guarded by
	// mu.
	pendingMissed map[int64]string

	// webProbeMu guards lastWebProbeAt (web_research.go's ProbeWeb /
	// maybeProbeWeb) — read/written from both Start()'s one-shot probe
	// goroutine and scheduleLoop's periodic re-probe check.
	webProbeMu     sync.Mutex
	lastWebProbeAt time.Time

	// toolModes is the P7 per-model native/fenced resolution cache
	// (tool_parse.go's resolveToolMode/recordToolMode), guarded by mu —
	// keyed by brain model name so a smith.model change invalidates
	// automatically.
	toolModes map[string]string

	// pruneMu guards lastPruneAt (retention.go's maybePrune), mirroring
	// webProbeMu's shape exactly.
	pruneMu     sync.Mutex
	lastPruneAt time.Time

	// lastRetention is the most recent prune's outcome, guarded by mu —
	// read by retentionStatus (SelfContext.Retention), written by
	// pruneOnce's recordRetentionResult.
	lastRetention RetentionResult

	// selfReviewMu guards lastSelfReviewAt (self_review.go's
	// maybeSelfReview), mirroring pruneMu's shape exactly.
	selfReviewMu     sync.Mutex
	lastSelfReviewAt time.Time

	// lastSelfReview is the most recent self-review sweep's outcome, guarded
	// by mu — read by selfReviewStatus (SelfContext.SelfReview), written by
	// selfReviewOnce's recordSelfReviewResult.
	lastSelfReview SelfReviewResult

	// brainResidencyMu guards lastBrainResidencyAt (brain_residency.go's
	// maybeEnsureBrainResident), mirroring selfReviewMu's shape exactly.
	brainResidencyMu     sync.Mutex
	lastBrainResidencyAt time.Time

	// lastBrainResidency is the most recent on-demand-or-periodic load
	// attempt's outcome, guarded by mu — read by brainResidencyStatus
	// (SelfContext.BrainResidency), written by ensureBrainLoaded.
	lastBrainResidency BrainResidencyResult

	// stopSchedule stops the periodic sweep scheduler (nil until Start).
	stopSchedule context.CancelFunc

	// bgCtx is the long-lived context an approved action's executor runs on
	// (execute.go's executeAction, kicked off from ApproveAction) — it must
	// outlive the HTTP request that triggered the approval but still be
	// cancelled on Stop(), so it is captured once in Start() from the same
	// cancelable context stopSchedule derives from. nil until Start() runs
	// (e.g. in tests that call ApproveAction directly); callers fall back to
	// context.Background() when nil.
	bgCtx context.Context
}

// New returns a Smith with defaults filled in. Never panics on nil deps.
func New(d Deps) *Smith {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.CmdlinePath == "" {
		d.CmdlinePath = "/proc/cmdline"
	}
	if d.HTTPClient == nil {
		d.HTTPClient = &http.Client{Timeout: 3 * time.Second}
	}
	return &Smith{d: d, openAnomaly: map[string]int64{}, autoRecoveredAt: map[string]time.Time{}, autonomyRunsAt: map[string][]time.Time{}, chatFailures: map[int64]int{}, pendingMissed: map[int64]string{}}
}

// logf prefixes + forwards to the wired logger.
func (s *Smith) logf(format string, args ...any) {
	s.d.Logf("smith: "+format, args...)
}

// snapshot returns the latest collector snapshot, or nil when no source is
// wired or no snapshot has been taken yet.
func (s *Smith) snapshot() *collector.Snapshot {
	if s.d.Source == nil {
		return nil
	}
	return s.d.Source.Current()
}

// loadedIdleChainMember scans the scheduler snapshot for a chain member
// already loaded on a slot with idle time above brainChainIdleFloorS —
// chain-preference order wins over slot order. A member that is loaded but
// BUSY is skipped (never contend with active traffic); a member not loaded
// at all is skipped (loading is ensureBrainLoaded's job). ok=false sends the
// caller down the normal resolution path.
func (s *Smith) loadedIdleChainMember(ctx context.Context, chain []string) (BrainResolution, bool) {
	st := s.d.Sched.Status()
	for _, member := range chain {
		for slot, mode := range st.Slots {
			if mode != member {
				continue
			}
			idle, known := st.IdleSeconds[slot]
			if !known || idle == nil || *idle < brainChainIdleFloorS {
				break // loaded but busy/unknown — skip this member, try the next
			}
			return BrainResolution{
				Resolution: BrainLocalSlot,
				Model:      member,
				Slot:       slot,
				Detail: fmt.Sprintf("brain chain: %s already loaded and idle (%.0fs) on %s — zero load wait (settings: %s)",
					member, *idle, strings.ToUpper(slot), SettingBrainChain),
			}, true
		}
	}
	return BrainResolution{}, false
}

// Brain resolves where smith's own inference runs (docs/v5-smith.md §4.1):
// with a brain chain provisioned (smith.brain_chain, S5), any chain member
// already loaded AND idle on a slot answers directly — zero load wait, in
// chain-preference order, never contending with active traffic; otherwise
// the effective model is chain[0] when a chain exists, else smith.model →
// Catalog.ConfigByName hit = local (slot via sched.Status), Catalog.
// ListOfferings WireModel hit = remote offering, neither = deterministic_
// only. Pure reads; never triggers a load (that's ensureBrainLoaded), never
// evicts anything for a brain.
func (s *Smith) Brain(ctx context.Context) BrainResolution {
	chain := s.BrainChain(ctx)
	if len(chain) > 0 && s.d.Sched != nil {
		if br, ok := s.loadedIdleChainMember(ctx, chain); ok {
			return br
		}
	}
	model := s.settingModel(ctx)
	if len(chain) > 0 {
		model = chain[0]
	}
	if model == "" {
		return BrainResolution{
			Resolution: BrainDeterministicOnly,
			Detail:     "smith.model unset — deterministic tier only",
		}
	}
	if s.d.Catalog == nil {
		return BrainResolution{
			Resolution: BrainDeterministicOnly,
			Model:      model,
			Detail:     "catalog unavailable — cannot resolve " + model,
		}
	}

	// Local Config hit?
	if cfg, err := s.d.Catalog.ConfigByName(ctx, model); err == nil && cfg.ID != 0 {
		slot := ""
		if s.d.Sched != nil {
			for name, mode := range s.d.Sched.Status().Slots {
				if mode == model {
					slot = name
					break
				}
			}
		}
		if slot != "" {
			return BrainResolution{
				Resolution: BrainLocalSlot,
				Model:      model,
				Slot:       slot,
				Detail:     "local config on slot " + strings.ToUpper(slot),
			}
		}
		return BrainResolution{
			Resolution: BrainDeterministicOnly,
			Model:      model,
			Detail:     "configured model " + model + " is not currently loaded on any slot",
		}
	}

	// Remote Offering hit (enabled offerings only)?
	if offerings, err := s.d.Catalog.ListOfferings(ctx); err == nil {
		for _, o := range offerings {
			if o.Enabled && o.WireModel == model {
				return BrainResolution{
					Resolution: BrainRemote,
					Model:      model,
					Provider:   o.ProviderName,
					Detail:     "remote offering via provider " + o.ProviderName,
				}
			}
		}
	}

	return BrainResolution{
		Resolution: BrainDeterministicOnly,
		Model:      model,
		Detail:     "model " + model + " resolves to no local config or enabled offering",
	}
}

// settingModel reads smith.model ("" when unset/unreadable). The settings KV
// stores raw JSON, so the value is a JSON string — strip the quotes.
func (s *Smith) settingModel(ctx context.Context) string {
	raw, ok := s.settingJSON(ctx, SettingModel)
	if !ok {
		return ""
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		// Tolerate a bare (non-JSON) value too.
		return strings.TrimSpace(string(raw))
	}
	return strings.TrimSpace(model)
}

// settingJSON returns the raw JSON bytes for key, or ok=false when the
// settings dependency is nil, the key is unset, or the read fails.
func (s *Smith) settingJSON(ctx context.Context, key string) ([]byte, bool) {
	if s.d.Settings == nil {
		return nil, false
	}
	raw, err := s.d.Settings.Get(ctx, key)
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	return raw, true
}

// ModelSetting returns the raw smith.model value, including when it
// doesn't resolve to anything (empty string when unset). Distinct from
// Brain(), which resolves it into a full BrainResolution — this is for the
// Settings UI, which needs to show/edit the configured value even when
// it's currently unresolvable.
func (s *Smith) ModelSetting(ctx context.Context) string {
	return s.settingModel(ctx)
}

// HandoffOfferings reads smith.handoff_offerings (the ordered Offering IDs
// the P3 remote brain-swap probe tries, most-preferred first). Empty slice,
// never nil, when unset or unreadable — safe to marshal directly.
func (s *Smith) HandoffOfferings(ctx context.Context) []int64 {
	raw, ok := s.settingJSON(ctx, SettingHandoffOfferings)
	if !ok {
		return []int64{}
	}
	var ids []int64
	if err := json.Unmarshal(raw, &ids); err != nil {
		return []int64{}
	}
	if ids == nil {
		ids = []int64{}
	}
	return ids
}

// TailscaleWatchPeers reads smith.tailscale.watch_peers (P6 FR8) — the
// hostnames the tailscale_peers check treats as infra-critical. Empty
// slice, never nil, when unset/unreadable.
func (s *Smith) TailscaleWatchPeers(ctx context.Context) []string {
	raw, ok := s.settingJSON(ctx, SettingTailscaleWatchPeers)
	if !ok {
		return []string{}
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return []string{}
	}
	if names == nil {
		names = []string{}
	}
	return names
}

// TrackedBinary is one smith.binaries.tracked entry (P6 FR6) — an
// operator-curated identity for something worth version-tracking. Kind
// mirrors smith_binaries.kind's CHECK constraint (migration 0038).
type TrackedBinary struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"` // llama_build | python_pkg | runtime | service
	Path       string `json:"path"`
	SourceKind string `json:"source_kind"` // "" | "git"
	SourceRef  string `json:"source_ref"`  // git tree root, when source_kind=="git"
	// UpstreamRef is the remote-tracking ref to measure drift against
	// (e.g. "origin/master") for git-kind binaries. Empty ⇒ upstream drift
	// isn't measured for this binary (binary_versions still reports
	// installed-vs-source-tree). Additive — old settings JSON without this
	// field decodes as "".
	UpstreamRef string `json:"upstream_ref,omitempty"`
}

// MeshService is one smith.mesh.services entry (open-source-readiness
// finding 1) — an operator-curated mesh inventory entry the reachability
// family asks about. Name is the canonical entity id, Aliases are the
// surface phrases the classifier matches, Address is the host[:port]
// reported by reachability answers.
type MeshService struct {
	Name    string   `json:"name"`
	Aliases []string `json:"aliases"`
	Address string   `json:"address"`
}

// MeshServices reads smith.mesh.services. Empty slice, never nil, when
// unset/unreadable — a deployment with no mesh inventory simply has no
// reachability entities beyond the code-curated live probes
// ("internet"/"tailnet").
func (s *Smith) MeshServices(ctx context.Context) []MeshService {
	raw, ok := s.settingJSON(ctx, SettingMeshServices)
	if !ok {
		return []MeshService{}
	}
	var services []MeshService
	if err := json.Unmarshal(raw, &services); err != nil {
		return []MeshService{}
	}
	if services == nil {
		services = []MeshService{}
	}
	return services
}

// settingBool reads key as a JSON bool, defaulting to def when unset,
// unreadable, or Settings is nil.
func (s *Smith) settingBool(ctx context.Context, key string, def bool) bool {
	raw, ok := s.settingJSON(ctx, key)
	if !ok {
		return def
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return def
	}
	return v
}

// BinariesEnabled reads smith.binaries.enabled (default true — the
// migration seeds it explicitly, this is only the in-code fallback for an
// unreadable value).
func (s *Smith) BinariesEnabled(ctx context.Context) bool {
	return s.settingBool(ctx, SettingBinariesEnabled, true)
}

// TrackedBinaries reads smith.binaries.tracked. Empty slice, never nil,
// when unset/unreadable.
func (s *Smith) TrackedBinaries(ctx context.Context) []TrackedBinary {
	raw, ok := s.settingJSON(ctx, SettingBinariesTracked)
	if !ok {
		return []TrackedBinary{}
	}
	var tracked []TrackedBinary
	if err := json.Unmarshal(raw, &tracked); err != nil {
		return []TrackedBinary{}
	}
	if tracked == nil {
		tracked = []TrackedBinary{}
	}
	return tracked
}

// ComfyUIEnabled reads smith.comfyui.enabled (default true).
func (s *Smith) ComfyUIEnabled(ctx context.Context) bool {
	return s.settingBool(ctx, SettingComfyUIEnabled, true)
}

// settingStringList reads key as a JSON []string. Empty slice, never nil,
// when unset/unreadable.
func (s *Smith) settingStringList(ctx context.Context, key string) []string {
	raw, ok := s.settingJSON(ctx, key)
	if !ok {
		return []string{}
	}
	var v []string
	if err := json.Unmarshal(raw, &v); err != nil {
		return []string{}
	}
	if v == nil {
		v = []string{}
	}
	return v
}

// settingString reads key as a JSON string, defaulting to "" when
// unset/unreadable.
func (s *Smith) settingString(ctx context.Context, key string) string {
	raw, ok := s.settingJSON(ctx, key)
	if !ok {
		return ""
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return v
}

// ComfyUIUnit reads smith.comfyui.unit.
func (s *Smith) ComfyUIUnit(ctx context.Context) string {
	return s.settingString(ctx, SettingComfyUIUnit)
}

// ComfyUIURL reads smith.comfyui.url (e.g. "http://127.0.0.1:3001").
func (s *Smith) ComfyUIURL(ctx context.Context) string {
	return s.settingString(ctx, SettingComfyUIURL)
}

// ComfyUIModelRoots reads smith.comfyui.model_roots — deliberately a list
// (fact 1, docs/v5-smith.md §4.9's amendment): ForgeHost's real installation has
// TWO distinct model directories, and a single hardcoded root would miss
// half the disk.
func (s *Smith) ComfyUIModelRoots(ctx context.Context) []string {
	return s.settingStringList(ctx, SettingComfyUIModelRoots)
}

// ComfyUIKeepFiles reads smith.comfyui.keep_files — full paths
// proposeComfyUIDelete excludes from any future delete proposal. Empty,
// never nil, when unset.
func (s *Smith) ComfyUIKeepFiles(ctx context.Context) []string {
	return s.settingStringList(ctx, SettingComfyUIKeepFiles)
}

// BuildRefreshWatchlist reads smith.build_refresh.watchlist — keywords
// binary_versions matches (case-insensitive substring) against fetched
// upstream commit subjects. Empty, never nil, when unset.
func (s *Smith) BuildRefreshWatchlist(ctx context.Context) []string {
	return s.settingStringList(ctx, SettingBuildRefreshWatchlist)
}

// BrainChain reads smith.brain_chain — the ordered list of acceptable
// local brain configs. Nil (never an empty non-nil slice distinction
// matters to callers) when unset or malformed: a malformed value fails
// closed to "no chain" rather than guessing.
func (s *Smith) BrainChain(ctx context.Context) []string {
	raw, ok := s.settingJSON(ctx, SettingBrainChain)
	if !ok {
		return nil
	}
	var chain []string
	if err := json.Unmarshal(raw, &chain); err != nil {
		return nil
	}
	out := make([]string, 0, len(chain))
	for _, m := range chain {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// brainChainIdleFloorS is the minimum idle seconds for a loaded slot to
// count as "already loaded and idle" in the brain-chain preference scan —
// low enough that a resident brain is normally usable, high enough to never
// grab a slot serving active traffic. Deliberately NOT smith.turn_budget-
// style configurable in v1; S1's eviction threshold (180s default) remains
// the scheduler's own concern.
const brainChainIdleFloorS = 30.0

// ComfyUIWorkflowDirs reads smith.comfyui.workflow_dirs.
func (s *Smith) ComfyUIWorkflowDirs(ctx context.Context) []string {
	return s.settingStringList(ctx, SettingComfyUIWorkflowDirs)
}

// TurnBudget reads smith.turn_budget, falling back to DefaultTurnBudget
// field-by-field for anything missing or invalid (the smith.Schedule
// template). Values ≤0 fall back too — a zero budget would make every turn
// fail instantly, the opposite failure mode of the constants this replaces.
func (s *Smith) TurnBudget(ctx context.Context) TurnBudget {
	out := DefaultTurnBudget()
	raw, ok := s.settingJSON(ctx, SettingTurnBudget)
	if !ok {
		return out
	}
	var v struct {
		FirstTurnS    *int `json:"first_turn_s"`
		EscalationS   *int `json:"escalation_s"`
		RoundTimeoutS *int `json:"round_timeout_s"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.FirstTurnS != nil && *v.FirstTurnS > 0 {
		out.FirstTurnS = *v.FirstTurnS
	}
	if v.EscalationS != nil && *v.EscalationS > 0 {
		out.EscalationS = *v.EscalationS
	}
	if v.RoundTimeoutS != nil && *v.RoundTimeoutS > 0 {
		out.RoundTimeoutS = *v.RoundTimeoutS
	}
	return out
}

// Schedule reads + decodes smith.schedule, falling back to DefaultSchedule
// field-by-field for anything missing or invalid.
func (s *Smith) Schedule(ctx context.Context) Schedule {
	out := DefaultSchedule()
	raw, ok := s.settingJSON(ctx, SettingSchedule)
	if !ok {
		return out
	}
	var v struct {
		Quick   *string `json:"quick"`
		Deep    *string `json:"deep"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.Quick != nil && parseDurationOrZero(*v.Quick) > 0 {
		out.Quick = *v.Quick
	}
	if v.Deep != nil && parseDurationOrZero(*v.Deep) > 0 {
		out.Deep = *v.Deep
	}
	if v.Enabled != nil {
		out.Enabled = *v.Enabled
	}
	return out
}

// Thresholds reads + decodes smith.thresholds, falling back to
// DefaultThresholds field-by-field for anything missing or invalid.
func (s *Smith) Thresholds(ctx context.Context) Thresholds {
	out := DefaultThresholds()
	raw, ok := s.settingJSON(ctx, SettingThresholds)
	if !ok {
		return out
	}
	var v struct {
		GTTWarnPct                *float64 `json:"gtt_warn_pct"`
		GTTCritPct                *float64 `json:"gtt_crit_pct"`
		DiskWarnPct               *float64 `json:"disk_warn_pct"`
		DiskCritPct               *float64 `json:"disk_crit_pct"`
		DeviceLostWindowMinutes   *int     `json:"device_lost_window_minutes"`
		BuildRefreshBehindN       *int     `json:"build_refresh_behind_n"`
		CompressorFailOpenWarnPct *float64 `json:"compressor_failopen_warn_pct"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	applyPct := func(dst *float64, src *float64) {
		if src != nil && *src > 0 && *src <= 100 {
			*dst = *src
		}
	}
	applyPct(&out.GTTWarnPct, v.GTTWarnPct)
	applyPct(&out.GTTCritPct, v.GTTCritPct)
	applyPct(&out.DiskWarnPct, v.DiskWarnPct)
	applyPct(&out.DiskCritPct, v.DiskCritPct)
	if v.DeviceLostWindowMinutes != nil && *v.DeviceLostWindowMinutes > 0 {
		out.DeviceLostWindowMinutes = *v.DeviceLostWindowMinutes
	}
	if v.BuildRefreshBehindN != nil && *v.BuildRefreshBehindN > 0 {
		out.BuildRefreshBehindN = *v.BuildRefreshBehindN
	}
	applyPct(&out.CompressorFailOpenWarnPct, v.CompressorFailOpenWarnPct)
	return out
}

// AutoRecoverDeviceLost reads smith.auto_recover_device_lost (JSON bool),
// defaulting to true when unset/invalid — a device-lost auto-reload is the
// safe default (the recovery is a trivial unload→reload; escalation is the
// only guarded part). Use the explicit setting for deployments that want a
// human in the loop for every recovery.
func (s *Smith) AutoRecoverDeviceLost(ctx context.Context) bool {
	raw, ok := s.settingJSON(ctx, SettingAutoRecoverDeviceLost)
	if !ok {
		return true
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return true
	}
	return v
}

// AutoHandoffCloud reads smith.auto_handoff_cloud (JSON bool), defaulting to
// true when unset/invalid — gracefully swapping a crash-looping local brain
// to a healthy cloud offering is the safe default (it keeps the reasoning
// tier alive during the outage; the swap is audited and reversible). Set
// false to force operator-runbook escalation instead of automatic cloud
// handoff.
func (s *Smith) AutoHandoffCloud(ctx context.Context) bool {
	raw, ok := s.settingJSON(ctx, SettingAutoHandoffCloud)
	if !ok {
		return true
	}
	var v bool
	if err := json.Unmarshal(raw, &v); err != nil {
		return true
	}
	return v
}

// ToolsConfig is the P7 read-only tool-loop config (smith.tools). Mode is
// one of the toolMode* constants (tool_parse.go) — "auto" (default)
// detects native-vs-fenced per model, optimistic-native, demoting to
// fenced on real evidence; "native"/"fenced" pin the mode (the escape
// hatch a deferred per-model verification gap needs); "off" disables the
// loop entirely regardless of Enabled.
type ToolsConfig struct {
	Enabled   bool   `json:"enabled"`
	Mode      string `json:"mode"`
	MaxRounds int    `json:"max_rounds"`
}

// DefaultToolsConfig is used when smith.tools is unset or unreadable.
// Enabled:true by default is deliberate: the "off" state is already
// reachable structurally (no brain resolvable → deterministic tier, no
// loop runs at all), and the operator has three explicit off-switches
// (enabled:false, mode:"off", smith.model unset). MaxRounds defaults to 6
// to accommodate the phased loop structure (plan 1 + investigate 3 + verify
// 1 + answer 1) without starving investigation; turnBudget (480s) remains
// the real ceiling.
func DefaultToolsConfig() ToolsConfig {
	return ToolsConfig{Enabled: true, Mode: toolModeAuto, MaxRounds: 6}
}

// ToolsConfig reads + decodes smith.tools, falling back to
// DefaultToolsConfig field-by-field for anything missing or invalid.
func (s *Smith) ToolsConfig(ctx context.Context) ToolsConfig {
	out := DefaultToolsConfig()
	raw, ok := s.settingJSON(ctx, SettingTools)
	if !ok {
		return out
	}
	var v struct {
		Enabled   *bool   `json:"enabled"`
		Mode      *string `json:"mode"`
		MaxRounds *int    `json:"max_rounds"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.Enabled != nil {
		out.Enabled = *v.Enabled
	}
	if v.Mode != nil && validToolMode(*v.Mode) {
		out.Mode = *v.Mode
	}
	if v.MaxRounds != nil && *v.MaxRounds >= 1 && *v.MaxRounds <= 8 {
		out.MaxRounds = *v.MaxRounds
	}
	return out
}

// Retention is the P7 findings+web-cache retention config (smith.retention).
// A tier's *Days <= 0 skips that tier entirely rather than deleting
// everything — the same footgun-avoidance as store.RunRetention's
// days<=0 guard (internal/store/sampler.go). Findings attached to an
// investigation are never pruned by any tier, at any age (retention.go).
type Retention struct {
	Enabled         bool `json:"enabled"`
	OKDays          int  `json:"ok_days"`
	InfoHours       int  `json:"info_hours"`
	WarnCritDays    int  `json:"warn_crit_days"`
	WebCacheDays    int  `json:"web_cache_days"`
	WebCacheMaxRows int  `json:"web_cache_max_rows"`
}

// DefaultRetention is used when smith.retention is unset or unreadable.
func DefaultRetention() Retention {
	return Retention{Enabled: true, OKDays: 7, InfoHours: 1, WarnCritDays: 180, WebCacheDays: 30, WebCacheMaxRows: 2000}
}

// RetentionConfig reads + decodes smith.retention, falling back to
// DefaultRetention field-by-field for anything missing or invalid.
func (s *Smith) RetentionConfig(ctx context.Context) Retention {
	out := DefaultRetention()
	raw, ok := s.settingJSON(ctx, SettingRetention)
	if !ok {
		return out
	}
	var v struct {
		Enabled         *bool `json:"enabled"`
		OKDays          *int  `json:"ok_days"`
		InfoHours       *int  `json:"info_hours"`
		WarnCritDays    *int  `json:"warn_crit_days"`
		WebCacheDays    *int  `json:"web_cache_days"`
		WebCacheMaxRows *int  `json:"web_cache_max_rows"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.Enabled != nil {
		out.Enabled = *v.Enabled
	}
	// Negative/zero values are meaningful ("skip this tier") — only reject
	// unset (nil), never clamp a caller's explicit 0.
	if v.OKDays != nil {
		out.OKDays = *v.OKDays
	}
	if v.InfoHours != nil {
		out.InfoHours = *v.InfoHours
	}
	if v.WarnCritDays != nil {
		out.WarnCritDays = *v.WarnCritDays
	}
	if v.WebCacheDays != nil {
		out.WebCacheDays = *v.WebCacheDays
	}
	if v.WebCacheMaxRows != nil && *v.WebCacheMaxRows >= 0 {
		out.WebCacheMaxRows = *v.WebCacheMaxRows
	}
	return out
}

// SelfReview is the periodic self-review sweep's config (smith.self_review,
// self_review.go, Thread C). GraceMinutes is how long an open investigation
// or a done_unverified/pending action must sit untouched before the sweep
// re-checks it — avoids reviewing something the operator (or a still-running
// action) hasn't had a chance to react to yet.
type SelfReview struct {
	Enabled      bool `json:"enabled"`
	GraceMinutes int  `json:"grace_minutes"`
}

// DefaultSelfReview is used when smith.self_review is unset or unreadable.
func DefaultSelfReview() SelfReview {
	return SelfReview{Enabled: true, GraceMinutes: 30}
}

// SelfReviewConfig reads + decodes smith.self_review, falling back to
// DefaultSelfReview field-by-field for anything missing or invalid.
func (s *Smith) SelfReviewConfig(ctx context.Context) SelfReview {
	out := DefaultSelfReview()
	raw, ok := s.settingJSON(ctx, SettingSelfReview)
	if !ok {
		return out
	}
	var v struct {
		Enabled      *bool `json:"enabled"`
		GraceMinutes *int  `json:"grace_minutes"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.Enabled != nil {
		out.Enabled = *v.Enabled
	}
	if v.GraceMinutes != nil && *v.GraceMinutes >= 0 {
		out.GraceMinutes = *v.GraceMinutes
	}
	return out
}

// BrainResidency is the operator's opt-in "keep the brain always loaded"
// choice (smith.brain_residency, brain_residency.go). This is separate
// from the per-turn on-demand escalation ensureBrainLoaded always does
// regardless of this setting — StayResident additionally makes
// maybeEnsureBrainResident proactively (re)load the brain on a schedule,
// accepting the standing VRAM cost even when nothing has asked for it yet.
type BrainResidency struct {
	StayResident bool `json:"stay_resident"`
}

// DefaultBrainResidency is used when smith.brain_residency is unset or
// unreadable — false, so a brain is never kept loaded without an explicit
// operator opt-in.
func DefaultBrainResidency() BrainResidency {
	return BrainResidency{StayResident: false}
}

// BrainResidencyConfig reads + decodes smith.brain_residency, falling back
// to DefaultBrainResidency for anything missing or invalid.
func (s *Smith) BrainResidencyConfig(ctx context.Context) BrainResidency {
	out := DefaultBrainResidency()
	raw, ok := s.settingJSON(ctx, SettingBrainResidency)
	if !ok {
		return out
	}
	var v struct {
		StayResident *bool `json:"stay_resident"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return out
	}
	if v.StayResident != nil {
		out.StayResident = *v.StayResident
	}
	return out
}

// parseDurationOrZero parses a duration string, returning 0 on any error so
// callers can treat 0 as "invalid/unset".
func parseDurationOrZero(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// SelfContext assembles the §4.1 picture on demand. Pure reads.
func (s *Smith) SelfContext(ctx context.Context) SelfContext {
	now := s.d.Now()
	brain := s.Brain(ctx)
	// Tier reflects whether a brain is resolvable at all, not a live a0
	// /healthz probe (decideTier in reasoning.go does that at chat-turn
	// time) — keeping this frequently-polled status assembly free of a
	// network round-trip.
	tier := TierDeterministic
	if brain.Resolution != BrainDeterministicOnly {
		tier = TierReasoning
	}
	sc := SelfContext{
		Tier:           tier,
		Brain:          brain,
		Slots:          map[string]SlotAllocation{},
		Alerts:         []AlertInfo{},
		CheckCount:     len(registry),
		FastCheckCount: fastCheckCount(),
		Schedule:       s.Schedule(ctx),
		Web:            WebStatus{Enabled: s.WebConfig(ctx).Enabled, Providers: s.WebProviders(ctx)},
		Tools:          s.toolsStatus(ctx, brain.Model),
		Retention:      s.retentionStatus(ctx),
		SelfReview:     s.selfReviewStatus(ctx),
		BrainResidency: s.brainResidencyStatus(ctx),
	}

	if snap := s.snapshot(); snap != nil {
		sc.Hostname = snap.Hostname
		sc.SnapshotTakenAt = snap.TakenAt.Unix()
		sc.SnapshotAgeS = now.Sub(snap.TakenAt).Seconds()
		sc.Metrics = summarizeMetrics(snap.Metrics)
		for _, a := range snap.Alerts {
			sc.Alerts = append(sc.Alerts, AlertInfo{Code: a.Code, Msg: a.Msg, Port: a.Port, Unit: a.Unit})
		}
	}

	if s.d.Sched != nil {
		st := s.d.Sched.Status()
		sc.MemoryBudget = BudgetSummary{
			TotalBytes: st.MemoryBudget.TotalBytes,
			UsedBytes:  st.MemoryBudget.UsedBytes,
			FreeBytes:  st.MemoryBudget.FreeBytes,
		}
		for slot, mode := range st.Slots {
			alloc := SlotAllocation{Mode: mode, Label: st.SlotLabels[slot]}
			if st.IdleSeconds != nil {
				alloc.IdleSeconds = st.IdleSeconds[slot]
			}
			if st.SlotMemoryBytes != nil {
				alloc.MemoryBytes = st.SlotMemoryBytes[slot]
			}
			sc.Slots[slot] = alloc
		}
	} else if s.d.Engine != nil {
		for _, slot := range s.d.Engine.Slots() {
			sc.Slots[slot] = SlotAllocation{Label: strings.ToUpper(slot)}
		}
	}

	if missed, _ := s.MissedPatterns(ctx); len(missed) > 0 {
		sc.MissedPatterns = missed
	}

	return sc
}

// summarizeMetrics projects collector.Metrics into the wire summary.
func summarizeMetrics(m collector.Metrics) *MetricsSummary {
	return &MetricsSummary{
		Mode:              m.Mode,
		MemTotalBytes:     m.Memory.TotalBytes,
		MemUsedBytes:      m.Memory.UsedBytes,
		MemAvailBytes:     m.Memory.AvailBytes,
		MemPct:            m.Memory.Pct,
		GTTUsedBytes:      m.GTTUsedBytes,
		GTTTotalBytes:     m.GTTTotalBytes,
		GPUUsePct:         m.GPUUsePct,
		TempCelsius:       m.TempCelsius,
		UptimeSeconds:     m.UptimeSeconds,
		PackagePowerW:     m.PackagePowerW,
		DiskTotalBytes:    m.Disk.TotalBytes,
		DiskFreeBytes:     m.Disk.FreeBytes,
		DiskUsedBytes:     m.Disk.UsedBytes,
		DiskPct:           m.Disk.Pct,
		InferenceRSSBytes: m.InferenceRSSBytes,
	}
}

// ErrNotFound is surfaced by findings reads when the store is not wired.
var ErrStoreUnwired = errors.New("smith: store not wired")
