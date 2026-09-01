// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith/comfyui"
	"github.com/jsaigou/the-forge/internal/smith/web"
	"github.com/jsaigou/the-forge/internal/store"
)

// Check categories (docs/v5-smith.md §4.2).
const (
	CategorySlots    = "slots"
	CategoryMemory   = "memory"
	CategoryGPU      = "gpu"
	CategoryNetwork  = "network"
	CategoryStorage  = "storage"
	CategoryDaemons  = "daemons"
	CategoryServices = "services"
	CategoryConfig   = "config"
)

// Sweep scope + persistence kinds (docs/v5-smith.md §4.2/§4.4).
const (
	ScopeQuick = "quick" // Fast checks only
	ScopeDeep  = "deep"  // every registered check

	SweepManual    = "manual"
	SweepScheduled = "scheduled"
	SweepAnomaly   = "anomaly"
)

// EventFindingsNew is the SSE event published after each sweep completes
// (Contract 1 amendment, docs/v5-smith.md §5).
const EventFindingsNew = "smith:findings_new"

// Check is one deterministic check (docs/v5-smith.md §4.2). Checks are pure
// reads over the CheckEnv — they never mutate anything, and they must
// degrade (SeverityInfo "skipped" findings) rather than fail when a dep they
// need is absent.
type Check struct {
	ID       string
	Name     string
	Category string
	Fast     bool // included in the quick sweep?
	// Skip, when set and returning true for this sweep's env, suppresses the
	// check entirely — no finding at all, not even a skip finding. Used by
	// checks gated on a "this module is disabled" setting (comfyui_health/
	// comfyui_prune under smith.comfyui.enabled=false): a disabled module
	// producing an info "skipped" row every sweep is the noise retention.go
	// exists to fight, so the dispatch layer drops it before it can be
	// persisted or proposed against.
	Skip func(env *CheckEnv) bool
	Run  func(ctx context.Context, env *CheckEnv) Finding
	// ManualOnly excludes this check from every scheduled sweep (quick AND
	// deep) — it only ever runs when explicitly selected by ID
	// (selectChecks' checkIDs branch, which is checked before scope and so
	// bypasses this entirely). comfyui_prune is the first user: proposing a
	// file deletion is not something that should happen unprompted in a
	// background sweep the operator never asked for (S7-followup smith UX
	// sprint, 2026-08-26) — detection now only runs from a dedicated
	// operator-triggered "check for unused files" action.
	ManualOnly bool
}

// CheckEnv carries everything checks may read. Built once per sweep by
// checkEnv; every field may be nil/zero — checks guard for it.
type CheckEnv struct {
	Snap        *collector.Snapshot
	Catalog     store.Catalog
	Sched       sched.Scheduler
	Store       *store.DB
	Cfg         func() *config.Config
	HTTP        *http.Client
	Dial        func(port int) bool
	CmdlinePath string
	Now         func() time.Time
	Thresholds  Thresholds
	// Web is the P5 web-research provider (docs/v5-smith.md §4.8), used
	// only by blocked_work_recheck. nil ⇒ that check reports itself
	// skipped, zero network calls.
	Web web.Service

	// JournalErrors is the Deps seam of the same name (forge-* unit
	// journals, bounded last-N) — gpu_device_lost's unit-journal source.
	JournalErrors func(ctx context.Context, n int, since time.Time) ([]string, error)
	// KernelJournal is the Deps seam of the same name (journalctl -k,
	// bounded last-N) — gpu_device_lost's kernel-ring source.
	KernelJournal func(ctx context.Context, n int, since time.Time) ([]string, error)
	// GitAhead is the Deps seam of the same name (git rev-list --count) —
	// binary_versions' upstream-drift probe.
	GitAhead func(ctx context.Context, root, ref string) (int, error)

	// GitBehindLog is the Deps seam of the same name (git log --format=%s)
	// — binary_versions' watchlist-match commit-subject source (S6 phase 2).
	GitBehindLog func(ctx context.Context, root, ref string, maxN int) ([]string, error)

	// BuildRefreshWatchlist is the resolved smith.build_refresh.watchlist
	// setting — keywords binary_versions matches against GitBehindLog's
	// fetched subjects. Empty when unset; the check then fetches nothing
	// (no reason to pay the extra git-log call with nothing to match).
	BuildRefreshWatchlist []string

	// GitLsRemote is the Deps seam of the same name (git ls-remote <url>
	// HEAD) — binary_versions' upstream-NIGHTLY drift probe (P3smith).
	GitLsRemote func(ctx context.Context, url string) (string, error)

	// ForkUpstreams is every build_refresh fork with upstream-nightly
	// tracking resolved ON (settings entry joined with its migration-0066
	// DB row; only track_upstream=1 forks with an allowed URL). Empty when
	// binaries are disabled or nothing is tracked — the nightly drift mode
	// then contributes nothing to binary_versions.
	ForkUpstreams []forkUpstreamTrack

	// BlockedItems is the blocked-work tracker parsed live from the
	// operator-local file (Deps.BlockedWorkPath) — blocked_work_recheck's
	// input. Empty when no path is wired or the file is absent (fresh
	// install): the check then reports ok/nothing-to-recheck.
	BlockedItems []BlockedItem

	// TailscalePeers is the Deps seam of the same name (P6 FR8), copied
	// through unchanged.
	TailscalePeers func(ctx context.Context) ([]collector.Peer, bool)
	// TailscaleWatchPeers is the resolved smith.tailscale.watch_peers
	// setting — hostnames (matched by NodeOnline's "<name>." DNSName prefix
	// rule) the tailscale_peers check treats as infra-critical. Empty when
	// Settings is nil or the key is unset/malformed.
	TailscaleWatchPeers []string

	// BinaryVersion is the Deps seam of the same name (P6 FR6), copied
	// through unchanged.
	BinaryVersion func(ctx context.Context, path string) (string, error)
	// TrackedBinaries is the resolved smith.binaries.tracked setting.
	// Already empty when BinariesEnabled is false — callers don't need to
	// check both.
	TrackedBinaries []TrackedBinary
	// BinariesEnabled is the resolved smith.binaries.enabled setting, kept
	// distinct from an empty TrackedBinaries so the check can tell "off" and
	// "nothing configured" apart in its summary text.
	BinariesEnabled bool

	// ComfyUI is the Deps seam of the same name (P6 FR7), copied through
	// unchanged.
	ComfyUI comfyui.Client
	// ComfyUIEnabled is the resolved smith.comfyui.enabled setting.
	ComfyUIEnabled bool
	// ComfyUIUnit is the resolved smith.comfyui.unit setting (the systemd
	// unit comfyui_health checks — "" when unset).
	ComfyUIUnit string
	// ComfyUIPort is parsed from smith.comfyui.url — comfyui_health's port
	// dial target. 0 when the URL is empty/unparseable/portless.
	ComfyUIPort int
	// ComfyUIModelRoots/ComfyUIWorkflowDirs are the resolved
	// smith.comfyui.{model_roots,workflow_dirs} settings — comfyui_prune's
	// BuildMap inputs.
	ComfyUIModelRoots   []string
	ComfyUIWorkflowDirs []string
	// ComfyUIKeepFiles is the resolved smith.comfyui.keep_files setting —
	// full paths proposeComfyUIDelete excludes from any delete proposal.
	ComfyUIKeepFiles []string

	// Logf is the shared diagnostic logger (Smith.logf, copied through
	// unchanged) — lets the free-function propose* helpers in propose.go
	// surface a swallowed error instead of silently dropping a proposal.
	// nil in every test literal; logf below is nil-safe for exactly that
	// reason.
	Logf func(format string, args ...any)
}

// cfg returns the infra config or nil when no Cfg func is wired.
func (e *CheckEnv) cfg() *config.Config {
	if e.Cfg == nil {
		return nil
	}
	return e.Cfg()
}

// logf is a nil-safe wrapper around Logf — every propose* free function can
// call e.logf(...) unconditionally, including in tests that build a bare
// &CheckEnv{}.
func (e *CheckEnv) logf(format string, args ...any) {
	if e == nil || e.Logf == nil {
		return
	}
	e.Logf(format, args...)
}

// registry is the check catalog (docs/v5-smith.md §4.2). Registry is
// data-ish Go, NOT store-backed — checks are code, their results are data.
// Order here is the stable execution + display order.
var registry = []Check{
	{
		ID: "gtt_ceiling", Name: "GTT ceiling", Category: CategoryMemory, Fast: true,
		Run: runGTTCeiling,
	},
	{
		ID: "disk_space", Name: "Disk space", Category: CategoryStorage, Fast: true,
		Run: runDiskSpace,
	},
	{
		ID: "binary_paths", Name: "Configured/catalog binary paths exist", Category: CategoryStorage, Fast: true,
		Run: runBinaryPaths,
	},
	{
		ID: "slot_agreement", Name: "Slot unit vs scheduler agreement", Category: CategorySlots, Fast: true,
		Run: runSlotAgreement,
	},
	{
		ID: "slot_model_identity", Name: "Configured vs actually-running model identity", Category: CategorySlots, Fast: true,
		Run: runSlotModelIdentity,
	},
	{
		ID: "n_ctx_actual", Name: "Configured vs actual n_ctx", Category: CategoryConfig, Fast: true,
		Run: runNCtxActual,
	},
	{
		ID: "gpu_hang", Name: "GPU hang indicators", Category: CategoryGPU, Fast: true,
		Run: runGPUHang,
	},
	{
		ID: "gpu_device_lost", Name: "GPU device-lost (kernel/unit journal)", Category: CategoryGPU, Fast: true,
		Run: runGPUDeviceLost,
	},
	{
		ID: "always_on_ports", Name: "Always-on service ports", Category: CategoryServices, Fast: true,
		Run: runAlwaysOnPorts,
	},
	{
		ID: "forge_self", Name: "forge self-check", Category: CategoryDaemons, Fast: true,
		Run: runForgeSelf,
	},
	{
		ID: "a0_reachability", Name: "a0 reachability", Category: CategoryNetwork, Fast: false,
		Run: runA0Reachability,
	},
	{
		ID: "compressor_reachability", Name: "Compressor per-proxy reachability", Category: CategoryNetwork, Fast: false,
		Run: runCompressorReachability,
	},
	{
		ID: "compressor_health", Name: "Compressor resource health", Category: CategoryMemory, Fast: false,
		Run: runCompressorHealth,
	},
	{
		ID: "compressor_failopen", Name: "Compressor fail-open rate", Category: CategoryServices, Fast: false,
		Run: runCompressorFailOpen,
	},
	{
		ID: "brain_resolvable", Name: "smith brain resolvable", Category: CategoryConfig, Fast: false,
		Run: runBrainResolvable,
	},
	{
		ID: "kernel_params", Name: "Kernel boot parameters", Category: CategoryConfig, Fast: false,
		Run: runKernelParams,
	},
	{
		ID: "blocked_work_recheck", Name: "Externally-blocked work recheck", Category: CategoryConfig, Fast: false,
		Run: runBlockedWorkRecheck,
	},
	{
		ID: "tailscale_peers", Name: "Watched tailscale peers", Category: CategoryNetwork, Fast: false,
		Run: runTailscalePeers,
	},
	{
		ID: "binary_versions", Name: "Tracked binary versions", Category: CategoryDaemons, Fast: false,
		Run: runBinaryVersions,
	},
	{
		ID: "comfyui_health", Name: "ComfyUI reachability", Category: CategoryServices, Fast: true,
		Skip: func(env *CheckEnv) bool { return !env.ComfyUIEnabled },
		Run:  runComfyUIHealth,
	},
	{
		ID: "comfyui_prune", Name: "ComfyUI unreferenced model files", Category: CategoryStorage, Fast: false,
		Skip:       func(env *CheckEnv) bool { return !env.ComfyUIEnabled },
		Run:        runComfyUIPrune,
		ManualOnly: true,
	},
}

// fastCheckCount returns how many checks run in the quick sweep.
func fastCheckCount() int {
	n := 0
	for _, c := range registry {
		if c.Fast {
			n++
		}
	}
	return n
}

// registryCheckIDs returns every registered check's ID (including
// ManualOnly ones — run_check and POST /smith/checks/run may still name
// them explicitly), in registry order. The single source both ListChecks
// (investigations.go, GET /api/v1/smith/checks) and the run_check tool's
// JSON-schema enum + partial-batch validation (tools.go) enumerate from, so
// neither can silently drift from what selectChecks actually accepts. Found
// live 2026-09-01: run_check's schema used to hand-list five example IDs
// out of ~20 real ones, so the reasoning tier hallucinated plausible-
// sounding ones that never existed (conversation 64).
func registryCheckIDs() []string {
	out := make([]string, len(registry))
	for i, c := range registry {
		out[i] = c.ID
	}
	return out
}

// registryCheckIDSet is registryCheckIDs as a membership set.
func registryCheckIDSet() map[string]bool {
	out := make(map[string]bool, len(registry))
	for _, c := range registry {
		out[c.ID] = true
	}
	return out
}

// checkEnv builds the per-sweep read environment.
func (s *Smith) checkEnv(ctx context.Context) *CheckEnv {
	env := &CheckEnv{
		Snap:        s.snapshot(),
		Catalog:     s.d.Catalog,
		Sched:       s.d.Sched,
		Store:       s.d.Store,
		Cfg:         s.d.Cfg,
		HTTP:        s.d.HTTPClient,
		Dial:        s.dialPort(),
		CmdlinePath: s.d.CmdlinePath,
		Now:         s.d.Now,
		Thresholds:  s.Thresholds(ctx),
		Web:         s.d.Web,

		TailscalePeers:      s.d.TailscalePeers,
		TailscaleWatchPeers: s.TailscaleWatchPeers(ctx),

		BlockedItems: s.ListBlockedItems(),

		BinaryVersion:   s.d.BinaryVersion,
		BinariesEnabled: s.BinariesEnabled(ctx),

		JournalErrors:         s.d.JournalErrors,
		KernelJournal:         s.d.KernelJournal,
		GitAhead:              s.d.GitAhead,
		GitBehindLog:          s.d.GitBehindLog,
		BuildRefreshWatchlist: s.BuildRefreshWatchlist(ctx),
		GitLsRemote:           s.d.GitLsRemote,

		ComfyUI:        s.d.ComfyUI,
		ComfyUIEnabled: s.ComfyUIEnabled(ctx),
		ComfyUIUnit:    s.ComfyUIUnit(ctx),
		ComfyUIPort:    urlPort(s.ComfyUIURL(ctx)),

		Logf: s.logf,
	}
	if env.BinariesEnabled {
		env.TrackedBinaries = s.TrackedBinaries(ctx)
		// Nightly-tracked forks (P3smith): resolved once per sweep. A
		// resolution failure degrades to an empty list — the nightly mode
		// contributes "unmeasurable" evidence, never fails the check.
		if tracks, err := s.resolvedForkUpstreams(ctx); err == nil {
			env.ForkUpstreams = tracks
		} else {
			s.logf("checkEnv: resolve fork upstream tracking: %v", err)
		}
	}
	if env.ComfyUIEnabled {
		env.ComfyUIModelRoots = s.ComfyUIModelRoots(ctx)
		env.ComfyUIWorkflowDirs = s.ComfyUIWorkflowDirs(ctx)
		env.ComfyUIKeepFiles = s.ComfyUIKeepFiles(ctx)
	}
	return env
}

// dialPort returns the Deps dialer, or a real loopback dial when none is
// wired (production). Compressor proxy ports are dynamic compressor_proxies
// rows, invisible to the collector's configured-ports map (probePorts
// dials cfg.Slots + cfg.Ports only) — reading Snap.Ports for them produced
// false "unhealthy" findings while the proxies were up (caught in
// live-verify, 2026-08-06).
func (s *Smith) dialPort() func(int) bool {
	if s.d.DialPort != nil {
		return s.d.DialPort
	}
	return dialLoopback
}

// urlPort parses port out of a "http://host:port" URL, or 0 on any
// unparseable/portless input.
func urlPort(rawURL string) int {
	u, err := url.Parse(rawURL)
	if err != nil || u.Port() == "" {
		return 0
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return 0
	}
	return port
}

// dialLoopback TCP-dials 127.0.0.1:<port> with a 1s timeout.
func dialLoopback(port int) bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// RunChecks executes a sweep and returns the findings.
//
// Selection: explicit checkIDs win when non-empty (unknown IDs are an
// error); otherwise scope selects — ScopeQuick runs Fast checks, ScopeDeep
// runs every registered check. sweepKind is the persistence attribution
// (manual|scheduled|anomaly).
//
// Findings are persisted to smith_findings when a Store is wired, and a
// smith:findings_new event is published when a Publisher is wired. The sweep
// is serialized: a concurrent RunChecks returns ErrAlreadyRunning.
func (s *Smith) RunChecks(ctx context.Context, scope string, checkIDs []string, sweepKind string) ([]Finding, error) {
	s.mu.Lock()
	if s.sweeping {
		s.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	s.sweeping = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.sweeping = false
		s.mu.Unlock()
	}()

	selected, err := selectChecks(scope, checkIDs)
	if err != nil {
		return nil, err
	}
	switch sweepKind {
	case SweepManual, SweepScheduled, SweepAnomaly:
	default:
		sweepKind = SweepManual
	}

	env := s.checkEnv(ctx)
	findings := runSelected(ctx, selected, env)

	at := s.d.Now()
	ids, err := s.persistFindings(ctx, findings, sweepKind, at, nil)
	if err != nil {
		// Persistence failure must not hide the findings from the caller —
		// log and return them anyway.
		s.logf("persist findings (%s sweep): %v", sweepKind, err)
	}
	s.proposeFrom(ctx, env, findings, ids, nil)

	if n, err := s.dedupCritFindings(ctx); err != nil {
		s.logf("dedup crit findings (%s sweep): %v", sweepKind, err)
	} else if n > 0 {
		s.logf("deduped %d duplicate crit finding(s) (%s sweep)", n, sweepKind)
	}
	s.pruneInfoTier(ctx)

	s.mu.Lock()
	s.lastSweepAt = at
	s.lastSweepKind = sweepKind
	s.lastSweepFindings = len(findings)
	s.mu.Unlock()

	if s.d.Publisher != nil {
		s.d.Publisher.Publish(EventFindingsNew, map[string]any{
			"sweep_kind": sweepKind,
			"count":      len(findings),
			"worst":      string(worstSeverity(findings)),
			"swept_at":   at.Unix(),
			"check_ids":  checkIDsOf(findings),
		})
	}
	return findings, nil
}

// runSelected runs checks against env and returns normalized findings. Pure
// — no persistence, no locking, no proposal generation. RunChecks (the
// sweep entry point) wraps this with the s.sweeping lock, persistFindings,
// and proposeFrom; the P7 tool loop's run_check tool (tools.go) calls this
// directly instead, since a tool must never take the sweep lock or create
// smith_actions rows (docs/v5-smith.md §9's read-only tool-loop guarantee).
func runSelected(ctx context.Context, checks []Check, env *CheckEnv) []Finding {
	findings := make([]Finding, 0, len(checks))
	for _, c := range checks {
		if c.Skip != nil && c.Skip(env) {
			continue
		}
		f := runOne(ctx, c, env)
		findings = append(findings, f.normalize())
	}
	return findings
}

// runOne executes one check, containing any panic as a crit finding — a
// check bug must take the check down, never the sweep (or the daemon).
func runOne(ctx context.Context, c Check, env *CheckEnv) (f Finding) {
	defer func() {
		if r := recover(); r != nil {
			f = Finding{
				CheckID:  c.ID,
				Severity: SeverityCrit,
				Summary:  fmt.Sprintf("check %s panicked: %v", c.ID, r),
				Evidence: map[string]any{"panic": fmt.Sprint(r)},
			}
		}
	}()
	f = c.Run(ctx, env)
	if f.CheckID == "" {
		f.CheckID = c.ID
	}
	return f
}

// selectChecks resolves the sweep selection.
func selectChecks(scope string, checkIDs []string) ([]Check, error) {
	if len(checkIDs) > 0 {
		byID := make(map[string]Check, len(registry))
		for _, c := range registry {
			byID[c.ID] = c
		}
		out := make([]Check, 0, len(checkIDs))
		for _, id := range checkIDs {
			c, ok := byID[id]
			if !ok {
				return nil, fmt.Errorf("smith: unknown check id %q", id)
			}
			out = append(out, c)
		}
		return out, nil
	}
	switch scope {
	case ScopeQuick, "":
		out := make([]Check, 0, fastCheckCount())
		for _, c := range registry {
			if c.Fast {
				out = append(out, c)
			}
		}
		return out, nil
	case ScopeDeep:
		out := make([]Check, 0, len(registry))
		for _, c := range registry {
			if c.ManualOnly {
				continue
			}
			out = append(out, c)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("smith: unknown scope %q (want quick|deep)", scope)
	}
}

// findCheckByID returns the registered Check with the given ID, or a zero
// Check when absent (callers check c.ID == "" or the zero Run is nil).
func findCheckByID(id string) Check {
	for _, c := range registry {
		if c.ID == id {
			return c
		}
	}
	return Check{}
}

// worstSeverity returns the highest severity among findings (ok when empty).
func worstSeverity(findings []Finding) Severity {
	worst := SeverityOK
	for _, f := range findings {
		if f.Severity.Rank() > worst.Rank() {
			worst = f.Severity
		}
	}
	return worst
}

// checkIDsOf collects the check IDs in finding order (stable, for events).
func checkIDsOf(findings []Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.CheckID)
	}
	return out
}

// skipFinding is the standard "cannot run" outcome — info, never a failure.
func skipFinding(checkID, reason string) Finding {
	return Finding{
		CheckID:  checkID,
		Severity: SeverityInfo,
		Summary:  "skipped: " + reason,
		Evidence: map[string]any{"skipped": reason},
	}
}

// pctOf computes used/total×100, guarding the zero divisor.
func pctOf(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// ── Check implementations ────────────────────────────────────────────────────

// runGTTCeiling — GTT allocation is the binding constraint on ForgeHost
// (AGENTS.md "Known Inference Constraints"). warn ≥ gtt_warn_pct (85),
// crit ≥ gtt_crit_pct (95); both tunable via smith.thresholds.
func runGTTCeiling(_ context.Context, env *CheckEnv) Finding {
	const id = "gtt_ceiling"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	m := env.Snap.Metrics
	if m.GTTUsedBytes == nil || m.GTTTotalBytes == nil || *m.GTTTotalBytes <= 0 {
		return skipFinding(id, "GTT metrics unavailable this cycle")
	}
	used, total := *m.GTTUsedBytes, *m.GTTTotalBytes
	pct := pctOf(used, total)
	ev := map[string]any{
		"gtt_used_bytes":  used,
		"gtt_total_bytes": total,
		"gtt_pct":         round2(pct),
		"warn_pct":        env.Thresholds.GTTWarnPct,
		"crit_pct":        env.Thresholds.GTTCritPct,
	}
	switch {
	case pct >= env.Thresholds.GTTCritPct:
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  fmt.Sprintf("GTT at %.1f%% of ceiling (≥%.0f%% crit)", pct, env.Thresholds.GTTCritPct),
			Evidence: ev, KBRefs: []string{"pitfalls:gtt-ceiling"}}
	case pct >= env.Thresholds.GTTWarnPct:
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("GTT at %.1f%% of ceiling (≥%.0f%% warn)", pct, env.Thresholds.GTTWarnPct),
			Evidence: ev, KBRefs: []string{"pitfalls:gtt-ceiling"}}
	default:
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:  fmt.Sprintf("GTT at %.1f%% of ceiling", pct),
			Evidence: ev}
	}
}

// runDiskSpace — models/data volume used% against disk_warn_pct/disk_crit_pct.
func runDiskSpace(_ context.Context, env *CheckEnv) Finding {
	const id = "disk_space"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	d := env.Snap.Metrics.Disk
	if d.TotalBytes <= 0 {
		return skipFinding(id, "disk probe unavailable this cycle")
	}
	pct := d.Pct
	if pct == 0 {
		pct = pctOf(d.UsedBytes, d.TotalBytes)
	}
	ev := map[string]any{
		"disk_total_bytes": d.TotalBytes,
		"disk_free_bytes":  d.FreeBytes,
		"disk_used_bytes":  d.UsedBytes,
		"disk_pct":         round2(pct),
		"warn_pct":         env.Thresholds.DiskWarnPct,
		"crit_pct":         env.Thresholds.DiskCritPct,
	}
	switch {
	case pct >= env.Thresholds.DiskCritPct:
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  fmt.Sprintf("disk %.1f%% used (≥%.0f%% crit)", pct, env.Thresholds.DiskCritPct),
			Evidence: ev}
	case pct >= env.Thresholds.DiskWarnPct:
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("disk %.1f%% used (≥%.0f%% warn)", pct, env.Thresholds.DiskWarnPct),
			Evidence: ev}
	default:
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:  fmt.Sprintf("disk %.1f%% used", pct),
			Evidence: ev}
	}
}

// runSlotAgreement — the collector's unit-observed slot occupancy must agree
// with the scheduler's view. A mismatch is the orphaned-unit / stale-slot
// pitfall space. A slot the collector still sees deactivating while the
// scheduler already calls it empty is an expected transient, not a finding.
func runSlotAgreement(_ context.Context, env *CheckEnv) Finding {
	const id = "slot_agreement"
	if env.Sched == nil {
		return skipFinding(id, "scheduler not wired")
	}
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	schedSlots := env.Sched.Status().Slots
	collSlots := env.Snap.Slots

	// Union of slot names from both views.
	names := map[string]struct{}{}
	for name := range schedSlots {
		names[name] = struct{}{}
	}
	for name := range collSlots {
		names[name] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)

	mismatches := []map[string]any{}
	for _, name := range sorted {
		schedMode := schedSlots[name]
		collMode := ""
		if st, ok := collSlots[name]; ok {
			collMode = st.Mode
		}
		if schedMode == collMode {
			continue
		}
		// Expected transient: the unit is still unloading, so the collector
		// keeps the old mode visible while the scheduler has already cleared
		// the slot (TimeoutStopSec=300-class unloads take minutes).
		if st, ok := collSlots[name]; ok && st.Unloading != nil && schedMode == "" {
			continue
		}
		mismatches = append(mismatches, map[string]any{
			"slot":           name,
			"scheduler_mode": schedMode,
			"unit_mode":      collMode,
		})
	}

	ev := map[string]any{"slots_checked": len(sorted), "mismatches": mismatches}
	if len(mismatches) > 0 {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("%d slot(s) disagree between unit state and scheduler", len(mismatches)),
			Evidence: ev, KBRefs: []string{"pitfalls:orphaned-slot-unit"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("all %d slot(s) agree between unit state and scheduler", len(sorted)),
		Evidence: ev}
}

// runNCtxActual — the silent-GTT-reduction pitfall: llama.cpp may initialize
// a smaller context than configured when GTT allocation fails. For every
// loaded slot with a live /props scrape, compare configured n_ctx (merged
// config, which overlays the catalog configs) against the actual value.
func runNCtxActual(ctx context.Context, env *CheckEnv) Finding {
	const id = "n_ctx_actual"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	cfg := env.cfg()
	if cfg == nil && env.Catalog == nil {
		return skipFinding(id, "no config or catalog source for configured n_ctx")
	}

	type slotCtx struct {
		Slot       string `json:"slot"`
		Mode       string `json:"mode"`
		Configured int    `json:"configured"`
		Actual     int    `json:"actual"`
	}
	reduced := []slotCtx{}
	checked := []slotCtx{}
	for name, st := range env.Snap.Slots {
		if st.Mode == "" {
			continue
		}
		inf, ok := env.Snap.Inference[name]
		if !ok || inf.NCtx <= 0 {
			continue // not scraped (yet) — nothing to compare
		}
		configured := configuredNCtx(ctx, env, st.Mode)
		if configured <= 0 {
			continue // no configured value to compare against
		}
		row := slotCtx{Slot: name, Mode: st.Mode, Configured: configured, Actual: inf.NCtx}
		checked = append(checked, row)
		if inf.NCtx < configured {
			reduced = append(reduced, row)
		}
	}
	sort.Slice(checked, func(i, j int) bool { return checked[i].Slot < checked[j].Slot })
	sort.Slice(reduced, func(i, j int) bool { return reduced[i].Slot < reduced[j].Slot })

	ev := map[string]any{"checked": checked, "reduced": reduced}
	if len(checked) == 0 {
		return skipFinding(id, "no loaded slot has a live n_ctx scrape")
	}
	if len(reduced) > 0 {
		first := reduced[0]
		summary := fmt.Sprintf("slot %s (%s): actual n_ctx %d < configured %d — kernel silently reduced the context",
			first.Slot, first.Mode, first.Actual, first.Configured)
		if len(reduced) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(reduced)-1)
		}
		return Finding{CheckID: id, Severity: SeverityCrit, Summary: summary,
			Evidence: ev, KBRefs: []string{"pitfalls:silent-context-reduction"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("actual n_ctx matches configured on %d loaded slot(s)", len(checked)),
		Evidence: ev}
}

// configuredNCtx resolves the configured context for a mode: merged config
// first (it overlays catalog configs with Context = configs.n_ctx), catalog
// ConfigByName as fallback. 0 when unknown.
func configuredNCtx(ctx context.Context, env *CheckEnv, mode string) int {
	if cfg := env.cfg(); cfg != nil {
		if m, ok := cfg.Modes[mode]; ok && len(m.Services) > 0 && m.Services[0].Context > 0 {
			return m.Services[0].Context
		}
	}
	if env.Catalog != nil {
		if c, err := env.Catalog.ConfigByName(ctx, mode); err == nil && c.NCtx > 0 {
			return c.NCtx
		}
	}
	return 0
}

// runGPUHang — hang indicators: collector INFERENCE_HANG alerts (the hang
// detector's requests_processing>0 + stalled-TPS rule) plus the raw
// per-slot "requests in flight but ~0 tokens/sec" condition.
func runGPUHang(_ context.Context, env *CheckEnv) Finding {
	const id = "gpu_hang"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	hung := []map[string]any{}
	for _, a := range env.Snap.Alerts {
		if a.Code == "INFERENCE_HANG" {
			hung = append(hung, map[string]any{
				"source": "collector_alert", "port": a.Port, "msg": a.Msg,
			})
		}
	}
	for name, inf := range env.Snap.Inference {
		if inf.RequestsProcessing > 0 && inf.TokensPerSecond < 0.1 {
			hung = append(hung, map[string]any{
				"source":              "metrics_stall",
				"slot":                name,
				"requests_processing": inf.RequestsProcessing,
				"tokens_per_second":   inf.TokensPerSecond,
			})
		}
	}
	sort.Slice(hung, func(i, j int) bool {
		return fmt.Sprint(hung[i]["source"], hung[i]["slot"], hung[i]["port"]) <
			fmt.Sprint(hung[j]["source"], hung[j]["slot"], hung[j]["port"])
	})

	ev := map[string]any{"hung": hung}
	if len(hung) > 0 {
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  fmt.Sprintf("%d GPU hang indicator(s) active", len(hung)),
			Evidence: ev, KBRefs: []string{"pitfalls:inference-hang"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  "no GPU hang indicators",
		Evidence: ev}
}

// runGPUDeviceLost — device-lost detection from the journals, the signal
// gpu_hang misses. A Vulkan/ROCm device-lost (amdgpu "ring comp_X.Y timeout"
// → "device wedged" in the kernel journal, "ErrorDeviceLost" /
// "vk::Queue::submit" in the forge-a* unit journal) leaves llama-server's
// /health green — the process survives, only every request errors — so the
// collector's stall detector and the engine's health check never fire (the
// 2026-08-16 qwen38-27b incident: unresponsive 26 min, /health ok the whole
// time). This check reads the two journal seams directly. crit when a
// device-lost signature is present in either journal.
func runGPUDeviceLost(ctx context.Context, env *CheckEnv) Finding {
	const id = "gpu_device_lost"
	if env.JournalErrors == nil && env.KernelJournal == nil {
		return skipFinding(id, "no journal seams wired (JournalErrors/KernelJournal)")
	}

	// Bound the read to a real window — without --since, a resolved
	// incident's journal lines read as a fresh crit indefinitely, which is
	// what made autorecover's confirmation gate unable to tell a live
	// device-lost from a stale one (found live 2026-08-18).
	windowMin := env.Thresholds.DeviceLostWindowMinutes
	if windowMin <= 0 {
		windowMin = DefaultThresholds().DeviceLostWindowMinutes
	}
	now := time.Now()
	if env.Now != nil {
		now = env.Now()
	}
	since := now.Add(-time.Duration(windowMin) * time.Minute)

	ev := map[string]any{"since": since.Format(time.RFC3339), "window_minutes": windowMin}
	matches := []map[string]any{}

	// Kernel journal: the amdgpu driver's own words.
	if env.KernelJournal != nil {
		lines, err := env.KernelJournal(ctx, 300, since)
		if err != nil {
			ev["kernel_journal_error"] = err.Error()
		} else {
			kernel := matchLines(lines, `ring\s+comp_\S+\s+timeout`, `device\s+wedged`, `ErrorDeviceLost`)
			if len(kernel) > 0 {
				matches = append(matches, map[string]any{"source": "kernel_journal", "matches": kernel})
			}
		}
	}

	// Unit journals: llama-server's send_error lines.
	if env.JournalErrors != nil {
		lines, err := env.JournalErrors(ctx, 300, since)
		if err != nil {
			ev["unit_journal_error"] = err.Error()
		} else {
			unit := matchLines(lines, `ErrorDeviceLost`, `vk::Queue::submit`, `device\s+lost`)
			if len(unit) > 0 {
				matches = append(matches, map[string]any{"source": "unit_journal", "matches": unit})
			}
		}
	}

	ev["matches"] = matches
	if len(matches) > 0 {
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  fmt.Sprintf("%d device-lost signature(s) in journals (last %dm)", len(matches), windowMin),
			Evidence: ev, KBRefs: []string{"pitfalls:inference-hang"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("no GPU device-lost signatures in journals (last %dm)", windowMin),
		Evidence: ev}
}

// matchLines returns up to 5 recent lines matching any of the regexes,
// newest first (the journal seams return most-recent-last, so iterate from
// the end). Bounded both by count and line length — evidence must never
// bloat a finding row.
func matchLines(lines []string, patterns ...string) []string {
	if len(lines) == 0 || len(patterns) == 0 {
		return nil
	}
	re := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if r, err := regexp.Compile(p); err == nil {
			re = append(re, r)
		}
	}
	var out []string
	for i := len(lines) - 1; i >= 0 && len(out) < 5; i-- {
		line := lines[i]
		if len(line) > 300 {
			line = line[:300]
		}
		for _, r := range re {
			if r.MatchString(line) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

// runAlwaysOnPorts — every configured auxiliary-service port (embedding,
// stt, tts, aligner, …) should have a listener. Anything down is surfaced;
// inference still works, so severity caps at warn.
func runAlwaysOnPorts(_ context.Context, env *CheckEnv) Finding {
	const id = "always_on_ports"
	cfg := env.cfg()
	if cfg == nil || len(cfg.Ports) == 0 {
		return skipFinding(id, "no auxiliary service ports configured")
	}
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}
	down := []map[string]any{}
	names := make([]string, 0, len(cfg.Ports))
	for name := range cfg.Ports {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		port := cfg.Ports[name]
		if !env.Snap.Ports[port] {
			down = append(down, map[string]any{"service": name, "port": port})
		}
	}
	ev := map[string]any{"checked": len(names), "down": down}
	if len(down) > 0 {
		first := down[0]
		summary := fmt.Sprintf("service %q not listening on port %v", first["service"], first["port"])
		if len(down) > 1 {
			summary += fmt.Sprintf(" (+%d more)", len(down)-1)
		}
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: summary, Evidence: ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("all %d always-on service port(s) listening", len(names)),
		Evidence: ev}
}

// runForgeSelf — the daemon's own integrity surface: PRAGMA quick_check
// plus a foreign_key_check. Known-live FK violations (4 in compatibilities
// as of 2026-08-06) surface as info — smith surfaces them, never auto-fixes
// (docs/v5-smith.md §10 item 7).
func runForgeSelf(ctx context.Context, env *CheckEnv) Finding {
	const id = "forge_self"
	if env.Store == nil {
		return skipFinding(id, "store not wired")
	}
	ev := map[string]any{}

	var quick string
	if err := env.Store.SQL().QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&quick); err != nil {
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  "DB integrity check failed to run: " + err.Error(),
			Evidence: map[string]any{"quick_check_error": err.Error()}}
	}
	ev["quick_check"] = quick
	if quick != "ok" {
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  "DB integrity: PRAGMA quick_check reported " + quick,
			Evidence: ev}
	}

	rows, err := env.Store.SQL().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		// quick_check passed; FK check unavailable is a degraded read, not
		// an integrity failure.
		ev["foreign_key_check_error"] = err.Error()
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:  "DB integrity ok (foreign_key_check unavailable)",
			Evidence: ev}
	}
	defer rows.Close()
	fkTables := map[string]int{}
	fkCount := 0
	for rows.Next() {
		var table string
		var rowid int64
		var referred string
		var fkID int
		if err := rows.Scan(&table, &rowid, &referred, &fkID); err == nil {
			fkCount++
			fkTables[table]++
		}
	}
	ev["fk_violations"] = fkCount
	ev["fk_violation_tables"] = fkTables
	if fkCount > 0 {
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary:  fmt.Sprintf("DB integrity ok; %d known foreign-key violation(s) surfaced (not auto-fixed)", fkCount),
			Evidence: ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  "DB integrity ok (quick_check + foreign_key_check clean)",
		Evidence: ev}
}

// runA0Reachability — loopback probe of a0's unauthenticated /healthz
// (docs/v5-smith.md §4.2, FR3). a0 is the only supported way in for agents,
// so an unreachable a0 is crit even though local slots may still serve.
func runA0Reachability(ctx context.Context, env *CheckEnv) Finding {
	const id = "a0_reachability"
	cfg := env.cfg()
	if cfg == nil {
		return skipFinding(id, "config not wired")
	}
	port := listenPort(cfg.Server.RouterListen)
	if port == 0 {
		return skipFinding(id, "a0 listen address has no port: "+cfg.Server.RouterListen)
	}
	if env.HTTP == nil {
		return skipFinding(id, "http client not wired")
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	start := env.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return skipFinding(id, "build request: "+err.Error())
	}
	resp, err := env.HTTP.Do(req)
	latencyMS := float64(env.Now().Sub(start).Milliseconds())
	if err != nil {
		return Finding{CheckID: id, Severity: SeverityCrit,
			Summary:  fmt.Sprintf("a0 unreachable on port %d: %v", port, err),
			Evidence: map[string]any{"url": url, "error": err.Error()}}
	}
	defer resp.Body.Close()
	ev := map[string]any{"url": url, "status": resp.StatusCode, "latency_ms": latencyMS}
	if resp.StatusCode != http.StatusOK {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("a0 healthz returned HTTP %d", resp.StatusCode),
			Evidence: ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("a0 healthy on port %d (%.0fms)", port, latencyMS),
		Evidence: ev}
}

// runCompressorReachability — per-proxy health (docs/v5-smith.md §4.2, FR3):
// reproduces the 2026-07-29 black-hole failure mode as coded detection. For
// every registered, non-orphaned proxy row: the observed unit must be
// active AND the proxy's port must answer a direct loopback dial — proxy
// ports are dynamic rows, absent from the collector's configured-ports map,
// so Snap.Ports cannot be used here. When no store is wired, falls back to
// scanning the snapshot for forge-compress@* units.
func runCompressorReachability(ctx context.Context, env *CheckEnv) Finding {
	const id = "compressor_reachability"
	if env.Snap == nil {
		return skipFinding(id, "no collector snapshot")
	}

	type proxyState struct {
		Service string `json:"service"`
		Unit    string `json:"unit"`
		Port    int    `json:"port"`
		Active  bool   `json:"active"`
		PortUp  bool   `json:"port_up"`
	}
	states := []proxyState{}

	if env.Store != nil {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		proxies, err := env.Store.Routing().Proxies(ctx)
		cancel()
		if err != nil {
			return skipFinding(id, "proxy list unavailable: "+err.Error())
		}
		for _, p := range proxies {
			if !p.OrphanedAt.IsZero() {
				continue
			}
			st := proxyState{Service: p.Service, Unit: p.Unit, Port: p.Port}
			if u, ok := env.Snap.Units[p.Unit]; ok {
				st.Active = u.Active()
			}
			if p.Port > 0 && env.Dial != nil {
				st.PortUp = env.Dial(p.Port)
			}
			states = append(states, st)
		}
	} else {
		// Fallback: whatever compressor units the collector observed.
		for unit, u := range env.Snap.Units {
			if !strings.HasPrefix(unit, "forge-compress@") {
				continue
			}
			states = append(states, proxyState{Service: unit, Unit: unit, Active: u.Active()})
		}
		sort.Slice(states, func(i, j int) bool { return states[i].Unit < states[j].Unit })
	}

	if len(states) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:  "no compressor proxies registered",
			Evidence: map[string]any{"proxies": states}}
	}

	down := []proxyState{}
	for _, st := range states {
		if !st.Active || (st.Port > 0 && !st.PortUp) {
			down = append(down, st)
		}
	}
	ev := map[string]any{"proxies": states, "down": down}
	if len(down) > 0 {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("%d of %d compressor prox(ies) unhealthy — remote routing may black-hole", len(down), len(states)),
			Evidence: ev, KBRefs: []string{"pitfalls:compressor-black-hole"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("all %d compressor prox(ies) healthy", len(states)),
		Evidence: ev}
}

// runCompressorHealth — resource health (Sprint 4, resource bounding +
// monitoring, docs/v5-headroom-replacement.md): a distinct check from
// compressor_reachability above, deliberately not a rewrite of it —
// compressor_reachability is reachability only (unit active + port dial),
// same one-check-one-concern
// pattern as gtt_ceiling vs. disk_space. This one reads each non-orphaned
// proxy's compressor_samples window and applies ClassifyCompressorHealth
// (compressor_health.go) — the same judgment the Dashboard's Compressor tile
// applies, so the two surfaces never disagree.
func runCompressorHealth(ctx context.Context, env *CheckEnv) Finding {
	const id = "compressor_health"
	if env.Store == nil {
		return skipFinding(id, "no store")
	}
	compressors := env.Store.Compressors()
	if compressors == nil {
		return skipFinding(id, "compressor samples unavailable")
	}

	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	proxies, err := env.Store.Routing().Proxies(pctx)
	cancel()
	if err != nil {
		return skipFinding(id, "proxy list unavailable: "+err.Error())
	}

	windowHours := env.Thresholds.CompressorRSSWindowHours
	if windowHours <= 0 {
		windowHours = DefaultThresholds().CompressorRSSWindowHours
	}
	since := time.Now().Add(-time.Duration(windowHours * float64(time.Hour)))

	type proxyHealth struct {
		Service      string  `json:"service"`
		Status       string  `json:"status"`
		RSSBytes     int64   `json:"rss_bytes,omitempty"`
		RSSGrowthPct float64 `json:"rss_growth_pct,omitempty"`
		RestartDelta int64   `json:"restart_delta,omitempty"`
	}
	var results []proxyHealth
	var warn []proxyHealth
	for _, p := range proxies {
		if !p.OrphanedAt.IsZero() {
			continue
		}
		rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		samples, err := compressors.Range(rctx, p.Service, since)
		cancel()
		if err != nil {
			continue // best-effort per proxy — one read failure doesn't sink the whole check
		}
		result := ClassifyCompressorHealth(samples, env.Thresholds)
		ph := proxyHealth{
			Service: p.Service, Status: result.Status,
			RSSBytes: result.WindowEnd.RSSBytes, RSSGrowthPct: result.RSSGrowthPct,
			RestartDelta: result.RestartDelta,
		}
		results = append(results, ph)
		if result.Status == "restarting" || result.Status == "memory_growth" {
			warn = append(warn, ph)
		}
	}

	if len(results) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary:  "no compressor proxies registered",
			Evidence: map[string]any{"proxies": results}}
	}

	ev := map[string]any{"proxies": results, "window_hours": windowHours}
	if len(warn) > 0 {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("%d of %d compressor(s) show leak-shaped growth or restart churn over %.0fh", len(warn), len(results), windowHours),
			Evidence: ev}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("all %d compressor(s) resource-healthy over %.0fh", len(results), windowHours),
		Evidence: ev}
}

// runCompressorFailOpen computes the per-proxy fail-open rate over a 1-hour
// rolling window. Fail-open rate = fail_open_total / (requests + timeout +
// canceled). When the rate crosses the CompressorFailOpenWarnPct threshold,
// the check emits a warn finding — the proposer then proposes raising the
// fail-open budget (proposeFailOpenBudgetIncrease in propose.go).
func runCompressorFailOpen(ctx context.Context, env *CheckEnv) Finding {
	const id = "compressor_failopen"
	if env.Store == nil {
		return skipFinding(id, "no store")
	}

	threshold := env.Thresholds.CompressorFailOpenWarnPct
	if threshold <= 0 {
		threshold = DefaultThresholds().CompressorFailOpenWarnPct
	}

	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	summaries, err := env.Store.Routing().SavingsSummary(pctx, time.Now().Add(-time.Hour))
	if err != nil {
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary: "failed to read savings: " + err.Error()}
	}

	type proxyResult struct {
		Service     string  `json:"service"`
		FailOpenPct float64 `json:"fail_open_pct"`
		FailOpens   int64   `json:"fail_opens"`
		Requests    int64   `json:"requests"`
	}
	var warn []proxyResult
	var ok []proxyResult

	for svc, s := range summaries {
		total := s.Requests + s.RequestsTimeout + s.RequestsCanceled
		if total == 0 {
			continue // no traffic — skip
		}
		pct := float64(s.FailOpenTotal) / float64(total) * 100
		result := proxyResult{Service: svc, FailOpenPct: pct, FailOpens: s.FailOpenTotal, Requests: total}
		if pct >= threshold {
			warn = append(warn, result)
		} else {
			ok = append(ok, result)
		}
	}

	if len(warn) > 0 {
		ev := map[string]any{"warn": warn, "ok": ok, "threshold_pct": threshold}
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("%d of %d compressor(s) exceed %.0f%% fail-open rate over 1h", len(warn), len(warn)+len(ok), threshold),
			Evidence: ev}
	}

	n := len(ok)
	if n == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary: "no compressor proxies with traffic in the last hour"}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  fmt.Sprintf("all %d compressor(s) below %.0f%% fail-open rate over 1h", n, threshold),
		Evidence: map[string]any{"ok": ok, "threshold_pct": threshold}}
}

// runBrainResolvable confirms smith.model currently resolves to a local
// slot or an enabled remote offering (mirrors Brain()'s resolution logic
// standalone, since Check funcs are pure functions over CheckEnv, not
// Smith methods). Used by executeAction's post-verify for settings_change
// actions that touch smith.model (handoff.go's brain-swap and swap-back)
// — see verifyChecksFor (execute.go) — but also runs in any deep sweep
// like any other registry check. "deterministic_only" is a legitimate
// steady state for smith in general (warn, not crit): this check exists so
// a brain-swap action's post-verify can tell the difference between "the
// swap worked" and "it silently didn't," not to police smith's brain
// setting in isolation.
func runBrainResolvable(ctx context.Context, env *CheckEnv) Finding {
	const id = "brain_resolvable"
	if env.Store == nil {
		return skipFinding(id, "store not wired")
	}
	raw, err := env.Store.Settings().Get(ctx, SettingModel)
	if err != nil || len(raw) == 0 {
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: "smith.model is unset — brain unresolvable"}
	}
	var model string
	if err := json.Unmarshal(raw, &model); err != nil {
		model = strings.TrimSpace(string(raw))
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return Finding{CheckID: id, Severity: SeverityWarn, Summary: "smith.model is empty — brain unresolvable"}
	}
	if env.Catalog == nil {
		return skipFinding(id, "catalog not wired")
	}

	if cfg, err := env.Catalog.ConfigByName(ctx, model); err == nil && cfg.ID != 0 {
		if env.Sched != nil {
			for _, mode := range env.Sched.Status().Slots {
				if mode == model {
					return Finding{CheckID: id, Severity: SeverityOK,
						Summary: "brain resolves to local config " + model, Evidence: map[string]any{"model": model}}
				}
			}
		}
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary: "configured brain " + model + " is not currently loaded on any slot", Evidence: map[string]any{"model": model}}
	}
	if offerings, err := env.Catalog.ListOfferings(ctx); err == nil {
		for _, o := range offerings {
			if o.Enabled && o.WireModel == model {
				return Finding{CheckID: id, Severity: SeverityOK,
					Summary: "brain resolves to remote offering via " + o.ProviderName, Evidence: map[string]any{"model": model, "provider": o.ProviderName}}
			}
		}
	}
	return Finding{CheckID: id, Severity: SeverityWarn,
		Summary: "smith.model " + model + " resolves to no local config or enabled offering", Evidence: map[string]any{"model": model}}
}

// kernelParamChecks are the FR8 boot parameters: the KFD-eviction
// mitigations pinned in AGENTS.md ("Kernel mitigations are applied").
var kernelParamChecks = []struct {
	Param string
	Want  string
}{
	{Param: "amdgpu.mcbp", Want: "0"},
	{Param: "amdgpu.vm_fragment_size", Want: "9"},
}

// runKernelParams — /proc/cmdline must carry the amdgpu mitigations.
// Missing params are a warn + guidance runbook (the fix needs a bootloader
// edit + reboot — never something smith executes).
func runKernelParams(_ context.Context, env *CheckEnv) Finding {
	const id = "kernel_params"
	raw, err := os.ReadFile(env.CmdlinePath)
	if err != nil {
		return skipFinding(id, "cannot read "+env.CmdlinePath+": "+err.Error())
	}
	cmdline := strings.TrimSpace(string(raw))
	fields := strings.Fields(cmdline)
	missing := []string{}
	present := []string{}
	for _, kc := range kernelParamChecks {
		want := kc.Param + "=" + kc.Want
		found := false
		for _, f := range fields {
			if f == want {
				found = true
				break
			}
		}
		if found {
			present = append(present, want)
		} else {
			missing = append(missing, want)
		}
	}
	ev := map[string]any{"present": present, "missing": missing}
	if len(missing) > 0 {
		return Finding{CheckID: id, Severity: SeverityWarn,
			Summary:  fmt.Sprintf("kernel boot params missing: %s (GPU eviction mitigations not applied)", strings.Join(missing, ", ")),
			Evidence: ev, KBRefs: []string{"runbook:kernel-boot-params"}}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary:  "kernel boot params carry the amdgpu mitigations",
		Evidence: ev}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// listenPort extracts the port from a ":8085" / "127.0.0.1:8085" listen
// address. 0 when unparseable.
func listenPort(addr string) int {
	if addr == "" {
		return 0
	}
	if _, portStr, err := net.SplitHostPort(addr); err == nil {
		if p, err := strconv.Atoi(portStr); err == nil {
			return p
		}
		return 0
	}
	// Bare ":8085" also parses via SplitHostPort; bare "8085" does not.
	if p, err := strconv.Atoi(strings.TrimPrefix(addr, ":")); err == nil {
		return p
	}
	return 0
}

// round2 rounds to 2 decimals for stable evidence values.
func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// evidenceJSON marshals a finding's evidence for persistence ({} on error —
// evidence must never block a persist).
func evidenceJSON(ev map[string]any) string {
	if ev == nil {
		return "{}"
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return "{}"
	}
	return string(b)
}
