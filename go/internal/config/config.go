// SPDX-License-Identifier: Apache-2.0

// Package config holds forge's infra config shape — modes, slots, ports,
// hardware, paths — and loads it from the store (config.LoadFromStore).
// App-mutated state lives in the same store. Reload on SIGHUP or restart.
//
// Originally a read-only forge.toml file (V5 design decision 1,
// docs/v5-plan.md); the file and its TOML decoder were retired once
// LoadFromStore replaced Load/Parse as the sole loader (TOML decommission,
// docs/v5-toml-decommission.md, cutover live 2026-07-28, code deleted
// Phase 8 §8). The struct shapes are unchanged — every downstream read site
// (engine, collector, registry, profile, the merged-config seam) still gets
// the identical *Config.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jsaigou/the-forge/internal/store"
)

// Config is the root of forge's infra config, built by LoadFromStore.
type Config struct {
	Server    Server
	Paths     Paths
	Slots     map[string]Slot
	Ports     map[string]int // auxiliary services: embedding, stt, ...
	Modes     map[string]Mode
	Scheduler SchedulerDefault
	Monitor   Monitor
	Tailscale Tailscale
	Cost      Cost
}

// Server holds listen addresses and the state-db location. Canonical V4
// ports: dashboard 5000, a0 8085, MCP 8095 (never 8086 — forge-aligner).
type Server struct {
	Listen       string `json:"listen"`        // dashboard + API, default ":5000"
	RouterListen string `json:"router_listen"` // a0, default ":8085"
	MCPListen    string `json:"mcp_listen"`    // default ":8095"
	DBPath       string `json:"db_path"`       // default "/var/lib/forge/forge.db"
	TTSUnit      string `json:"tts_unit"`      // TTS systemd unit, default "forge-tts"
}

type Paths struct {
	ModelsDir    string `json:"models_dir"`    // e.g. /opt/forge/models
	SysconfigDir string `json:"sysconfig_dir"` // e.g. /etc/sysconfig
	StateDir     string `json:"state_dir"`     // e.g. /var/lib/forge
	IconsDir     string `json:"icons_dir"`
	VulkanBin    string `json:"vulkan_bin"` // llama-server (vulkan build)
	RocmBin      string `json:"rocm_bin"`   // llama-server (rocm build)
}

// ResolveModelPath joins a relative model/mmproj file_path with ModelsDir.
// Absolute paths and empty strings pass through unchanged.
//
// This is the single canonical resolution rule for a Service.Model / .MMProj
// path — the same rule the registry's resolveArtifactPath already applied
// for card display. The engine's path builders (modeWeightBytes,
// writeServiceFiles, recordHistory) used to do a bare filepath.Join, which
// for an absolute file_path produced ModelsDir + "/abs/path" — a nonexistent
// path — and the fit check refused to load with "model weights not found on
// disk" even though the file existed and the card showed its size. All call
// sites now route through this helper so the engine and registry agree.
func (p Paths) ResolveModelPath(path string) string {
	if path == "" || p.ModelsDir == "" {
		return path
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.ModelsDir, path)
}

// Slot describes one inference bay. Defaults: a1/A1/8080, a2/A2/8081,
// a3/A3/8087, a4/A4/8088. (V4/early-V5 used primary/secondary for a1/a2 —
// renamed 2026-07-29 for consistency with a3/a4.)
type Slot struct {
	Unit  string // systemd unit name without .service
	Port  int
	Label string // display label, e.g. "A1"
	Order int    // display/placement order
}

// Mode mirrors the V4 [modes.<name>] block (docs/design.md), minus anything
// app-mutated.
type Mode struct {
	Label       string
	Family      string
	Description string
	Color       string
	Icon        string
	Tags        []string
	Default     bool
	Type        string // "" (inference) or "service"
	Unit        string // service modes only
	Services    []Service

	// ConfigID is the catalog configs.id for this mode (Phase B: B2).
	// Populated by the merged-config seam from the store; 0 when the mode
	// comes from the file config (pre-migration) or is a service. The
	// engine resolves mode name → ConfigID here, then passes the int64
	// downstream to registry.WeightEstimateBytes / PowerEstPer1m — no
	// string lookups in the hot path.
	ConfigID int64
}

// Service is one llama-server (or vllm) launch spec inside a mode.
type Service struct {
	Model           string
	Alias           string
	Context         int
	PortRole        string // a1 | a2 | a3 | a4 | none
	Backend         string // vulkan | rocm | vllm (default vulkan)
	LlamaBin        string // optional per-service llama-server override (custom builds ahead of upstream); empty → launcher picks by backend
	MMProj          string
	ExtraArgs       []string
	StartupTimeoutS int
}

// SchedulerDefault seeds scheduler tunables; runtime changes persist to the
// store's settings table and override these.
type SchedulerDefault struct {
	IdleUnloadS            int `json:"idle_unload_s"`
	SmallJobTokenThreshold int `json:"small_job_token_threshold"`
	PriorityJumpCap        int `json:"priority_jump_cap"`
	ReservationSoonMin     int `json:"reservation_soon_min"`
}

// Monitor holds collector cadence and hang-detection thresholds (V4
// MonitorSettings defaults).
type Monitor struct {
	PollIntervalS   int `json:"poll_interval_s"`     // default 2 (Sprint K, was 4 — see cycle()'s slow-probe gating)
	HangTPSThousand int `json:"hang_tps_thousandth"` // TPS threshold ×1000 (default 100 = 0.1)
	HangSustainS    int `json:"hang_sustain_s"`      // default 90
	SwitchCooldownS int `json:"switch_cooldown_s"`   // default 120
	GTTWarnPct      int `json:"gtt_warn_pct"`        // default 85
}

// Tailscale holds mesh facts.
type Tailscale struct {
	Hostname string `json:"hostname"` // e.g. forge.example.ts.net
	RPID     string `json:"rp_id"`    // reserved (WebAuthn dropped in V5.0)
}

// Cost is the electricity-cost model behind local-model $/1M-token
// estimates (BE-COST, docs/v5-review-fixes.md F5): the card/usage cost is
// no longer a hand-typed guess (models.toml's old power_est_per_1m /
// power_cost_per_1k, off by ~35x for at least one model) but computed as
//
//	cost_per_1M = power_kW × (1e6 / (tps × 3600)) × rate_per_kWh
//
// tps is the model's tokens/sec (internal/registry reads it from the
// curated performance.measured_ts field today; a future PROFILE track will
// supply real measured T/s instead — see internal/registry's modelTPS()).
type Cost struct {
	// PowerKW is the assumed inference-time power draw in kW. Default
	// 0.14 (140W) approximates a Strix Halo APU's board power under
	// sustained llama.cpp inference load.
	PowerKW float64 `json:"power_kw"`

	// RatePerKWh is the electricity rate, denominated in RateCurrency.
	// Default ≈0.21 (see DefaultRatePerKWh) — operators should override
	// this with their actual local rate.
	RatePerKWh float64 `json:"rate_per_kwh"`

	// RateCurrency is the currency RatePerKWh is denominated in. Default
	// "USD" — matches the card/usage cost fields, which are USD-denominated
	// (Sprint 0 §0.2); see DefaultRatePerKWh's comment for how the default
	// value was derived from a JPY rate.
	RateCurrency string `json:"rate_currency"`

	// OverheadW is the assumed rest-of-system power draw NOT captured by
	// the amdgpu PPT package-power sensor (RAM, NVMe, fans, NIC, USB —
	// everything outside the APU package rail). Added to a measured or
	// assumed package-watts figure before the PSU-efficiency division, via
	// WallWatts, to approximate real wall power. Default 25W is an
	// order-of-magnitude guess for a Strix Halo mini-PC; operators should
	// calibrate it against a plug meter.
	OverheadW float64 `json:"overhead_w"`

	// PSUEfficiency is the power supply's DC-out/AC-in efficiency ratio,
	// (0, 1]. Default 0.90. Divides the package+overhead wattage in
	// WallWatts to account for conversion loss.
	PSUEfficiency float64 `json:"psu_efficiency"`

	// MaxPowerW is the package-level power ceiling used to scale the
	// Dashboard's Overview power chart/tile against a real hardware limit
	// instead of whatever happened to be the highest reading in the
	// selected window. Default 140W (see DefaultMaxPowerW) — the operator's
	// hardware's published sustained/peak APU package draw, NOT the power
	// adapter's rated supply capacity (which has deliberate compressor and
	// isn't the meaningful ceiling). Fully operator-editable for other
	// hardware.
	MaxPowerW float64 `json:"max_power_w"`
}

// WallWatts approximates real wall power from a package/board-power
// reading (measured or assumed) by adding the rest-of-system overhead and
// dividing by PSU efficiency. One shared helper so every cost computation
// (the per-model card estimate and the whole-server measured figure) agrees
// on the same formula — writing it out at each call site is exactly how two
// cost paths end up silently disagreeing.
func (c Cost) WallWatts(packageW float64) float64 {
	return (packageW + c.OverheadW) / c.PSUEfficiency
}

// ResolveCost returns a fully-populated Cost — every field either from cfg
// (when set and valid) or the package Default*, regardless of whether cfg
// is nil or was constructed without going through applyDefaults (a bare
// struct literal, as in tests, or a future caller). Callers that need a
// usable Cost outside the New()/LoadFromStore() path (the cost/savings
// HTTP handlers; internal/registry.powerRate has its own inline copy of
// this same resolution, predating this helper) should use this instead of
// re-deriving the same fallback chain.
func ResolveCost(cfg *Config) Cost {
	out := Cost{
		PowerKW:       DefaultPowerKW,
		RatePerKWh:    DefaultRatePerKWh,
		RateCurrency:  DefaultRateCurrency,
		OverheadW:     DefaultOverheadW,
		PSUEfficiency: DefaultPSUEfficiency,
		MaxPowerW:     DefaultMaxPowerW,
	}
	if cfg == nil {
		return out
	}
	if cfg.Cost.PowerKW > 0 {
		out.PowerKW = cfg.Cost.PowerKW
	}
	if cfg.Cost.RatePerKWh > 0 {
		out.RatePerKWh = cfg.Cost.RatePerKWh
	}
	if cfg.Cost.RateCurrency != "" {
		out.RateCurrency = cfg.Cost.RateCurrency
	}
	if cfg.Cost.OverheadW > 0 {
		out.OverheadW = cfg.Cost.OverheadW
	}
	if cfg.Cost.PSUEfficiency > 0 {
		out.PSUEfficiency = cfg.Cost.PSUEfficiency
	}
	if cfg.Cost.MaxPowerW > 0 {
		out.MaxPowerW = cfg.Cost.MaxPowerW
	}
	return out
}

// Defaults for Cost, exported so callers that compute a cost estimate
// without a *Config in hand (nil cfg — Phase 4 stub environments,
// internal/registry unit tests) still get a sane number.
const (
	// DefaultPowerKW: 140W, an approximate Strix Halo APU board-power draw
	// under sustained inference load.
	DefaultPowerKW = 0.14

	// DefaultRatePerKWh: ≈$0.21/kWh — a USD-equivalent approximation of
	// Tokyo TEPCO's residential "Everyday Plan" average rate, roughly
	// ¥31/kWh blended across its tiered pricing as of 2026, converted at
	// an approximate ¥150/$1. This is an order-of-magnitude default, not a
	// billed rate; it drifts with FX and seasonal tier averaging, so
	// operators should override [cost] rate_per_kwh with their real rate.
	DefaultRatePerKWh = 0.21

	// DefaultRateCurrency matches the card/usage cost fields' USD
	// denomination (Sprint 0 §0.2).
	DefaultRateCurrency = "USD"

	// DefaultOverheadW: 25W, an order-of-magnitude guess for the
	// rest-of-system draw (RAM, NVMe, fans, NIC) a Strix Halo mini-PC's
	// amdgpu PPT sensor doesn't see. Calibrate against a plug meter.
	DefaultOverheadW = 25.0

	// DefaultPSUEfficiency: 0.90, a typical switching-PSU efficiency at
	// partial load.
	DefaultPSUEfficiency = 0.90

	// DefaultMaxPowerW: 140W. Dashboard cost/savings sprint follow-up —
	// researched against the operator's actual hardware (a GMKtec Ryzen AI
	// Max+ 395 mini PC): multiple independent sources (GMKtec's own product
	// page, WCCFTech, PCWorld, Tom's Hardware) agree on 120W sustained /
	// 140W peak APU package draw (the device also exposes firmware-
	// selectable power profiles — Quiet 54W / Balanced 85W / Performance
	// 140W). No source publishes a "145W" figure specifically; 140W is the
	// best-documented number. Purely a scaling ceiling for the Overview
	// power chart/tile — never used for cost math — so a few watts of
	// default error costs nothing, and it's fully operator-editable.
	DefaultMaxPowerW = 140.0
)

// New builds a *Config from an in-memory value, applying defaults and
// validating exactly like LoadFromStore does — for callers that already
// have the data in Go (tests, in-process construction) rather than the
// store. The struct-literal equivalent of what the retired Parse did for a
// TOML document.
func New(cfg Config) (*Config, error) {
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// LoadFromStore builds a *Config from store-backed infra settings plus the
// `slots` table, instead of parsing forge.toml (TOML decommission Phase 1,
// docs/v5-toml-decommission.md §4). It returns the identical *Config shape
// Load/Parse produce, so every downstream read site (engine, collector,
// registry, profile, the merged-config seam) keeps working unchanged.
//
// Modes is left as an empty map: mode/model data has never come from this
// loader — it's overlaid separately by the merged-config seam
// (cmd/forge/merged_config.go) directly from the catalog.
//
// Any `infra.*` settings key that is unset is treated the same as an absent
// TOML table (zero value, then applyDefaults fills it in) — this lets
// LoadFromStore run against a store that hasn't been through the Phase 2
// cutover migration yet without erroring.
//
// Not wired into main.go yet (Phase 3 wires this in as part of the actual
// cutover) — building and testing it standalone here touches nothing live.
func LoadFromStore(ctx context.Context, st store.Store) (*Config, error) {
	cfg := &Config{Modes: map[string]Mode{}}
	settings := st.Settings()

	for key, dst := range map[string]any{
		"infra.server":    &cfg.Server,
		"infra.paths":     &cfg.Paths,
		"infra.ports":     &cfg.Ports,
		"infra.scheduler": &cfg.Scheduler,
		"infra.monitor":   &cfg.Monitor,
		"infra.tailscale": &cfg.Tailscale,
		"infra.cost":      &cfg.Cost,
	} {
		if err := getSetting(ctx, settings, key, dst); err != nil {
			return nil, err
		}
	}

	slots, err := st.Catalog().ListSlots(ctx)
	if err != nil {
		return nil, fmt.Errorf("config: load slots: %w", err)
	}
	cfg.Slots = make(map[string]Slot, len(slots))
	for _, s := range slots {
		cfg.Slots[s.Name] = Slot{Unit: s.Unit, Port: s.Port, Label: s.Label, Order: s.SortOrder}
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

// getSetting reads a JSON-KV settings key into dst. A missing key is not an
// error — dst keeps its zero value, exactly like an absent TOML table today.
func getSetting(ctx context.Context, settings store.Settings, key string, dst any) error {
	raw, err := settings.Get(ctx, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("config: load %s: %w", key, err)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("config: parse %s: %w", key, err)
	}
	return nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = ":5000"
	}
	if c.Server.RouterListen == "" {
		c.Server.RouterListen = ":8085"
	}
	if c.Server.MCPListen == "" {
		c.Server.MCPListen = ":8095"
	}
	if c.Server.TTSUnit == "" {
		c.Server.TTSUnit = "forge-tts"
	}
	if c.Server.DBPath == "" {
		c.Server.DBPath = "/var/lib/forge/forge.db"
	}
	if c.Monitor.PollIntervalS == 0 {
		c.Monitor.PollIntervalS = 2
	}
	if c.Monitor.HangTPSThousand == 0 {
		c.Monitor.HangTPSThousand = 100
	}
	if c.Monitor.HangSustainS == 0 {
		c.Monitor.HangSustainS = 90
	}
	if c.Monitor.SwitchCooldownS == 0 {
		c.Monitor.SwitchCooldownS = 120
	}
	if c.Monitor.GTTWarnPct == 0 {
		c.Monitor.GTTWarnPct = 85
	}
	if c.Scheduler.IdleUnloadS == 0 {
		c.Scheduler.IdleUnloadS = 180
	}
	if c.Scheduler.SmallJobTokenThreshold == 0 {
		c.Scheduler.SmallJobTokenThreshold = 1500
	}
	if c.Scheduler.PriorityJumpCap == 0 {
		c.Scheduler.PriorityJumpCap = 2
	}
	if c.Scheduler.ReservationSoonMin == 0 {
		c.Scheduler.ReservationSoonMin = 10
	}
	if c.Cost.PowerKW <= 0 {
		c.Cost.PowerKW = DefaultPowerKW
	}
	if c.Cost.RatePerKWh <= 0 {
		c.Cost.RatePerKWh = DefaultRatePerKWh
	}
	if c.Cost.RateCurrency == "" {
		c.Cost.RateCurrency = DefaultRateCurrency
	}
	if c.Cost.OverheadW <= 0 {
		c.Cost.OverheadW = DefaultOverheadW
	}
	if c.Cost.PSUEfficiency <= 0 {
		c.Cost.PSUEfficiency = DefaultPSUEfficiency
	}
	if c.Cost.MaxPowerW <= 0 {
		c.Cost.MaxPowerW = DefaultMaxPowerW
	}
	for name, svc := range c.allServices() {
		if svc.Backend == "" {
			svc.Backend = "vulkan"
		}
		if svc.StartupTimeoutS == 0 {
			svc.StartupTimeoutS = 300 // kept in step with merged_config.go's catalog-path default
		}
		c.Modes[name.mode].Services[name.idx] = *svc
	}
}

type serviceKey struct {
	mode string
	idx  int
}

func (c *Config) allServices() map[serviceKey]*Service {
	out := map[serviceKey]*Service{}
	for mode := range c.Modes {
		for i := range c.Modes[mode].Services {
			out[serviceKey{mode, i}] = &c.Modes[mode].Services[i]
		}
	}
	return out
}

func (c *Config) validate() error {
	if c.Cost.PSUEfficiency <= 0 || c.Cost.PSUEfficiency > 1 {
		return fmt.Errorf("cost: psu_efficiency must be in (0, 1], got %v", c.Cost.PSUEfficiency)
	}
	if c.Cost.OverheadW < 0 {
		return fmt.Errorf("cost: overhead_w must be >= 0, got %v", c.Cost.OverheadW)
	}
	if c.Cost.MaxPowerW <= 0 || c.Cost.MaxPowerW > 2000 {
		return fmt.Errorf("cost: max_power_w must be in (0, 2000], got %v", c.Cost.MaxPowerW)
	}
	seen := map[int]string{}
	for name, slot := range c.Slots {
		if slot.Unit == "" || slot.Port == 0 {
			return fmt.Errorf("slot %s: unit and port are required", name)
		}
		if other, dup := seen[slot.Port]; dup {
			return fmt.Errorf("slots %s and %s share port %d", other, name, slot.Port)
		}
		seen[slot.Port] = name
	}
	for name, mode := range c.Modes {
		if mode.Type == "service" {
			continue
		}
		for _, svc := range mode.Services {
			switch svc.Backend {
			case "vulkan", "rocm", "vllm":
			default:
				return fmt.Errorf("mode %s: unknown backend %q", name, svc.Backend)
			}
		}
	}
	return nil
}
