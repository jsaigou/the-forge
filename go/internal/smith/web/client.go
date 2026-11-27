// SPDX-License-Identifier: Apache-2.0

package web

// client.go — shared HTTP plumbing for the three adapters. Mirrors
// internal/providers/client.go's httpClient interface indirection (tests
// inject an httptest.Server-backed client without this package exporting
// *http.Client), plus two concrete transports: a plain one for the
// operator-configured searxng/firecrawl base URLs, and a guarded one for
// `direct`, which fetches arbitrary URLs a search result chose.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/jsaigou/the-forge/internal/netguard"
)

// maxBodyBytes caps every response body (docs/v5-smith.md §4.8: "1 MB body
// cap"). One extra byte is read to detect truncation without buffering the
// whole (potentially huge) upstream body.
const maxBodyBytes = 1 << 20

// httpClient is the minimal *http.Client surface the adapters use.
type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

// fetchResult is the raw outcome of one HTTP call before an adapter
// interprets the body.
type fetchResult struct {
	Body        []byte
	StatusCode  int
	ContentType string
	Truncated   bool
}

func doGet(ctx context.Context, c httpClient, userAgent, url string, headers map[string]string, timeout time.Duration) (fetchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return fetchResult{}, err
	}
	return doRequest(c, userAgent, req, headers)
}

func doPostJSON(ctx context.Context, c httpClient, userAgent, url string, body any, headers map[string]string, timeout time.Duration) (fetchResult, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	raw, err := json.Marshal(body)
	if err != nil {
		return fetchResult{}, err
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return fetchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return doRequest(c, userAgent, req, headers)
}

func doRequest(c httpClient, userAgent string, req *http.Request, headers map[string]string) (fetchResult, error) {
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return fetchResult{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return fetchResult{}, err
	}
	truncated := false
	if len(raw) > maxBodyBytes {
		raw = raw[:maxBodyBytes]
		truncated = true
	}
	return fetchResult{
		Body:        raw,
		StatusCode:  resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Truncated:   truncated,
	}, nil
}

func newPlainHTTPClient() *http.Client {
	return &http.Client{}
}

// newGuardedHTTPClient is netguard.NewClient (dial-time SSRF guard; see there). allow, when non-nil, overrides the guard for hosts it
// approves; production leaves it nil, tests pointing `direct` at an httptest.Server (127.0.0.1) must set it explicitly.
func newGuardedHTTPClient(allow func(host string) bool) *http.Client {
	return netguard.NewClient(allow)
}

// isPublicIP is the SSRF predicate shared with every other server-side fetcher; the rules (and why they are a table, not a list of
// helper calls) live in internal/netguard.
func isPublicIP(ip net.IP) bool { return netguard.IsPublic(ip) }

// validateFetchURL is netguard.ValidateURL. It runs before ANY fetch adapter, including firecrawl, which otherwise bypasses the direct
// adapter's dial-time guard; the direct adapter keeps its dial-time check as defence in depth against DNS rebinding.
func validateFetchURL(ctx context.Context, rawURL string, allow func(host string) bool) error {
	return netguard.ValidateURL(ctx, rawURL, allow)
}
