// SPDX-License-Identifier: Apache-2.0

package httpapi

// compressor_retired_test.go — T6 (docs/v5-compression-redesign-execution.md
// §7): the compressor is retired from the request path (CONTRACTS C7,
// ADR-0017). These tests deliberately do NOT use OverrideRetiredForTest, so
// they exercise the production default.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/compressorctl"
)

func TestRetiredProviderLinkDoesNotProvisionUnit(t *testing.T) {
	if !compressorctl.Retired() {
		t.Fatal("compressor must be retired by default")
	}
	s, _, sys := newProvisionedTestServer(t)
	do(t, s, authedRequest("POST", "/api/v1/providers",
		strings.NewReader(`{"name":"deepseek","target_url":"https://api.deepseek.com/v1"}`)))
	w := do(t, s, authedRequest("PUT", "/api/v1/providers/deepseek",
		strings.NewReader(`{"compressor_proxy":"deepseek"}`)))
	if w.Code != 200 {
		t.Fatalf("PUT = %d, want 200: %s", w.Code, w.Body.String())
	}
	if len(sys.started)+len(sys.restarted) != 0 {
		t.Errorf("no unit may be started/restarted on link: started=%v restarted=%v", sys.started, sys.restarted)
	}
	entries, _ := os.ReadDir(s.deps.CompressorProvisioner.EnvDir)
	if len(entries) != 0 {
		t.Errorf("no env file may be written, found %d", len(entries))
	}
}

func TestRetiredProxyCreateAndMigrateGone(t *testing.T) {
	s, _, sys := newProvisionedTestServer(t)
	w := do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/create",
		strings.NewReader(`{"service":"x","label":"X","target_url":"https://a.example/v1"}`)))
	if w.Code != 410 {
		t.Fatalf("create = %d, want 410: %s", w.Code, w.Body.String())
	}
	w = do(t, s, authedRequest("POST", "/api/v1/compressor/proxy/migrate", strings.NewReader(`{"service":"x"}`)))
	if w.Code != 410 {
		t.Fatalf("migrate = %d, want 410: %s", w.Code, w.Body.String())
	}
	if len(sys.started)+len(sys.restarted)+len(sys.stopped) != 0 {
		t.Errorf("systemd must be untouched: started=%v restarted=%v stopped=%v", sys.started, sys.restarted, sys.stopped)
	}
}

func TestRetiredPassthroughCannotBeDisabled(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t)
	for _, body := range []string{
		`{"scope":"all","enabled":false}`,
		`{"scope":"proxy","service":"deepseek","enabled":false}`,
	} {
		w := do(t, s, authedRequest("PUT", "/api/v1/compressor/passthrough", strings.NewReader(body)))
		if w.Code != 409 {
			t.Errorf("%s: code = %d, want 409: %s", body, w.Code, w.Body.String())
		}
	}
	// Turning bypass ON stays allowed and idempotent.
	w := do(t, s, authedRequest("PUT", "/api/v1/compressor/passthrough", strings.NewReader(`{"scope":"all","enabled":true}`)))
	if w.Code != 200 {
		t.Errorf("enable bypass = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestRetiredConfigDefaultsPassthroughAllTrue(t *testing.T) {
	s, _, _ := newProvisionedTestServer(t) // fake settings has no passthrough_all key
	w := do(t, s, authedRequest("GET", "/api/v1/compressor/config", nil))
	if w.Code != 200 {
		t.Fatalf("config = %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		PassthroughAll bool `json:"passthrough_all"`
		Retired        bool `json:"retired"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.PassthroughAll || !got.Retired {
		t.Errorf("passthrough_all=%v retired=%v, want both true when key absent", got.PassthroughAll, got.Retired)
	}
}
