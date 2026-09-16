// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// capability_substitutionSched is a controllable sched.Scheduler fake for
// capability-tier substitution tests: a fixed Status().Slots occupancy map,
// per-model EnsureLoaded outcomes, a per-model CouldLoad verdict, and an
// EnsureLoaded call log (model names, in order) for assertions like "exactly
// two EnsureLoaded calls, never a third".
type capability_substitutionSched struct {
	sched.Stub
	slots        map[string]string // slot -> loaded config name
	ensureResult map[string]sched.Ticket
	ensureErr    map[string]error
	couldLoad    map[string]sched.Placement
	calls        []string // EnsureLoaded call log, in order
}

func (s *capability_substitutionSched) Status() sched.Status {
	return sched.Status{Slots: s.slots}
}

func (s *capability_substitutionSched) CouldLoad(_ context.Context, model string, _ time.Duration) (sched.Placement, error) {
	if pl, ok := s.couldLoad[model]; ok {
		return pl, nil
	}
	return sched.Placement{Slot: "a1"}, nil // default: feasible, non-terminal
}

func (s *capability_substitutionSched) EnsureLoaded(_ context.Context, req sched.EnsureRequest) (sched.Ticket, error) {
	s.calls = append(s.calls, req.Model)
	if err, ok := s.ensureErr[req.Model]; ok {
		return sched.Ticket{Model: req.Model, Status: "failed"}, err
	}
	if t, ok := s.ensureResult[req.Model]; ok {
		t.Model = req.Model
		return t, nil
	}
	return sched.Ticket{Model: req.Model, Status: "failed"}, nil
}

// substitutionHarness bundles the store + scheduler + router used across
// this file's tests, with a policy already set on the shared capability tier.
type substitutionHarness struct {
	t          *testing.T
	db         *store.DB
	sched      *capability_substitutionSched
	srv        *Server
	classID    int64
	cat        *fakeCatalog
	sharedPort int
}

func newSubstitutionHarness(t *testing.T, mode string) *substitutionHarness {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	classID, err := db.Catalog().CreateCapabilityTier(context.Background(), store.CapabilityTier{Name: "test-class", Mode: mode})
	if err != nil {
		t.Fatalf("CreateCapabilityTier: %v", err)
	}

	fs := &capability_substitutionSched{
		slots:        map[string]string{"a1": "", "a2": "", "a3": "", "a4": ""},
		ensureResult: map[string]sched.Ticket{},
		ensureErr:    map[string]error{},
		couldLoad:    map[string]sched.Placement{},
	}

	// One shared upstream stands in for every slot — these tests care which
	// config NAME got routed (visible via the disclosure headers this
	// sprint adds), not which physical port answered.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"id":"ok","object":"chat.completion","choices":[]}`))
	}))
	t.Cleanup(upstream.Close)
	port := portFromURL(t, upstream.URL)
	ports := map[string]int{"a1": port, "a2": port, "a3": port, "a4": port}
	cat := newFakeCatalog()
	cat.setProbe(port, SlotProbe{Healthy: true, ModelPath: "/m.gguf"})

	srv := NewWithDeps(Deps{
		Cfg:          testCfg(nil, nil),
		Catalog:      cat,
		StoreCatalog: db.Catalog(),
		Sched:        fs,
		Slots:        ports,
		Auth:         &stubAuth{validToken: "x"},
	})

	return &substitutionHarness{t: t, db: db, sched: fs, srv: srv, classID: classID, cat: cat, sharedPort: port}
}

// setSupportsTools flips the shared upstream's live-probed tool-calling
// capability (all slots in this harness share one port — see the
// constructor's own comment on why that's fine for these tests).
func (h *substitutionHarness) setSupportsTools(v bool) {
	h.cat.setProbe(h.sharedPort, SlotProbe{Healthy: true, ModelPath: "/m.gguf", SupportsTools: v})
}

// seed creates a config in the harness's shared capability tier at the given
// rank, optionally sharing weightFilePath with another config (same-weights
// gate testing).
func (h *substitutionHarness) seed(name string, rank int, opts ...seedConfigOpts) int64 {
	h.t.Helper()
	var o seedConfigOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	o.capabilityTierID, o.capabilityRank = h.classID, rank
	return seedCatalogConfig(h.t, h.db.Catalog(), name, 16384, "visible", o)
}

// loadOn marks name as already loaded on slot in the fake scheduler's
// Status() occupancy.
func (h *substitutionHarness) loadOn(slot, name string) { h.sched.slots[slot] = name }

func (h *substitutionHarness) request(model string, extra string) *httptest.ResponseRecorder {
	h.t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]`
	if extra != "" {
		body += "," + extra
	}
	body += "}"
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.RemoteAddr = "100.64.0.1:1234"
	rec := httptest.NewRecorder()
	h.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestCapabilitySubstitution_PreferSmarterSubstitutesLoadedPeer is the basic positive
// case: a strictly-better-ranked peer is already loaded, prefer_smarter is
// on, and the request for the weaker config must be served by the peer —
// disclosed via headers, never touching EnsureLoaded for the weaker config
// at all (mode 2's whole point: don't load `want` only to discard it).
func TestCapabilitySubstitution_PreferSmarterSubstitutesLoadedPeer(t *testing.T) {
	h := newSubstitutionHarness(t, "prefer_smarter")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.ensureResult["strong"] = sched.Ticket{Status: "loaded", TargetSlot: "a2"}

	rec := h.request("weak", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "strong" {
		t.Errorf("X-Forge-Model-Served = %q, want %q", got, "strong")
	}
	if got := rec.Header().Get("X-Forge-Model-Requested"); got != "weak" {
		t.Errorf("X-Forge-Model-Requested = %q, want %q", got, "weak")
	}
	if got := rec.Header().Get("X-Forge-Capability-Substitution"); got != "prefer_smarter" {
		t.Errorf("X-Forge-Capability-Substitution = %q, want %q", got, "prefer_smarter")
	}
	for _, m := range h.sched.calls {
		if m == "weak" {
			t.Error("EnsureLoaded(weak) was called — prefer_smarter must never load `want` before substituting")
		}
	}
}

// TestCapabilitySubstitution_NeverAcrossSameWeightsSiblings reproduces the exact
// shape of the 2026-08-22 incident this gate exists to prevent repeating:
// two Configs are duplicate artifact rows over the identical GGUF file
// (gemma4-26b-a4b / -nothink), one ranked strictly better. Even under
// prefer_smarter, a same-weights sibling must never be treated as a
// substitute — ADR-0006's rejected case (routing to a sibling with
// different generation flags silently changes behavior).
func TestCapabilitySubstitution_NeverAcrossSameWeightsSiblings(t *testing.T) {
	h := newSubstitutionHarness(t, "prefer_smarter")
	h.seed("base", 10, seedConfigOpts{weightFilePath: "shared.gguf"})
	h.seed("base-nothink", 0, seedConfigOpts{weightFilePath: "shared.gguf"})
	h.loadOn("a2", "base-nothink")
	h.sched.ensureResult["base"] = sched.Ticket{Status: "loaded", TargetSlot: "a1"}

	rec := h.request("base", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "" {
		t.Errorf("X-Forge-Model-Served = %q, want empty (no substitution across same-weights siblings)", got)
	}
	found := false
	for _, m := range h.sched.calls {
		if m == "base" {
			found = true
		}
		if m == "base-nothink" {
			t.Error("EnsureLoaded(base-nothink) was called — same-weights sibling must never be substituted")
		}
	}
	if !found {
		t.Error("EnsureLoaded(base) was never called — want should have loaded normally")
	}
}

// TestCapabilitySubstitution_TrustsTicketNotStaleStatus: the candidate-gathering
// snapshot (sched.Status()) says the peer is loaded, but the scheduler's
// own EnsureLoaded — the authoritative, freshly-evaluated call — reports it
// failed (e.g. evicted out from under the snapshot in the gap). The router
// must never fabricate a port from the stale snapshot; it must fall
// through and load `want` normally instead.
func TestCapabilitySubstitution_TrustsTicketNotStaleStatus(t *testing.T) {
	h := newSubstitutionHarness(t, "prefer_smarter")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong") // stale: Status() still shows it loaded
	h.sched.ensureErr["strong"] = context.DeadlineExceeded
	h.sched.ensureResult["weak"] = sched.Ticket{Status: "loaded", TargetSlot: "a1"}

	rec := h.request("weak", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "" {
		t.Errorf("X-Forge-Model-Served = %q, want empty (substitute's EnsureLoaded failed, must fall through)", got)
	}
}

// TestCapabilitySubstitution_SingleShot: `want` fails, its one substitute is tried
// exactly once (the "second chance" in catalogChain's doc comment) and
// also fails — the response must surface `want`'s own error, and
// EnsureLoaded must be called exactly twice (want, then the one
// substitute), never a third time.
func TestCapabilitySubstitution_SingleShot(t *testing.T) {
	h := newSubstitutionHarness(t, "fallback_only")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.ensureErr["weak"] = context.DeadlineExceeded
	h.sched.ensureErr["strong"] = context.DeadlineExceeded

	rec := h.request("weak", "")
	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"model":"weak"`) {
		t.Errorf("error body must name the originally requested model: %s", rec.Body.String())
	}
	if len(h.sched.calls) != 2 || h.sched.calls[0] != "weak" || h.sched.calls[1] != "strong" {
		t.Fatalf("EnsureLoaded calls = %v, want exactly [weak, strong]", h.sched.calls)
	}
}

// TestCapabilitySubstitution_ToolsRequestNeverSubstitutes: tool-calling capability
// has no column anywhere in the catalog (registry's own reconnaissance
// note) — a request carrying tools must fail closed rather than guess a
// substitute supports the same tools, even with an eligible, strictly
// better-ranked peer already loaded.
func TestCapabilitySubstitution_ToolsRequestNeverSubstitutes(t *testing.T) {
	h := newSubstitutionHarness(t, "prefer_smarter")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.ensureResult["weak"] = sched.Ticket{Status: "loaded", TargetSlot: "a1"}
	h.setSupportsTools(false) // the peer's live probe reports no tool support

	rec := h.request("weak", `"tools":[{"type":"function","function":{"name":"x"}}]`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "" {
		t.Errorf("X-Forge-Model-Served = %q, want empty (a tools request must not substitute onto a peer with no live-probed tool support)", got)
	}
}

// TestCapabilitySubstitution_ToolsRequestSubstitutesWhenPeerSupportsTools is Sprint
// P4's relaxation: a tools-bearing request MAY substitute onto a peer whose
// own live /props chat_template_caps reports real tool-calling support —
// no longer a blanket refusal now that a real, live-probed signal exists
// (Sprint P3 fail-closed unconditionally here since no such data existed).
func TestCapabilitySubstitution_ToolsRequestSubstitutesWhenPeerSupportsTools(t *testing.T) {
	h := newSubstitutionHarness(t, "prefer_smarter")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.ensureResult["strong"] = sched.Ticket{Status: "loaded", TargetSlot: "a2"}
	h.setSupportsTools(true)

	rec := h.request("weak", `"tools":[{"type":"function","function":{"name":"x"}}]`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "strong" {
		t.Errorf("X-Forge-Model-Served = %q, want %q (peer's live probe confirms tool support)", got, "strong")
	}
}

// TestCapabilitySubstitution_ResponseFormatNeverSubstitutes: no live-probeable
// signal exists for structured-output support (unlike tool-calling), so
// this stays an unconditional refusal regardless of peer capability.
func TestCapabilitySubstitution_ResponseFormatNeverSubstitutes(t *testing.T) {
	h := newSubstitutionHarness(t, "prefer_smarter")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.ensureResult["weak"] = sched.Ticket{Status: "loaded", TargetSlot: "a1"}
	h.setSupportsTools(true)

	rec := h.request("weak", `"response_format":{"type":"json_object"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "" {
		t.Errorf("X-Forge-Model-Served = %q, want empty (response_format has no live signal to relax against)", got)
	}
}

// TestCapabilitySubstitution_OffByDefault confirms the whole feature is inert unless
// a policy is explicitly set — the capability tier exists and an eligible loaded
// peer is present, but mode is "" (unset).
func TestCapabilitySubstitution_OffByDefault(t *testing.T) {
	h := newSubstitutionHarness(t, "")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.ensureResult["weak"] = sched.Ticket{Status: "loaded", TargetSlot: "a1"}

	rec := h.request("weak", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "" {
		t.Errorf("X-Forge-Model-Served = %q, want empty (policy off must be a no-op)", got)
	}
}

// TestCapabilitySubstitution_FallbackOnlyRequiresTerminal: fallback_only must not
// substitute when `want` is merely queued/retryable (CouldLoad
// non-terminal) — only a genuinely infeasible `want` should trigger it.
func TestCapabilitySubstitution_FallbackOnlyRequiresTerminal(t *testing.T) {
	h := newSubstitutionHarness(t, "fallback_only")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.couldLoad["weak"] = sched.Placement{Slot: "", Terminal: false} // retryable, not infeasible
	h.sched.ensureResult["weak"] = sched.Ticket{Status: "loaded", TargetSlot: "a1"}

	rec := h.request("weak", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "" {
		t.Errorf("X-Forge-Model-Served = %q, want empty (non-terminal must not substitute under fallback_only)", got)
	}
	if len(h.sched.calls) != 1 || h.sched.calls[0] != "weak" {
		t.Fatalf("EnsureLoaded calls = %v, want exactly [weak]", h.sched.calls)
	}
}

// TestCapabilitySubstitution_FallbackOnlySubstitutesWhenTerminal is the mode-1
// positive case: `want` is genuinely infeasible (CouldLoad terminal) and an
// equal-or-better peer is loaded — substitution happens before
// EnsureLoaded(want) is ever attempted.
func TestCapabilitySubstitution_FallbackOnlySubstitutesWhenTerminal(t *testing.T) {
	h := newSubstitutionHarness(t, "fallback_only")
	h.seed("weak", 10)
	h.seed("strong", 0)
	h.loadOn("a2", "strong")
	h.sched.couldLoad["weak"] = sched.Placement{Terminal: true, Reason: sched.ReasonModelTooLarge}
	h.sched.ensureResult["strong"] = sched.Ticket{Status: "loaded", TargetSlot: "a2"}

	rec := h.request("weak", "")
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Forge-Model-Served"); got != "strong" {
		t.Errorf("X-Forge-Model-Served = %q, want %q", got, "strong")
	}
	if got := rec.Header().Get("X-Forge-Capability-Substitution"); got != "fallback_only" {
		t.Errorf("X-Forge-Capability-Substitution = %q, want %q", got, "fallback_only")
	}
	for _, m := range h.sched.calls {
		if m == "weak" {
			t.Error("EnsureLoaded(weak) was called — fallback_only must substitute before attempting a known-infeasible load")
		}
	}
}

// TestDecideSubstitute is a pure table test over decideSubstitute itself —
// no I/O, no scheduler, no store.
func TestDecideSubstitute(t *testing.T) {
	want := store.Config{Name: "weak", CapabilityRank: 10}
	better := peerCandidate{config: store.Config{Name: "strong", CapabilityRank: 0}, slot: "a2"}
	equal := peerCandidate{config: store.Config{Name: "twin", CapabilityRank: 10}, slot: "a3"}
	worse := peerCandidate{config: store.Config{Name: "weaker", CapabilityRank: 20}, slot: "a4"}

	t.Run("prefer_smarter picks strictly better, not equal", func(t *testing.T) {
		_, _, _, ok := decideSubstitute(subPreferSmarter, want, []peerCandidate{equal}, false)
		if ok {
			t.Error("an equally-ranked peer must not be preferred — it isn't smarter")
		}
		sub, slot, _, ok := decideSubstitute(subPreferSmarter, want, []peerCandidate{worse, better}, false)
		if !ok || sub.Name != "strong" || slot != "a2" {
			t.Errorf("decideSubstitute = %+v/%q/%v, want strong/a2/true", sub, slot, ok)
		}
	})

	t.Run("fallback_only requires terminal", func(t *testing.T) {
		if _, _, _, ok := decideSubstitute(subFallbackOnly, want, []peerCandidate{better}, false); ok {
			t.Error("fallback_only must not substitute when want is not terminal")
		}
		sub, _, _, ok := decideSubstitute(subFallbackOnly, want, []peerCandidate{better}, true)
		if !ok || sub.Name != "strong" {
			t.Errorf("decideSubstitute = %+v/%v, want strong/true", sub, ok)
		}
	})

	t.Run("fallback_only accepts equal rank, not worse", func(t *testing.T) {
		sub, _, _, ok := decideSubstitute(subFallbackOnly, want, []peerCandidate{equal}, true)
		if !ok || sub.Name != "twin" {
			t.Errorf("decideSubstitute = %+v/%v, want twin/true (equal rank is an acceptable stand-in)", sub, ok)
		}
		if _, _, _, ok := decideSubstitute(subFallbackOnly, want, []peerCandidate{worse}, true); ok {
			t.Error("a worse-ranked peer must never be substituted")
		}
	})

	t.Run("off never substitutes", func(t *testing.T) {
		if _, _, _, ok := decideSubstitute(subOff, want, []peerCandidate{better}, true); ok {
			t.Error("subOff must never substitute")
		}
	})

	t.Run("no candidates never substitutes", func(t *testing.T) {
		if _, _, _, ok := decideSubstitute(subPreferSmarter, want, nil, false); ok {
			t.Error("no candidates must never substitute")
		}
	})
}

func TestHasToolFields(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"plain", map[string]any{"model": "x", "messages": []any{}}, false},
		{"tools", map[string]any{"tools": []any{"x"}}, true},
		{"functions", map[string]any{"functions": []any{"x"}}, true},
		{"tool_choice", map[string]any{"tool_choice": "auto"}, true},
		{"response_format alone is not a tool field", map[string]any{"response_format": map[string]any{"type": "json_object"}}, false},
		{"tools explicit null", map[string]any{"tools": nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasToolFields(tc.body); got != tc.want {
				t.Errorf("hasToolFields(%v) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

func TestHasResponseFormat(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"plain", map[string]any{"model": "x"}, false},
		{"response_format", map[string]any{"response_format": map[string]any{"type": "json_object"}}, true},
		{"response_format explicit null", map[string]any{"response_format": nil}, false},
		{"tools alone is not response_format", map[string]any{"tools": []any{"x"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasResponseFormat(tc.body); got != tc.want {
				t.Errorf("hasResponseFormat(%v) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
