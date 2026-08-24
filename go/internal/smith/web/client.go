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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
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

// newGuardedHTTPClient returns a client whose DialContext rejects
// loopback/private/link-local/CGNAT destinations, resolved and checked at
// dial time — not by pre-resolving the host and validating a separate
// lookup, which a DNS-rebinding attacker could answer differently between
// the check and the connect. allow, when non-nil, overrides the guard for
// hosts it approves; production leaves it nil. Tests pointing `direct` at
// an httptest.Server (127.0.0.1) must set it explicitly.
func newGuardedHTTPClient(allow func(host string) bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if allow != nil && allow(host) {
					return dialer.DialContext(ctx, network, addr)
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, fmt.Errorf("web: resolve %q: %w", host, err)
				}
				var lastErr error
				for _, ip := range ips {
					if !isPublicIP(ip.IP) {
						lastErr = fmt.Errorf("web: direct fetch blocked (non-public address %s for %q)", ip.IP, host)
						continue
					}
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				if lastErr == nil {
					lastErr = fmt.Errorf("web: no address for %q", host)
				}
				return nil, lastErr
			},
		},
	}
}

// isPublicIP rejects loopback, RFC1918/ULA private, link-local, unspecified,
// and CGNAT (100.64.0.0/10 — this fleet's tailnet range) addresses.
// net.IP.IsPrivate() does not cover CGNAT, hence the explicit check.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
		return false
	}
	return true
}

// validateFetchURL validates that a URL's resolved host has only public IP
// addresses, preventing SSRF to internal services (a0, MCP, ops dashboard,
// ComfyUI, embeddings — all on the tailnet). This check runs before ANY
// fetch adapter, including firecrawl, which otherwise bypasses the direct
// adapter's dial-time guard in newGuardedHTTPClient. The direct adapter
// retains its dial-time check as defense-in-depth against DNS rebinding.
// allow, when non-nil, overrides the guard for hosts it approves (tests
// pointing at httptest.Server on 127.0.0.1).
func validateFetchURL(ctx context.Context, rawURL string, allow func(host string) bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("web: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("web: scheme %q not allowed (http/https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("web: url has no host")
	}
	if allow != nil && allow(host) {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("web: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return fmt.Errorf("web: fetch blocked (non-public address %s for %q)", ip.IP, host)
		}
	}
	return nil
}
