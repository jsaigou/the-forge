// SPDX-License-Identifier: Apache-2.0

package smith

// checks_binaries.go implements smith P6's binary/dependency tracker
// (docs/v5-smith.md §4.9 FR6). Deep-sweep-only: both probes are cheap
// (a bounded exec, a couple of file reads) but there is no reason to run
// them every 60m — a build doesn't drift that fast.
//
// Two independent, always-measured-never-guessed signals per tracked
// binary (smith.binaries.tracked, migration 0038):
//  1. installed  — Deps.BinaryVersion execs "<path> --version".
//  2. source     — gitTreeVersion (binaries.go) reads the source tree's
//     checked-out commit with plain file reads, no git subprocess.
// A mismatch between the two (the installed binary's embedded commit hash
// isn't a prefix of the source tree's current HEAD) means the tree has
// moved past what's actually running — "rebuild recommended", the FR6
// finding this check exists to produce. Severity is deliberately capped at
// info: a stale build is never urgent enough to warrant the Console alert
// path warn/crit carries.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// nightlyLsRemoteTimeout bounds binary_versions' per-fork git ls-remote
// probe — a network round trip inside a check, so a tight leash: an
// unmeasurable result is visible evidence, never worth stalling the sweep.
const nightlyLsRemoteTimeout = 15 * time.Second

// gitBehindLogMaxN bounds GitBehindLog's fetch — a keyword match needs a
// screenful of subjects, not a full changelog.
const gitBehindLogMaxN = 50

// binaryStatus is one tracked binary's resolved state — the finding's
// evidence shape.
type binaryStatus struct {
	Name             string `json:"name"`
	Kind             string `json:"kind"`
	Path             string `json:"path"`
	InstalledVersion string `json:"installed_version"`
	InstalledKnown   bool   `json:"installed_known"`
	SourceRef        string `json:"source_ref,omitempty"`
	SourceVersion    string `json:"source_version,omitempty"`
	SourceKnown      bool   `json:"source_known"`
	Stale            bool   `json:"stale"` // both known and they disagree
	// UpstreamRef + UpstreamAhead: the 2026-08-17 build-refresh addition.
	// UpstreamAhead is how many commits the source tree is ahead of its
	// configured upstream ref (git rev-list --count HEAD..<ref>). A value > 0
	// means "the installed build lags upstream — a rebuild should be
	// considered" (the "is there a newer version" question, measured instead
	// of guessed). -1 = unmeasurable (no GitAhead seam, or no UpstreamRef
	// configured).
	UpstreamRef   string `json:"upstream_ref,omitempty"`
	UpstreamAhead int    `json:"upstream_ahead"`
	// Upstream-nightly tracking (P3smith): when the fork behind this
	// binary has track_upstream=1 and a usable upstream URL, NightlyURL is
	// that URL, NightlyHeadSha the remote HEAD's short sha (git ls-remote,
	// read-only, timeout-bounded), LastBuiltSha what build_refresh last
	// recorded building from (migration 0066), and NightlyDrift true when
	// the two shas disagree — i.e. upstream moved past the last build.
	NightlyURL     string `json:"upstream_nightly_url,omitempty"`
	NightlyHeadSha string `json:"upstream_head_sha,omitempty"`
	LastBuiltSha   string `json:"last_built_upstream_sha,omitempty"`
	NightlyDrift   bool   `json:"nightly_drift"`
	// UpstreamSubjects/WatchlistMatches: S6 phase 2 (feedback F1's
	// commit-subject-fetching half). Subjects are the HEAD..UpstreamRef
	// commit subject lines (Deps.GitBehindLog, same range UpstreamAhead
	// counts) — only fetched when there IS measurable drift and an operator
	// watchlist to match against, so a quiet host pays nothing extra.
	// WatchlistMatches is visibility only: it does not itself widen
	// proposeRebuildRunbook's threshold gate (see SettingBuildRefreshWatchlist's
	// doc comment) — an operator-flagged keyword surfaces even below-threshold
	// drift as worth a look, without smith auto-proposing anything wider.
	UpstreamSubjects []string `json:"upstream_subjects,omitempty"`
	WatchlistMatches []string `json:"watchlist_matches,omitempty"`
}

// matchWatchlist returns, for each subject line containing (case-
// insensitive substring) any watchlist keyword, "<keyword>: <subject>" —
// paired so the evidence/summary shows WHY a subject matched, not just that
// one did.
func matchWatchlist(subjects, watchlist []string) []string {
	if len(subjects) == 0 || len(watchlist) == 0 {
		return nil
	}
	var out []string
	for _, subj := range subjects {
		lower := strings.ToLower(subj)
		for _, kw := range watchlist {
			kw = strings.TrimSpace(kw)
			if kw == "" {
				continue
			}
			if strings.Contains(lower, strings.ToLower(kw)) {
				out = append(out, kw+": "+subj)
				break // one match per subject is enough evidence
			}
		}
	}
	return out
}

// runBinaryVersions is the check registered in registry (checks.go).
func runBinaryVersions(ctx context.Context, env *CheckEnv) Finding {
	const id = "binary_versions"
	if !env.BinariesEnabled {
		return skipFinding(id, "binary tracking disabled (smith.binaries.enabled is false)")
	}
	tracked := env.TrackedBinaries
	if len(tracked) == 0 {
		return Finding{CheckID: id, Severity: SeverityOK,
			Summary: "no tracked binaries configured (smith.binaries.tracked is empty)"}
	}

	statuses := make([]binaryStatus, 0, len(tracked))
	var stale []string
	var behind []string
	var driftBelow []string
	var watchlistHits []string
	// Nightly-tracked fork state by source_ref (P3smith).
	trackedByRef := map[string]forkUpstreamTrack{}
	for _, fu := range env.ForkUpstreams {
		trackedByRef[fu.SourceRef] = fu
	}
	var nightlyDrift []string
	var nightlyUnrecorded []string
	for _, tb := range tracked {
		st := binaryStatus{Name: tb.Name, Kind: tb.Kind, Path: tb.Path, SourceRef: tb.SourceRef,
			UpstreamRef: tb.UpstreamRef, UpstreamAhead: -1}

		if v, ok := installedProbe(ctx, env.BinaryVersion, tb.Path); ok {
			st.InstalledVersion, st.InstalledKnown = v, true
		}
		if tb.SourceKind == "git" && tb.SourceRef != "" {
			if sha, err := gitTreeVersion(tb.SourceRef); err == nil && sha != "" {
				st.SourceVersion, st.SourceKnown = sha, true
			}
		}

		if st.InstalledKnown && st.SourceKnown {
			hash := extractCommitHash(st.InstalledVersion)
			if hash != "" && !strings.HasPrefix(st.SourceVersion, hash) {
				st.Stale = true
				stale = append(stale, tb.Name)
			}
		}

		// Upstream drift: how far is the source tree (and therefore the
		// installed build) behind upstream? Only when the operator configured
		// an upstream_ref AND the GitAhead seam is wired — a pure bonus
		// signal, never a failure when absent.
		//
		// S6 threshold (feedback F1): drift only enters the behind-list (and
		// earns the runbook suggestion) at build_refresh_behind_n commits
		// (default 500 — single-digit drift spammed suggestions nobody
		// wanted). Below-threshold drift stays VISIBLE in the evidence and
		// as an info note without proposing anything.
		if tb.SourceKind == "git" && tb.UpstreamRef != "" && env.GitAhead != nil {
			if n, err := env.GitAhead(ctx, tb.SourceRef, tb.UpstreamRef); err == nil {
				st.UpstreamAhead = n
				if n >= env.Thresholds.BuildRefreshBehindN {
					behind = append(behind, fmt.Sprintf("%s (%d)", tb.Name, n))
				} else if n > 0 {
					driftBelow = append(driftBelow, fmt.Sprintf("%s (%d)", tb.Name, n))
				}
				// Commit-subject watchlist match (S6 phase 2): only worth the
				// extra git-log call when there's measurable drift AND an
				// operator watchlist to match against.
				if n > 0 && env.GitBehindLog != nil && len(env.BuildRefreshWatchlist) > 0 {
					if subjects, logErr := env.GitBehindLog(ctx, tb.SourceRef, tb.UpstreamRef, gitBehindLogMaxN); logErr == nil {
						st.UpstreamSubjects = subjects
						if matches := matchWatchlist(subjects, env.BuildRefreshWatchlist); len(matches) > 0 {
							st.WatchlistMatches = matches
							watchlistHits = append(watchlistHits, fmt.Sprintf("%s (%d match(es))", tb.Name, len(matches)))
						}
					}
					// Unmeasurable (git log failed) is silent, same posture as
					// GitAhead/GitLsRemote — a bonus signal, never a failure.
				}
			}
		}
		// Upstream-NIGHTLY drift mode (P3smith): for forks with tracking
		// enabled, compare the remote HEAD (one bounded git ls-remote — a
		// network call, but read-only and per-fork opt-in) against the sha
		// build_refresh recorded when it last built this tree. Threshold
		// gating note: BuildRefreshBehindN counts COMMITS, which a bare
		// ls-remote sha cannot yield without fetching the repo — so the
		// nightly mode reuses the threshold's EVALUATION SHAPE instead:
		// drift is surfaced as evidence plus a runbook ref exactly like
		// above-threshold rev-list drift (the operator explicitly opted
		// this fork into nightly watching, so any recorded-vs-HEAD
		// divergence is already at the granularity that threshold exists
		// to gate), and "unmeasurable"/"no build recorded" states stay
		// visible without proposing anything.
		if fu, ok := trackedByRef[tb.SourceRef]; ok {
			st.NightlyURL = fu.UpstreamURL
			st.LastBuiltSha = shortSha(fu.LastBuiltSha)
			if env.GitLsRemote != nil {
				lctx, cancel := context.WithTimeout(ctx, nightlyLsRemoteTimeout)
				sha, err := env.GitLsRemote(lctx, fu.UpstreamURL)
				cancel()
				switch {
				case err != nil || sha == "":
					// Unmeasurable — visible in evidence, never a failure.
				case fu.LastBuiltSha == "":
					st.NightlyHeadSha = shortSha(sha)
					nightlyUnrecorded = append(nightlyUnrecorded, tb.Name)
				case shortSha(fu.LastBuiltSha) != shortSha(sha):
					st.NightlyHeadSha = shortSha(sha)
					st.NightlyDrift = true
					nightlyDrift = append(nightlyDrift,
						fmt.Sprintf("%s (built %s, upstream HEAD %s)", tb.Name, shortSha(fu.LastBuiltSha), shortSha(sha)))
				default:
					st.NightlyHeadSha = shortSha(sha)
				}
			}
		}
		statuses = append(statuses, st)
	}

	confidence, confidenceNote := binaryVersionsConfidence(statuses)
	ev := map[string]any{"binaries": statuses}
	if len(stale) > 0 || len(behind) > 0 || len(nightlyDrift) > 0 {
		parts := []string{}
		if len(stale) > 0 {
			parts = append(parts, fmt.Sprintf("%d source tree(s) ahead of the installed build: %s", len(stale), strings.Join(stale, ", ")))
		}
		if len(behind) > 0 {
			parts = append(parts, fmt.Sprintf("%d build(s) behind upstream: %s", len(behind), strings.Join(behind, ", ")))
		}
		if len(nightlyDrift) > 0 {
			parts = append(parts, fmt.Sprintf("%d nightly-tracked fork(s) whose recorded build sha differs from upstream HEAD: %s",
				len(nightlyDrift), strings.Join(nightlyDrift, ", ")))
		}
		if len(watchlistHits) > 0 {
			parts = append(parts, fmt.Sprintf("%d build(s) drifted with a commit subject matching the watchlist: %s",
				len(watchlistHits), strings.Join(watchlistHits, ", ")))
		}
		refs := []string{"research:llamacpp-build-status"}
		if len(behind) > 0 || len(nightlyDrift) > 0 {
			refs = append(refs, "runbook:build-refresh")
		}
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary:  strings.Join(parts, "; "),
			Evidence: ev, KBRefs: refs,
			Confidence: confidence, ConfidenceNote: confidenceNote}
	}
	if len(driftBelow) > 0 {
		// S6: below-threshold drift stays visible — info only, NO runbook
		// ref (nothing proposes a rebuild off this). A watchlist hit still
		// gets its own clause even here — the point of the watchlist is
		// surfacing a flagged keyword regardless of raw commit count.
		summary := fmt.Sprintf("%d build(s) drifted from upstream but below the %d-commit refresh threshold: %s",
			len(driftBelow), env.Thresholds.BuildRefreshBehindN, strings.Join(driftBelow, ", "))
		if len(watchlistHits) > 0 {
			summary += fmt.Sprintf("; %d matched the watchlist despite being below threshold: %s",
				len(watchlistHits), strings.Join(watchlistHits, ", "))
		}
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary: summary, Evidence: ev,
			Confidence: confidence, ConfidenceNote: confidenceNote}
	}
	if len(nightlyUnrecorded) > 0 {
		// Tracking enabled but no build recorded yet (P3smith) — visible,
		// info-only: the first build_record_upstream_sha step fills it in.
		return Finding{CheckID: id, Severity: SeverityInfo,
			Summary: fmt.Sprintf("%d nightly-tracked fork(s) have no recorded upstream build sha yet: %s",
				len(nightlyUnrecorded), strings.Join(nightlyUnrecorded, ", ")),
			Evidence:   ev,
			Confidence: confidence, ConfidenceNote: confidenceNote}
	}
	return Finding{CheckID: id, Severity: SeverityOK,
		Summary: fmt.Sprintf("%d tracked binaries checked; source tree matches (or is unmeasurable against) the installed build for all", len(tracked)), Evidence: ev,
		Confidence: confidence, ConfidenceNote: confidenceNote}
}

// binaryVersionsConfidence derives Confidence from how many tracked
// binaries the check could actually read both sides of (installed version
// + source tree version) — Tier 1 Sprint 4. A binary neither side measured
// for can't be judged stale/fresh at all; without this, such a binary
// silently reads as "no drift" indistinguishably from one genuinely
// confirmed fresh.
func binaryVersionsConfidence(statuses []binaryStatus) (Confidence, string) {
	var unmeasured []string
	for _, st := range statuses {
		if !st.InstalledKnown || !st.SourceKnown {
			unmeasured = append(unmeasured, st.Name)
		}
	}
	if len(unmeasured) == 0 {
		return ConfidenceHigh, ""
	}
	note := fmt.Sprintf("%d of %d tracked binaries unmeasurable (installed or source version unreadable): %s",
		len(unmeasured), len(statuses), strings.Join(unmeasured, ", "))
	if len(unmeasured) == len(statuses) {
		return ConfidenceLow, note
	}
	return ConfidenceMedium, note
}
