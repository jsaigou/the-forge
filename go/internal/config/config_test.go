// SPDX-License-Identifier: Apache-2.0

package config

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// sampleConfig is the struct-literal equivalent of the old sample TOML
// fixture — kept as a builder function (not a package-level value) since
// several tests mutate a copy of it.
func sampleConfig() Config {
	return Config{
		Server: Server{Listen: ":5001"},
		Paths:  Paths{ModelsDir: "/opt/forge/models"},
		Slots: map[string]Slot{
			"a1": {Unit: "forge-a1", Port: 8080, Label: "A1", Order: 1},
			"a3":      {Unit: "forge-a3", Port: 8087, Label: "A3", Order: 3},
		},
		Ports: map[string]int{"embedding": 8083, "stt": 8084},
		Modes: map[string]Mode{
			"gemma4": {
				Label: "Gemma 4", Family: "Gemma", Default: true,
				Services: []Service{{Model: "gemma4.gguf", Alias: "gemma4", Context: 131072, PortRole: "a1"}},
			},
			"creative": {Label: "ComfyUI", Type: "service", Unit: "ai-mode-comfyui"},
		},
	}
}

func TestParseSample(t *testing.T) {
	cfg, err := New(sampleConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.Server.Listen != ":5001" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.RouterListen != ":8085" {
		t.Errorf("router_listen default = %q, want :8085", cfg.Server.RouterListen)
	}
	if cfg.Slots["a3"].Port != 8087 {
		t.Errorf("a3 port = %d, want 8087 (8086 is forge-aligner!)", cfg.Slots["a3"].Port)
	}
	svc := cfg.Modes["gemma4"].Services[0]
	if svc.Backend != "vulkan" {
		t.Errorf("backend default = %q, want vulkan", svc.Backend)
	}
	if svc.StartupTimeoutS != 300 {
		t.Errorf("startup_timeout_s default = %d, want 300", svc.StartupTimeoutS)
	}
	if cfg.Monitor.PollIntervalS != 2 || cfg.Scheduler.IdleUnloadS != 180 {
		t.Error("monitor/scheduler defaults not applied")
	}
	// BE-COST (F5): [cost] defaults apply when the section is absent.
	if cfg.Cost.PowerKW != DefaultPowerKW {
		t.Errorf("cost.power_kw default = %v, want %v", cfg.Cost.PowerKW, DefaultPowerKW)
	}
	if cfg.Cost.RatePerKWh != DefaultRatePerKWh {
		t.Errorf("cost.rate_per_kwh default = %v, want %v", cfg.Cost.RatePerKWh, DefaultRatePerKWh)
	}
	if cfg.Cost.RateCurrency != DefaultRateCurrency {
		t.Errorf("cost.rate_currency default = %q, want %q", cfg.Cost.RateCurrency, DefaultRateCurrency)
	}
	// Cost/savings sprint 2026-07-30: wall-power model defaults.
	if cfg.Cost.OverheadW != DefaultOverheadW {
		t.Errorf("cost.overhead_w default = %v, want %v", cfg.Cost.OverheadW, DefaultOverheadW)
	}
	if cfg.Cost.PSUEfficiency != DefaultPSUEfficiency {
		t.Errorf("cost.psu_efficiency default = %v, want %v", cfg.Cost.PSUEfficiency, DefaultPSUEfficiency)
	}
}

// TestParseCostOverride confirms [cost] is admin-overridable (BE-COST F5:
// "rate is configurable"), not just default-only.
func TestParseCostOverride(t *testing.T) {
	c := sampleConfig()
	c.Cost = Cost{PowerKW: 0.2, RatePerKWh: 0.30, RateCurrency: "JPY", OverheadW: 40, PSUEfficiency: 0.85}
	cfg, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cfg.Cost.PowerKW != 0.2 {
		t.Errorf("cost.power_kw = %v, want 0.2", cfg.Cost.PowerKW)
	}
	if cfg.Cost.RatePerKWh != 0.30 {
		t.Errorf("cost.rate_per_kwh = %v, want 0.30", cfg.Cost.RatePerKWh)
	}
	if cfg.Cost.RateCurrency != "JPY" {
		t.Errorf("cost.rate_currency = %q, want JPY", cfg.Cost.RateCurrency)
	}
	if cfg.Cost.OverheadW != 40 {
		t.Errorf("cost.overhead_w = %v, want 40", cfg.Cost.OverheadW)
	}
	if cfg.Cost.PSUEfficiency != 0.85 {
		t.Errorf("cost.psu_efficiency = %v, want 0.85", cfg.Cost.PSUEfficiency)
	}
}

// TestCostWallWatts checks the shared helper directly, independent of the
// registry's use of it.
func TestCostWallWatts(t *testing.T) {
	c := Cost{OverheadW: 25, PSUEfficiency: 0.9}
	got := c.WallWatts(140)
	want := (140.0 + 25) / 0.9
	if got != want {
		t.Errorf("WallWatts(140) = %v, want %v", got, want)
	}
}

// TestResolveCost checks the shared full-resolution helper: nil cfg, a
// zero-value Cost (bare literal, not through applyDefaults), and a partially
// set Cost all resolve every field to either the set value or the default.
func TestResolveCost(t *testing.T) {
	if got := ResolveCost(nil); got.PowerKW != DefaultPowerKW || got.PSUEfficiency != DefaultPSUEfficiency {
		t.Errorf("ResolveCost(nil) = %+v", got)
	}
	cfg := &Config{Cost: Cost{PowerKW: 0.5}} // everything else unset
	got := ResolveCost(cfg)
	if got.PowerKW != 0.5 {
		t.Errorf("ResolveCost: PowerKW = %v, want 0.5", got.PowerKW)
	}
	if got.RatePerKWh != DefaultRatePerKWh || got.RateCurrency != DefaultRateCurrency ||
		got.OverheadW != DefaultOverheadW || got.PSUEfficiency != DefaultPSUEfficiency {
		t.Errorf("ResolveCost: unset fields not defaulted: %+v", got)
	}
}

// TestValidateRejectsBadCostFields exercises validate() directly (bypassing
// applyDefaults, which corrects any <=0 PSUEfficiency/OverheadW to a default
// before validate() would ever see it via the normal New()/LoadFromStore
// path) — this is validate()'s own defense-in-depth for a caller that
// constructs Config without going through applyDefaults, or a future
// settings-write path that re-validates a live config directly.
func TestValidateRejectsBadCostFields(t *testing.T) {
	cases := []struct {
		name string
		cost Cost
	}{
		{"psu_efficiency zero", Cost{PowerKW: 0.14, RatePerKWh: 0.21, PSUEfficiency: 0, OverheadW: 25}},
		{"psu_efficiency over one", Cost{PowerKW: 0.14, RatePerKWh: 0.21, PSUEfficiency: 1.5, OverheadW: 25}},
		{"overhead_w negative", Cost{PowerKW: 0.14, RatePerKWh: 0.21, PSUEfficiency: 0.9, OverheadW: -5}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := sampleConfig()
			c.Cost = tc.cost
			if err := c.validate(); err == nil {
				t.Errorf("validate() with %+v: want error, got nil", tc.cost)
			}
		})
	}
}

func TestParseRejectsDuplicatePorts(t *testing.T) {
	c := sampleConfig()
	c.Slots["a3"] = Slot{Unit: "forge-a3", Port: 8080, Label: "A3", Order: 3} // collides with a1's 8080
	if _, err := New(c); err == nil {
		t.Error("duplicate slot ports must be rejected")
	}
}

func TestParseRejectsBadBackend(t *testing.T) {
	c := sampleConfig()
	c.Modes["gemma4"].Services[0].Backend = "cuda"
	if _, err := New(c); err == nil {
		t.Error("unknown backend must be rejected")
	}
}

// TestResolveModelPath pins the canonical resolution rule shared by the
// engine and the registry for a Service.Model / .MMProj path. The load bug
// found live 2026-07-25 was that the engine did a bare filepath.Join
// unconditionally and broke on absolute paths — this is the contract both
// paths now share.
func TestResolveModelPath(t *testing.T) {
	p := Paths{ModelsDir: "/opt/forge/models"}

	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"empty passes through", "", ""},
		{"relative joined", "qwen.gguf", "/opt/forge/models/qwen.gguf"},
		{"relative subdir joined", "sub/qwen.gguf", "/opt/forge/models/sub/qwen.gguf"},
		{"absolute passes through", "/var/lib/forge/laguna.gguf", "/var/lib/forge/laguna.gguf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.ResolveModelPath(tc.in); got != tc.want {
				t.Errorf("ResolveModelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	// Empty ModelsDir → pass-through (avoids producing "/file.gguf").
	emptyP := Paths{}
	if got := emptyP.ResolveModelPath("qwen.gguf"); got != "qwen.gguf" {
		t.Errorf("ResolveModelPath with empty ModelsDir = %q, want %q", got, "qwen.gguf")
	}
}

// TestLoadFromStoreEmpty proves LoadFromStore behaves like Parse on an
// absent TOML document: no infra.* settings keys set, no slots seeded —
// every field falls back to applyDefaults(), exactly like a bare "[server]"
// document would (TOML decommission Phase 1, docs/v5-toml-decommission.md
// §4).
func TestLoadFromStoreEmpty(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	cfg, err := LoadFromStore(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if cfg.Server.RouterListen != ":8085" {
		t.Errorf("router_listen default = %q, want :8085", cfg.Server.RouterListen)
	}
	if cfg.Monitor.PollIntervalS != 2 || cfg.Scheduler.IdleUnloadS != 180 {
		t.Error("monitor/scheduler defaults not applied")
	}
	if cfg.Cost.PowerKW != DefaultPowerKW {
		t.Errorf("cost.power_kw default = %v, want %v", cfg.Cost.PowerKW, DefaultPowerKW)
	}
	if cfg.Cost.OverheadW != DefaultOverheadW || cfg.Cost.PSUEfficiency != DefaultPSUEfficiency {
		t.Errorf("cost wall-power defaults not applied: overhead_w=%v psu_efficiency=%v",
			cfg.Cost.OverheadW, cfg.Cost.PSUEfficiency)
	}
	if len(cfg.Slots) != 0 {
		t.Errorf("Slots = %+v, want empty (no slots seeded)", cfg.Slots)
	}
	if len(cfg.Modes) != 0 {
		t.Errorf("Modes = %+v, want empty (Modes never comes from LoadFromStore)", cfg.Modes)
	}
}

// TestLoadFromStorePopulated exercises the real path: infra.* settings keys
// set and slots rows present, mirroring what the Phase 2 cutover migration
// will write.
func TestLoadFromStorePopulated(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	settings := db.Settings()

	set := func(key string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", key, err)
		}
		if err := settings.Set(ctx, key, raw); err != nil {
			t.Fatalf("Set %s: %v", key, err)
		}
	}
	set("infra.server", Server{Listen: ":5001", RouterListen: ":8085"})
	set("infra.paths", Paths{ModelsDir: "/opt/forge/models"})
	set("infra.ports", map[string]int{"embedding": 8083, "stt": 8084})
	// Deliberately the OLD shape — no overhead_w/psu_efficiency keys at all
	// (what every infra.cost row written before this sprint looks like) —
	// must still load cleanly and take the wall-power defaults rather than
	// erroring or leaving them at an unvalidatable zero.
	set("infra.cost", Cost{PowerKW: 0.2, RatePerKWh: 0.30, RateCurrency: "JPY"})

	if _, err := db.Catalog().CreateSlot(ctx, store.Slot{
		Name: "a3", Unit: "forge-a3", Port: 8087, Label: "A3", SortOrder: 3,
	}); err != nil {
		t.Fatalf("CreateSlot: %v", err)
	}

	cfg, err := LoadFromStore(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if cfg.Server.Listen != ":5001" {
		t.Errorf("listen = %q, want :5001", cfg.Server.Listen)
	}
	if cfg.Paths.ModelsDir != "/opt/forge/models" {
		t.Errorf("models_dir = %q", cfg.Paths.ModelsDir)
	}
	if cfg.Ports["stt"] != 8084 {
		t.Errorf("ports[stt] = %d, want 8084", cfg.Ports["stt"])
	}
	if cfg.Slots["a3"].Port != 8087 {
		t.Errorf("a3 port = %d, want 8087 (8086 is forge-aligner!)", cfg.Slots["a3"].Port)
	}
	if cfg.Cost.RateCurrency != "JPY" {
		t.Errorf("cost.rate_currency = %q, want JPY", cfg.Cost.RateCurrency)
	}
	if cfg.Cost.OverheadW != DefaultOverheadW || cfg.Cost.PSUEfficiency != DefaultPSUEfficiency {
		t.Errorf("old-shaped infra.cost (no overhead_w/psu_efficiency) must still default them: got overhead_w=%v psu_efficiency=%v",
			cfg.Cost.OverheadW, cfg.Cost.PSUEfficiency)
	}
	// cfg here came from an infra.server value with no cookie_secure key at
	// all (the "old shape" every real infra.server row written before
	// sprint 4 looks like) — must still default true, not silently read as
	// false. See TestCookieSecureDefaultAndOverride for the explicit-value
	// cases.
	if cfg.Server.CookieSecure == nil || !*cfg.Server.CookieSecure {
		t.Errorf("old-shaped infra.server (no cookie_secure) must default CookieSecure=true, got %v", cfg.Server.CookieSecure)
	}
}

// TestCookieSecureDefaultAndOverride pins issue #27's tri-state semantics:
// an absent infra.server.cookie_secure resolves to the safe default (true),
// and an operator's explicit false (the tailscale-serve-only opt-out) is
// never silently overridden back to true.
func TestCookieSecureDefaultAndOverride(t *testing.T) {
	// New() with a bare zero-value Config (no store involved at all) —
	// mirrors a from-scratch New() caller the same way TestLoadFromStoreEmpty
	// mirrors LoadFromStore's.
	def, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if def.Server.CookieSecure == nil || !*def.Server.CookieSecure {
		t.Errorf("New(Config{}) CookieSecure = %v, want true", def.Server.CookieSecure)
	}

	falseVal := false
	off, err := New(Config{Server: Server{CookieSecure: &falseVal}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if off.Server.CookieSecure == nil || *off.Server.CookieSecure {
		t.Errorf("explicit CookieSecure=false was not preserved, got %v", off.Server.CookieSecure)
	}

	// Round-trip through the store, the real production path: an operator
	// who explicitly opted out must stay opted out across a reload.
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	raw, err := json.Marshal(Server{Listen: ":5001", CookieSecure: &falseVal})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Settings().Set(ctx, "infra.server", raw); err != nil {
		t.Fatalf("Set infra.server: %v", err)
	}
	stored, err := LoadFromStore(ctx, db)
	if err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	if stored.Server.CookieSecure == nil || *stored.Server.CookieSecure {
		t.Errorf("stored explicit CookieSecure=false was not preserved through LoadFromStore, got %v", stored.Server.CookieSecure)
	}
}
