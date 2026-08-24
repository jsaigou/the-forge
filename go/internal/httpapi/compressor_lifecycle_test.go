// SPDX-License-Identifier: Apache-2.0

package httpapi

// compressor_lifecycle_test.go — Phase 2 (docs/v5-headroom-topology.md §5):
// provider link/unlink-triggered provisioning, and the now-wired
// POST /api/v1/compressor/{restart,proxy/teardown} handlers.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/compressorctl"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// fakeProxySystemd is a scriptable compressor.Systemd for httpapi tests.
type fakeProxySystemd struct {
	mu                          sync.Mutex
	started, stopped, restarted []string
}

func (f *fakeProxySystemd) Start(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, unit)
	return nil
}

func (f *fakeProxySystemd) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, unit)
	return nil
}

func (f *fakeProxySystemd) Restart(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, unit)
	return nil
}

// newProvisionedTestServer builds a Server with fake Routing store +
// a real *compressorctl.Provisioner (TemplatePrefix "forge-compress@",
// the only one that exists since Sprint 7 dropped the legacy headroom@
// template, docs/v5-headroom-replacement.md) backed by a fake Systemd and a
// temp env dir.
func newProvisionedTestServer(t *testing.T) (*Server, *fakeCompressor, *fakeProxySystemd) {
	t.Helper()
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	fh := &fakeCompressor{}
	sys := &fakeProxySystemd{}
	prov := &compressorctl.Provisioner{Systemd: sys, EnvDir: t.TempDir(), TemplatePrefix: "forge-compress@"}
	s := New(Deps{
		Snapshots:             collector.NewStatic(nil),
		Engine:                &engine.Stub{},
		Sched:                 &sched.Stub{},
		Auth:                  &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:                events,
		Publish:                events,
		Config:                func() *config.Config { return cfg },
		Hostname:              "test-host",
		Routing:               fh,
		CompressorProvisioner: prov,
		Settings:              newFakeSettings(),
	})
	t.Cleanup(func() { s.Close() })
	return s, fh, sys
}

func TestProviderUpdateLinkProvisionsProxy(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"deepseek","target_url":"https://api.deepseek.com/v1"}`)))

	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":"deepseek"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}

	proxies, _ := fh.Proxies(context.Background())
	if len(proxies) != 1 {
		t.Fatalf("proxies = %+v, want 1 row", proxies)
	}
	p := proxies[0]
	if p.Service != "deepseek" || p.TargetURL != "https://api.deepseek.com/v1" || p.Token == "" {
		t.Errorf("provisioned row = %+v", p)
	}
	if p.Port < compressorctl.DefaultBasePort {
		t.Errorf("port = %d, want >= %d", p.Port, compressorctl.DefaultBasePort)
	}
	if !p.OrphanedAt.IsZero() {
		t.Errorf("newly provisioned row should not be orphaned")
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "forge-compress@deepseek" {
		t.Errorf("restarted = %v, want [forge-compress@deepseek] (Provision now uses Restart, not Start)", sys.restarted)
	}

	providers, _ := fh.Providers(context.Background())
	if providers[0].CompressorProxyName != "deepseek" {
		t.Errorf("provider compressor_proxy = %q, want deepseek", providers[0].CompressorProxyName)
	}
}

func TestProviderUpdateUnlinkTearsDownProxy(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"deepseek","target_url":"https://api.deepseek.com/v1"}`)))
	do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":"deepseek"}`)))

	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":""}`)))
	if w.Code != 200 {
		t.Fatalf("PUT unlink = %d, want 200: %s", w.Code, w.Body.String())
	}

	if len(sys.stopped) != 1 || sys.stopped[0] != "forge-compress@deepseek" {
		t.Errorf("stopped = %v, want [forge-compress@deepseek]", sys.stopped)
	}
	proxies, _ := fh.Proxies(context.Background())
	if len(proxies) != 1 || proxies[0].OrphanedAt.IsZero() {
		t.Errorf("proxy row should still exist and be orphaned: %+v", proxies)
	}
}

func TestProviderUpdateRetargetReconciles(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"deepseek","target_url":"https://old.example.com/v1"}`)))
	do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":"deepseek"}`)))

	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"target_url":"https://new.example.com/v1"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT retarget = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(sys.restarted) != 2 || sys.restarted[1] != "forge-compress@deepseek" {
		t.Errorf("restarted = %v, want two [forge-compress@deepseek] (Provision on link, then Reconcile on retarget)", sys.restarted)
	}
	proxies, _ := fh.Proxies(context.Background())
	if proxies[0].TargetURL != "https://new.example.com/v1" {
		t.Errorf("proxy target_url = %q, want updated", proxies[0].TargetURL)
	}
}

func TestProviderUpdateRetargetLegacyProxyRefused(t *testing.T) {
	// A provider already linked to a legacy hand-created proxy (mirrors
	// aiand -> "headroom-external") must not have its retarget silently
	// no-op: Provisioner.Reconcile refuses, and that error must surface as
	// a real failure to the caller rather than a false 200.
	s, fh, sys := newProvisionedTestServer(t)
	ctx := context.Background()
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"aiand","target_url":"https://api.aiand.com/v1"}`)))
	aiand, _, err := fh.ProviderByName(ctx, "aiand")
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.SaveProxy(ctx, store.ProxyRow{Service: "external", Unit: "headroom-external", Port: 8791, TargetURL: "https://api.aiand.com/v1", Token: "t", ProviderID: &aiand.ID}); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, authedRequest("PUT", "/api/v1/providers/aiand",
		strings.NewReader(`{"target_url":"https://new.aiand.example.com/v1"}`)))
	if w.Code != 500 {
		t.Fatalf("PUT retarget on legacy proxy = %d, want 500 (refused): %s", w.Code, w.Body.String())
	}
	if len(sys.restarted) != 0 {
		t.Errorf("restarted = %v, want no restart attempted", sys.restarted)
	}
	// The provider's target_url must NOT have changed either — the whole
	// update is refused atomically rather than partially applied.
	providers, _ := fh.Providers(ctx)
	if providers[0].TargetURL != "https://api.aiand.com/v1" {
		t.Errorf("provider target_url = %q, want unchanged (update should have been refused atomically)", providers[0].TargetURL)
	}
}

func TestProviderDeleteTearsDownLinkedProxy(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"deepseek","target_url":"https://api.deepseek.com/v1"}`)))
	do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":"deepseek"}`)))

	w := do(t, s, authedRequest("DELETE", "/api/v1/providers/deepseek", nil))
	if w.Code != 200 {
		t.Fatalf("DELETE = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(sys.stopped) != 1 {
		t.Errorf("stopped = %v, want 1 stop call", sys.stopped)
	}
	proxies, _ := fh.Proxies(context.Background())
	if len(proxies) != 1 || proxies[0].OrphanedAt.IsZero() {
		t.Errorf("proxy should remain, orphaned: %+v", proxies)
	}
}

func TestProviderDeleteSharedProxyNotTornDownWhileReferenced(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"deepseek","target_url":"https://api.deepseek.com/v1"}`)))
	do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":"shared"}`)))
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"other","target_url":"https://api.other.com/v1"}`)))
	do(t, s, authedRequest("PUT", "/api/v1/providers/other",
		strings.NewReader(`{"compressor_proxy":"shared"}`)))
	sys.stopped = nil // reset after setup provisioning noise

	w := do(t, s, authedRequest("DELETE", "/api/v1/providers/deepseek", nil))
	if w.Code != 200 {
		t.Fatalf("DELETE = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(sys.stopped) != 0 {
		t.Errorf("stopped = %v, want no stop — still referenced by 'other'", sys.stopped)
	}
	proxies, _ := fh.Proxies(context.Background())
	if len(proxies) != 1 || !proxies[0].OrphanedAt.IsZero() {
		t.Errorf("shared proxy should remain active: %+v", proxies)
	}
}

func TestCompressorLifecycleRestartAndTeardown(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	ctx := context.Background()
	// Deliberately a legacy-shaped Unit ("headroom-external", not
	// "headroom@external" — mirrors aiand's real proxy) to prove the
	// restart/teardown handlers use the row's stored Unit verbatim rather
	// than reconstructing "headroom@<service>" from the service name; that
	// reconstruction was a real bug (would have restarted/stopped a
	// nonexistent unit instead of the real one).
	if err := fh.SaveProxy(ctx, store.ProxyRow{Service: "external", Unit: "headroom-external", Port: 8791, TargetURL: "https://x", Token: "t"}); err != nil {
		t.Fatal(err)
	}

	w := do(t, s, authedRequest("POST", "/api/v1/compressor/restart", strings.NewReader(`{"service":"external"}`)))
	if w.Code != 200 {
		t.Fatalf("restart = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "headroom-external" {
		t.Errorf("restarted = %v, want [headroom-external]", sys.restarted)
	}

	w = do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/teardown", strings.NewReader(`{"service":"external"}`)))
	if w.Code != 200 {
		t.Fatalf("teardown = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(sys.stopped) != 1 || sys.stopped[0] != "headroom-external" {
		t.Errorf("stopped = %v, want [headroom-external]", sys.stopped)
	}
	proxies, _ := fh.Proxies(ctx)
	if len(proxies) != 1 || proxies[0].OrphanedAt.IsZero() {
		t.Errorf("proxy should be orphaned after teardown: %+v", proxies)
	}

	// Restarting an orphaned (torn-down) proxy should be rejected, not
	// silently restart a unit no one expects to be running.
	w = do(t, s, authedRequest("POST", "/api/v1/compressor/restart", strings.NewReader(`{"service":"external"}`)))
	if w.Code != 409 {
		t.Errorf("restart of orphaned proxy = %d, want 409: %s", w.Code, w.Body.String())
	}
}

func TestCompressorLifecycleUnknownServiceNotFound(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/restart", strings.NewReader(`{"service":"nonexistent"}`)))
	if w.Code != 404 {
		t.Errorf("restart unknown service = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestCompressorLifecycleMissingServiceValidationError(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/restart", strings.NewReader(`{}`)))
	if w.Code != 422 {
		t.Errorf("restart missing service = %d, want 422: %s", w.Code, w.Body.String())
	}
}

// TestCompressorProxyCreateDefaultTemplateUsesLegacyProvisioner covers the
// default behavior (Template unset): the single forge-compress@
// Provisioner (Sprint 7 dropped the legacy headroom@ one — see
// newProvisionedTestServer's doc comment).
func TestCompressorProxyCreateDefaultTemplateUsesLegacyProvisioner(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/create",
		strings.NewReader(`{"service":"a1","label":"A1","target_url":"http://127.0.0.1:8080"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	proxies, _ := fh.Proxies(context.Background())
	if len(proxies) != 1 || proxies[0].Unit != "forge-compress@a1" {
		t.Fatalf("proxies = %+v, want one row on forge-compress@a1", proxies)
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "forge-compress@a1" {
		t.Errorf("restarted = %v, want [forge-compress@a1]", sys.restarted)
	}
}

// TestCompressorProxyCreateCompressTemplateUsesCompressProvisioner is Sprint
// 3's real gap being closed (docs/v5-headroom-replacement.md): the shared
// "external" instance has no pre-existing ProxyRow to migrate via
// handleCompressorMigrate (that handler requires one) — it needs to be
// created fresh, on the forge-compress@ template.
func TestCompressorProxyCreateCompressTemplateUsesCompressProvisioner(t *testing.T) {
	s, fh, sys := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/create",
		strings.NewReader(`{"service":"external","label":"External (shared)","target_url":"dynamic (per-request via x-compress-base-url)","template":"compress"}`)))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
	}
	proxies, _ := fh.Proxies(context.Background())
	if len(proxies) != 1 || proxies[0].Unit != "forge-compress@external" {
		t.Fatalf("proxies = %+v, want one row on forge-compress@external", proxies)
	}
	if len(sys.restarted) != 1 || sys.restarted[0] != "forge-compress@external" {
		t.Errorf("restarted = %v, want [forge-compress@external]", sys.restarted)
	}
}

// TestCompressorProxyCreateInvalidTemplateRejected covers validate()'s new
// field.
func TestCompressorProxyCreateInvalidTemplateRejected(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/create",
		strings.NewReader(`{"service":"a1","label":"A1","target_url":"http://x","template":"bogus"}`)))
	if w.Code != 422 {
		t.Errorf("create with bogus template = %d, want 422: %s", w.Code, w.Body.String())
	}
}

// TestCompressorConfigExternalEnabledReflectsSetting covers Sprint 10's
// additive field (docs/v5-headroom-replacement.md): GET
// /api/v1/compressor/config must report the live compressor.external_enabled
// setting so the routing diagram can compute each provider's EFFECTIVE proxy
// (dedicated link vs shared "external" fallback vs direct) exactly the way
// router.remoteCompressorBaseURL does — Providers[].CompressorProxy only
// carries dedicated links.
func TestCompressorConfigExternalEnabledReflectsSetting(t *testing.T) {
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	settings := newFakeSettings()
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Routing:   &fakeCompressor{},
		Settings:  settings,
	})
	t.Cleanup(func() { s.Close() })

	get := func() compressorConfigResponse {
		t.Helper()
		w := do(t, s, authedRequest("GET", "/api/v1/compressor/config", nil))
		if w.Code != 200 {
			t.Fatalf("GET compressor/config = %d, want 200: %s", w.Code, w.Body.String())
		}
		var resp compressorConfigResponse
		decodeJSON(t, w.Body, &resp)
		return resp
	}

	if got := get().ExternalEnabled; got {
		t.Errorf("external_enabled = %v, want false (setting absent)", got)
	}
	raw, _ := json.Marshal(true)
	if err := settings.Set(context.Background(), "compressor.external_enabled", raw); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got := get().ExternalEnabled; !got {
		t.Errorf("external_enabled = %v, want true after setting it", got)
	}
}

// TestCompressorProxyCreateCompressTemplateWithoutCompressProvisionerWired
// covers the 501 guard when no Provisioner is wired at all.
func TestCompressorProxyCreateCompressTemplateWithoutCompressProvisionerWired(t *testing.T) {
	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	fh := &fakeCompressor{}
	s := New(Deps{
		Snapshots: collector.NewStatic(nil),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: authz.Identity{Name: "admin", Role: authz.RoleAdmin}},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Routing:   fh,
		Settings:  newFakeSettings(),
	})
	t.Cleanup(func() { s.Close() })

	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/create",
		strings.NewReader(`{"service":"external","label":"External","target_url":"dynamic","template":"compress"}`)))
	if w.Code != http.StatusNotImplemented {
		t.Errorf("create with unwired provisioner = %d, want 501: %s", w.Code, w.Body.String())
	}
}
