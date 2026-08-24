// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// TailscaleLocalAPI answers node-online questions from tailscaled's LocalAPI
// unix socket — no `tailscale status` subprocess (V4 shelled out via
// tailscale.py). SocketPath "" uses the default tailscaled socket.
type TailscaleLocalAPI struct {
	SocketPath string
	client     *http.Client
}

const defaultTailscaleSocket = "/var/run/tailscale/tailscaled.sock"

// Peer is one tailnet peer, projected from tailscaled's LocalAPI status
// response (smith P6 FR8 — the full peer list, previously discarded by
// NodeOnline which only ever matched one name and returned a bool).
type Peer struct {
	DNSName  string `json:"dns_name"`
	Online   bool   `json:"online"`
	OS       string `json:"os"`
	Relay    string `json:"relay"`
	ExitNode bool   `json:"exit_node"`
}

// httpClient lazily builds (once) the unix-socket-dialing client shared by
// NodeOnline and Peers.
func (t *TailscaleLocalAPI) httpClient() *http.Client {
	if t.client == nil {
		sock := t.SocketPath
		if sock == "" {
			sock = defaultTailscaleSocket
		}
		t.client = &http.Client{
			Timeout: 3 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sock)
				},
			},
		}
	}
	return t.client
}

// tailscaleStatus is the subset of tailscaled's LocalAPI /status response
// this package reads.
type tailscaleStatus struct {
	Peer map[string]struct {
		DNSName      string `json:"DNSName"`
		Online       bool   `json:"Online"`
		OS           string `json:"OS"`
		Relay        string `json:"Relay"`
		ExitNodeOnly bool   `json:"ExitNodeOption"`
	} `json:"Peer"`
}

// fetchStatus GETs and decodes the LocalAPI status response, or reports ok
// false on any transport/decode failure — every caller degrades on that,
// never panics or half-reads.
func (t *TailscaleLocalAPI) fetchStatus(ctx context.Context) (tailscaleStatus, bool) {
	var status tailscaleStatus
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://local-tailscaled.sock/localapi/v0/status", nil)
	if err != nil {
		return status, false
	}
	resp, err := t.httpClient().Do(req)
	if err != nil {
		return status, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return status, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return status, false
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return status, false
	}
	return status, true
}

// NodeOnline reports whether a peer whose DNS name starts with "<node>." is
// online (same match rule as V4 monitor._tailscale_node_online).
func (t *TailscaleLocalAPI) NodeOnline(ctx context.Context, node string) bool {
	status, ok := t.fetchStatus(ctx)
	if !ok {
		return false
	}
	for _, peer := range status.Peer {
		if strings.HasPrefix(peer.DNSName, node+".") {
			return peer.Online
		}
	}
	return false
}

// Peers returns the full tailnet peer list, or (nil, false) on any
// transport/decode failure — smith's tailscale_peers check (P6 FR8) skips
// itself on false rather than reporting a peer list that might be a
// zero-value artifact of a failed fetch.
func (t *TailscaleLocalAPI) Peers(ctx context.Context) ([]Peer, bool) {
	status, ok := t.fetchStatus(ctx)
	if !ok {
		return nil, false
	}
	out := make([]Peer, 0, len(status.Peer))
	for _, p := range status.Peer {
		out = append(out, Peer{
			DNSName: p.DNSName, Online: p.Online, OS: p.OS, Relay: p.Relay, ExitNode: p.ExitNodeOnly,
		})
	}
	return out, true
}
