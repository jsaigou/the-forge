// SPDX-License-Identifier: Apache-2.0

package authz

// identity.go — Sprint 0-AUTH §3.2: NetworkIdentityProvider.
//
// An interface that resolves an inbound request to a trusted network
// principal (or none). Selected by config; this is what lets one policy engine
// serve tailnet and public deployments:
//
//   - tailscale: resolve the actual login via Tailscale LocalAPI WhoIs on the
//     source IP. Dual-path (§3.2): if RemoteAddr is loopback (request arrived
//     via the svc:ops service proxy → localhost:5000), WhoIs the trusted
//     X-Forwarded-For; otherwise WhoIs RemoteAddr directly.
//   - forward_auth_header: a trusted reverse proxy sets a header (Phase C).
//     The header is trusted ONLY when the immediate peer is in a configured
//     trusted CIDR (§8 spoofing guard).
//   - none: no network identity; every resource falls through to
//     password/passkey (public deployments with no trusted front door).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"
)

// NetworkIdentityProvider resolves an inbound request to a trusted network
// principal (or none).
type NetworkIdentityProvider interface {
	// Identify returns the trusted network principal for r, or ok=false.
	// The principal is opaque to the policy engine — it is matched against
	// identity_links to bootstrap a local account session.
	Identify(r *http.Request) (principal string, ok bool)
	// Name returns the provider identifier ("tailscale", "none", ...).
	Name() string
}

// NoNetworkIdentity is the "none" provider: no network identity is ever
// resolved. Every resource falls through to password/passkey.
type NoNetworkIdentity struct{}

func (NoNetworkIdentity) Identify(_ *http.Request) (string, bool) { return "", false }
func (NoNetworkIdentity) Name() string                            { return "none" }

// ── Tailscale LocalAPI WhoIs ────────────────────────────────────────────────

// TailscaleWhoIsClient is the minimal LocalAPI surface the identity provider
// needs. The real implementation dials the tailscaled unix socket; tests
// inject a fake.
type TailscaleWhoIsClient interface {
	// WhoIs resolves a remote IP:port to a Tailscale user login. Returns
	// ok=false when the IP is not a Tailscale peer or has no associated user
	// (tagged server nodes return no User — correctly not treated as human).
	WhoIs(ctx context.Context, remoteAddr string) (login string, ok bool)
}

// TailscaleIdentityProvider resolves network identity via Tailscale LocalAPI
// WhoIs on the source IP (§3.2). It uses the dual-path address resolution
// from authz.EffectiveRemoteAddr: loopback → trusted XFF, else RemoteAddr.
type TailscaleIdentityProvider struct {
	Client TailscaleWhoIsClient
}

func (t *TailscaleIdentityProvider) Name() string { return "tailscale" }

// Identify resolves the request's source IP to a Tailscale login via WhoIs.
// Dual-path (§3.2): loopback → trusted XFF, else RemoteAddr directly.
func (t *TailscaleIdentityProvider) Identify(r *http.Request) (string, bool) {
	remoteAddr, err := netip.ParseAddr(stripPort(r.RemoteAddr))
	if err != nil {
		return "", false
	}
	eff := EffectiveRemoteAddr(remoteAddr, r.Header.Get("X-Forwarded-For"))
	if !IsTailnetAddr(eff) {
		return "", false
	}
	// WhoIs needs ip:port; the port doesn't matter for user resolution but
	// the LocalAPI expects the canonical form. Use a dummy port.
	whoIsAddr := fmt.Sprintf("%s:0", eff.String())
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	login, ok := t.Client.WhoIs(ctx, whoIsAddr)
	if !ok || login == "" {
		return "", false
	}
	return login, true
}

func stripPort(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// TailscaleLocalWhoIsClient dials the tailscaled LocalAPI unix socket to
// resolve WhoIs. It extends the collector/tailscale.go seam (that file has
// NodeOnline; this adds WhoIs for the auth subsystem).
type TailscaleLocalWhoIsClient struct {
	SocketPath string
	client     *http.Client
}

const defaultTailscaleSocketPath = "/var/run/tailscale/tailscaled.sock"

// WhoIsResult is the subset of the LocalAPI /whois response we use.
// The user login is in UserProfile.LoginName (not User.Name). Tagged server
// nodes have Node.Tags set — they return a UserProfile with the machine name
// as LoginName, but we correctly reject them as non-human (§3.2).
type WhoIsResult struct {
	Node struct {
		Tags []string `json:"Tags"` // non-empty = tagged server → not human
	} `json:"Node"`
	UserProfile struct {
		LoginName string `json:"LoginName"`
	} `json:"UserProfile"`
}

func (c *TailscaleLocalWhoIsClient) client_() *http.Client {
	if c.client == nil {
		sock := c.SocketPath
		if sock == "" {
			sock = defaultTailscaleSocketPath
		}
		c.client = &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		}
	}
	return c.client
}

func (c *TailscaleLocalWhoIsClient) WhoIs(ctx context.Context, remoteAddr string) (string, bool) {
	url := fmt.Sprintf("http://local-tailscaled.sock/localapi/v0/whois?addr=%s", remoteAddr)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", false
	}
	resp, err := c.client_().Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", false
	}
	var result WhoIsResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", false
	}
	// Tagged server nodes are not human users (§3.2: "tagged server nodes
	// return no User — correctly not treated as human users").
	if len(result.Node.Tags) > 0 {
		return "", false
	}
	login := strings.TrimSpace(result.UserProfile.LoginName)
	if login == "" {
		return "", false
	}
	return login, true
}

// ── Forward-auth header provider (Phase C, §3.2/§8) ──────────────────────────

// ForwardAuthHeaderProvider resolves network identity from a trusted reverse
// proxy header (e.g. X-Auth-Request-User set by oauth2-proxy, Traefik
// ForwardAuth, Caddy reverse_proxy, etc.).
//
// Security (§8): the header is trusted ONLY when the immediate peer
// (RemoteAddr) is within the configured TrustedCIDRs. If the peer is not in a
// trusted range, the header is treated as attacker-supplied and ignored —
// the request falls through to unauthenticated. This prevents header spoofing
// by direct-to-port clients that set the header themselves.
//
// Config (§3.2): auth.provider.forward_auth_header.header_name (default
// X-Auth-Request-User), auth.provider.forward_auth_header.trusted_cidrs
// (comma-separated CIDRs, e.g. "10.0.0.0/8,172.16.0.0/12").
type ForwardAuthHeaderProvider struct {
	// HeaderName is the HTTP header carrying the trusted principal.
	// Default: X-Auth-Request-User.
	HeaderName string
	// TrustedCIDRs are the networks from which the header is trusted.
	// If empty, the provider never identifies anyone (fail-closed).
	TrustedCIDRs []netip.Prefix
}

func (f *ForwardAuthHeaderProvider) Name() string { return "forward_auth_header" }

// Identify resolves the principal from the configured header, but only when
// the immediate peer is in a trusted CIDR (§8 spoofing guard).
func (f *ForwardAuthHeaderProvider) Identify(r *http.Request) (string, bool) {
	if len(f.TrustedCIDRs) == 0 {
		return "", false
	}
	peer, err := netip.ParseAddr(stripPort(r.RemoteAddr))
	if err != nil {
		return "", false
	}
	trusted := false
	for _, cidr := range f.TrustedCIDRs {
		if cidr.Contains(peer) {
			trusted = true
			break
		}
	}
	if !trusted {
		return "", false
	}
	headerName := f.HeaderName
	if headerName == "" {
		headerName = "X-Auth-Request-User"
	}
	principal := strings.TrimSpace(r.Header.Get(headerName))
	if principal == "" {
		return "", false
	}
	return principal, true
}

// ParseCIDRs parses a comma-separated list of CIDR strings (e.g.
// "10.0.0.0/8,172.16.0.0/12") into netip.Prefix values. Invalid entries are
// skipped (the caller should validate that the result is non-empty before use).
func ParseCIDRs(raw string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range strings.Split(raw, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(s)
		if err != nil {
			continue
		}
		out = append(out, prefix)
	}
	return out
}
