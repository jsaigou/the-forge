// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/store"
)

// serverWithSmith builds a test Server backed by a real in-memory store.DB
// and a wired smith.Smith — same pattern as serverWithNotifications. The
// collector source carries a crafted snapshot (GTT at ~97% so the quick sweep
// yields a crit finding) so status + checks/run + findings have real data.
// The bus is wired as both Publisher and Subscriber so the anomaly hook is
// active; agent.Start is called so the hook goroutine is running.
func serverWithSmith(t *testing.T, ident authz.Identity) (*Server, *store.DB, *bus.Bus) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	events := bus.New()

	total := int64(120 << 30)
	used := int64(float64(total) * 0.97)
	snap := &collector.Snapshot{
		Hostname: "test-host",
		Metrics: collector.Metrics{
			GTTUsedBytes:  &used,
			GTTTotalBytes: &total,
		},
		Units:          map[string]collector.UnitState{},
		Slots:          map[string]collector.SlotState{},
		Inference:      map[string]collector.SlotInference{},
		Ports:          map[int]bool{},
		BookmarkHealth: map[string]bool{},
	}

	agent := smith.New(smith.Deps{
		Store:      db,
		Catalog:    db.Catalog(),
		Settings:   db.Settings(),
		Source:     collector.NewStatic(snap),
		Publisher:  events,
		Subscriber: events,
		// The blocked-work KB reads the synthetic example tracker — real
		// trackers are operator-local layer-2 data, never shipped.
		BlockedWorkPath: "../smith/testdata/blocked-work-example.md",
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	agent.Start(ctx)
	t.Cleanup(agent.Stop)

	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(snap),
		Engine:    &engine.Stub{},
		Sched:     &sched.Stub{},
		Auth:      &stubAuth{identity: ident},
		Events:    events,
		Publish:   events,
		Config:    func() *config.Config { return cfg },
		Hostname:  "test-host",
		Smith:     agent,
		Settings:  db.Settings(),
	})
	t.Cleanup(func() { s.Close() })
	return s, db, events
}

func TestSmithStatusReturnsSelfContext(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/status", nil))
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Decode into the smith.SelfContext wire shape and assert the P1 fields.
	var sc smith.SelfContext
	decodeJSON(t, w.Body, &sc)
	if sc.Tier != smith.TierDeterministic {
		t.Errorf("tier = %q, want %q", sc.Tier, smith.TierDeterministic)
	}
	if sc.Hostname != "test-host" {
		t.Errorf("hostname = %q, want test-host", sc.Hostname)
	}
	if sc.Metrics == nil || sc.Metrics.GTTUsedBytes == nil {
		t.Errorf("metrics missing GTT summary: %+v", sc.Metrics)
	}
	// No smith.model set ⇒ deterministic_only brain.
	if sc.Brain.Resolution != smith.BrainDeterministicOnly {
		t.Errorf("brain.resolution = %q, want deterministic_only", sc.Brain.Resolution)
	}
	if sc.CheckCount == 0 || sc.FastCheckCount == 0 {
		t.Errorf("check counts = %d/%d, want non-zero", sc.CheckCount, sc.FastCheckCount)
	}
}

func TestSmithStatusUnwired(t *testing.T) {
	s := newTestServer(t) // no Smith wired
	w := do(t, s, authedRequest("GET", "/api/v1/smith/status", nil))
	if w.Code != 503 {
		t.Errorf("status = %d, want 503 when smith unwired", w.Code)
	}
}

func TestSmithChecksRunPersistsAndLists(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// Run a quick sweep.
	body := bytes.NewBufferString(`{"scope":"quick"}`)
	w := do(t, s, authedRequest("POST", "/api/v1/smith/checks/run", body))
	if w.Code != 200 {
		t.Fatalf("checks/run = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var run smithChecksRunResponse
	decodeJSON(t, w.Body, &run)
	if run.Count == 0 || len(run.Findings) != run.Count {
		t.Fatalf("run.Count = %d, findings = %d, want >0 and equal", run.Count, len(run.Findings))
	}
	// The crafted 97% GTT snapshot must yield a crit finding.
	if run.Worst != string(smith.SeverityCrit) {
		t.Errorf("worst = %q, want crit (GTT at 97%%)", run.Worst)
	}

	// Findings are persisted and listed.
	w = do(t, s, authedRequest("GET", "/api/v1/smith/findings", nil))
	if w.Code != 200 {
		t.Fatalf("findings = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var list smithFindingsResponse
	decodeJSON(t, w.Body, &list)
	if list.Count != run.Count {
		t.Errorf("findings.Count = %d, want %d (persisted from the sweep)", list.Count, run.Count)
	}

	// Severity filter narrows to the crit finding(s).
	w = do(t, s, authedRequest("GET", "/api/v1/smith/findings?severity=crit", nil))
	if w.Code != 200 {
		t.Fatalf("findings?severity=crit = %d, want 200", w.Code)
	}
	var crits smithFindingsResponse
	decodeJSON(t, w.Body, &crits)
	if crits.Count == 0 {
		t.Errorf("expected at least one crit finding, got %d", crits.Count)
	}
	// The gtt_ceiling check's crit path emits KBRefs (checks.go) — migration
	// 0036 made persistFindings/ListFindings round-trip it. A regression
	// here means kb_refs silently went back to being dropped on write, the
	// exact gap 0036 fixed.
	var sawGTTCeilingKBRef bool
	for _, f := range crits.Findings {
		if f.Severity != smith.SeverityCrit {
			t.Errorf("finding severity = %q, want crit", f.Severity)
		}
		if f.CheckID == "gtt_ceiling" {
			for _, ref := range f.KBRefs {
				if ref == "pitfalls:gtt-ceiling" {
					sawGTTCeilingKBRef = true
				}
			}
		}
	}
	if !sawGTTCeilingKBRef {
		t.Errorf("expected the persisted gtt_ceiling finding to carry kb_refs=[pitfalls:gtt-ceiling], findings=%+v", crits.Findings)
	}
}

func TestSmithChecksRunExplicitIDsAndValidation(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// Explicit check_ids.
	body := bytes.NewBufferString(`{"check_ids":["gtt_ceiling"]}`)
	w := do(t, s, authedRequest("POST", "/api/v1/smith/checks/run", body))
	if w.Code != 200 {
		t.Fatalf("checks/run(ids) = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var run smithChecksRunResponse
	decodeJSON(t, w.Body, &run)
	if run.Count != 1 || run.Findings[0].CheckID != "gtt_ceiling" {
		t.Errorf("expected a single gtt_ceiling finding, got %+v", run.Findings)
	}

	// Unknown scope → 422.
	w = do(t, s, authedRequest("POST", "/api/v1/smith/checks/run",
		bytes.NewBufferString(`{"scope":"sideways"}`)))
	if w.Code != 422 {
		t.Errorf("checks/run(bad scope) = %d, want 422", w.Code)
	}
}

func TestSmithRoleGate(t *testing.T) {
	// A viewer is authenticated but lacks the operator role every smith route
	// requires — 403 before the handler (and before the smith-wired check).
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "viewer", Role: authz.RoleViewer})
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/smith/status"},
		{"GET", "/api/v1/smith/findings"},
		{"POST", "/api/v1/smith/checks/run"},
		{"GET", "/api/v1/smith/investigations"},
		{"POST", "/api/v1/smith/investigations"},
		{"GET", "/api/v1/smith/investigations/1"},
		{"POST", "/api/v1/smith/investigations/1/checks"},
		{"PATCH", "/api/v1/smith/investigations/1"},
		{"GET", "/api/v1/smith/checks"},
	} {
		w := do(t, s, authedRequest(route.method, route.path, bytes.NewBufferString(`{}`)))
		if w.Code != 403 {
			t.Errorf("%s %s as viewer = %d, want 403", route.method, route.path, w.Code)
		}
	}
}

// ── Investigations (Wave 2) ──────────────────────────────────────────────────

func TestSmithChecksCatalog(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})
	w := do(t, s, authedRequest("GET", "/api/v1/smith/checks", nil))
	if w.Code != 200 {
		t.Fatalf("checks = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp smithChecksResponse
	decodeJSON(t, w.Body, &resp)
	if resp.Count == 0 {
		t.Fatalf("expected non-zero check count")
	}
	// Verify a known check is present with metadata.
	found := false
	for _, c := range resp.Checks {
		if c.ID == "gtt_ceiling" {
			found = true
			if c.Name == "" || c.Category == "" {
				t.Errorf("check %s has empty name or category: %+v", c.ID, c)
			}
		}
	}
	if !found {
		t.Errorf("gtt_ceiling check not found in catalog")
	}
}

func TestSmithInvestigationCRUD(t *testing.T) {
	s, _, _ := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// 1. Manual open.
	body := bytes.NewBufferString(`{"trigger":"manual","summary":"test investigation"}`)
	w := do(t, s, authedRequest("POST", "/api/v1/smith/investigations", body))
	if w.Code != 201 {
		t.Fatalf("create = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var inv smith.Investigation
	decodeJSON(t, w.Body, &inv)
	if inv.Trigger != "manual" || inv.Status != "open" {
		t.Errorf("trigger=%q status=%q, want manual/open", inv.Trigger, inv.Status)
	}
	if inv.ID == 0 {
		t.Fatalf("investigation ID = 0")
	}
	idStr := strconv.FormatInt(inv.ID, 10)

	// 2. Get detail — no findings yet.
	w = do(t, s, authedRequest("GET", "/api/v1/smith/investigations/"+idStr, nil))
	if w.Code != 200 {
		t.Fatalf("detail = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail smithInvestigationDetailResponse
	decodeJSON(t, w.Body, &detail)
	if detail.ID != inv.ID {
		t.Errorf("detail ID = %d, want %d", detail.ID, inv.ID)
	}

	// 3. Run checks into the investigation.
	checkBody := bytes.NewBufferString(`{"scope":"quick"}`)
	w = do(t, s, authedRequest("POST", "/api/v1/smith/investigations/"+idStr+"/checks", checkBody))
	if w.Code != 200 {
		t.Fatalf("checks = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var run smithInvestigationChecksResponse
	decodeJSON(t, w.Body, &run)
	if run.Count == 0 {
		t.Errorf("expected findings from check run into investigation")
	}

	// 4. Get detail — should have findings now.
	w = do(t, s, authedRequest("GET", "/api/v1/smith/investigations/"+idStr, nil))
	decodeJSON(t, w.Body, &detail)
	if len(detail.Findings) != run.Count {
		t.Errorf("detail findings = %d, want %d", len(detail.Findings), run.Count)
	}
	for _, f := range detail.Findings {
		if f.InvestigationID == nil || *f.InvestigationID != inv.ID {
			t.Errorf("finding investigation_id = %v, want %d", f.InvestigationID, inv.ID)
		}
	}

	// 5. Resolve.
	resolveBody := bytes.NewBufferString(`{"status":"resolved"}`)
	w = do(t, s, authedRequest("PATCH", "/api/v1/smith/investigations/"+idStr, resolveBody))
	if w.Code != 200 {
		t.Fatalf("resolve = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	decodeJSON(t, w.Body, &inv)
	if inv.Status != "resolved" {
		t.Errorf("status = %q, want resolved", inv.Status)
	}
	if inv.ClosedAt == nil {
		t.Errorf("closed_at should be set after resolve")
	}

	// 6. List — the resolved investigation appears with ?status=resolved.
	w = do(t, s, authedRequest("GET", "/api/v1/smith/investigations?status=resolved", nil))
	if w.Code != 200 {
		t.Fatalf("list(resolved) = %d, want 200", w.Code)
	}
	var list smithInvestigationsResponse
	decodeJSON(t, w.Body, &list)
	if list.Count != 1 {
		t.Errorf("list(resolved).Count = %d, want 1", list.Count)
	}

	// 7. Invalid trigger on create → 422.
	w = do(t, s, authedRequest("POST", "/api/v1/smith/investigations",
		bytes.NewBufferString(`{"trigger":"anomaly:FOO"}`)))
	if w.Code != 422 {
		t.Errorf("create(anomaly trigger) = %d, want 422", w.Code)
	}

	// 8. Invalid resolve status → 422.
	w = do(t, s, authedRequest("PATCH", "/api/v1/smith/investigations/"+idStr,
		bytes.NewBufferString(`{"status":"frob"}`)))
	if w.Code != 422 {
		t.Errorf("resolve(frob) = %d, want 422", w.Code)
	}
}

func TestSmithAnomalyHook(t *testing.T) {
	s, _, events := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// Publish a crit notification on the bus.
	events.Publish("notification:new", map[string]any{
		"id":      int64(1),
		"code":    "INFERENCE_HANG",
		"subject": "8080",
		"message": "inference hang detected",
	})

	// Wait for the anomaly hook goroutine to process the event and open
	// an investigation with attached findings.
	waitFor(t, 5*time.Second, "anomaly investigation to appear", func() bool {
		w := do(t, s, authedRequest("GET", "/api/v1/smith/investigations", nil))
		var resp smithInvestigationsResponse
		decodeJSON(t, w.Body, &resp)
		return resp.Count > 0
	})

	// Assert exactly one investigation with the right trigger.
	w := do(t, s, authedRequest("GET", "/api/v1/smith/investigations", nil))
	var invs smithInvestigationsResponse
	decodeJSON(t, w.Body, &invs)
	if invs.Count != 1 {
		t.Fatalf("expected 1 investigation, got %d", invs.Count)
	}
	if invs.Investigations[0].Trigger != "anomaly:INFERENCE_HANG" {
		t.Errorf("trigger = %q, want anomaly:INFERENCE_HANG", invs.Investigations[0].Trigger)
	}
	if invs.Investigations[0].Status != "open" {
		t.Errorf("status = %q, want open", invs.Investigations[0].Status)
	}

	// Get detail — should have findings from the deep sweep.
	idStr := strconv.FormatInt(invs.Investigations[0].ID, 10)
	w = do(t, s, authedRequest("GET", "/api/v1/smith/investigations/"+idStr, nil))
	if w.Code != 200 {
		t.Fatalf("detail = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var detail smithInvestigationDetailResponse
	decodeJSON(t, w.Body, &detail)
	if len(detail.Findings) == 0 {
		t.Fatalf("expected findings attached to the anomaly investigation")
	}
	for _, f := range detail.Findings {
		if f.SweepKind != "anomaly" {
			t.Errorf("finding sweep_kind = %q, want anomaly", f.SweepKind)
		}
	}
}

func TestSmithAnomalyDebounce(t *testing.T) {
	s, _, events := serverWithSmith(t, authz.Identity{Name: "operator", Role: authz.RoleAdmin})

	// Publish the same crit code twice (back to back). The bus buffer (20)
	// holds both; the single-goroutine consumer processes them in order.
	for i := 0; i < 2; i++ {
		events.Publish("notification:new", map[string]any{
			"code":    "UNIT_OOM",
			"subject": "forge-a1",
			"message": "oom kill detected",
		})
	}

	// Wait for the anomaly hook to process at least one event.
	waitFor(t, 5*time.Second, "first anomaly investigation to appear", func() bool {
		w := do(t, s, authedRequest("GET", "/api/v1/smith/investigations", nil))
		var resp smithInvestigationsResponse
		decodeJSON(t, w.Body, &resp)
		return resp.Count > 0
	})

	// Give the hook time to process the second event (the first sweep
	// holds the sweeping lock; the second event waits in the channel).
	time.Sleep(500 * time.Millisecond)

	// Assert exactly one investigation (debounce — not two).
	w := do(t, s, authedRequest("GET", "/api/v1/smith/investigations", nil))
	var invs smithInvestigationsResponse
	decodeJSON(t, w.Body, &invs)
	if invs.Count != 1 {
		t.Fatalf("expected 1 investigation (debounce), got %d", invs.Count)
	}
	if invs.Investigations[0].Trigger != "anomaly:UNIT_OOM" {
		t.Errorf("trigger = %q, want anomaly:UNIT_OOM", invs.Investigations[0].Trigger)
	}

	// The investigation should have findings attached (from one or both sweeps).
	idStr := strconv.FormatInt(invs.Investigations[0].ID, 10)
	w = do(t, s, authedRequest("GET", "/api/v1/smith/investigations/"+idStr, nil))
	if w.Code != 200 {
		t.Fatalf("detail = %d, want 200", w.Code)
	}
	var detail smithInvestigationDetailResponse
	decodeJSON(t, w.Body, &detail)
	if len(detail.Findings) == 0 {
		t.Errorf("expected findings attached to the debounce investigation")
	}
}

// waitFor polls cond every 50ms until it returns true or the deadline passes.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}
