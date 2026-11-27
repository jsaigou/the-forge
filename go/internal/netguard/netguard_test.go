// SPDX-License-Identifier: Apache-2.0

package netguard

import (
	"net"
	"net/netip"
	"testing"
)

// The verdicts below come from probing the previous smith/web isPublicIP with the same addresses; the ones marked "was public" are the bypasses.
func TestIsPublic(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
		note string
	}{
		// public
		{"8.8.8.8", true, ""}, {"1.1.1.1", true, ""}, {"2606:4700::1111", true, ""}, {"2001:4860:4860::8888", true, "just outside 2001::/23"},
		{"100.63.255.255", true, "just below CGNAT"}, {"100.128.0.0", true, "just above CGNAT"},
		{"172.15.255.255", true, "just below 172.16/12"}, {"172.32.0.0", true, "just above 172.16/12"},
		{"198.17.255.255", true, "just below 198.18/15"}, {"198.20.0.0", true, "just above 198.18/15"},
		{"223.255.255.255", true, "last before multicast"},
		// loopback, private, link-local, CGNAT (the tailnet), unspecified
		{"0.0.0.0", false, ""}, {"127.0.0.1", false, ""}, {"127.255.255.255", false, ""}, {"10.0.0.1", false, ""}, {"172.16.0.1", false, ""}, {"172.31.255.255", false, ""},
		{"192.168.1.1", false, ""}, {"169.254.169.254", false, "cloud metadata"}, {"100.64.0.1", false, "tailnet"}, {"100.127.255.255", false, "tailnet"},
		{"::", false, ""}, {"::1", false, ""}, {"fe80::1", false, ""}, {"fc00::1", false, ""}, {"fd12:3456::1", false, ""},
		// was public
		{"0.1.2.3", false, "0.0.0.0/8 beyond 0.0.0.0"}, {"192.0.0.1", false, "IETF protocol assignments"}, {"192.0.2.1", false, "TEST-NET-1"},
		{"192.88.99.1", false, "6to4 relay anycast"}, {"198.18.0.1", false, "benchmarking"}, {"198.19.255.255", false, "benchmarking"},
		{"198.51.100.1", false, "TEST-NET-2"}, {"203.0.113.1", false, "TEST-NET-3"},
		{"224.0.0.1", false, "multicast"}, {"239.1.1.1", false, "multicast"}, {"240.0.0.1", false, "reserved"}, {"255.255.255.255", false, "broadcast"},
		{"ff02::1", false, "multicast"}, {"ff0e::1", false, "global multicast"},
		{"2001:db8::1", false, "documentation"}, {"3fff::1", false, "documentation"}, {"100::1", false, "discard-only"}, {"2001::1", false, "Teredo"}, {"2001:1::1", false, "IETF protocol assignments"},
		{"5f00::1", false, "SRv6 SIDs"},
		// IPv6 forms that embed an IPv4 address: judged by what they carry, or refused outright
		{"::ffff:127.0.0.1", false, "mapped loopback"}, {"::ffff:10.0.0.1", false, "mapped private"}, {"::ffff:198.18.0.1", false, "mapped benchmarking"}, {"::ffff:8.8.8.8", true, "mapped public"},
		{"64:ff9b::7f00:1", false, "NAT64 embedding 127.0.0.1"}, {"64:ff9b::808:808", false, "NAT64 refused outright, even for a public v4"}, {"64:ff9b:1::1", false, "local-use NAT64"},
		{"2002:7f00:1::", false, "6to4 embedding 127.0.0.1"}, {"2002:808:808::", false, "6to4 refused outright"},
		{"::7f00:1", false, "v4-compatible ::/96 form"},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test address %q", c.ip)
		}
		if got := IsPublic(ip); got != c.want {
			t.Errorf("IsPublic(%s) = %v, want %v (%s)", c.ip, got, c.want, c.note)
		}
	}
	if IsPublic(nil) || IsPublic(net.IP{1, 2, 3}) {
		t.Error("a nil or malformed address must never be public")
	}
}

// Every address in each rejected prefix must be rejected: first, last and one in the middle.
func TestEveryBlockedPrefixRejectsItsWholeRange(t *testing.T) {
	for _, p := range blocked {
		first := p.Masked().Addr()
		last := lastAddr(p)
		mid := first
		for i := 0; i < 3; i++ { // a few steps in, staying inside the prefix
			if n := mid.Next(); p.Contains(n) {
				mid = n
			}
		}
		for _, a := range []netip.Addr{first, mid, last} {
			if IsPublicAddr(a) {
				t.Errorf("%s inside %s was reported public", a, p)
			}
		}
	}
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().AsSlice()
	for i := range b {
		bits := p.Bits() - i*8
		switch {
		case bits >= 8:
		case bits <= 0:
			b[i] = 0xff
		default:
			b[i] |= 0xff >> bits
		}
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}
