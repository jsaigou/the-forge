// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/ctxledger"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

func TestContextCreationJSONShape(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	led := ctxledger.New(ctxledger.Config{Sink: db.ContextCreation()})
	led.Start(context.Background())
	t.Cleanup(led.Close)

	const sentinel = "SENTINEL_HANDLER_77"
	msg := func(r, c string) map[string]any { return map[string]any{"role": r, "content": c} }
	mk := func(msgs ...map[string]any) []byte {
		b, _ := json.Marshal(map[string]any{"messages": msgs})
		return b
	}
	led.Observe(mk(msg("user", sentinel), msg("assistant", "b"), msg("user", "c")), "alice", "m")
	led.Observe(mk(msg("user", sentinel), msg("assistant", "b"), msg("user", "c"), msg("assistant", "d")), "alice", "m")
	// alice edits history -> suspect; bob is clean
	led.Observe(mk(msg("user", sentinel), msg("assistant", "b"), msg("user", "EDIT"), msg("assistant", "d"), msg("user", "e")), "alice", "m")
	led.Observe(mk(msg("user", "x")), "bob", "m")

	events := bus.New()
	cfg, _ := config.New(config.Config{Server: config.Server{Listen: ":0"}})
	s := New(Deps{
		Snapshots: collector.NewStatic(nil), Engine: &engine.Stub{}, Sched: &sched.Stub{},
		Auth:   &stubAuth{identity: authz.Identity{Name: "op", Role: authz.RoleOperator}},
		Events: events, Publish: events, Config: func() *config.Config { return cfg },
		Hostname: "h", CtxLedger: led,
	})
	t.Cleanup(func() { s.Close() })

	w := do(t, s, authedRequest("GET", "/api/v1/context/creation?window=24h&by=consumer", nil))
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), sentinel) {
		t.Fatal("content leaked into endpoint")
	}
	var raw map[string]json.RawMessage
	decodeJSON(t, strings.NewReader(w.Body.String()), &raw)
	for _, k := range []string{"window", "by", "rows", "total_requests", "total_created_chars", "schema_chars_p50",
		"reuse_p50", "reuse_samples", "cache_bust_suspects", "suspects_by_consumer", "ledger"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	var led2 map[string]any
	json.Unmarshal(raw["ledger"], &led2)
	for _, k := range []string{"observed", "dropped", "parse_failures", "panics", "flush_errors"} {
		if _, ok := led2[k]; !ok {
			t.Errorf("ledger missing snake_case key %q: %v", k, led2)
		}
	}
	for k := range led2 {
		if k != strings.ToLower(k) {
			t.Errorf("ledger key %q not lower-case", k)
		}
	}
	var res struct {
		Total int64                  `json:"cache_bust_suspects"`
		Subc  []ctxledger.SuspectRow `json:"suspects_by_consumer"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Total != 1 || len(res.Subc) != 1 || res.Subc[0].Consumer != "alice" ||
		res.Subc[0].Suspects != 1 || res.Subc[0].Requests != 3 {
		t.Fatalf("suspects: total=%d by=%+v", res.Total, res.Subc)
	}

	if w := do(t, s, authedRequest("GET", "/api/v1/context/creation?window=bogus", nil)); w.Code != 422 {
		t.Fatalf("bad window status %d", w.Code)
	}
}
