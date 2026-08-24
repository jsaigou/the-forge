// SPDX-License-Identifier: Apache-2.0

package smith

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/collector"
)

func TestTailscalePeers_RegisteredDeepOnly(t *testing.T) {
	c := findCheck(t, "tailscale_peers")
	if c.Fast {
		t.Error("tailscale_peers must be deep-sweep only (Fast=false)")
	}
}

func TestTailscalePeers_NilSeam(t *testing.T) {
	env := &CheckEnv{TailscaleWatchPeers: []string{"pairnode"}}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityInfo || f.Evidence["skipped"] == nil {
		t.Errorf("f = %+v, want a skip finding", f)
	}
}

func TestTailscalePeers_NoWatchList(t *testing.T) {
	env := &CheckEnv{
		TailscalePeers: func(context.Context) ([]collector.Peer, bool) { return nil, true },
	}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok (nothing configured to watch)", f.Severity)
	}
}

func TestTailscalePeers_FetchFailure(t *testing.T) {
	env := &CheckEnv{
		TailscaleWatchPeers: []string{"pairnode"},
		TailscalePeers:      func(context.Context) ([]collector.Peer, bool) { return nil, false },
	}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %s, want warn (LocalAPI unreachable)", f.Severity)
	}
}

func TestTailscalePeers_AllOnline(t *testing.T) {
	env := &CheckEnv{
		TailscaleWatchPeers: []string{"pairnode", "core"},
		TailscalePeers: func(context.Context) ([]collector.Peer, bool) {
			return []collector.Peer{
				{DNSName: "pairnode.example.ts.net.", Online: true},
				{DNSName: "core.example.ts.net.", Online: true},
				{DNSName: "sastun.example.ts.net.", Online: false}, // unwatched, must not affect result
			}, true
		},
	}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityOK {
		t.Errorf("severity = %s, want ok", f.Severity)
	}
}

func TestTailscalePeers_OneOffline(t *testing.T) {
	env := &CheckEnv{
		TailscaleWatchPeers: []string{"pairnode", "core"},
		TailscalePeers: func(context.Context) ([]collector.Peer, bool) {
			return []collector.Peer{
				{DNSName: "pairnode.example.ts.net.", Online: false},
				{DNSName: "core.example.ts.net.", Online: true},
			}, true
		},
	}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %s, want warn", f.Severity)
	}
	if f.Summary == "" {
		t.Error("expected a non-empty summary naming the offline peer")
	}
}

func TestTailscalePeers_WatchedPeerNotAPeerAtAll(t *testing.T) {
	// A watched hostname that isn't in the peer list at all (never paired,
	// typo'd in settings, etc.) must be treated the same as offline — never
	// silently ignored.
	env := &CheckEnv{
		TailscaleWatchPeers: []string{"nonexistent-host"},
		TailscalePeers: func(context.Context) ([]collector.Peer, bool) {
			return []collector.Peer{{DNSName: "pairnode.example.ts.net.", Online: true}}, true
		},
	}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %s, want warn (watched peer not found)", f.Severity)
	}
}

// TestTailscalePeers_PrefixMatchNotSubstring guards the same DNSName
// prefix-match convention NodeOnline uses — "core" must not accidentally
// match "core-unlock.example.ts.net." (a real distinct peer seen
// live on ForgeHost).
func TestTailscalePeers_PrefixMatchNotSubstring(t *testing.T) {
	env := &CheckEnv{
		TailscaleWatchPeers: []string{"core"},
		TailscalePeers: func(context.Context) ([]collector.Peer, bool) {
			return []collector.Peer{
				{DNSName: "core-unlock.example.ts.net.", Online: true},
			}, true
		},
	}
	f := runTailscalePeers(context.Background(), env)
	if f.Severity != SeverityWarn {
		t.Errorf("severity = %s, want warn (core itself was never actually found)", f.Severity)
	}
}
