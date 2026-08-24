// SPDX-License-Identifier: Apache-2.0

package web

// signals.go — deterministic "has this unblocked?" signal derivation for
// the smith P5 §4.9 blocked-item recheck (checks_blocked_recheck.go, in
// package smith). No LLM anywhere in this file.
//
// Most blocked-work tracker URLs are GitHub PR/issue links —
// rewriting those to the REST API and reading the real {state, merged}
// fields is deterministic and correct, and unauthenticated GitHub allows
// 60 req/hr, far above this check's ≤3-fetches-per-sweep budget. Everything
// else falls back to comparing the fetched body's sha256 against the
// previous cached hash — a much weaker, honestly-labeled "page changed"
// signal, never claimed as confirmation.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

var (
	githubPRRe    = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/pull/(\d+)`)
	githubIssueRe = regexp.MustCompile(`^https?://github\.com/([\w.-]+)/([\w.-]+)/issues/(\d+)`)
)

// UnblockSignal is the result of one deterministic recheck. Cached reports
// whether the underlying fetch was served from smith_web_cache (true) or
// made a real network call (false) — the caller's budget (≤3 network
// fetches per sweep) counts only the latter.
type UnblockSignal struct {
	Changed bool   // true = a positive signal was found (merged/closed/content changed)
	Detail  string // human-readable reason, safe to put in a Finding summary
	Cached  bool
}

// CheckUnblockSignal fetches rawURL (through svc, respecting ttl as the
// cache-cooldown) and derives a signal. prevSHA256 is the caller's last
// recorded hash for this URL ("" if never checked); the returned string is
// the hash to persist for next time — callers must persist it regardless of
// Changed, so the cooldown-via-cache and the hash-diff both advance.
func CheckUnblockSignal(ctx context.Context, svc Service, rawURL, prevSHA256 string, ttl time.Duration) (UnblockSignal, string, error) {
	if m := githubPRRe.FindStringSubmatch(rawURL); m != nil {
		return checkGitHubState(ctx, svc, fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls/%s", m[1], m[2], m[3]), ttl, "pull request")
	}
	if m := githubIssueRe.FindStringSubmatch(rawURL); m != nil {
		return checkGitHubState(ctx, svc, fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%s", m[1], m[2], m[3]), ttl, "issue")
	}
	doc, err := svc.FetchWithTTL(ctx, rawURL, ttl)
	if err != nil {
		return UnblockSignal{}, prevSHA256, err
	}
	sha := sha256Hex(doc.Text)
	if prevSHA256 != "" && sha != prevSHA256 {
		return UnblockSignal{Changed: true, Detail: "page content changed since last check", Cached: doc.Cached}, sha, nil
	}
	return UnblockSignal{Cached: doc.Cached}, sha, nil
}

type githubStateResponse struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
}

// checkGitHubState uses FetchDirect, not the normal Fetch chain — firecrawl's
// markdown extraction would mangle a JSON API response, and the GitHub API
// itself needs no scraping help.
func checkGitHubState(ctx context.Context, svc Service, apiURL string, ttl time.Duration, kind string) (UnblockSignal, string, error) {
	doc, err := svc.FetchDirect(ctx, apiURL, ttl)
	if err != nil {
		return UnblockSignal{}, "", err
	}
	sha := sha256Hex(doc.Text)
	var gh githubStateResponse
	if err := json.Unmarshal([]byte(doc.Text), &gh); err != nil {
		return UnblockSignal{Cached: doc.Cached}, sha, fmt.Errorf("web: parse github %s response: %w", kind, err)
	}
	if gh.Merged {
		return UnblockSignal{Changed: true, Detail: kind + " merged", Cached: doc.Cached}, sha, nil
	}
	if gh.State == "closed" {
		return UnblockSignal{Changed: true, Detail: kind + " closed", Cached: doc.Cached}, sha, nil
	}
	return UnblockSignal{Cached: doc.Cached}, sha, nil
}
