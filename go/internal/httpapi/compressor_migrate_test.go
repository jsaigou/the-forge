// SPDX-License-Identifier: Apache-2.0

package httpapi

// compressor_migrate_test.go — Sprint 3 (docs/v5-headroom-replacement.md):
// POST /api/v1/compressor/proxy/migrate, which moves an existing proxy row
// from a headroom@ (or legacy) unit onto the forge-compress@ template.

import (
	"context"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

func TestCompressorMigrate_MovesUnitAndRotatesToken(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	ctx := context.Background()
	if err := fh.SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Unit: "headroom@deepseek", Port: 8790,
		TargetURL: "https://api.deepseek.com/v1", Token: "old-secret", Label: "deepseek",
	}); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{"service":"deepseek"}`)))
	if w.Code != 200 {
		t.Fatalf("migrate = %d, want 200: %s", w.Code, w.Body.String())
	}

	if len(sys.stopped) != 1 || sys.stopped[0] != "headroom@deepseek" {
		t.Errorf("stopped = %v, want [headroom@deepseek] (the OLD unit torn down)", sys.stopped)
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "forge-compress@deepseek" {
		t.Errorf("restarted = %v, want [forge-compress@deepseek] (the NEW unit provisioned via Provision, which uses Restart — see the 2026-08-19 already-running-unit bugfix)", sys.restarted)
	}

	proxies, _ := fh.Proxies(ctx)
	if len(proxies) != 1 {
		t.Fatalf("expected exactly 1 row (updated in place, not duplicated), got %d: %+v", len(proxies), proxies)
	}
	row := proxies[0]
	if row.Service != "deepseek" {
		t.Errorf("Service changed: got %q", row.Service)
	}
	if row.Unit != "forge-compress@deepseek" {
		t.Errorf("Unit = %q, want forge-compress@deepseek", row.Unit)
	}
	if row.TargetURL != "https://api.deepseek.com/v1" {
		t.Errorf("TargetURL changed: got %q", row.TargetURL)
	}
	if row.Port != 8790 {
		t.Errorf("Port changed: got %d, want 8790 (same port reused — old unit is stopped first)", row.Port)
	}
	if row.Token == "old-secret" || row.Token == "" {
		t.Errorf("Token = %q, want a freshly generated one (never reuse a secret across a unit boundary)", row.Token)
	}
}

func TestCompressorMigrate_UnknownServiceNotFound(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{"service":"nonexistent"}`)))
	if w.Code != 404 {
		t.Errorf("migrate unknown service = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestCompressorMigrate_OrphanedProxyRefused(t *testing.T) {
	s, fh, _ := newProvisionedTestServer(t)
	ctx := context.Background()
	if err := fh.SaveProxy(ctx, store.ProxyRow{Service: "external", Unit: "headroom-external", Port: 8791, TargetURL: "https://x", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/teardown", strings.NewReader(`{"service":"external"}`)))
	if w.Code != 200 {
		t.Fatalf("teardown = %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{"service":"external"}`)))
	if w.Code != 409 {
		t.Errorf("migrate orphaned proxy = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestCompressorMigrate_AlreadyMigratedRefused(t *testing.T) {
	s, fh, _ := newProvisionedTestServer(t)
	ctx := context.Background()
	if err := fh.SaveProxy(ctx, store.ProxyRow{
		Service: "local", Unit: "forge-compress@local", Port: 8793, Token: "t",
	}); err != nil {
		t.Fatal(err)
	}
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{"service":"local"}`)))
	if w.Code != 409 {
		t.Errorf("migrate already-on-forge-compress@ proxy = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestCompressorMigrate_MissingServiceValidationError(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{}`)))
	if w.Code != 422 {
		t.Errorf("migrate missing service = %d, want 422: %s", w.Code, w.Body.String())
	}
}

// TestCompressorMigrate_ThenRestartUsesCorrectProvisioner is the
// provisionerFor regression test: after migrating, restarting the SAME
// service must go through the forge-compress@ Provisioner (so a future
// teardown correctly removes ITS OWN env file), not silently fall back to
// the default headroom@ one just because that's what most rows still use.
func TestCompressorMigrate_ThenRestartUsesCorrectProvisioner(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	ctx := context.Background()
	if err := fh.SaveProxy(ctx, store.ProxyRow{
		Service: "deepseek", Unit: "headroom@deepseek", Port: 8790,
		TargetURL: "https://api.deepseek.com/v1", Token: "old-secret",
	}); err != nil {
		t.Fatal(err)
	}
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{"service":"deepseek"}`)))
	if w.Code != 200 {
		t.Fatalf("migrate = %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, authedRequest("POST", "/api/v1/compressor/restart", strings.NewReader(`{"service":"deepseek"}`)))
	if w.Code != 200 {
		t.Fatalf("restart after migrate = %d: %s", w.Code, w.Body.String())
	}
	if len(sys.restarted) != 2 || sys.restarted[1] != "forge-compress@deepseek" {
		t.Errorf("restarted = %v, want two [forge-compress@deepseek] (Provision during migrate, then this explicit restart)", sys.restarted)
	}

	w = do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/teardown", strings.NewReader(`{"service":"deepseek"}`)))
	if w.Code != 200 {
		t.Fatalf("teardown after migrate = %d: %s", w.Code, w.Body.String())
	}
	if len(sys.stopped) != 2 || sys.stopped[1] != "forge-compress@deepseek" {
		t.Errorf("stopped = %v, want [headroom@deepseek forge-compress@deepseek]", sys.stopped)
	}
}
