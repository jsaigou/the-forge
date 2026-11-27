// SPDX-License-Identifier: Apache-2.0

package providers

// client.go — shared HTTP plumbing for the health-probe + credits clients.
// Kept minimal + dependency-free (net/http only) so the package stays
// portable across the daemon + tests. The httpClient interface lets tests
// inject an httptest.Server-backed client without leaking *http.Client
// into the package's public API.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/netguard"
)

// httpClient is the minimal *http.Client surface the fetchers use. Defined
// as an interface so tests can substitute a test-only client (e.g. one
// that pins a transport to an httptest.Server). *http.Client satisfies it
// implicitly.
type httpClient interface {
	Do(*http.Request) (*http.Response, error)
}

// newDefaultHTTPClient returns a client with the probe timeout applied as
// the overall request timeout. The catalog refresh wraps each call in a
// context with the same timeout, so this is belt-and-suspenders against a
// client that ignores the request context (some do).
func newDefaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		// Go keeps Authorization on a redirect to the same host, including https -> http, which would put the provider key on the wire in
		// clear. Refuse any hop that downgrades the scheme, and cap the chain.
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("providers: stopped after 5 redirects")
			}
			if len(via) > 0 && via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return errors.New("providers: refusing a redirect from https to " + req.URL.Scheme)
			}
			return nil
		},
		// Default transport is fine — these are short-lived fetches to
		// external provider APIs, not high-throughput routing.
	}
}

// fetchJSON issues a GET to url with the given bearer token + timeout and
// returns the raw JSON body. A non-2xx response, network error, or
// deadline exceeded returns an error — the caller decides whether to fall
// back to a stale cache or surface "unknown".
//
// The bearer token is the provider's API key (a secret); it is sent only
// over TLS (callers must use https:// URLs) and never appears in logs or
// error messages (errors wrap the URL + status, not the token).
func fetchJSON(ctx context.Context, c httpClient, url, bearer string, timeout time.Duration) ([]byte, int, error) {
	return fetchJSONWithHeaders(ctx, c, url, bearer, nil, timeout)
}

// fetchJSONWithHeaders is fetchJSON plus arbitrary extra request headers —
// used by clients whose provider requires more than just a bearer token
// (e.g. AI&'s Analytics API, which also requires X-Org-ID; see
// docs.aiand.com/analytics/metrics/ "Auth"). extraHeaders may be nil.
func fetchJSONWithHeaders(ctx context.Context, c httpClient, url, bearer string, extraHeaders map[string]string, timeout time.Duration) ([]byte, int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if bearer != "" {
		// The provider key is a secret: send it over TLS, or to a loopback test server, never over clear text to a remote host.
		if !bearerTransportOK(req.URL) {
			return nil, 0, errors.New("providers: refusing to send a bearer token over a non-TLS, non-loopback URL")
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := ioReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// ioReadAll is io.ReadAll, indirected so the package's only io use is
// here (keeps imports tidy; the test file can substitute a no-op reader).
func ioReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	return io.ReadAll(r)
}

// bearerTransportOK reports whether a bearer token may be attached to a request for u: https anywhere, or plain http only to a destination that
// is not on the public internet: loopback, private/CGNAT/link-local literals (the tailnet lives in the CGNAT block), "localhost" and *.ts.net names.
// Those paths are local or WireGuard-encrypted, so cleartext http there does not expose the key; plain http to a public host does.
func bearerTransportOK(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".ts.net") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && !netguard.IsPublic(ip)
}
