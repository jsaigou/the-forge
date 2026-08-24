// SPDX-License-Identifier: Apache-2.0

package smith

// build_refresh_ops.go implements every native Op the build_refresh
// procedure (procedures/build_refresh.go) declares, wired into
// runNativeOp's switch (procedure.go) — autonomous-remediation Sprint 6,
// docs/v5-smith.md §13.4. Every op resolves its target fork fresh, each
// call, via resolveBuildRefreshFork (build_refresh_forks.go) — never
// caches path/flag data across steps — and every external command still
// goes through Deps.RunStep with fixed argv composed here in Go, exactly
// the posture registry.go documents for a native Op: "the real allowlist
// stays in the native op handler itself."

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/smith/procedures"
	"github.com/jsaigou/the-forge/internal/store"
)

const (
	buildRefreshRecordTimeout    = 10 * time.Second
	buildRefreshFetchTimeout     = 60 * time.Second
	buildRefreshPrecheckTimeout  = 15 * time.Second
	buildRefreshRebaseTimeout    = 2 * time.Minute
	buildRefreshConfigureTimeout = 3 * time.Minute
	buildRefreshBuildTimeout     = 45 * time.Minute
	buildRefreshVerifyTimeout    = 30 * time.Second
	buildRefreshRelabelTimeout   = 10 * time.Second
	buildRefreshReliabilityHTTP  = 10 * time.Minute
	// buildRefreshGiantPrefillHTTP bounds the giant cold-prefill call
	// ALONE — sized for the slowest real canary class: eval run 75
	// measured the 1M-context nemotron prefill at ~88K real tokens
	// (480K chars ≈ 5.5 chars/token, not 4) processing at 36–54 t/s ≈
	// 30–40 min. The generic reliability budget above stays short for the
	// small completions; only the scenario the whole test exists for gets
	// the long leash.
	buildRefreshGiantPrefillHTTP = 60 * time.Minute
	buildRefreshJournalTimeout   = 15 * time.Second
	buildRefreshMetricsTimeout   = 10 * time.Second
	buildRefreshCleanupTimeout   = 30 * time.Second
	// buildRefreshGiantPrefillChars approximates ~120K tokens at ~4
	// chars/token (build-refresh.md §4's documented reliability scenario —
	// the exact scenario that caused the 2026-08-16 device-lost incident).
	// It is the LARGE-context target: canaries with smaller configured
	// context get a prompt scaled to what they can actually hold — see
	// giantPrefillCharsFor (eval run 72's HTTP 400: a 131072-context
	// canary physically cannot hold the 120K-token scenario).
	buildRefreshGiantPrefillChars = 480_000
)

// giantPrefillCharsFor sizes the giant-cold-prefill scenario to the
// canary: the full documented prompt when the canary's configured context
// can hold it, otherwise ~75% of that context at a conservative 3.5
// chars/token (dense-tokenizer-safe). A prompt the canary's own context
// cannot hold gets an HTTP 400 from llama-server before the test ever
// exercises the build — measuring nothing but the mismatch. nCtx <= 0
// (unknown config) keeps the full scenario: fail loudly on a real
// mismatch, never silently shrink the test.
func giantPrefillCharsFor(nCtx int) int {
	if nCtx <= 0 {
		return buildRefreshGiantPrefillChars
	}
	scaled := int(float64(nCtx) * 0.75 * 3.5)
	if scaled < buildRefreshGiantPrefillChars {
		return scaled
	}
	return buildRefreshGiantPrefillChars
}

// buildRefreshDirName is the fixed, stable build directory name per
// backend — reused across runs (an incremental cmake build on re-run,
// including the iterative fix→redeploy→re-run loop this sprint's
// evaluation uses) rather than a fresh timestamped dir every time.
func buildRefreshDirName(backend string) string { return "build-smith-" + backend }

// isBuildRefreshCandidatePath reports whether a build row's binary sits in
// one of this procedure's own build-smith-* directories — i.e. the row is
// a candidate (or an already-promoted candidate), not an original build.
// Every consumer of discovered builds must agree on this predicate: a
// reliability re-run that treated its own candidate rows as original
// builds would test candidates-of-candidates and repoint configs onto
// them (found via eval run 75's mid-reliability-test interruption).
func isBuildRefreshCandidatePath(b store.Build) bool {
	return strings.Contains(b.BinaryPath, "/"+buildRefreshDirName(b.Backend)+"/")
}

// runStep is a thin wrapper over Deps.RunStep for every op below — one
// place that fails closed when the seam is unwired, matching every other
// dispatch path's ErrProcedureUnwired convention.
func (s *Smith) runStep(ctx context.Context, timeout time.Duration, cwd string, argv ...string) (procedures.StepResult, error) {
	if s.d.RunStep == nil {
		return procedures.StepResult{}, ErrProcedureUnwired
	}
	return s.d.RunStep(ctx, procedures.StepSpec{Argv: argv, Cwd: cwd, Timeout: timeout})
}

// gitRootAllowed guards every build_refresh op before it ever execs
// anything against a SourceRef — same injection-guard shape as
// binaries.go's BinaryPathAllowed, but for a directory (a git worktree
// root) rather than a single binary file.
func gitRootAllowed(path string) (bool, string) {
	if path == "" {
		return false, "empty path"
	}
	if !filepath.IsAbs(path) {
		return false, "path must be absolute"
	}
	if strings.ContainsAny(path, "\t\n;|&$`") {
		return false, "path contains disallowed characters"
	}
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return false, "not a git worktree: " + err.Error()
	}
	_ = info
	return true, ""
}

// buildsUnderSourceTree is the pure matching rule behind
// buildsForSourceTree and propose.go's proposal-time blast-radius
// disclosure — one tree, one rule, so what a proposal DISCLOSES and what
// a run later DISCOVERS can never diverge. A build path is expected to
// look like "<sourceRef>/<builddir>/bin/llama-server"; anything else is
// skipped, not guessed at.
func buildsUnderSourceTree(all []store.Build, sourceRef string) []store.Build {
	var out []store.Build
	for _, b := range all {
		if b.BinaryPath == "" {
			continue
		}
		// .../<builddir>/bin/llama-server -> Dir -> .../<builddir>/bin ->
		// Dir -> .../<builddir> -> Dir -> sourceRef.
		root := filepath.Dir(filepath.Dir(filepath.Dir(b.BinaryPath)))
		if root == sourceRef {
			out = append(out, b)
		}
	}
	return out
}

// buildsForSourceTree discovers every catalog build whose binary lives
// under sourceRef — the Sprint 6 "G8" fix: rather than trust a single
// hardcoded backend per fork (which the tracked-binary/catalog mismatch
// found live on ForgeHost this sprint proved can be wrong), the real set of
// backends to refresh is read fresh off store.Catalog every run.
func (s *Smith) buildsForSourceTree(ctx context.Context, sourceRef string) ([]store.Build, error) {
	if s.d.Catalog == nil {
		return nil, ErrCatalogChangeUnwired
	}
	all, err := s.d.Catalog.ListBuilds(ctx)
	if err != nil {
		return nil, fmt.Errorf("smith: build_refresh: list builds: %w", err)
	}
	return buildsUnderSourceTree(all, sourceRef), nil
}

// configsForBuild returns every catalog config currently pointed at
// buildID — build_catalog_promote's repoint list and build_cleanup_old's
// safety check both need the same real, live query.
func (s *Smith) configsForBuild(ctx context.Context, buildID int64) ([]store.Config, error) {
	all, err := s.d.Catalog.ListConfigs(ctx)
	if err != nil {
		return nil, fmt.Errorf("smith: build_refresh: list configs: %w", err)
	}
	var out []store.Config
	for _, c := range all {
		if c.BuildID == buildID {
			out = append(out, c)
		}
	}
	return out, nil
}

// combineStepResults joins N sub-operations' output (one per discovered
// backend) into a single StepResult, labeled so the run journal shows
// which backend each block belongs to. Returns the first real error
// encountered (a build_refresh loop stops at the first failing backend
// rather than attempting the rest — a build that doesn't configure/compile
// cleanly needs a human's attention before anything downstream matters).
type labeledResult struct {
	label  string
	result procedures.StepResult
	err    error
}

func combineStepResults(parts []labeledResult) (procedures.StepResult, error) {
	var out procedures.StepResult
	var stdout, stderr strings.Builder
	for _, p := range parts {
		fmt.Fprintf(&stdout, "── %s ──\n%s\n", p.label, p.result.Stdout)
		if p.result.Stderr != "" {
			fmt.Fprintf(&stderr, "── %s ──\n%s\n", p.label, p.result.Stderr)
		}
		out.Duration += p.result.Duration
		if p.err != nil {
			out.Stdout, out.Stderr = stdout.String(), stderr.String()
			return out, fmt.Errorf("%s: %w", p.label, p.err)
		}
	}
	out.Stdout, out.Stderr = stdout.String(), stderr.String()
	return out, nil
}

// ── step 0: record the installed version ────────────────────────────────

func (s *Smith) opBuildRecordInstalled(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	tb, _, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	if ok, reason := BinaryPathAllowed(tb.Path); !ok {
		return procedures.StepResult{}, fmt.Errorf("smith: build_refresh: tracked path %q not allowed: %s", tb.Path, reason)
	}
	return s.runStep(ctx, buildRefreshRecordTimeout, "", tb.Path, "--version")
}

// ── step 1: fetch ────────────────────────────────────────────────────────

func (s *Smith) opBuildGitFetch(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	return s.runStep(ctx, buildRefreshFetchTimeout, fork.SourceRef, "git", "-C", fork.SourceRef, "fetch", fork.Remote)
}

// ── step 2: precheck ─────────────────────────────────────────────────────

// opBuildGitPrecheck refuses to touch a tree with real uncommitted work or
// an already-in-progress rebase. It deliberately does NOT refuse a
// detached HEAD: live inspection of ForgeHost this sprint found every tracked
// fork is normally operated detached, pinned to a specific tested commit
// (a sane way to run a production inference binary — a moving branch tip
// would let an unrelated `git pull` upstream silently move what's running)
// — `git rebase <upstream>` works perfectly well from a detached HEAD, so
// treating that as disqualifying would make even the intended "this should
// rebase cleanly" cases (the vulkan/puzzle forks) abort before ever
// reaching the interesting part of the evaluation. Only a genuinely dirty
// tree or a rebase already underway are real reasons to stop.
func (s *Smith) opBuildGitPrecheck(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	res, err := s.runStep(ctx, buildRefreshPrecheckTimeout, fork.SourceRef, "git", "-C", fork.SourceRef, "status", "--porcelain")
	if err != nil {
		return res, fmt.Errorf("smith: build_refresh: git status failed: %w", err)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return res, fmt.Errorf("smith: build_refresh: %s has uncommitted changes — refusing to rebase over real work:\n%s", fork.SourceRef, res.Stdout)
	}
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if _, statErr := os.Stat(filepath.Join(fork.SourceRef, ".git", dir)); statErr == nil {
			return res, fmt.Errorf("smith: build_refresh: %s already has a rebase in progress (.git/%s exists)", fork.SourceRef, dir)
		}
	}
	return res, nil
}

// ── step 3: rebase ───────────────────────────────────────────────────────

// opBuildGitRebase is deliberately idempotent-tolerant: if a human resolved
// a prior conflict pause by running `git rebase --continue` themselves
// outside Smith (the runbook's documented expectation — Smith is not
// resolving merge conflicts), HEAD is already based on upstream by the
// time this re-runs after checkpoint approval, and `git rebase <ref>`
// against an already-current tree is a real, harmless no-op success. If
// the conflict is still unresolved, this errors again exactly as it did
// the first time, correctly re-pausing at the same checkpoint.
func (s *Smith) opBuildGitRebase(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	return s.runStep(ctx, buildRefreshRebaseTimeout, fork.SourceRef, "git", "-C", fork.SourceRef, "rebase", fork.UpstreamRef)
}

// ── steps 4-5: configure + build, per discovered backend ────────────────

func (s *Smith) opBuildCmakeConfigure(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}
	if len(builds) == 0 {
		return procedures.StepResult{}, fmt.Errorf("smith: build_refresh: no catalog builds found under %s — nothing to refresh", fork.SourceRef)
	}
	var parts []labeledResult
	for _, b := range builds {
		flags, ok := fork.Backends[b.Backend]
		if !ok {
			return procedures.StepResult{}, fmt.Errorf("smith: build_refresh: catalog build %q (id %d) under %s has backend %q, which this fork's reviewed registry has no flags for", b.Name, b.ID, fork.SourceRef, b.Backend)
		}
		dir := filepath.Join(fork.SourceRef, buildRefreshDirName(b.Backend))
		argv := append([]string{"cmake", "-S", fork.SourceRef, "-B", dir, "-DCMAKE_BUILD_TYPE=Release"}, flags.ConfigureFlags...)
		res, cerr := s.runStep(ctx, buildRefreshConfigureTimeout, fork.SourceRef, argv...)
		parts = append(parts, labeledResult{label: b.Backend, result: res, err: cerr})
		if cerr != nil {
			break
		}
	}
	return combineStepResults(parts)
}

func (s *Smith) opBuildCmakeBuild(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}
	var parts []labeledResult
	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue // already reported by configure; nothing new to add here
		}
		dir := filepath.Join(fork.SourceRef, buildRefreshDirName(b.Backend))
		res, berr := s.runStep(ctx, buildRefreshBuildTimeout, fork.SourceRef, "cmake", "--build", dir, "-j", "32")
		parts = append(parts, labeledResult{label: b.Backend, result: res, err: berr})
		if berr != nil {
			break
		}
	}
	return combineStepResults(parts)
}

// ── step 6: verify each new binary ───────────────────────────────────────

// opBuildVerifyBinary re-runs build-refresh.md §3's three checks per
// backend: --version prints a fresh commit, RUNPATH carries the $ORIGIN
// literal (the documented "-b9859 RUNPATH" deploy-outage trap), and a
// clean-env `ldd` resolves ROCm libs from the intended install (the
// "7.13→7.15 silent-nothing trap"). RunStep's own child environment is
// ALREADY the minimal base (Sprint 6's G2 fix) — no separate `env -i`
// wrapper is needed to get a clean-env ldd; it's the default now.
func (s *Smith) opBuildVerifyBinary(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}
	var parts []labeledResult
	for _, b := range builds {
		flags, ok := fork.Backends[b.Backend]
		if !ok {
			continue
		}
		bin := filepath.Join(fork.SourceRef, buildRefreshDirName(b.Backend), "bin", "llama-server")
		if ok, reason := BinaryPathAllowed(bin); !ok {
			parts = append(parts, labeledResult{label: b.Backend + " --version", err: fmt.Errorf("built binary not allowed: %s", reason)})
			break
		}
		verRes, verErr := s.runStep(ctx, buildRefreshVerifyTimeout, "", bin, "--version")
		parts = append(parts, labeledResult{label: b.Backend + " --version", result: verRes, err: verErr})
		if verErr != nil {
			break
		}
		rpathRes, rpathErr := s.runStep(ctx, buildRefreshVerifyTimeout, "", "readelf", "-d", bin)
		if rpathErr == nil && !strings.Contains(rpathRes.Stdout, "$ORIGIN") {
			rpathErr = fmt.Errorf("RUNPATH does not contain $ORIGIN (build-refresh.md's documented deploy-outage trap): %s", rpathRes.Stdout)
		}
		parts = append(parts, labeledResult{label: b.Backend + " RUNPATH", result: rpathRes, err: rpathErr})
		if rpathErr != nil {
			break
		}
		lddRes, lddErr := s.runStep(ctx, buildRefreshVerifyTimeout, "", "ldd", bin)
		if lddErr == nil && flags.LibDirSubstring != "" && !strings.Contains(lddRes.Stdout, flags.LibDirSubstring) {
			lddErr = fmt.Errorf("ldd did not resolve libs from the intended install %q (7.13->7.15 silent-nothing trap):\n%s", flags.LibDirSubstring, lddRes.Stdout)
		}
		parts = append(parts, labeledResult{label: b.Backend + " ldd", result: lddRes, err: lddErr})
		if lddErr != nil {
			break
		}
	}
	return combineStepResults(parts)
}

// ── step 7: relabel ──────────────────────────────────────────────────────

func (s *Smith) opBuildRelabel(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}
	var parts []labeledResult
	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue
		}
		bin := filepath.Join(fork.SourceRef, buildRefreshDirName(b.Backend), "bin", "llama-server")
		res, rerr := s.runStep(ctx, buildRefreshRelabelTimeout, "", "chcon", "-t", "bin_t", bin)
		parts = append(parts, labeledResult{label: b.Backend, result: res, err: rerr})
		if rerr != nil {
			break
		}
	}
	return combineStepResults(parts)
}

// ── shared: load a config through the same evict-aware path the real
// load_config action uses ─────────────────────────────────────────────────

// loadConfigViaPlacer mirrors execute.go's dispatchLoadConfig placement
// logic (FitPlan for the slot + eviction list, evict, then Load) rather
// than reinventing slot selection — the one difference is this picks its
// own slot via FitPlan instead of trusting a pre-chosen one from an
// action's Detail, since build_refresh has no human-authored placement to
// defer to.
//
// Residency reconciliation (found live in eval run 62): a config already
// resident was started from whatever binary it pointed at BEFORE the
// caller repointed it — the engine's double-load guard refuses a second
// load, and even if it didn't, the running instance is the OLD binary,
// which is exactly what this call's caller is trying to replace. So a
// resident config is unloaded first; the fresh load then runs the current
// catalog state. The maintenance window the reliability step holds blocks
// NEW loads from racing in while this happens, but cannot retroactively
// evict what was already resident before the window opened — that is this
// code's job, not the window's.
func (s *Smith) loadConfigViaPlacer(ctx context.Context, configName string) (slot string, err error) {
	if s.d.Placer == nil {
		return "", ErrPlacerUnwired
	}
	if resident := s.findLoadedSlot(configName); resident != "" {
		if res := s.d.Placer.Unload(ctx, resident); !res.Success {
			return "", fmt.Errorf("smith: build_refresh: unload resident %s from %s before reloading it: %s", configName, resident, res.Message)
		}
	}
	plan, err := s.d.Placer.FitPlan(configName)
	if err != nil {
		return "", fmt.Errorf("smith: build_refresh: fit-plan %s: %w", configName, err)
	}
	if plan.Slot == "" {
		return "", fmt.Errorf("smith: build_refresh: %s won't fit: %s", configName, plan.Message)
	}
	for _, evict := range plan.Evict {
		if res := s.d.Placer.Unload(ctx, evict); !res.Success {
			return "", fmt.Errorf("smith: build_refresh: evict %s to make room for %s: %s", evict, configName, res.Message)
		}
	}
	result := s.d.Placer.Load(ctx, configName, plan.Slot)
	if !result.Success {
		return "", fmt.Errorf("smith: build_refresh: load %s onto %s: %s", configName, plan.Slot, result.Message)
	}
	return plan.Slot, nil
}

// ── step 8: reliability test ─────────────────────────────────────────────

// buildRefreshCandidateSuffix marks a catalog build row this procedure
// itself created and hasn't been promoted/cleaned up yet — used to tell a
// candidate apart from the pre-existing build it's meant to replace,
// purely by re-reading the catalog (never by threading state between
// steps; every op re-derives everything it needs fresh, same as the rest
// of this codebase).
const buildRefreshCandidateSuffix = " (smith refresh candidate)"

// giantPrefillParagraph is repeated to build the ~120K-token cold prefill
// build-refresh.md §4 documents as the exact scenario that caused the
// 2026-08-16 qwen38-27b device-lost incident — real generated content, not
// a network fetch, so this test has no external dependency.
const giantPrefillParagraph = "The scheduler places any mode on whichever slot is currently free, never pinned to one physical slot, and unified memory addressing lets the kernel satisfy a single large contiguous allocation from either classic GTT-backed pages or HMM-backed system memory depending on which pool has room at request time. "

func buildGiantPrefillPrompt(chars int) string {
	var b strings.Builder
	for b.Len() < chars {
		b.WriteString(giantPrefillParagraph)
	}
	return b.String()
}

// directCompletion POSTs a minimal OpenAI-compatible chat completion
// directly to a slot's own loopback port (bypassing a0/headroom
// entirely — this is testing the raw llama-server build itself, not the
// routing layer in front of it), following streamChatCompletion's own
// documented Transport-reuse-without-Timeout pattern so a long real
// prefill isn't cut off by HTTPClient's ~3s healthz-probe timeout.
func (s *Smith) directCompletion(ctx context.Context, slot, model, prompt string, maxTokens int) (time.Duration, error) {
	if s.d.HTTPClient == nil {
		return 0, ErrHTTPUnwired
	}
	base, err := s.directSlotBaseURL(slot)
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": maxTokens,
		"stream":     false,
	})
	if err != nil {
		return 0, fmt.Errorf("smith: build_refresh: marshal completion request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("smith: build_refresh: build completion request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forge-Requested-By", "smith")
	client := &http.Client{Transport: s.d.HTTPClient.Transport}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), fmt.Errorf("smith: build_refresh: completion request: %w", err)
	}
	defer resp.Body.Close()
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusOK {
		return elapsed, fmt.Errorf("smith: build_refresh: completion returned HTTP %d", resp.StatusCode)
	}
	var decoded struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return elapsed, fmt.Errorf("smith: build_refresh: decode completion response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return elapsed, fmt.Errorf("smith: build_refresh: completion returned no choices")
	}
	return elapsed, nil
}

// deviceLostPatterns are build-refresh.md §4.5's documented failure
// signatures — checked with a plain, case-sensitive-off substring scan
// (Go has no journalctl grep -iE equivalent worth building for four fixed
// strings).
var deviceLostPatterns = []string{"ErrorDeviceLost", "device wedged", "vk::Queue::submit", "ring_timeout", "ring timeout"}

func scanJournalForDeviceLost(stdout string) []string {
	lower := strings.ToLower(stdout)
	var hits []string
	for _, p := range deviceLostPatterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			hits = append(hits, p)
		}
	}
	return hits
}

// opBuildReliabilityTest is the one step that touches live traffic —
// NeedsMaintenance is declared true on it (build_refresh.go), so the
// maintenance window is already held by the time this runs. Per backend:
// creates a real, durable candidate catalog build row for the just-built
// binary, repoints ONE representative config at it (the runbook's own
// documented shortcut — "register a test config, or repoint the real
// one"), loads it, runs the giant-cold-prefill scenario plus one
// unload/reload cycle, and scans the journal for the documented
// device-lost signatures. Any failure reverts the representative config's
// BuildID back to what it was before this ran — production is never left
// pointed at an unvetted binary just because a test failed.
func (s *Smith) opBuildReliabilityTest(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	if s.d.Catalog == nil {
		return procedures.StepResult{}, ErrCatalogChangeUnwired
	}
	// reliabilityTestOneBackend's later direct Placer.Unload/Load calls are
	// only safe because loadConfigViaPlacer (which nil-checks) always runs
	// first in the same call — checked here too, explicitly, so that
	// ordering isn't the only thing standing between a nil Placer and a
	// daemon-crashing panic (matches opBuildCatalogPromote's same check).
	if s.d.Placer == nil {
		return procedures.StepResult{}, ErrPlacerUnwired
	}
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}

	var parts []labeledResult
	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue
		}
		if isBuildRefreshCandidatePath(b) {
			// A re-run (crash-resume or a second procedure run over the
			// same tree) discovers its own candidate rows alongside the
			// originals; testing one would create a candidate-of-a-
			// candidate and repoint the canary onto it. Only original
			// builds are test subjects.
			continue
		}
		label := b.Backend
		res, rerr := s.reliabilityTestOneBackend(ctx, fork, b)
		parts = append(parts, labeledResult{label: label, result: res, err: rerr})
		if rerr != nil {
			break
		}
	}
	return combineStepResults(parts)
}

func (s *Smith) reliabilityTestOneBackend(ctx context.Context, fork buildRefreshFork, oldBuild store.Build) (procedures.StepResult, error) {
	var out procedures.StepResult
	var log strings.Builder
	start := time.Now()

	repName, ok := fork.RepresentativeConfig[oldBuild.Backend]
	if !ok {
		return out, fmt.Errorf("no representative config declared for backend %s", oldBuild.Backend)
	}
	cfg, err := s.d.Catalog.ConfigByName(ctx, repName)
	if err != nil {
		return out, fmt.Errorf("resolve representative config %q: %w", repName, err)
	}
	originalBuildID := cfg.BuildID

	newBin := filepath.Join(fork.SourceRef, buildRefreshDirName(oldBuild.Backend), "bin", "llama-server")
	candidateID, adopted, err := s.findOrCreateCandidateBuild(ctx, oldBuild, newBin)
	if err != nil {
		return out, err
	}
	if adopted {
		// The run journal must say so — an operator reading why a row
		// already exists deserves the real history, not a mystery.
		fmt.Fprintf(&log, "adopted existing candidate build id %d for %s (%s) from an earlier interrupted run\n", candidateID, oldBuild.Name, newBin)
	} else {
		fmt.Fprintf(&log, "candidate build id %d created for %s (%s)\n", candidateID, oldBuild.Name, newBin)
	}

	revert := func(reason error) error {
		target := reliabilityRevertTarget(originalBuildID, candidateID, oldBuild.ID)
		if target != originalBuildID {
			fmt.Fprintf(&log, "revert anchor for %s was the candidate itself (an earlier interrupted attempt repointed it); restoring build id %d instead\n", repName, target)
		}
		cfg.BuildID = target
		if uerr := s.d.Catalog.UpdateConfig(ctx, cfg); uerr != nil {
			fmt.Fprintf(&log, "REVERT FAILED for config %s: %v (original error: %v)\n", repName, uerr, reason)
		} else {
			fmt.Fprintf(&log, "reverted config %s back to build id %d\n", repName, target)
		}
		out.Stdout = log.String()
		out.Duration = time.Since(start)
		return reason
	}

	cfg.BuildID = candidateID
	if err := s.d.Catalog.UpdateConfig(ctx, cfg); err != nil {
		return out, fmt.Errorf("repoint %s to candidate build: %w", repName, err)
	}
	fmt.Fprintf(&log, "repointed config %s to candidate build id %d\n", repName, candidateID)

	slot, err := s.loadConfigViaPlacer(ctx, repName)
	if err != nil {
		revertErr := revert(fmt.Errorf("load %s on the candidate build: %w", repName, err))
		return out, revertErr
	}
	fmt.Fprintf(&log, "%s loaded on %s\n", repName, slot)

	prefillChars := giantPrefillCharsFor(cfg.NCtx)
	prefillCtx, cancel := context.WithTimeout(ctx, buildRefreshGiantPrefillHTTP)
	elapsed, err := s.directCompletion(prefillCtx, slot, repName, buildGiantPrefillPrompt(prefillChars), 8)
	cancel()
	if err != nil {
		revertErr := revert(fmt.Errorf("giant cold-prefill test: %w", err))
		return out, revertErr
	}
	fmt.Fprintf(&log, "giant cold prefill (%d chars, sized to the canary's %d-token context) completed in %s\n", prefillChars, cfg.NCtx, elapsed)

	if res := s.d.Placer.Unload(ctx, slot); !res.Success {
		revertErr := revert(fmt.Errorf("unload/reload cycle: unload %s: %s", slot, res.Message))
		return out, revertErr
	}
	if reloaded := s.d.Placer.Load(ctx, repName, slot); !reloaded.Success {
		revertErr := revert(fmt.Errorf("unload/reload cycle: reload %s: %s", repName, reloaded.Message))
		return out, revertErr
	}
	tinyCtx, tinyCancel := context.WithTimeout(ctx, 30*time.Second)
	if _, err := s.directCompletion(tinyCtx, slot, repName, "Reply with the single word: ok.", 8); err != nil {
		tinyCancel()
		revertErr := revert(fmt.Errorf("post-reload health completion: %w", err))
		return out, revertErr
	}
	tinyCancel()
	fmt.Fprintf(&log, "unload/reload cycle on %s: ok\n", slot)

	if sl, ok := s.cfg().Slots[slot]; ok && sl.Unit != "" {
		jctx, jcancel := context.WithTimeout(ctx, buildRefreshJournalTimeout)
		jres, jerr := s.runStep(jctx, buildRefreshJournalTimeout, "", "journalctl", "-u", sl.Unit, "-n", "500", "--no-pager")
		jcancel()
		if jerr == nil {
			if hits := scanJournalForDeviceLost(jres.Stdout); len(hits) > 0 {
				revertErr := revert(fmt.Errorf("journal for %s shows device-lost signature(s): %s", sl.Unit, strings.Join(hits, ", ")))
				return out, revertErr
			}
			fmt.Fprintf(&log, "journal scan for %s: clean\n", sl.Unit)
		} else {
			fmt.Fprintf(&log, "journal scan for %s: could not read (%v) — not treated as a failure, just unconfirmed\n", sl.Unit, jerr)
		}
	}

	out.Stdout = log.String()
	out.Duration = time.Since(start)
	return out, nil
}

// reliabilityRevertTarget returns the build ID a failed reliability test
// must restore the canary config to. Normally that's the build the config
// pointed at when this attempt started — but when an earlier attempt was
// interrupted between its repoint and its revert (eval run 75's mid-test
// kill), the config already sits on the candidate, so the per-attempt
// anchor IS the candidate, and restoring it would "revert" production
// onto the very unvetted build the test exists to gate. In that case the
// true pre-run build is the backend's original row.
func reliabilityRevertTarget(originalBuildID, candidateID, oldBuildID int64) int64 {
	if originalBuildID == candidateID {
		return oldBuildID
	}
	return originalBuildID
}

// findOrCreateCandidateBuild locates a candidate build row a prior
// interrupted or failed run already created for this (old build, backend)
// and adopts it — or creates a fresh one when none exists. Without
// adoption, every failed retry leaves an orphaned " (smith refresh
// candidate)" row behind (eval run 62's failure left exactly one: the
// vulkan candidate for a reliability test that never got to run), and the
// catalog slowly fills with rows nothing references. The build directory
// itself is deliberately kept either way — a retry rebuilds it
// incrementally instead of recompiling from scratch.
func (s *Smith) findOrCreateCandidateBuild(ctx context.Context, oldBuild store.Build, newBin string) (int64, bool, error) {
	wantName := oldBuild.Name + buildRefreshCandidateSuffix
	all, err := s.d.Catalog.ListBuilds(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("list builds for candidate adoption: %w", err)
	}
	for _, b := range all {
		if b.Name == wantName && b.Backend == oldBuild.Backend && b.BinaryPath == newBin {
			return b.ID, true, nil
		}
	}
	id, err := s.d.Catalog.CreateBuild(ctx, store.Build{
		EngineID: oldBuild.EngineID, Name: wantName,
		BinaryPath: newBin, Backend: oldBuild.Backend,
		Reason: "autonomous-remediation Sprint 6 build_refresh candidate, pending promotion",
	})
	if err != nil {
		return 0, false, fmt.Errorf("create candidate build row: %w", err)
	}
	return id, false, nil
}

// ── step 9: perf measurement (informational — see build_refresh.go's
// doc comment on the scope reduction from the runbook's full vulkan-vs-
// rocm bake-off) ──────────────────────────────────────────────────────────

func (s *Smith) opBuildPerfMeasure(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}
	var parts []labeledResult
	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue
		}
		repName, ok := fork.RepresentativeConfig[b.Backend]
		if !ok {
			continue
		}
		res, perr := s.measureDecodeTPS(ctx, repName)
		parts = append(parts, labeledResult{label: b.Backend + " " + repName, result: res, err: perr})
		// A perf-measurement failure is informational, not fatal — the
		// checkpoint that follows this step shows whatever evidence was
		// gathered, missing figures included, rather than aborting a
		// build that already passed reliability over a /metrics scrape
		// hiccup.
	}
	out, err := combineStepResults(parts)
	if err != nil {
		return out, err
	}
	// The promote decision checkpoint fires right after this step. The
	// operator approves a promote against a NAMED blast radius — the exact
	// configs that will be repointed and to which candidate — never
	// against a tracked label that may misdescribe what the run actually
	// touches (the G8 lesson: "llama.cpp (vulkan)" backed nemotron's rocm
	// build, and only live inspection caught it). A plan that can't be
	// computed is disclosed as such — the decision gets worse evidence,
	// never silently less.
	if note, perr := s.promoteRepointPlan(ctx, fork, builds); perr == nil {
		out.CheckpointNote = note
	} else {
		out.CheckpointNote = "WARNING: the promote repoint plan could not be computed (" + perr.Error() + ") — review the run journal carefully before approving."
	}
	return out, nil
}

// promoteRepointPlan enumerates exactly what build_catalog_promote will
// change if the operator approves the upcoming checkpoint: every config
// still on each old build under this tree, and the candidate build it
// will be repointed to. Read-only — it re-derives everything from the
// live catalog the same way promote itself will, so it can never drift
// from what actually happens.
func (s *Smith) promoteRepointPlan(ctx context.Context, fork buildRefreshFork, builds []store.Build) (string, error) {
	var lines []string
	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue
		}
		if isBuildRefreshCandidatePath(b) {
			continue // already the candidate — promote never migrates away from it
		}
		candidateID, err := s.findCandidateBuildID(ctx, builds, b.Backend)
		if err != nil {
			return "", err
		}
		configs, err := s.configsForBuild(ctx, b.ID)
		if err != nil {
			return "", err
		}
		if len(configs) == 0 {
			lines = append(lines, fmt.Sprintf("%s (build %d, %s): no configs to repoint", b.Name, b.ID, b.Backend))
			continue
		}
		names := make([]string, len(configs))
		for i, c := range configs {
			names[i] = c.Name
		}
		lines = append(lines, fmt.Sprintf("%s (build %d, %s) → candidate build %d: %s",
			b.Name, b.ID, b.Backend, candidateID, strings.Join(names, ", ")))
	}
	return "Promote will repoint: " + strings.Join(lines, " | ") + ". Approve to promote; abort to leave the candidates parked.", nil
}

// measureDecodeTPS reuses build-refresh.md §5's own documented technique:
// a /metrics counter delta around one small real completion. Errors are
// returned but (per the caller above) never fail the run.
func (s *Smith) measureDecodeTPS(ctx context.Context, configName string) (procedures.StepResult, error) {
	slot := s.findLoadedSlot(configName)
	if slot == "" {
		return procedures.StepResult{}, fmt.Errorf("%s is not currently loaded — cannot measure", configName)
	}
	base, err := s.directSlotBaseURL(slot)
	if err != nil {
		return procedures.StepResult{}, err
	}
	before, err := s.scrapeDecodeCounters(ctx, base)
	if err != nil {
		return procedures.StepResult{}, fmt.Errorf("scrape /metrics before: %w", err)
	}
	if _, err := s.directCompletion(ctx, slot, configName, "Write a short paragraph about scheduler design.", 300); err != nil {
		return procedures.StepResult{}, fmt.Errorf("measurement completion: %w", err)
	}
	after, err := s.scrapeDecodeCounters(ctx, base)
	if err != nil {
		return procedures.StepResult{}, fmt.Errorf("scrape /metrics after: %w", err)
	}
	dTok, dSec := after.tokens-before.tokens, after.seconds-before.seconds
	msg := fmt.Sprintf("%s: unmeasurable (zero elapsed seconds in the delta)", configName)
	if dSec > 0 {
		msg = fmt.Sprintf("%s: %.1f decode t/s (%.0f tokens / %.2fs)", configName, dTok/dSec, dTok, dSec)
	}
	return procedures.StepResult{Stdout: msg}, nil
}

type decodeCounters struct{ tokens, seconds float64 }

func (s *Smith) scrapeDecodeCounters(ctx context.Context, base string) (decodeCounters, error) {
	if s.d.HTTPClient == nil {
		return decodeCounters{}, ErrHTTPUnwired
	}
	mctx, cancel := context.WithTimeout(ctx, buildRefreshMetricsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(mctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return decodeCounters{}, err
	}
	client := &http.Client{Transport: s.d.HTTPClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return decodeCounters{}, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return decodeCounters{}, err
	}
	var out decodeCounters
	for _, line := range strings.Split(buf.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "llamacpp:tokens_predicted_total"):
			out.tokens = lastFloatField(line)
		case strings.HasPrefix(line, "llamacpp:tokens_predicted_seconds_total"):
			out.seconds = lastFloatField(line)
		}
	}
	return out, nil
}

func lastFloatField(line string) float64 {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[len(fields)-1], 64)
	return v
}

// findLoadedSlot reports which slot (if any) currently has configName
// loaded, via the same live scheduler status dispatchLoadConfig already
// reads — "" when not currently loaded.
func (s *Smith) findLoadedSlot(configName string) string {
	if s.d.Sched == nil {
		return ""
	}
	for slot, mode := range s.d.Sched.Status().Slots {
		if mode == configName {
			return slot
		}
	}
	return ""
}

// ── step 10: promote the candidate to every remaining config ────────────

// opBuildCatalogPromote is Sprint 6's G9 fix — the first real execution
// path for a smith-driven catalog write (sourcing.go's applyCatalogChange
// has stood unimplemented since P6). For each backend, finds the candidate
// build step 8 already created and vetted, finds every OTHER config still
// on the old build, repoints each to the candidate (reloading it live only
// when it's actually resident right now — a config that isn't currently
// loaded picks up the new build on its own next natural load, with no
// forced cycle here), and reverts every repoint made in THIS call if any
// one of them fails partway through — never a half-migrated fleet.
func (s *Smith) opBuildCatalogPromote(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	if s.d.Catalog == nil {
		return procedures.StepResult{}, ErrCatalogChangeUnwired
	}
	// Every other dispatcher in this codebase nil-checks Deps.Placer before
	// ever calling it (dispatchUnloadSlot, loadConfigViaPlacer above) — this
	// step reaches a direct s.d.Placer.Unload/Load call further down only
	// when a repointed config happens to be resident right now, so the nil
	// case is easy to miss in testing and would otherwise panic (crashing
	// the whole daemon: executeAction's spawning goroutine has no recover).
	if s.d.Placer == nil {
		return procedures.StepResult{}, ErrPlacerUnwired
	}
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}

	var log strings.Builder
	start := time.Now()
	type migrated struct {
		cfg             store.Config
		originalBuildID int64
	}
	var done []migrated
	revertAll := func(reason error) error {
		for _, m := range done {
			m.cfg.BuildID = m.originalBuildID
			if uerr := s.d.Catalog.UpdateConfig(ctx, m.cfg); uerr != nil {
				fmt.Fprintf(&log, "REVERT FAILED for config %s: %v\n", m.cfg.Name, uerr)
			} else {
				fmt.Fprintf(&log, "reverted config %s to build id %d\n", m.cfg.Name, m.originalBuildID)
			}
		}
		return reason
	}

	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue
		}
		if isBuildRefreshCandidatePath(b) {
			continue // this IS the candidate, not an old build to migrate away from
		}
		candidateID, ferr := s.findCandidateBuildID(ctx, builds, b.Backend)
		if ferr != nil {
			revertErr := revertAll(ferr)
			return procedures.StepResult{Stdout: log.String()}, revertErr
		}
		configs, cerr := s.configsForBuild(ctx, b.ID)
		if cerr != nil {
			revertErr := revertAll(cerr)
			return procedures.StepResult{Stdout: log.String()}, revertErr
		}
		for _, cfg := range configs {
			original := cfg.BuildID
			if original == candidateID {
				// An earlier interrupted promote attempt already repointed
				// this config; its per-attempt anchor is the candidate
				// itself, which would make revertAll "restore" it onto the
				// candidate. The true pre-promote build is this loop's old
				// build row (same reasoning as reliabilityRevertTarget).
				original = b.ID
			}
			cfg.BuildID = candidateID
			if uerr := s.d.Catalog.UpdateConfig(ctx, cfg); uerr != nil {
				revertErr := revertAll(fmt.Errorf("repoint config %s: %w", cfg.Name, uerr))
				return procedures.StepResult{Stdout: log.String()}, revertErr
			}
			done = append(done, migrated{cfg: cfg, originalBuildID: original})
			fmt.Fprintf(&log, "repointed config %s: build %d -> %d\n", cfg.Name, original, candidateID)

			if slot := s.findLoadedSlot(cfg.Name); slot != "" {
				if res := s.d.Placer.Unload(ctx, slot); !res.Success {
					revertErr := revertAll(fmt.Errorf("reload resident config %s: unload: %s", cfg.Name, res.Message))
					return procedures.StepResult{Stdout: log.String()}, revertErr
				}
				if res := s.d.Placer.Load(ctx, cfg.Name, slot); !res.Success {
					revertErr := revertAll(fmt.Errorf("reload resident config %s: load: %s", cfg.Name, res.Message))
					return procedures.StepResult{Stdout: log.String()}, revertErr
				}
				fmt.Fprintf(&log, "reloaded resident config %s on %s with the new build\n", cfg.Name, slot)
			}
		}
	}
	log.WriteString("promotion complete\n")
	return procedures.StepResult{Stdout: log.String(), Duration: time.Since(start)}, nil
}

// findCandidateBuildID locates backend's candidate row among an
// already-fetched builds list.
func (s *Smith) findCandidateBuildID(ctx context.Context, builds []store.Build, backend string) (int64, error) {
	for _, b := range builds {
		if b.Backend == backend && isBuildRefreshCandidatePath(b) {
			return b.ID, nil
		}
	}
	return 0, fmt.Errorf("no candidate build found for backend %s — did the reliability test step run and succeed?", backend)
}

// ── step 11: clean up the old build ──────────────────────────────────────

// opBuildCleanupOld is the one irreversible step — it runs only after the
// second checkpoint (promote decision, then this). Confirms no config
// still references the old build row (a real, live re-check — never
// assumed from step 10's own bookkeeping alone) before removing the row
// and its on-disk directory.
func (s *Smith) opBuildCleanupOld(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	if s.d.Catalog == nil {
		return procedures.StepResult{}, ErrCatalogChangeUnwired
	}
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	builds, err := s.buildsForSourceTree(ctx, fork.SourceRef)
	if err != nil {
		return procedures.StepResult{}, err
	}

	var log strings.Builder
	start := time.Now()
	for _, b := range builds {
		if _, ok := fork.Backends[b.Backend]; !ok {
			continue
		}
		if isBuildRefreshCandidatePath(b) {
			continue // the candidate — now the live build, never cleaned up
		}
		stillUsed, cerr := s.configsForBuild(ctx, b.ID)
		if cerr != nil {
			return procedures.StepResult{Stdout: log.String()}, cerr
		}
		if len(stillUsed) > 0 {
			names := make([]string, len(stillUsed))
			for i, c := range stillUsed {
				names[i] = c.Name
			}
			return procedures.StepResult{Stdout: log.String()}, fmt.Errorf("old build %q (id %d) still referenced by config(s) %s — refusing to delete", b.Name, b.ID, strings.Join(names, ", "))
		}
		oldDir := filepath.Dir(filepath.Dir(b.BinaryPath)) // .../<olddir>/bin -> .../<olddir>
		if ok, reason := gitRootAllowed(filepath.Dir(oldDir)); !ok {
			return procedures.StepResult{Stdout: log.String()}, fmt.Errorf("refusing to delete %s: parent %s failed the allowed-root check: %s", oldDir, filepath.Dir(oldDir), reason)
		}
		res, rerr := s.runStep(ctx, buildRefreshCleanupTimeout, "", "rm", "-rf", oldDir)
		if rerr != nil {
			return procedures.StepResult{Stdout: log.String() + res.Stdout}, fmt.Errorf("remove %s: %w", oldDir, rerr)
		}
		fmt.Fprintf(&log, "removed on-disk dir %s\n", oldDir)
		if uerr := s.d.Catalog.UpdateBuild(ctx, store.Build{ID: b.ID, EngineID: b.EngineID, Name: "(retired) " + b.Name, BinaryPath: "", Backend: b.Backend, Reason: b.Reason + " — retired by build_refresh cleanup"}); uerr != nil {
			fmt.Fprintf(&log, "WARNING: removed %s on disk but could not clear catalog build row %d: %v\n", oldDir, b.ID, uerr)
		} else {
			fmt.Fprintf(&log, "cleared catalog build row %d\n", b.ID)
		}
	}
	log.WriteString("cleanup complete\n")
	return procedures.StepResult{Stdout: log.String(), Duration: time.Since(start)}, nil
}

// ── upstream-nightly tracking (P3smith): record what a build was made from ──

// buildRefreshLsRemoteTimeout bounds one git ls-remote probe — a network
// round trip to the fork's upstream, so it gets its own leash independent
// of the caller's context (which may be a whole multi-hour run).
const buildRefreshLsRemoteTimeout = 30 * time.Second

// shortSha returns the 7-char short form both sides of every upstream-sha
// comparison use (git ls-remote prints full shas; the recorded value may be
// either). Shorter-than-7 inputs pass through unchanged — never panic on
// truncated data.
func shortSha(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// opBuildRecordUpstreamSha is build_refresh's P3smith step: after the tree
// has been fetched/rebased/built/verified, record WHICH upstream revision
// this run built from, so binary_versions' nightly drift mode can compare
// future remote HEADs against something real. No-op success for forks
// without tracking enabled (the procedure step is unconditional; tracking
// is per-fork opt-in). Fails closed when tracking IS enabled but the sha
// can't be probed or persisted — a silently-unrecorded build would make
// every later drift report meaningless.
func (s *Smith) opBuildRecordUpstreamSha(ctx context.Context, params map[string]string) (procedures.StepResult, error) {
	start := time.Now()
	_, fork, err := s.resolveBuildRefreshFork(ctx, params["binary"])
	if err != nil {
		return procedures.StepResult{}, err
	}
	rows, err := s.loadForkUpstreamRows(ctx)
	if err != nil {
		return procedures.StepResult{}, err
	}
	var row *forkUpstreamTrack
	for i := range rows {
		if rows[i].SourceRef == fork.SourceRef {
			row = &rows[i]
			break
		}
	}
	eff := effectiveForkUpstream(fork, row)
	if !eff.TrackUpstream || eff.UpstreamURL == "" {
		return procedures.StepResult{
			Stdout:   fmt.Sprintf("%s: upstream-nightly tracking not enabled — nothing recorded", fork.SourceRef),
			Duration: time.Since(start),
		}, nil
	}
	if ok, reason := upstreamURLAllowed(eff.UpstreamURL); !ok {
		return procedures.StepResult{}, fmt.Errorf("smith: build_refresh: tracked upstream url %q not allowed: %s", eff.UpstreamURL, reason)
	}
	if s.d.GitLsRemote == nil {
		return procedures.StepResult{}, ErrGitLsRemoteUnwired
	}
	lctx, cancel := context.WithTimeout(ctx, buildRefreshLsRemoteTimeout)
	sha, err := s.d.GitLsRemote(lctx, eff.UpstreamURL)
	cancel()
	if err != nil || sha == "" {
		return procedures.StepResult{}, fmt.Errorf("smith: build_refresh: ls-remote %s: %v", eff.UpstreamURL, err)
	}
	if err := s.recordForkLastBuiltSha(ctx, fork.SourceRef, sha); err != nil {
		return procedures.StepResult{}, err
	}
	return procedures.StepResult{
		Stdout:   fmt.Sprintf("recorded upstream HEAD %s (%s) as last_built_upstream_sha for %s", shortSha(sha), eff.UpstreamURL, fork.SourceRef),
		Duration: time.Since(start),
	}, nil
}
