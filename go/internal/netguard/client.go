// SPDX-License-Identifier: Apache-2.0

package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// Allow, when non-nil, approves hosts that bypass the guard. Production passes nil; tests that point at an httptest.Server on 127.0.0.1 must
// pass one explicitly.
type Allow func(host string) bool

// NewClient returns an HTTP client whose DialContext resolves the host AT DIAL TIME, rejects any non-public address, and dials the vetted IP
// itself. Checking a separate, earlier lookup would let a DNS-rebinding answer differ between the check and the connect. Every redirect hop
// dials through the same transport, so a redirect to an internal address is refused too. The transport sets no Proxy, so HTTP(S)_PROXY in
// the environment cannot route around the guard. Callers set Timeout.
func NewClient(allow Allow) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
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
					return nil, fmt.Errorf("netguard: resolve %q: %w", host, err)
				}
				var lastErr error
				for _, ip := range ips {
					if !IsPublic(ip.IP) {
						lastErr = fmt.Errorf("netguard: fetch blocked (non-public address %s for %q)", ip.IP, host)
						continue
					}
					conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
					if dialErr == nil {
						return conn, nil
					}
					lastErr = dialErr
				}
				if lastErr == nil {
					lastErr = fmt.Errorf("netguard: no address for %q", host)
				}
				return nil, lastErr
			},
		},
	}
}

// ValidateURL checks rawURL before any fetch: http or https only, a host, no embedded credentials, and every resolved address public. It is
// the pre-check for adapters that fetch from a third party (which never pass through NewClient); the dial-time check in NewClient remains the
// defence against rebinding for direct fetches.
func ValidateURL(ctx context.Context, rawURL string, allow Allow) error {
	u, err := parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if allow != nil && allow(host) {
		return nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("netguard: resolve %q: %w", host, err)
	}
	for _, ip := range ips {
		if !IsPublic(ip.IP) {
			return fmt.Errorf("netguard: fetch blocked (non-public address %s for %q)", ip.IP, host)
		}
	}
	return nil
}

// CheckURL is the save-time, no-network check for an operator-supplied URL: the shape rules of ValidateURL, and a literal IP host must be
// public. A hostname is not resolved here (DNS can change after the save); the dial-time guard in NewClient is what stops it later.
func CheckURL(rawURL string) error {
	u, err := parse(rawURL)
	if err != nil {
		return err
	}
	if a, perr := netip.ParseAddr(u.Hostname()); perr == nil && !IsPublicAddr(a) {
		return fmt.Errorf("netguard: %s is not a public address", a)
	}
	return nil
}

func parse(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("netguard: parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("netguard: scheme %q not allowed (http/https only)", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, errors.New("netguard: url has no host")
	}
	if u.User != nil {
		return nil, errors.New("netguard: credentials in the url are not allowed")
	}
	return u, nil
}
