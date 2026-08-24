// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"net/netip"
	"testing"
)

func TestParseToken(t *testing.T) {
	cases := []struct {
		token string
		kind  KeyKind
		keyid string
		ok    bool
	}{
		{"sk-forge-a6a0da5609b8-abcdefghijklmnop", KindForge, "a6a0da5609b8", true},
		{"sk-router-0123456789ab-secretsecret1234", KindRouter, "0123456789ab", true},
		{"sk-mcp-ffffffffffff-secret_secret-99999", KindMCP, "ffffffffffff", true},
		{"sk-mcp-FFFFFFFFFFFF-secretsecret1234", "", "", false}, // uppercase keyid
		{"sk-mcp-ffffffffff-secretsecret1234", "", "", false},   // keyid too short
		{"sk-other-a6a0da5609b8-secretsecret1234", "", "", false},
		{"Bearer sk-forge-a6a0da5609b8-x", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		kind, keyid, _, err := ParseToken(c.token)
		if c.ok != (err == nil) {
			t.Errorf("ParseToken(%q) err=%v, want ok=%v", c.token, err, c.ok)
			continue
		}
		if c.ok && (kind != c.kind || keyid != c.keyid) {
			t.Errorf("ParseToken(%q) = (%s, %s), want (%s, %s)", c.token, kind, keyid, c.kind, c.keyid)
		}
	}
}

func TestRoleAllows(t *testing.T) {
	if !RoleAdmin.Allows(RoleOperator) || !RoleOperator.Allows(RoleViewer) {
		t.Error("role ordering broken")
	}
	if RoleViewer.Allows(RoleOperator) || RoleOperator.Allows(RoleAdmin) {
		t.Error("privilege escalation")
	}
	if Role("bogus").Allows(RoleViewer) {
		t.Error("unknown role must not allow anything")
	}
}

func TestIsTailnetAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"100.100.100.100": true,  // ForgeHost's tailnet IP
		"100.64.0.1":    true,  // range start
		"100.127.255.9": true,  // range end
		"100.128.0.1":   false, // just past /10
		"192.168.1.10":  false,
		"127.0.0.1":     false,
	} {
		if got := IsTailnetAddr(netip.MustParseAddr(addr)); got != want {
			t.Errorf("IsTailnetAddr(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestEffectiveRemoteAddr(t *testing.T) {
	loop := netip.MustParseAddr("127.0.0.1")
	tail := netip.MustParseAddr("100.100.100.100")

	// tailscale serve path: loopback + XFF -> trust XFF first hop.
	if got := EffectiveRemoteAddr(loop, "100.100.100.100, 127.0.0.1"); got != tail {
		t.Errorf("loopback+XFF = %v, want %v", got, tail)
	}
	// Direct path: real tailnet IP -> XFF must be IGNORED even if present.
	if got := EffectiveRemoteAddr(tail, "8.8.8.8"); got != tail {
		t.Errorf("direct+spoofed XFF = %v, want %v", got, tail)
	}
	// Loopback with garbage XFF falls back to remote addr.
	if got := EffectiveRemoteAddr(loop, "not-an-ip"); got != loop {
		t.Errorf("bad XFF = %v, want %v", got, loop)
	}
}
