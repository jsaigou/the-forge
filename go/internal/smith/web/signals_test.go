// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeSignalService is a hand-rolled Service implementing only what
// CheckUnblockSignal needs — no real network, full control over responses.
type fakeSignalService struct {
	docs map[string]*Document
	errs map[string]error
}

func (f *fakeSignalService) Search(context.Context, string, int) ([]Result, error) { return nil, nil }
func (f *fakeSignalService) Fetch(ctx context.Context, url string) (*Document, error) {
	return f.FetchDirect(ctx, url, 0)
}
func (f *fakeSignalService) FetchWithTTL(ctx context.Context, url string, _ time.Duration) (*Document, error) {
	return f.FetchDirect(ctx, url, 0)
}
func (f *fakeSignalService) FetchDirect(_ context.Context, url string, _ time.Duration) (*Document, error) {
	if err, ok := f.errs[url]; ok {
		return nil, err
	}
	if d, ok := f.docs[url]; ok {
		return d, nil
	}
	return nil, errors.New("fake: no doc for " + url)
}
func (f *fakeSignalService) Providers(context.Context) []ProviderStatus { return nil }
func (f *fakeSignalService) Probe(context.Context)                     {}

func TestCheckUnblockSignal_GitHubPRMerged(t *testing.T) {
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://api.github.com/repos/ggml-org/llama.cpp/pulls/22105": {Text: `{"state":"closed","merged":true}`},
	}}
	sig, _, err := CheckUnblockSignal(context.Background(), svc, "https://github.com/ggml-org/llama.cpp/pull/22105", "", time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if !sig.Changed || sig.Detail != "pull request merged" {
		t.Errorf("sig = %+v, want Changed=true Detail=\"pull request merged\"", sig)
	}
}

func TestCheckUnblockSignal_GitHubIssueClosed(t *testing.T) {
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://api.github.com/repos/ggml-org/llama.cpp/issues/22384": {Text: `{"state":"closed","merged":false}`},
	}}
	sig, _, err := CheckUnblockSignal(context.Background(), svc, "https://github.com/ggml-org/llama.cpp/issues/22384", "", time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if !sig.Changed || sig.Detail != "issue closed" {
		t.Errorf("sig = %+v, want Changed=true Detail=\"issue closed\"", sig)
	}
}

func TestCheckUnblockSignal_GitHubStillOpen(t *testing.T) {
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://api.github.com/repos/ggml-org/llama.cpp/pulls/22105": {Text: `{"state":"open","merged":false}`},
	}}
	sig, _, err := CheckUnblockSignal(context.Background(), svc, "https://github.com/ggml-org/llama.cpp/pull/22105", "", time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if sig.Changed {
		t.Errorf("sig = %+v, want Changed=false for a still-open PR", sig)
	}
}

func TestCheckUnblockSignal_GitHubURLRewrite(t *testing.T) {
	// The exact rewrite (org/repo/N preserved, path swapped) is what makes
	// this deterministic — assert the fake was hit at the expected API URL,
	// not just that *some* URL resolved.
	svc := &fakeSignalService{docs: map[string]*Document{}}
	_, _, err := CheckUnblockSignal(context.Background(), svc, "https://github.com/foo-org/bar.repo/pull/99", "", time.Hour)
	if err == nil || err.Error() != "fake: no doc for https://api.github.com/repos/foo-org/bar.repo/pulls/99" {
		t.Fatalf("unexpected error (rewrite mismatch): %v", err)
	}
}

func TestCheckUnblockSignal_NonGitHub_HashDiff(t *testing.T) {
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://example.com/status": {Text: "new content"},
	}}
	prevHash := sha256Hex("old content")
	sig, newHash, err := CheckUnblockSignal(context.Background(), svc, "https://example.com/status", prevHash, time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if !sig.Changed || sig.Detail != "page content changed since last check" {
		t.Errorf("sig = %+v, want a changed-content signal", sig)
	}
	if newHash != sha256Hex("new content") {
		t.Errorf("newHash = %q, want sha256 of the fetched body", newHash)
	}
}

func TestCheckUnblockSignal_NonGitHub_NoChange(t *testing.T) {
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://example.com/status": {Text: "same content"},
	}}
	prevHash := sha256Hex("same content")
	sig, _, err := CheckUnblockSignal(context.Background(), svc, "https://example.com/status", prevHash, time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if sig.Changed {
		t.Errorf("sig = %+v, want Changed=false when the hash matches", sig)
	}
}

func TestCheckUnblockSignal_NonGitHub_FirstCheckNeverClaimsChange(t *testing.T) {
	// prevSHA256 == "" means "never checked before" — must not report a
	// spurious "changed" on the very first observation.
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://example.com/status": {Text: "content"},
	}}
	sig, _, err := CheckUnblockSignal(context.Background(), svc, "https://example.com/status", "", time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if sig.Changed {
		t.Error("first-ever check should never claim Changed=true")
	}
}

func TestCheckUnblockSignal_CachedPropagates(t *testing.T) {
	svc := &fakeSignalService{docs: map[string]*Document{
		"https://example.com/status": {Text: "x", Cached: true},
	}}
	sig, _, err := CheckUnblockSignal(context.Background(), svc, "https://example.com/status", "", time.Hour)
	if err != nil {
		t.Fatalf("CheckUnblockSignal: %v", err)
	}
	if !sig.Cached {
		t.Error("Cached should propagate from the underlying Document")
	}
}

func TestCheckUnblockSignal_FetchError(t *testing.T) {
	svc := &fakeSignalService{errs: map[string]error{
		"https://example.com/status": errors.New("network down"),
	}}
	_, _, err := CheckUnblockSignal(context.Background(), svc, "https://example.com/status", "", time.Hour)
	if err == nil {
		t.Fatal("expected the fetch error to propagate")
	}
}
