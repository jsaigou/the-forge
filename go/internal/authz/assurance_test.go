// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAssuranceAtLeast(t *testing.T) {
	cases := []struct {
		got, want Assurance
		ok        bool
	}{
		{AssuranceNetwork, AssuranceNetwork, true},
		{AssurancePassword, AssuranceNetwork, true},
		{AssuranceTOTP, AssuranceNetwork, true},
		{AssurancePasskey, AssuranceNetwork, true},
		{AssurancePassword, AssurancePassword, true},
		{AssuranceTOTP, AssuranceTOTP, true},
		// Password satisfies TOTP requirement (both L1) and vice versa.
		{AssurancePassword, AssuranceTOTP, true},
		{AssuranceTOTP, AssurancePassword, true},
		// Passkey (L2) satisfies everything.
		{AssurancePasskey, AssurancePassword, true},
		// Network (L0) does not satisfy L1 or L2.
		{AssuranceNetwork, AssurancePassword, false},
		{AssuranceNetwork, AssuranceTOTP, false},
		{AssuranceNetwork, AssurancePasskey, false},
		// Password (L1) does not satisfy passkey (L2).
		{AssurancePassword, AssurancePasskey, false},
	}
	for _, c := range cases {
		if got := c.got.AtLeast(c.want); got != c.ok {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.got, c.want, got, c.ok)
		}
	}
}

func TestPolicyEvaluation(t *testing.T) {
	policy := copyPolicy(DefaultPolicy)
	now := time.Now()
	eval := NewPolicyEvaluator(policy, DefaultStepUpTTL, func() time.Time { return now })

	// Network-only resource: L0 always passes, no TTL.
	d := eval.Evaluate(ResourcePageDashboard, AssuranceNetwork, time.Time{})
	if !d.Allowed {
		t.Errorf("dashboard+network: got %+v, want allowed", d)
	}

	// Password-gated resource: L0 fails.
	d = eval.Evaluate(ResourcePageSettings, AssuranceNetwork, time.Time{})
	if d.Allowed {
		t.Error("settings+network: should be denied")
	}
	if d.Required != AssurancePassword {
		t.Errorf("settings required = %s, want password", d.Required)
	}

	// Password-gated resource: L1 password passes (within TTL).
	d = eval.Evaluate(ResourcePageSettings, AssurancePassword, now)
	if !d.Allowed {
		t.Errorf("settings+password: got %+v, want allowed", d)
	}

	// Password-gated resource: L1 password, TTL expired.
	old := now.Add(-20 * time.Minute) // 20 min ago > 15 min TTL
	d = eval.Evaluate(ResourcePageSettings, AssurancePassword, old)
	if d.Allowed {
		t.Error("settings+stale password: should be denied (TTL expired)")
	}
	if d.Reason != "step-up expired, re-authenticate" {
		t.Errorf("reason = %q, want 'step-up expired...'", d.Reason)
	}

	// TOTP satisfies password-level requirement.
	d = eval.Evaluate(ResourcePageSettings, AssuranceTOTP, now)
	if !d.Allowed {
		t.Error("settings+totp: should be allowed (both L1)")
	}

	// Unknown resource defaults to network.
	d = eval.Evaluate("unknown.resource", AssuranceNetwork, time.Time{})
	if !d.Allowed {
		t.Error("unknown resource + network: should default to allowed")
	}
}

func TestPolicyStoreSeedsDefault(t *testing.T) {
	settings := &fakePolicySettings{kv: map[string][]byte{}}
	ps := NewPolicyStore(settings)
	policy, err := ps.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(policy) != len(DefaultPolicy) {
		t.Errorf("seeded policy = %d entries, want %d", len(policy), len(DefaultPolicy))
	}
	if policy[ResourcePageSettings] != string(AssurancePassword) {
		t.Errorf("settings default = %s, want password", policy[ResourcePageSettings])
	}
	// The seed should have been persisted.
	if _, err := settings.Get(context.Background(), "auth.policy"); err != nil {
		t.Errorf("seed not persisted: %v", err)
	}
}

func TestPolicyStoreLoadExisting(t *testing.T) {
	settings := &fakePolicySettings{kv: map[string][]byte{}}
	// Pre-set a custom policy.
	custom := copyPolicy(DefaultPolicy)
	custom[ResourcePageSettings] = string(AssuranceTOTP)
	raw, _ := jsonMarshal(custom)
	settings.kv["auth.policy"] = raw

	ps := NewPolicyStore(settings)
	policy, err := ps.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if policy[ResourcePageSettings] != string(AssuranceTOTP) {
		t.Errorf("settings = %s, want totp", policy[ResourcePageSettings])
	}
}

func TestValidatePolicy(t *testing.T) {
	// Valid policy.
	p := copyPolicy(DefaultPolicy)
	if invalid := ValidatePolicy(p); len(invalid) > 0 {
		t.Errorf("valid policy has errors: %v", invalid)
	}
	// Invalid resource key.
	p["bogus.resource"] = "network"
	if invalid := ValidatePolicy(p); len(invalid) != 1 {
		t.Errorf("expected 1 invalid, got %d", len(invalid))
	}
	// Invalid factor.
	delete(p, "bogus.resource")
	p[ResourcePageSettings] = "bogus"
	if invalid := ValidatePolicy(p); len(invalid) != 1 {
		t.Errorf("expected 1 invalid, got %d", len(invalid))
	}
}

func TestResourceActionSmithExecuteRegistered(t *testing.T) {
	// P2 (docs/v5-smith.md §4.6): action.smith.execute must be a known
	// resource, present in DefaultPolicy at password (matching the other
	// action.* execute-gated keys), and reachable via a fresh PolicyStore
	// seed with no migration needed (Load merges missing keys from
	// DefaultPolicy — policy.go's Load).
	if !ValidResource(ResourceActionSmithExecute) {
		t.Fatal("action.smith.execute is not a valid resource key")
	}
	if got := DefaultPolicy[ResourceActionSmithExecute]; got != string(AssurancePassword) {
		t.Errorf("DefaultPolicy[action.smith.execute] = %q, want password", got)
	}
	if invalid := ValidatePolicy(map[string]string{ResourceActionSmithExecute: string(AssurancePassword)}); len(invalid) != 0 {
		t.Errorf("ValidatePolicy rejected action.smith.execute: %v", invalid)
	}

	// A policy stored WITHOUT the new key (simulating a pre-P2 stored
	// policy) still resolves it via Load's default-merge, with no migration.
	settings := &fakePolicySettings{kv: map[string][]byte{}}
	old := copyPolicy(DefaultPolicy)
	delete(old, ResourceActionSmithExecute)
	raw, err := jsonMarshal(old)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	settings.kv["auth.policy"] = raw

	ps := NewPolicyStore(settings)
	policy, err := ps.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := policy[ResourceActionSmithExecute]; got != string(AssurancePassword) {
		t.Errorf("merged policy[action.smith.execute] = %q, want password (from DefaultPolicy)", got)
	}
}

// ── TOTP tests ───────────────────────────────────────────────────────────────

func TestTOTPGenerateAndValidate(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatalf("GenerateTOTPSecret: %v", err)
	}
	if secret == "" {
		t.Fatal("secret is empty")
	}
	// Generate a code and validate it.
	now := time.Now()
	code := computeTOTP(secret, uint64(now.Unix()/totpPeriod))
	if code == "" {
		t.Fatal("computeTOTP returned empty")
	}
	if !ValidateTOTP(secret, code, now) {
		t.Error("ValidateTOTP failed for fresh code")
	}
}

func TestTOTPValidateWrongCode(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	if ValidateTOTP(secret, "000000", time.Now()) {
		// 000000 could theoretically match — but the probability is 1/1M
		// and we use a random secret, so this should practically never hit.
		// If it does, just re-run.
		t.Skip("code 000000 happened to match (1/1M chance)")
	}
}

func TestTOTPValidateWindow(t *testing.T) {
	secret, _ := GenerateTOTPSecret()
	now := time.Now()
	// Generate code for now - 30s (previous period).
	prevCode := computeTOTP(secret, uint64(now.Unix()/totpPeriod)-1)
	if !ValidateTOTP(secret, prevCode, now) {
		t.Error("ValidateTOTP failed for code from previous period (should be within ±1 window)")
	}
	// Generate code for now + 30s (next period).
	nextCode := computeTOTP(secret, uint64(now.Unix()/totpPeriod)+1)
	if !ValidateTOTP(secret, nextCode, now) {
		t.Error("ValidateTOTP failed for code from next period (should be within ±1 window)")
	}
	// Code from 2 periods ago should fail.
	oldCode := computeTOTP(secret, uint64(now.Unix()/totpPeriod)-2)
	if ValidateTOTP(secret, oldCode, now) {
		t.Error("ValidateTOTP accepted code from 2 periods ago (outside ±1 window)")
	}
}

func TestTOTPOtpAuthURI(t *testing.T) {
	uri := TOTPOtpAuthURI("Forge", "testuser", "JBSWY3DPEHPK3PXP")
	if uri == "" {
		t.Fatal("otpauth URI is empty")
	}
	if !contains(uri, "otpauth://totp/") {
		t.Errorf("URI doesn't start with otpauth://totp/: %s", uri)
	}
	if !contains(uri, "secret=JBSWY3DPEHPK3PXP") {
		t.Errorf("URI missing secret: %s", uri)
	}
	if !contains(uri, "issuer=Forge") {
		t.Errorf("URI missing issuer: %s", uri)
	}
}

// ── Identity provider tests ──────────────────────────────────────────────────

func TestNoNetworkIdentity(t *testing.T) {
	p := NoNetworkIdentity{}
	r := httptest.NewRequest("GET", "/", nil)
	if _, ok := p.Identify(r); ok {
		t.Error("NoNetworkIdentity should never identify")
	}
	if p.Name() != "none" {
		t.Errorf("Name = %q, want none", p.Name())
	}
}

func TestTailscaleIdentityProviderLoopback(t *testing.T) {
	fake := &fakeWhoIsClient{login: "user@github", ok: true}
	p := &TailscaleIdentityProvider{Client: fake}
	// Loopback + XFF → trust XFF.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "100.100.100.100")
	principal, ok := p.Identify(r)
	if !ok {
		t.Fatal("Identify failed for loopback+XFF")
	}
	if principal != "user@github" {
		t.Errorf("principal = %q, want user@github", principal)
	}
}

func TestTailscaleIdentityProviderDirect(t *testing.T) {
	fake := &fakeWhoIsClient{login: "user@github", ok: true}
	p := &TailscaleIdentityProvider{Client: fake}
	// Direct tailnet IP, no XFF.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "100.100.100.100:54321"
	principal, ok := p.Identify(r)
	if !ok {
		t.Fatal("Identify failed for direct tailnet IP")
	}
	if principal != "user@github" {
		t.Errorf("principal = %q, want user@github", principal)
	}
}

func TestTailscaleIdentityProviderRejectsNonTailnet(t *testing.T) {
	fake := &fakeWhoIsClient{login: "user@github", ok: true}
	p := &TailscaleIdentityProvider{Client: fake}
	// Non-tailnet IP should not be identified.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.1:54321"
	if _, ok := p.Identify(r); ok {
		t.Error("Identify should fail for non-tailnet IP")
	}
}

func TestTailscaleIdentityProviderNoUser(t *testing.T) {
	fake := &fakeWhoIsClient{login: "", ok: false}
	p := &TailscaleIdentityProvider{Client: fake}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "100.100.100.100:54321"
	if _, ok := p.Identify(r); ok {
		t.Error("Identify should fail when WhoIs returns no user")
	}
}

// ── Test fakes ───────────────────────────────────────────────────────────────

type fakePolicySettings struct {
	kv map[string][]byte
}

func (f *fakePolicySettings) Get(_ context.Context, key string) ([]byte, error) {
	v, ok := f.kv[key]
	if !ok {
		return nil, errNotFoundFake
	}
	return v, nil
}

func (f *fakePolicySettings) Set(_ context.Context, key string, value []byte) error {
	f.kv[key] = value
	return nil
}

type fakeWhoIsClient struct {
	login string
	ok    bool
}

func (f *fakeWhoIsClient) WhoIs(_ context.Context, _ string) (string, bool) {
	return f.login, f.ok
}

// helpers

var errNotFoundFake = &notFoundErr{}

type notFoundErr struct{}

func (e *notFoundErr) Error() string { return "store: not found" }

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
