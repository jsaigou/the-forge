// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// TestAuditList exercises GET /api/v1/audit end-to-end through the real
// mux (role gating included, via newCostTestServer's admin stubAuth) —
// Sprint C's first read of audit_log, which was write-only before this.
func TestAuditList(t *testing.T) {
	s, _, _, fa, _ := newCostTestServer(t)
	ctx := context.Background()

	if err := fa.Write(ctx, store.AuditEntry{Actor: "testuser", Action: "catalog_config_update", Target: "7", Detail: "qwen36 — reason: bumped context"}); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	if err := fa.Write(ctx, store.AuditEntry{Actor: "testuser", Action: "catalog_model_update", Target: "7", Detail: "Qwen3.6-35B"}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	w := do(t, s, authedRequest("GET", "/api/v1/audit?action_prefix=catalog_config_&target=7", nil))
	if w.Code != 200 {
		t.Fatalf("GET /api/v1/audit = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp auditListResponse
	decodeJSON(t, w.Body, &resp)
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1 (action_prefix+target should disambiguate the id-7 collision)", len(resp.Entries))
	}
	if resp.Entries[0].Detail != "qwen36 — reason: bumped context" {
		t.Errorf("detail = %q, want the reason-carrying detail string", resp.Entries[0].Detail)
	}
}

// TestWithReason covers the operator-reason formatting used by every
// catalog create/update/delete audit call (Sprint C).
func TestWithReason(t *testing.T) {
	cases := []struct {
		name, detail, reason, want string
	}{
		{"no reason, empty detail unaffected", "qwen36", "", "qwen36"},
		{"reason appended to existing detail", "qwen36", "bumped context for a new agent workload", "qwen36 — reason: bumped context for a new agent workload"},
		{"reason with empty detail (delete path)", "", "no longer needed", "reason: no longer needed"},
		{"whitespace-only reason treated as absent", "qwen36", "   ", "qwen36"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := withReason(c.detail, c.reason)
			if got != c.want {
				t.Errorf("withReason(%q, %q) = %q, want %q", c.detail, c.reason, got, c.want)
			}
		})
	}

	// A reason far past the cap is truncated, never rejected — a save
	// should never fail just because the note ran long.
	long := make([]byte, maxAuditReasonLen+50)
	for i := range long {
		long[i] = 'a'
	}
	got := withReason("", string(long))
	if len(got) != len("reason: ")+maxAuditReasonLen {
		t.Errorf("withReason did not truncate: got len %d, want %d", len(got), len("reason: ")+maxAuditReasonLen)
	}
}
