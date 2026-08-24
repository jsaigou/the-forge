// SPDX-License-Identifier: Apache-2.0

package smith

// build_refresh_forks.go — the per-fork build facts the build_refresh
// procedure's native ops (build_refresh_ops.go) compose argv from
// (autonomous-remediation Sprint 6, docs/v5-smith.md §13.4).
//
// The registry is operator-editable settings JSON (smith.build_refresh.forks,
// seeded by migration 0061 with the code-reviewed entries — open-source-
// readiness finding 4: fork recipes are deployment data, unusable on any
// other install without code changes otherwise). The seeded entries were
// reviewed like code: cmake flags read directly off each tree's own real,
// already-working CMakeCache.txt (2026-08-20/21), carrying forward the two
// documented deploy-outage traps (build-refresh.md §2): the $ORIGIN RPATH
// literal (one argv element here — there is no shell layer) and poolside's
// `-DGGML_HIP_ROCWMMA_FATTN=ON` exception (upstream removed the flag
// everywhere else, PR #26046). Editing the setting is an operator decision
// under the same trust posture as smith.binaries.tracked — the procedure's
// propose→approve gate, promote checkpoint, and fail-closed cross-checks
// below still apply to whatever the setting says.
//
// Forks are keyed by SourceRef (the git tree's real filesystem root), not
// by the tracked binary's display Name — live inspection found
// smith.binaries.tracked can drift from the catalog `builds` table (Sprint
// 6's "G8"): whatever the catalog's `builds` table says today is what gets
// rebuilt (buildsForSourceTree, build_refresh_ops.go), never a hardcoded
// "this fork means vulkan/rocm".

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// buildBackendFlags is one (source tree, backend) pair's real cmake
// configure flags, minus -S/-B/-DCMAKE_BUILD_TYPE (the op supplies those —
// identical for every backend, so keeping them out of this table means one
// fewer place a copy-paste could diverge).
type buildBackendFlags struct {
	// Backend must match store.Build.Backend for the catalog builds this
	// flag set is meant to refresh ("vulkan" | "rocm") — cross-checked by
	// buildsForSourceTree so a discovered build with a backend this fork
	// has no flags for fails closed instead of guessing.
	Backend string `json:"backend"`
	// ConfigureFlags are appended verbatim to `cmake -S <tree> -B <dir>
	// -DCMAKE_BUILD_TYPE=Release`. Operator-reviewed settings data, never
	// templated, never LLM-composed.
	ConfigureFlags []string `json:"configure_flags"`
	// LibDirSubstring is checked for in a clean-env `ldd` of the built
	// binary (build_verify_binary) — confirms the binary actually
	// resolved its ROCm shared libs from the intended install, not a
	// stale one (build-refresh.md's "7.13→7.15 silent-nothing trap").
	// Empty for vulkan (nothing ROCm-specific to confirm).
	LibDirSubstring string `json:"lib_dir_substring,omitempty"`
}

// buildRefreshFork is one source tree's complete refresh recipe.
type buildRefreshFork struct {
	// SourceRef is the lookup key — must equal the live TrackedBinary's
	// own SourceRef exactly (verified at resolve time; a mismatch fails
	// closed rather than silently operating on the wrong tree).
	SourceRef string `json:"source_ref"`
	// Remote is the fetch remote name for build_git_fetch/build_git_rebase
	// (git remote -v, read live off each tree when the entry was reviewed).
	Remote string `json:"remote"`
	// UpstreamRef is what the rebase targets — copied from the tree's own
	// live smith.binaries.tracked entry at resolve time, NOT read from
	// this struct, so an operator edit to the tracked upstream_ref takes
	// effect without touching this registry; kept here only as the
	// expected value for a sanity cross-check (resolveBuildRefreshFork
	// errors loudly on a mismatch rather than silently trusting whichever
	// one disagrees).
	UpstreamRef string `json:"upstream_ref"`
	// Backends is keyed by store.Build.Backend.
	Backends map[string]buildBackendFlags `json:"backends"`
	// RepresentativeConfig names, per backend, the one real catalog Config
	// build_reliability_test repoints and loads to prove a refreshed
	// backend before promotion — the runbook's own documented shortcut
	// ("register a test config, or repoint the real one — see step 6").
	// Chosen as the smallest real consumer of that backend, never the
	// highest-risk one, so a bad build fails on the cheapest possible
	// canary. Sprint 6 eval corrections (2026-08-20/21): the original
	// vulkan pick's NAME read like a small model but was a 26B config with
	// a 52.9 GB live footprint, which crowded the budget enough to sink
	// the rocm canary's fit plan; the next pick (qwen3-swallow-8b) was
	// disqualified by the operator as translation-specialized on an old
	// architecture. gemma4-e4b-qat is smith's own brain config — acceptable
	// because brain residency stays off (stay_resident=false) and the
	// maintenance window blocks any new brain load during the test, but a
	// deployment that flips stay_resident on should re-review this pick.
	RepresentativeConfig map[string]string `json:"representative_config"`
	// UpstreamURL is the upstream-nightly tracking target (P3smith): an
	// https git URL whose HEAD (git ls-remote, read-only, timeout-bounded)
	// the binary_versions check compares against last_built_upstream_sha.
	// Optional — "" means this fork has no nightly tracking. A DB row in
	// smith_build_refresh_upstream (migration 0066) OVERRIDES this when
	// present, so tracking can be flipped without re-editing the reviewed
	// recipe JSON.
	UpstreamURL string `json:"upstream_url,omitempty"`
	// TrackUpstream enables the nightly drift mode for this fork. Default
	// false: today's behavior (operator-curated pinned repos) is unchanged
	// for every existing registry entry.
	TrackUpstream bool `json:"track_upstream,omitempty"`
}

// BuildRefreshForks reads smith.build_refresh.forks. Empty slice, never
// nil, when unset/unreadable — with no registered forks every build_refresh
// procedurization fails closed at resolution, which is the intended posture
// for a deployment that hasn't reviewed any trees yet.
func (s *Smith) BuildRefreshForks(ctx context.Context) []buildRefreshFork {
	raw, ok := s.settingJSON(ctx, SettingBuildRefreshForks)
	if !ok {
		return []buildRefreshFork{}
	}
	var forks []buildRefreshFork
	if err := json.Unmarshal(raw, &forks); err != nil {
		return []buildRefreshFork{}
	}
	if forks == nil {
		forks = []buildRefreshFork{}
	}
	return forks
}

// resolveBuildRefreshFork resolves params["binary"] (a smith.binaries.tracked
// Name, reshaped from the source runbook action's own Detail by
// procedurize.go — never operator free text) against the LIVE tracked-binary
// setting, then against the smith.build_refresh.forks registry by that
// entry's own SourceRef. Both lookups must agree, and the live entry's
// UpstreamRef must match the registry's reviewed expectation — any
// disagreement fails closed rather than silently operating on stale
// assumptions about a tree an operator may have reconfigured since the
// registry entry was reviewed.
func (s *Smith) resolveBuildRefreshFork(ctx context.Context, binaryName string) (TrackedBinary, buildRefreshFork, error) {
	var tb TrackedBinary
	found := false
	for _, cand := range s.TrackedBinaries(ctx) {
		if cand.Name == binaryName {
			tb, found = cand, true
			break
		}
	}
	if !found {
		return TrackedBinary{}, buildRefreshFork{}, fmt.Errorf("smith: build_refresh: %q is not in smith.binaries.tracked", binaryName)
	}
	if ok, reason := gitRootAllowed(tb.SourceRef); !ok {
		return TrackedBinary{}, buildRefreshFork{}, fmt.Errorf("smith: build_refresh: tracked binary %q source_ref %q not allowed: %s", binaryName, tb.SourceRef, reason)
	}
	var fork buildRefreshFork
	registered := false
	for _, cand := range s.BuildRefreshForks(ctx) {
		if cand.SourceRef == tb.SourceRef {
			fork, registered = cand, true
			break
		}
	}
	if !registered {
		return TrackedBinary{}, buildRefreshFork{}, fmt.Errorf("smith: build_refresh: no reviewed fork registered for source tree %q (tracked binary %q)", tb.SourceRef, binaryName)
	}
	if tb.UpstreamRef != fork.UpstreamRef {
		return TrackedBinary{}, buildRefreshFork{}, fmt.Errorf("smith: build_refresh: tracked binary %q upstream_ref %q disagrees with the reviewed fork registry's %q for %q — registry may be stale", binaryName, tb.UpstreamRef, fork.UpstreamRef, tb.SourceRef)
	}
	return tb, fork, nil
}

// ── upstream-nightly tracking state (P3smith) ───────────────────────────────
//
// forkUpstreamTrack is one fork's resolved upstream-tracking state: the
// settings entry's optional upstream_url/track_upstream overlaid with (and
// overridable by) the smith_build_refresh_upstream DB row (migration 0066),
// plus last_built_upstream_sha — runtime state that lives ONLY in the DB,
// written by build_refresh's build_record_upstream_sha step mid-run, never
// hand-edited in recipe JSON.
type forkUpstreamTrack struct {
	SourceRef     string
	UpstreamURL   string
	TrackUpstream bool
	LastBuiltSha  string // "" = no build recorded yet
}

// loadForkUpstreamRows reads every smith_build_refresh_upstream row. Empty
// slice, never nil; nil Store (tests, degraded daemons) reads as empty —
// tracking then simply doesn't resolve, which fails closed to "no nightly
// drift mode" rather than erroring a sweep.
func (s *Smith) loadForkUpstreamRows(ctx context.Context) ([]forkUpstreamTrack, error) {
	rows := []forkUpstreamTrack{}
	if s.d.Store == nil {
		return rows, nil
	}
	rs, err := s.d.Store.SQL().QueryContext(ctx,
		`SELECT source_ref, COALESCE(upstream_url, ''), track_upstream, COALESCE(last_built_upstream_sha, '')
		 FROM smith_build_refresh_upstream`)
	if err != nil {
		return nil, fmt.Errorf("smith: read smith_build_refresh_upstream: %w", err)
	}
	defer rs.Close()
	for rs.Next() {
		var t forkUpstreamTrack
		var track int
		if err := rs.Scan(&t.SourceRef, &t.UpstreamURL, &track, &t.LastBuiltSha); err != nil {
			return nil, fmt.Errorf("smith: scan smith_build_refresh_upstream: %w", err)
		}
		t.TrackUpstream = track != 0
		rows = append(rows, t)
	}
	return rows, rs.Err()
}

// effectiveForkUpstream merges a settings-registry entry with its DB row.
// The DB row contributes its URL when it carries one (the later operator
// edit), can ENABLE tracking, and always contributes last_built sha — but
// it never DISABLES tracking by omission: build_record_upstream_sha's
// upsert creates a sha-only row (track_upstream at its column default 0,
// URL NULL) as a routine side effect of building a settings-tracked fork,
// and that default-valued row must not flip the recipe's opt-in off. To
// stop tracking a fork, clear its settings entry (or point track/URL at a
// different value); the drift probe additionally requires a usable URL, so
// clearing the URL disables the mode everywhere.
func effectiveForkUpstream(fork buildRefreshFork, dbRow *forkUpstreamTrack) forkUpstreamTrack {
	out := forkUpstreamTrack{
		SourceRef:     fork.SourceRef,
		UpstreamURL:   fork.UpstreamURL,
		TrackUpstream: fork.TrackUpstream,
	}
	if dbRow == nil {
		return out
	}
	if dbRow.UpstreamURL != "" {
		out.UpstreamURL = dbRow.UpstreamURL
	}
	if dbRow.TrackUpstream {
		out.TrackUpstream = true
	}
	out.LastBuiltSha = dbRow.LastBuiltSha
	return out
}

// resolvedForkUpstreams joins the live fork registry with the DB rows and
// returns only genuinely-tracked forks (track_upstream AND a usable https
// URL). binary_versions consumes this for its nightly drift probe;
// build_record_upstream_sha resolves a single fork through the same merge.
func (s *Smith) resolvedForkUpstreams(ctx context.Context) ([]forkUpstreamTrack, error) {
	dbRows, err := s.loadForkUpstreamRows(ctx)
	if err != nil {
		return nil, err
	}
	byRef := make(map[string]forkUpstreamTrack, len(dbRows))
	for _, r := range dbRows {
		byRef[r.SourceRef] = r
	}
	var out []forkUpstreamTrack
	for _, fork := range s.BuildRefreshForks(ctx) {
		row := byRef[fork.SourceRef]
		var rowPtr *forkUpstreamTrack
		if _, ok := byRef[fork.SourceRef]; ok {
			rowPtr = &row
		}
		eff := effectiveForkUpstream(fork, rowPtr)
		if eff.TrackUpstream {
			if ok, _ := upstreamURLAllowed(eff.UpstreamURL); ok {
				out = append(out, eff)
			}
		}
	}
	return out, nil
}

// upstreamURLAllowed guards every git ls-remote invocation against a URL
// from settings/DB data (operator-editable, not compile-time constants):
// https(s) only, no whitespace/control/shell metacharacters — same
// injection-guard shape as BinaryPathAllowed/gitRootAllowed.
func upstreamURLAllowed(url string) (bool, string) {
	if url == "" {
		return false, "empty url"
	}
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return false, "only http(s) git URLs are allowed"
	}
	if strings.ContainsAny(url, "\t\n\r ;|&$`\"'\\") {
		return false, "url contains disallowed characters"
	}
	return true, ""
}

// recordForkLastBuiltSha upserts last_built_upstream_sha for one fork's
// source_ref. The full 40-char sha is stored; comparisons elsewhere use
// short prefixes on both sides.
func (s *Smith) recordForkLastBuiltSha(ctx context.Context, sourceRef, sha string) error {
	if s.d.Store == nil {
		return errors.New("smith: store not wired")
	}
	_, err := s.d.Store.SQL().ExecContext(ctx,
		`INSERT INTO smith_build_refresh_upstream (source_ref, upstream_url, track_upstream, last_built_upstream_sha, updated_at)
		 VALUES (?, NULL, 0, ?, strftime('%s','now'))
		 ON CONFLICT(source_ref) DO UPDATE SET last_built_upstream_sha = excluded.last_built_upstream_sha,
		   updated_at = excluded.updated_at`,
		sourceRef, sha)
	if err != nil {
		return fmt.Errorf("smith: record last-built upstream sha for %s: %w", sourceRef, err)
	}
	return nil
}
