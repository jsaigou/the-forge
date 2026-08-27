// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"net/http"
	"testing"
)

// TestFixedInfraServiceStartStop: Tier 1 Sprint 2 — STT/Embedding/Aligner
// previously had no start/stop route at all (only TTS did). newTestServerWith
// configures embedding+stt (Ports) but not aligner, so this also covers the
// "not configured on this deployment" gate.
func TestFixedInfraServiceStartStop(t *testing.T) {
	s := newTestServer(t)

	for _, path := range []string{"/api/v1/stt/start", "/api/v1/embedding/start"} {
		w := do(t, s, authedRequest("POST", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200; body=%s", path, w.Code, w.Body.String())
		}
	}

	w := do(t, s, authedRequest("POST", "/api/v1/aligner/start", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("aligner/start (not configured) = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
