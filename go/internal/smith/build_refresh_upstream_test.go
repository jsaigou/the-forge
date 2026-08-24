// SPDX-License-Identifier: Apache-2.0

package smith

// build_refresh_upstream_test.go — P3smith. Exercises the upstream-nightly
// tracking state model (settings entry × migration-0066 DB row merge), the
// build_record_upstream_sha op, and binary_versions' nightly drift mode.
// No network: git ls-remote goes through a fake seam.

import (
	"context"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func seedTrackedFork(t *testing.T, db *store.DB, sourceRef string) {
	t.Helper()
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef:     sourceRef,
		Remote:        "origin",
		UpstreamRef:   "origin/master",
		Backends:      map[string]buildBackendFlags{"vulkan": {Backend: "vulkan"}},
		UpstreamURL:   "https://example.com/upstream.git",
		TrackUpstream: true,
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "tracked-fork", Path: mustExecutable(t), SourceKind: "git",
		SourceRef: sourceRef, UpstreamRef: "origin/master",
	}))
}

func TestEffectiveForkUpstream_DBRowOverridesSettings(t *testing.T) {
	fork := buildRefreshFork{SourceRef: "/tree", UpstreamURL: "https://old.example/x.git", TrackUpstream: true}
	if got := effectiveForkUpstream(fork, nil); got.UpstreamURL != "https://old.example/x.git" || !got.TrackUpstream || got.LastBuiltSha != "" {
		t.Errorf("no-row merge = %+v", got)
	}
	row := forkUpstreamTrack{SourceRef: "/tree", UpstreamURL: "https://new.example/y.git", TrackUpstream: true, LastBuiltSha: "abc1234def"}
	got := effectiveForkUpstream(fork, &row)
	if got.UpstreamURL != "https://new.example/y.git" || !got.TrackUpstream || got.LastBuiltSha != "abc1234def" {
		t.Errorf("row override = %+v, want DB values to win", got)
	}
	// A sha-only row (what build_record_upstream_sha writes against a fork
	// with no prior row: URL NULL, track at column default 0) must NOT
	// disable the settings entry's own opt-in.
	shaOnly := forkUpstreamTrack{SourceRef: "/tree", LastBuiltSha: "abc1234def"}
	if got := effectiveForkUpstream(fork, &shaOnly); !got.TrackUpstream || got.UpstreamURL != "https://old.example/x.git" || got.LastBuiltSha != "abc1234def" {
		t.Errorf("sha-only row merge = %+v, want settings flags preserved + sha recorded", got)
	}
}

func TestResolvedForkUpstreams_OnlyTrackedWithAllowedURL(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db,
		buildRefreshFork{SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
			Backends:    map[string]buildBackendFlags{"vulkan": {Backend: "vulkan"}},
			UpstreamURL: "https://ok.example/x.git", TrackUpstream: true},
		buildRefreshFork{SourceRef: root + "-untracked", Remote: "origin", UpstreamRef: "origin/master",
			Backends: map[string]buildBackendFlags{"vulkan": {Backend: "vulkan"}}},
		buildRefreshFork{SourceRef: root + "-badurl", Remote: "origin", UpstreamRef: "origin/master",
			Backends:    map[string]buildBackendFlags{"vulkan": {Backend: "vulkan"}},
			UpstreamURL: "https://evil.example/x;rm -rf /", TrackUpstream: true},
	)
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "x", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	s := New(Deps{Store: db, Settings: db.Settings(), Logf: func(string, ...any) {}})

	tracks, err := s.resolvedForkUpstreams(context.Background())
	if err != nil {
		t.Fatalf("resolvedForkUpstreams: %v", err)
	}
	if len(tracks) != 1 || tracks[0].SourceRef != root {
		t.Errorf("tracks = %+v, want exactly the one allowed tracked fork", tracks)
	}
}

func TestOpBuildRecordUpstreamSha_RecordsThenUpdates(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedTrackedFork(t, db, root)
	shas := []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	call := 0
	s := New(Deps{
		Store:    db,
		Settings: db.Settings(),
		GitLsRemote: func(context.Context, string) (string, error) {
			sha := shas[min(call, len(shas)-1)]
			call++
			return sha, nil
		},
		Logf: func(string, ...any) {},
	})

	for i, want := range []string{"aaaaaaa", "bbbbbbb"} {
		res, err := s.opBuildRecordUpstreamSha(context.Background(), map[string]string{"binary": "tracked-fork"})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("record %d stdout = %q, want it to carry %s", i, res.Stdout, want)
		}
		rows, rerr := s.loadForkUpstreamRows(context.Background())
		if rerr != nil || len(rows) != 1 || rows[0].LastBuiltSha[:7] != want {
			t.Errorf("after record %d rows = %+v err=%v, want sha %s", i, rows, rerr, want)
		}
	}
}

func TestOpBuildRecordUpstreamSha_UntrackedIsHonestNoOp(t *testing.T) {
	root := newTestGitRoot(t)
	db := openDB(t)
	seedBuildRefreshForks(t, db, buildRefreshFork{
		SourceRef: root, Remote: "origin", UpstreamRef: "origin/master",
		Backends: map[string]buildBackendFlags{"vulkan": {Backend: "vulkan"}},
	})
	setSetting(t, db, SettingBinariesTracked, mustBinariesJSON(t, TrackedBinary{
		Name: "plain-fork", Path: mustExecutable(t), SourceKind: "git", SourceRef: root, UpstreamRef: "origin/master",
	}))
	called := false
	s := New(Deps{
		Store:    db,
		Settings: db.Settings(),
		GitLsRemote: func(context.Context, string) (string, error) {
			called = true
			return "", nil
		},
		Logf: func(string, ...any) {},
	})
	res, err := s.opBuildRecordUpstreamSha(context.Background(), map[string]string{"binary": "plain-fork"})
	if err != nil {
		t.Fatalf("untracked fork must no-op successfully: %v", err)
	}
	if called {
		t.Error("ls-remote must not run for an untracked fork")
	}
	if !strings.Contains(res.Stdout, "not enabled") {
		t.Errorf("stdout = %q, want an honest not-enabled note", res.Stdout)
	}
}

func TestBinaryVersions_NightlyDriftDetected(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root+"/.git/HEAD", "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp nightly", Kind: "llama_build", Path: "/opt/llama-server",
				SourceKind: "git", SourceRef: root},
		},
		ForkUpstreams: []forkUpstreamTrack{{
			SourceRef: root, UpstreamURL: "https://up.example/fork.git",
			TrackUpstream: true, LastBuiltSha: "deadbeef00000000000000000000000000000000",
		}},
		GitLsRemote: func(context.Context, string) (string, error) {
			return "feedface11111111111111111111111111111111", nil
		},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info on nightly drift", f.Severity)
	}
	hasRunbook := false
	for _, ref := range f.KBRefs {
		if ref == "runbook:build-refresh" {
			hasRunbook = true
		}
	}
	if !hasRunbook {
		t.Errorf("KBRefs = %v, want runbook:build-refresh on drift", f.KBRefs)
	}
	if !strings.Contains(f.Summary, "recorded build sha differs") {
		t.Errorf("summary = %q", f.Summary)
	}
	binaries, _ := f.Evidence["binaries"].([]binaryStatus)
	if len(binaries) != 1 || !binaries[0].NightlyDrift || binaries[0].NightlyHeadSha != "feedfac" || binaries[0].LastBuiltSha != "deadbee" {
		t.Errorf("evidence = %+v", binaries)
	}
}

func TestBinaryVersions_NightlyCurrentStaysOK(t *testing.T) {
	root := t.TempDir()
	mustWriteFile(t, root+"/.git/HEAD", "04b2b72cbabcdef0123456789abcdef01234567\n")
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp nightly", Kind: "llama_build", Path: "/opt/llama-server",
				SourceKind: "git", SourceRef: root},
		},
		ForkUpstreams: []forkUpstreamTrack{{
			SourceRef: root, UpstreamURL: "https://up.example/fork.git",
			TrackUpstream: true, LastBuiltSha: "feedface11111111111111111111111111111111",
		}},
		GitLsRemote: func(context.Context, string) (string, error) {
			return "feedface11111111111111111111111111111111", nil
		},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s (%s), want ok when recorded sha matches HEAD", f.Severity, f.Summary)
	}
}

func TestBinaryVersions_NightlyNoBuildRecordedVisibleOnly(t *testing.T) {
	root := t.TempDir()
	env := &CheckEnv{
		BinariesEnabled: true,
		TrackedBinaries: []TrackedBinary{
			{Name: "llama.cpp nightly", Kind: "llama_build", Path: "/opt/llama-server",
				SourceKind: "git", SourceRef: root},
		},
		ForkUpstreams: []forkUpstreamTrack{{
			SourceRef: root, UpstreamURL: "https://up.example/fork.git", TrackUpstream: true,
		}},
		GitLsRemote: func(context.Context, string) (string, error) {
			return "feedface11111111111111111111111111111111", nil
		},
	}
	f := runBinaryVersions(context.Background(), env)
	if f.Severity != SeverityInfo {
		t.Errorf("severity = %s, want info for tracking-without-a-recorded-build", f.Severity)
	}
	for _, ref := range f.KBRefs {
		if ref == "runbook:build-refresh" {
			t.Error("no build recorded yet must NOT earn the rebuild runbook ref")
		}
	}
}

func TestShortSha(t *testing.T) {
	if got := shortSha("abcdef0123456789"); got != "abcdef0" {
		t.Errorf("shortSha full = %q", got)
	}
	if got := shortSha("ab"); got != "ab" {
		t.Errorf("shortSha short = %q", got)
	}
}
