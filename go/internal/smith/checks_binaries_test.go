// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryVersions_RegisteredDeepOnly(t *testing.T) {
	c := findCheck(t, "binary_versions")
	if c.Fast {
		t.Error("binary_versions must be deep-sweep only (Fast=false)")
	}
}

func TestBinaryVersions_Disabled(t *testing.T) {
	env := &CheckEnv{
		BinariesEnabled: false,
		TrackedBinaries: []TrackedBinary{{Name: "x", Path: "/x"}}, // must be ignored while disabled
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityInfo || f.Evidence["skipped"] == nil {
		t.Errorf("f = %+v, want a skip finding", f)
	}
}

func TestBinaryVersions_NoneTracked(t *testing.T) {
	f := runBinaryVersions(context.Background(), &CheckEnv{BinariesEnabled: true})
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok", f.Severity)
	}
}

func TestBinaryVersions_NilBinaryVersionSeam_SourceOnlyStillRuns(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp", Kind: "llama_build", Path: "/opt/forge/llama.cpp/build/bin/llama-server",
				SourceKind: "git", SourceRef: root},
		},
	}
	f := runBinaryVersions(context.Background(), env)
	// Nothing to compare against (installed unknown) — must not be flagged stale.
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok (installed unknown, nothing to compare)", f.Severity)
	}
	binaries, _ := f.Evidence["binaries"].([]binaryStatus)
	if len(binaries) != 1 || !binaries[0].SourceKnown || binaries[0].InstalledKnown {
		t.Errorf("binaries = %+v, want source known / installed unknown", binaries)
	}
	// Tier 1 Sprint 4: the only tracked binary is unmeasurable on one side
	// (installed version unknown) -> low confidence, not silently "ok".
	if f.Confidence != ConfidenceLow {
		t.Errorf("Confidence = %q, want low (installed version unreadable for the only tracked binary)", f.Confidence)
	}
	if f.ConfidenceNote == "" {
		t.Errorf("ConfidenceNote empty, want it to name the unmeasurable binary")
	}
}

// TestBinaryVersions_ConfidenceMediumWhenSomeMeasurable: Tier 1 Sprint 4 —
// a mix of fully-measured and partially-measured binaries degrades to
// medium confidence, distinct from the all-unmeasurable low case above.
func TestBinaryVersions_ConfidenceMediumWhenSomeMeasurable(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "fully-measured", Kind: "llama_build", Path: "/p/a", SourceKind: "git", SourceRef: root},
			{Name: "unmeasurable", Kind: "llama_build", Path: "/p/b", SourceKind: "git", SourceRef: root},
		},
		BinaryVersion: func(_ context.Context, path string) (string, error) {
			if path == "/p/a" {
				return "version: 1 (04b2b72cb)\n", nil
			}
			return "", errors.New("exec failed")
		},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Confidence != ConfidenceMedium {
		t.Errorf("Confidence = %q, want medium (1 of 2 binaries unmeasurable)", f.Confidence)
	}
	if !strings.Contains(f.ConfidenceNote, "unmeasurable") {
		t.Errorf("ConfidenceNote = %q, want it to mention unmeasurable", f.ConfidenceNote)
	}
}

func TestBinaryVersions_StaleDetected(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp (vulkan)", Kind: "llama_build", Path: "/opt/forge/llama.cpp/build/bin/llama-server",
				SourceKind: "git", SourceRef: root},
		},
		BinaryVersion: func(context.Context, string) (string, error) {
			return "version: 10122 (8bf3c1130)\nbuilt with GNU 16.1.1 for Linux x86_64", nil
		},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (real ForgeHost fact-6 divergence)", f.Severity)
	}
	if len(f.KBRefs) == 0 {
		t.Error("expected a kb_ref pointing at the build-status corpus chunk")
	}
}

func TestBinaryVersions_MatchNotStale(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "8bf3c1130abcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp (vulkan)", Kind: "llama_build", Path: "/opt/forge/llama.cpp/build/bin/llama-server",
				SourceKind: "git", SourceRef: root},
		},
		BinaryVersion: func(context.Context, string) (string, error) {
			return "version: 10122 (8bf3c1130)", nil
		},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok (source HEAD matches installed hash)", f.Severity)
	}
}

func TestProposeRebuildRunbook_OnlyStaleAndInfo(t *testing.T) {
	f := Finding{
		CheckID: "binary_versions", Severity: SeverityInfo,
		Evidence: map[string]any{"binaries": []binaryStatus{
			{Name: "a", Path: "/p/a", SourceRef: "/s/a", Stale: true},
			{Name: "b", Path: "/p/b", SourceRef: "/s/b", Stale: false},
		}},
	}
	drafts := proposeRebuildRunbook(&CheckEnv{}, f, BrainResolution{})
	if len(drafts) != 1 {
		t.Fatalf("got %d drafts, want 1 (only the stale binary)", len(drafts))
	}
	if drafts[0].Kind != KindRunbook {
		t.Errorf("kind = %s, want runbook (never auto-executed)", drafts[0].Kind)
	}
	// Tier 1 Sprint 4: a stale local rebuild risks disrupting a live
	// slot's mapped binary — distinct from binary_upstream's build_refresh
	// path, which IS procedurizable and must not carry this reason.
	if !strings.Contains(string(drafts[0].Detail), `"why_cant_run":"risks_live_workload"`) {
		t.Errorf("detail = %s, want why_cant_run=risks_live_workload", drafts[0].Detail)
	}

	// Non-info severity must never propose anything.
	f.Severity = SeverityOK
	if drafts := proposeRebuildRunbook(&CheckEnv{}, f, BrainResolution{}); len(drafts) != 0 {
		t.Errorf("got %d drafts for a non-info finding, want 0", len(drafts))
	}
}

func TestBinaryVersions_UpstreamDriftDetected(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp (kintsugi)", Kind: "llama_build", Path: "/opt/forge/llama.cpp-kintsugi/build-rocm-new/bin/llama-server",
				SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master"},
		},
		BinaryVersion: func(context.Context, string) (string, error) {
			return "version: 10455 (04b2b72cb)\n", nil // installed == source tree HEAD
		},
		GitAhead: func(context.Context, string, string) (int, error) { return 298, nil },
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (upstream drift)", f.Severity)
	}
	// The finding must carry the build-refresh KB ref.
	found := false
	for _, ref := range f.KBRefs {
		if ref == "runbook:build-refresh" {
			found = true
		}
	}
	if !found {
		t.Errorf("KBRefs = %v, want runbook:build-refresh", f.KBRefs)
	}
	// Evidence must record the upstream count.
	bins, ok := f.Evidence["binaries"].([]binaryStatus)
	if !ok || len(bins) != 1 {
		t.Fatalf("evidence binaries = %#v", f.Evidence["binaries"])
	}
	if bins[0].UpstreamAhead != 298 {
		t.Errorf("UpstreamAhead = %d, want 298", bins[0].UpstreamAhead)
	}
}

func TestMatchWatchlist(t *testing.T) {
	subjects := []string{
		"fix CVE-2026-1234 in tokenizer",
		"minor cleanup, no functional change",
		"BREAKING: remove deprecated flag",
	}
	got := matchWatchlist(subjects, []string{"cve", "breaking"})
	want := []string{"cve: fix CVE-2026-1234 in tokenizer", "breaking: BREAKING: remove deprecated flag"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("matchWatchlist = %v, want %v", got, want)
	}
	if matchWatchlist(subjects, nil) != nil {
		t.Errorf("empty watchlist should match nothing")
	}
	if matchWatchlist(nil, []string{"cve"}) != nil {
		t.Errorf("no subjects should match nothing")
	}
	// A blank keyword (stray comma in the operator's list) must never match
	// everything.
	if got := matchWatchlist(subjects, []string{"", "  "}); got != nil {
		t.Errorf("blank keywords should match nothing, got %v", got)
	}
}

// TestBinaryVersions_WatchlistMatch covers S6 phase 2: a commit subject
// hitting the watchlist gets its own evidence/summary clause without
// elevating severity or bypassing proposeRebuildRunbook's independent
// threshold re-check (visibility only, per SettingBuildRefreshWatchlist's
// doc comment).
func TestBinaryVersions_WatchlistMatch(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	var logCalls int
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp (kintsugi)", Kind: "llama_build", Path: "/p",
				SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master"},
		},
		Thresholds: Thresholds{BuildRefreshBehindN: 500}, // 12 < threshold: driftBelow, not behind
		GitAhead:   func(context.Context, string, string) (int, error) { return 12, nil },
		GitBehindLog: func(context.Context, string, string, int) ([]string, error) {
			logCalls++
			return []string{"fix CVE-2026-9999 in prompt cache", "unrelated cleanup"}, nil
		},
		BuildRefreshWatchlist: []string{"CVE"},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info (visibility only, never elevated)", f.Severity)
	}
	if logCalls != 1 {
		t.Errorf("GitBehindLog called %d times, want 1", logCalls)
	}
	if !strings.Contains(f.Summary, "watchlist") {
		t.Errorf("summary = %q, want a watchlist clause even though drift is below threshold", f.Summary)
	}
	bins, ok := f.Evidence["binaries"].([]binaryStatus)
	if !ok || len(bins) != 1 {
		t.Fatalf("evidence binaries = %#v", f.Evidence["binaries"])
	}
	if len(bins[0].WatchlistMatches) != 1 || !strings.Contains(bins[0].WatchlistMatches[0], "CVE-2026-9999") {
		t.Errorf("WatchlistMatches = %v, want one CVE match", bins[0].WatchlistMatches)
	}
	if len(bins[0].UpstreamSubjects) != 2 {
		t.Errorf("UpstreamSubjects = %v, want both fetched subjects recorded", bins[0].UpstreamSubjects)
	}
}

// TestBinaryVersions_NoWatchlistSkipsGitBehindLog confirms the check never
// pays for the extra git-log call when there's nothing to match against —
// GitBehindLog wired but BuildRefreshWatchlist empty must not be invoked.
func TestBinaryVersions_NoWatchlistSkipsGitBehindLog(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	var logCalls int
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp (kintsugi)", Kind: "llama_build", Path: "/p",
				SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master"},
		},
		GitAhead: func(context.Context, string, string) (int, error) { return 12, nil },
		GitBehindLog: func(context.Context, string, string, int) ([]string, error) {
			logCalls++
			return nil, nil
		},
		// BuildRefreshWatchlist deliberately empty.
	}
	runBinaryVersions(context.Background(), env)
	if logCalls != 0 {
		t.Errorf("GitBehindLog called %d times, want 0 (no watchlist to match against)", logCalls)
	}
}

func TestBinaryVersions_UpstreamDriftUnmeasurableWithoutSeam(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, ".git", "HEAD"), "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp (kintsugi)", Kind: "llama_build", Path: "/p",
				SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master"},
		},
		BinaryVersion: func(context.Context, string) (string, error) {
			return "version: 10455 (04b2b72cb)\n", nil
		},
		// No GitAhead seam wired: upstream drift stays unmeasured (-1), and
		// the finding stays OK (a pure bonus signal, never a failure).
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok (no seam = unmeasured, not stale)", f.Severity)
	}
	bins, ok := f.Evidence["binaries"].([]binaryStatus)
	if !ok || bins[0].UpstreamAhead != -1 {
		t.Errorf("UpstreamAhead = %v, want -1 (unmeasured)", bins)
	}
}

func TestProposeRebuildRunbook_UpstreamBehind(t *testing.T) {
	f := Finding{
		CheckID: "binary_versions", Severity: SeverityInfo,
		Evidence: map[string]any{"binaries": []binaryStatus{
			{Name: "llama.cpp (kintsugi)", Path: "/p", SourceRef: "/s",
				UpstreamRef: "origin/master", UpstreamAhead: 298, InstalledVersion: "10455 (abc)", SourceVersion: "abc"},
			{Name: "b", Path: "/p/b", SourceRef: "/s/b", Stale: true},
		}},
	}
	// S6 mechanism test: explicit low threshold so the 298-commit drift
	// exercises the proposal path itself.
	drafts := proposeRebuildRunbook(&CheckEnv{Thresholds: Thresholds{BuildRefreshBehindN: 1}}, f, BrainResolution{})
	if len(drafts) != 2 {
		t.Fatalf("got %d drafts, want 2 (one upstream-behind + one stale)", len(drafts))
	}
	// The upstream-behind draft must carry the build-refresh recipe steps.
	if drafts[0].Kind != KindRunbook || !strings.Contains(drafts[0].Title, "behind") {
		t.Errorf("draft[0] = %+v, want runbook titled as behind-upstream", drafts[0])
	}
	// Tier 1 Sprint 4: this path (dedupeKeyBinaryUpstreamPrefix) IS
	// procedurizable via build_refresh (Sprint 6 capstone) — it must NOT
	// carry a why_cant_run reason, unlike the stale/binary_stale draft.
	if strings.Contains(string(drafts[0].Detail), "why_cant_run") {
		t.Errorf("detail = %s, upstream-behind is procedurizable and must not carry why_cant_run", drafts[0].Detail)
	}
	if !strings.Contains(string(drafts[1].Detail), `"why_cant_run":"risks_live_workload"`) {
		t.Errorf("draft[1] detail = %s, want why_cant_run=risks_live_workload for the stale binary", drafts[1].Detail)
	}
}

// TestS6_BuildRefreshThresholdMatrix pins S6's headline behavior: drift
// below the threshold stays VISIBLE (info finding naming the binary) but
// produces NO runbook suggestion; at/above it, the suggestion fires. The
// dedupe key downstream (runbook:binary_upstream:) is unchanged by
// threshold moves — a threshold change must never re-spam.
func TestS6_BuildRefreshThresholdMatrix(t *testing.T) {
	mkFinding := func(ahead int) Finding {
		return Finding{CheckID: "binary_versions", Severity: SeverityInfo,
			Evidence: map[string]any{"binaries": []binaryStatus{
				{Name: "llama.cpp (main tree)", Path: "/p", SourceRef: "/s",
					UpstreamRef: "origin/master", UpstreamAhead: ahead},
			}},
		}
	}
	env := func(threshold int) *CheckEnv {
		return &CheckEnv{Thresholds: Thresholds{BuildRefreshBehindN: threshold}}
	}

	for _, ahead := range []int{499} {
		drafts := proposeRebuildRunbook(env(500), mkFinding(ahead), BrainResolution{})
		if len(drafts) != 0 {
			t.Errorf("ahead=%d: %d drafts, want 0 below the 500 threshold", ahead, len(drafts))
		}
	}
	for _, ahead := range []int{500, 501} {
		drafts := proposeRebuildRunbook(env(500), mkFinding(ahead), BrainResolution{})
		if len(drafts) != 1 {
			t.Errorf("ahead=%d: %d drafts, want 1 at/above the threshold", ahead, len(drafts))
		}
	}

	// Default CheckEnv (zero thresholds) must behave like the documented
	// default, not like "everything proposes".
	if drafts := proposeRebuildRunbook(&CheckEnv{}, mkFinding(4), BrainResolution{}); len(drafts) != 0 {
		t.Errorf("zero-env with 4 commits ahead: %d drafts, want 0 (defensive default 500)", len(drafts))
	}
}
