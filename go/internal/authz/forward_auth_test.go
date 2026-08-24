// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"net/http/httptest"
	"testing"
)

func TestForwardAuthHeaderTrustedPeer(t *testing.T) {
	p := &ForwardAuthHeaderProvider{
		HeaderName:    "X-Auth-Request-User",
		TrustedCIDRs:  ParseCIDRs("10.0.0.0/8,172.16.0.0/12"),
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.1.5:54321"
	r.Header.Set("X-Auth-Request-User", "user@example.com")
	principal, ok := p.Identify(r)
	if !ok {
		t.Fatal("Identify failed for trusted peer")
	}
	if principal != "user@example.com" {
		t.Errorf("principal = %q, want user@example.com", principal)
	}
}

func TestForwardAuthHeaderUntrustedPeer(t *testing.T) {
	p := &ForwardAuthHeaderProvider{
		HeaderName:   "X-Auth-Request-User",
		TrustedCIDRs: ParseCIDRs("10.0.0.0/8"),
	}
	// Peer from 192.168.x.x — not in the trusted CIDR.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.5:54321"
	r.Header.Set("X-Auth-Request-User", "attacker@example.com")
	_, ok := p.Identify(r)
	if ok {
		t.Error("Identify should fail for untrusted peer (spoofing guard)")
	}
}

func TestForwardAuthHeaderNoTrustedCIDRs(t *testing.T) {
	p := &ForwardAuthHeaderProvider{
		HeaderName:   "X-Auth-Request-User",
		TrustedCIDRs: nil, // fail-closed
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Auth-Request-User", "user@example.com")
	_, ok := p.Identify(r)
	if ok {
		t.Error("Identify should fail when no trusted CIDRs configured")
	}
}

func TestForwardAuthHeaderEmptyHeader(t *testing.T) {
	p := &ForwardAuthHeaderProvider{
		HeaderName:   "X-Auth-Request-User",
		TrustedCIDRs: ParseCIDRs("10.0.0.0/8"),
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	// No header set.
	_, ok := p.Identify(r)
	if ok {
		t.Error("Identify should fail when header is empty")
	}
}

func TestForwardAuthHeaderDefaultHeaderName(t *testing.T) {
	p := &ForwardAuthHeaderProvider{
		HeaderName:   "", // should default to X-Auth-Request-User
		TrustedCIDRs: ParseCIDRs("10.0.0.0/8"),
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Auth-Request-User", "user@example.com")
	principal, ok := p.Identify(r)
	if !ok {
		t.Fatal("Identify failed with default header name")
	}
	if principal != "user@example.com" {
		t.Errorf("principal = %q", principal)
	}
}

func TestForwardAuthHeaderLoopbackPeer(t *testing.T) {
	p := &ForwardAuthHeaderProvider{
		HeaderName:   "X-Auth-Request-User",
		TrustedCIDRs: ParseCIDRs("127.0.0.0/8"),
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Auth-Request-User", "user@example.com")
	principal, ok := p.Identify(r)
	if !ok {
		t.Fatal("Identify failed for loopback peer")
	}
	if principal != "user@example.com" {
		t.Errorf("principal = %q", principal)
	}
}

func TestParseCIDRs(t *testing.T) {
	cidrs := ParseCIDRs("10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16")
	if len(cidrs) != 3 {
		t.Fatalf("ParseCIDRs = %d, want 3", len(cidrs))
	}
	// Invalid entries are skipped.
	cidrs = ParseCIDRs("10.0.0.0/8, invalid, 172.16.0.0/12")
	if len(cidrs) != 2 {
		t.Fatalf("ParseCIDRs with invalid entry = %d, want 2", len(cidrs))
	}
	// Empty string.
	cidrs = ParseCIDRs("")
	if len(cidrs) != 0 {
		t.Fatalf("ParseCIDRs('') = %d, want 0", len(cidrs))
	}
}

func TestForwardAuthHeaderName(t *testing.T) {
	p := &ForwardAuthHeaderProvider{}
	if p.Name() != "forward_auth_header" {
		t.Errorf("Name = %q, want forward_auth_header", p.Name())
	}
}
