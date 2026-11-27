// SPDX-License-Identifier: Apache-2.0

// Package netguard decides whether an IP address is safe to fetch on behalf of untrusted input (SSRF defence).
//
// The rule is an allow-by-default-deny: an address is public only if it is global unicast AND outside every special-purpose prefix below
// (the IANA IPv4 and IPv6 special-purpose registries, plus this fleet's tailnet range, which is CGNAT 100.64.0.0/10). A deny-list of "the
// ranges we thought of" missed NAT64, 6to4, Teredo and benchmarking space; anything this table does not name but that is not global unicast
// still fails closed through netip's IsGlobalUnicast.
//
// IPv6 forms that EMBED an IPv4 address (NAT64 64:ff9b::/96, 6to4 2002::/16, Teredo 2001::/32, v4-compatible ::/96) are refused outright rather
// than unwrapped: nothing a research fetch legitimately needs lives there, and a gateway that translates them would reach the embedded address.
// IPv4-mapped addresses (::ffff:a.b.c.d) ARE unwrapped and judged as the IPv4 address they are.
package netguard

import (
	"net"
	"net/netip"
)

// blocked lists the special-purpose prefixes; the tests assert that every one rejects its whole range.
var blocked = mustPrefixes(
	// IPv4
	"0.0.0.0/8",       // "this network" (only 0.0.0.0 is locally routable, the rest is never a destination)
	"10.0.0.0/8",      // RFC 1918
	"100.64.0.0/10",   // CGNAT: the tailnet
	"127.0.0.0/8",     // loopback
	"169.254.0.0/16",  // link-local, incl. cloud metadata 169.254.169.254
	"172.16.0.0/12",   // RFC 1918
	"192.0.0.0/24",    // IETF protocol assignments
	"192.0.2.0/24",    // TEST-NET-1
	"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
	"192.168.0.0/16",  // RFC 1918
	"198.18.0.0/15",   // benchmarking, commonly used for internal test and VPN space
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"224.0.0.0/4",     // multicast
	"240.0.0.0/4",     // reserved, incl. 255.255.255.255 broadcast
	// IPv6
	"::/96",          // unspecified, loopback, and the deprecated v4-compatible form
	"64:ff9b::/96",   // NAT64: embeds an IPv4 address
	"64:ff9b:1::/48", // local-use NAT64
	"100::/64",       // discard-only
	"2001::/23",      // IETF protocol assignments, incl. Teredo 2001::/32
	"2001:db8::/32",  // documentation
	"2002::/16",      // 6to4: embeds an IPv4 address
	"3fff::/20",      // documentation
	"5f00::/16",      // SRv6 SIDs
	"fc00::/7",       // unique local
	"fe80::/10",      // link-local
	"ff00::/8",       // multicast, every scope
)

func mustPrefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

// IsPublic reports whether ip is a public, routable unicast address. A nil or malformed address is never public.
func IsPublic(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	return IsPublicAddr(a)
}

// IsPublicAddr is IsPublic for a netip.Addr.
func IsPublicAddr(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	if !a.IsValid() || !a.IsGlobalUnicast() {
		return false
	}
	for _, p := range blocked {
		if p.Contains(a) {
			return false
		}
	}
	return true
}
