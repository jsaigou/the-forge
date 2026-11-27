// SPDX-License-Identifier: Apache-2.0

// Command forge is The Forge V5 single-binary daemon: dashboard +
// scheduler + a0 router + MCP in one process (docs/v5-plan.md design
// decision 3).
//
// Phase 9a integration wiring (this file): the Phase 1 stub implementations
// are replaced with the real components behind the same Contract 2
// interfaces. The wiring order follows the dependency graph:
//
//	store.Open  →  engine.NewDBus (systemd) + engine.NewManager
//	            →  collector.New (probe loop)  →  sched.New
//	            →  authz.New  →  httpapi.New / router.NewWithDeps / mcp.NewWithDeps
//
// The engine↔collector cycle (the engine pings the collector to open the
// hang-detection cooldown; the collector reads the engine for slot occupancy)
// is broken with a small settable notifier (see switchNotifier).
//
// This is a systemd-targeted linux daemon: it requires the system D-Bus and
// the state directory, so it does not run on a developer's macOS host beyond
// `forge -version`. The supervised ForgeHost parallel-run is Phase 9b.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/buildguard"
	"github.com/jsaigou/the-forge/internal/bus"
	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/compressorctl"
	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/ctxledger"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/fx"
	"github.com/jsaigou/the-forge/internal/hf"
	"github.com/jsaigou/the-forge/internal/hfdownload"
	"github.com/jsaigou/the-forge/internal/hostprobe"
	"github.com/jsaigou/the-forge/internal/httpapi"
	"github.com/jsaigou/the-forge/internal/inflight"
	"github.com/jsaigou/the-forge/internal/maintenance"
	"github.com/jsaigou/the-forge/internal/mcp"
	"github.com/jsaigou/the-forge/internal/profile"
	"github.com/jsaigou/the-forge/internal/providers"
	"github.com/jsaigou/the-forge/internal/registry"
	"github.com/jsaigou/the-forge/internal/router"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/settingsvc"
	"github.com/jsaigou/the-forge/internal/slotexec"
	"github.com/jsaigou/the-forge/internal/smith"
	"github.com/jsaigou/the-forge/internal/smith/comfyui"
	"github.com/jsaigou/the-forge/internal/smith/procedures"
	smithweb "github.com/jsaigou/the-forge/internal/smith/web"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/speechsetup"
	"github.com/jsaigou/the-forge/internal/store"
)

// version is stamped via -ldflags "-X main.version=...".
var version = "v0.8.0-dev"

// specialistEnvDir is where the daemon writes one <slot>.env per specialist slot (testuser-owned; created once by the operator, see systemd/forge-pool@.service).
const specialistEnvDir = "/var/lib/forge/pool"

func main() {
	var (
		listen      = flag.String("listen", "", "override [server].listen (dashboard + API)")
		dbPath      = flag.String("db", "", "state SQLite database (default /var/lib/forge/forge.db, or $FORGE_DB)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)

	// mint-key, config, keys, catalog, and smith are subcommands, not flags:
	// `forge mint-key ...` / `forge config ...` / `forge keys ...`
	// / `forge catalog ...` / `forge smith import-local ...`.
	// migrate-v4/repair-catalog-backend/migrate-infra-to-db
	// were one-time TOML-era tools, retired now that the cutover is done and
	// TOML parsing is gone entirely (TOML decommission Phase 8,
	// docs/v5-toml-decommission.md §8).
	// slot-exec is the generic engine launcher (ADR-0026 section 4): read-only, never boots a daemon, and replaces this process with the engine.
	if len(os.Args) > 1 && os.Args[1] == "slot-exec" {
		os.Exit(slotexec.Run(os.Args[2:], slotexec.Real()))
	}
	// launch-check is the read-only preflight for the slot_launches builder (Radiance phase 1b step d1): never boots a daemon, never writes.
	if len(os.Args) > 1 && os.Args[1] == "launch-check" {
		os.Exit(runLaunchCheck(os.Args[2:], os.Stdout, os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == "mint-key" {
		if err := runMintKey(os.Args[2:]); err != nil {
			log.Fatalf("forge mint-key: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "config" {
		if err := runConfigCLI(os.Args[2:]); err != nil {
			log.Fatalf("forge config: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "host-setup" {
		if err := runHostSetup(os.Args[2:]); err != nil {
			log.Fatalf("forge host-setup: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "keys" {
		if err := runKeysCLI(os.Args[2:]); err != nil {
			log.Fatalf("forge keys: %v", err)
		}
		return
	}
	// radiance register-build: registers a built Radiance engine as a catalog build for the installer (radiance_cli.go).
	if len(os.Args) > 1 && os.Args[1] == "radiance" {
		if err := runRadianceCLI(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			log.Fatalf("forge radiance: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "catalog" {
		if err := runCatalogCLI(os.Args[2:]); err != nil {
			log.Fatalf("forge catalog: %v", err)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "smith" {
		if err := runSmithCLI(os.Args[2:]); err != nil {
			log.Fatalf("forge smith: %v", err)
		}
		return
	}
	// CLI/TUI client verbs: `forge tui` plus scriptable subcommands —
	// status/models/load/unload/services/keys-export. All talk to the
	// RUNNING daemon over its dashboard API; they never boot one. A bare
	// `forge` still boots the daemon (that's how forge-daemon.service
	// invokes it) — the TUI must be explicit, never a default.
	if len(os.Args) > 1 && isCLIVerb(os.Args[1]) {
		if err := runCLI(os.Args); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// version is a subcommand alias for --version — a bare `forge
	// version` typo used to fall through flag.Parse() (which silently
	// ignores unrecognized positional args) straight into booting a full
	// second daemon on an already-live host, restarting whatever it
	// touched on the way (found live 2026-08-18: it re-ran the compressor
	// boot reconcile against production). Handled the same way as the
	// other subcommands, before flag.Parse() ever runs.
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("forge", version)
		return
	}

	flag.Parse()

	if *showVersion {
		fmt.Println("forge", version)
		return
	}

	// Refuse to boot on any leftover positional argument — the same
	// unknown-subcommand-boots-a-daemon defect class as above, for a typo
	// that isn't one of the five names handled explicitly. flag.Parse()
	// itself never errors on unrecognized non-flag args, only on
	// unrecognized -flags, so this check is the only thing that catches it.
	if flag.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "forge: unrecognized argument(s): %v\n\n", flag.Args())
		flag.Usage()
		os.Exit(2)
	}

	// Bootstrap without a file (TOML decommission Phase 3,
	// docs/v5-toml-decommission.md §4): the DB path is the only required
	// input left — everything else (server/paths/ports/scheduler/monitor/
	// tailscale/cost, slots, modes, router config) is store-backed. Explicit
	// flag wins over $FORGE_DB wins over the compiled-in default.
	resolvedDBPath := *dbPath
	if resolvedDBPath == "" {
		resolvedDBPath = os.Getenv("FORGE_DB")
	}
	if resolvedDBPath == "" {
		resolvedDBPath = "/var/lib/forge/forge.db"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hostname, _ := os.Hostname()
	events := bus.New()

	// ── State store (SQLite, single writer) ────────────────────────────────
	db, err := store.Open(resolvedDBPath)
	if err != nil {
		log.Fatalf("forge: open state db %s: %v", resolvedDBPath, err)
	}
	defer db.Close()
	log.Printf("forge: state db open at %s", resolvedDBPath)

	cfg, err := config.LoadFromStore(ctx, db)
	if err != nil {
		log.Fatalf("forge: load config from store: %v", err)
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}

	// cfgHolder makes the read-only config reloadable on SIGHUP for the
	// consumers that take a func() *config.Config (engine, collector) — they
	// re-read it every cycle. Listener addresses and the store/API wiring
	// snapshot the boot config and need a restart to change.
	var cfgHolder atomic.Pointer[config.Config]
	cfgHolder.Store(cfg)

	// Phase 2 (MODEL CATALOG): the merged-config provider overlays store-
	// backed catalog data (configs, services, artifacts) on top of the
	// store-backed infra config's Modes. Cached with a short TTL so hot
	// paths (CurrentMode, inferSlotMode) don't DB-thrash. All
	// engine/collector/registry/profile read sites go through getCfg, so
	// this single point makes them all store-backed.
	mergedProvider := newMergedConfigProvider(cfgHolder.Load, db.Catalog())
	getCfg := mergedProvider.Get

	// ── Systemd D-Bus adapter (shared by engine + collector) ───────────────
	dbusConn, err := engine.NewDBus(ctx)
	if err != nil {
		log.Fatalf("forge: systemd d-bus (required on the target host): %v", err)
	}
	defer dbusConn.Close()

	// Shared probe primitives: the GPU discovery caches its device dir, so a
	// single instance is shared between the engine (memory budgeting) and the
	// collector (metrics).
	gpu := &collector.GPU{}
	proc := collector.Proc{}

	// ComfyUI service coordinates for S1's memory eviction: the unit name
	// and URL live in the smith.comfyui.* settings keys (deployment data —
	// provisioned per install via import-local, never compiled in). The
	// closures resolve lazily so an import-local edit takes effect without
	// a restart. Empty unit/URL = the deployment has no (configured)
	// ComfyUI and eviction gracefully never proposes it.
	settings := db.Settings()
	comfyUnit, comfyFootprint, comfyURL := comfyUIHelpers(ctx, settings, dbusConn, proc)

	// ── Model registry (Phase B: catalog-backed) — reads card data + memory
	// estimates from the catalog DB (store.Catalog), not models.toml. Feeds
	// both the engine's memory budget (WeightEstimateBytes: safe_memory_bytes
	// benchmark) and the dashboard's GET /api/v1/configs/cards + models/cards.
	//
	// profileRunner is declared before the registry so the registry's
	// ProfileDecodeTPS closure can capture it by reference (same forward-ref
	// pattern as notifier and the engine's ProfileBytes below — the
	// registry↔profile construction cycle is broken with a settable pointer).
	var profileRunner *profile.Runner

	reg := registry.New(db.Catalog(), getCfg, db.Usage(),
		// ProfileDecodeTPS is the profile-aware pricing seam (BE-COST): a
		// fresh profile's measured decode_tps drives the card/usage
		// power_est_per_1m, falling back to the curated decode_tps benchmark
		// when nil/unwired or the profile is stale. nil until profileRunner
		// is set below; the closure dereferences at call time.
		registry.WithProfileDecodeTPS(func(mode string) (float64, bool) {
			if profileRunner == nil {
				return 0, false
			}
			return profileRunner.DecodeTPS(mode)
		}),
	)

	// switchNotifier breaks the engine↔collector construction cycle: the
	// engine needs a SwitchNotifier at build time, the collector does not
	// exist yet. It is pointed at the collector once both exist.
	notifier := &switchNotifier{}

	// specialistReserved is set once the specialist pool exists (it is built after the engine); the engine reads it through this pointer so
	// an LLM load never takes memory a restarting always-on specialist needs (ADR-0022).
	var specialistReserved atomic.Pointer[func() int64]
	var specialistDeclared atomic.Pointer[func() int64]
	// pooledConfig answers "is this catalog config hosted by the specialist pool?" for the engine, the scheduler and the router (ADR-0022: a pooled
	// config has exactly one owner and is never loaded into an LLM slot). Set once the pool exists.
	var pooledConfig atomic.Pointer[func(context.Context, string) bool]
	isPooledConfig := func(ctx context.Context, config string) bool {
		if f := pooledConfig.Load(); f != nil {
			return (*f)(ctx, config)
		}
		return false
	}

	// ── Engine (mode/slot lifecycle) ───────────────────────────────────────
	slotLaunchStore, slotLaunchEnforce := slotLaunchWiring(db.SlotLaunches())
	eng, err := engine.NewManager(engine.Deps{
		PooledConfig: isPooledConfig,
		SpecialistDeclaredBytes: func() int64 {
			if f := specialistDeclared.Load(); f != nil {
				return (*f)()
			}
			return 0
		},
		ReservedBytes: func() int64 {
			if f := specialistReserved.Load(); f != nil {
				return (*f)()
			}
			return 0
		},
		Cfg:    getCfg,
		Sys:    dbusConn,
		GPU:    gpu,
		Proc:   proc,
		Usage:  db.Usage(),
		Notify: notifier,
		// SlotLaunches: Phase 1b step d1 dual write of the slot_launches row, disarm on unload, adopt of running slots at start (ADR-0026).
		SlotLaunches: slotLaunchStore,
		// Enforcing (Phase 1b step e5): the generic shim (`forge slot-exec`) launches from this row, so a row that cannot be built or written must
		// fail the load rather than start the engine from stale or missing data. slotLaunchWiring is the one place this is decided (and tested).
		SlotLaunchEnforce: slotLaunchEnforce,
		// A shadow failure is a plain warn-level notification (no investigation): the load itself succeeded.
		OnSlotLaunchWarning: func(slot, mode, message string) {
			events.Publish(httpapi.EventNotificationNew, map[string]any{
				"code":    "SLOT_LAUNCH_SHADOW_FAILED",
				"subject": slot,
				"message": message,
			})
		},
		// LogTail lets a failed load carry the unit's own last log lines (redacted, capped) in its result, so a
		// startup crash can be diagnosed from the API instead of the journal (ADR-0020 F4).
		LogTail:             journalTail,
		WeightEstimateBytes: reg.WeightEstimateBytes,
		CuratedFigures: func(configID int64) []engine.CuratedFigure {
			var out []engine.CuratedFigure
			for _, f := range reg.CuratedMemory(configID) {
				out = append(out, engine.CuratedFigure{Bytes: f.Bytes, Tuple: f.Tuple})
			}
			return out
		},
		// ProfileBytes is the profiled safe-memory footprint (PROFILE track).
		// nil until profileRunner is set below; the closure dereferences at
		// call time (fit checks), by which point the runner exists.
		ProfileBytes: func(mode string) (int64, bool) {
			if profileRunner == nil {
				return 0, false
			}
			return profileRunner.SafeMemoryBytes(mode)
		},
		// ComfyUI's live GPU footprint feeds FitPlan's recovery math when
		// slots alone cannot free enough (S1). Nil-safe: the closure returns
		// 0 whenever the deployment has no configured ComfyUI.
		ComfyUIFootprintBytes: comfyFootprint,
		// UpdateChatTemplateCaps persists each successful load's /props
		// chat_template_caps probe onto its catalog config (T1, per-request
		// thinking control) — feeds the model card and, later, Part 2's
		// reasoning_effort translation layer.
		UpdateChatTemplateCaps: db.Catalog().UpdateConfigChatTemplateCaps,
		// OnGTTDrainTimeout surfaces waitGTTDrain's 20s-timeout warning (the
		// pre-hang/post-unload GTT-lingering signal that fired before both
		// 2026-08-16 device-lost hangs) as a notification:new bus event so
		// smith's anomaly hook can open an investigation. Same event shape
		// the notifications sync publishes — code + subject (a port here is
		// unknown, so subject is the empty string and the message carries the
		// byte figures).
		OnGTTDrainTimeout: func(before, after int64) {
			events.Publish(httpapi.EventNotificationNew, map[string]any{
				"code":    "GTT_DRAIN_TIMEOUT",
				"subject": "",
				"message": fmt.Sprintf("GTT still %d bytes after the 20s drain window (was %d) — lingering GPU allocation or driver delay", after, before),
			})
		},
	})
	if err != nil {
		log.Fatalf("forge: engine: %v", err)
	}

	// ── Maintenance gate (autonomous-remediation plan, Sprint 1) ────────────
	// The quiet-host guarantee a smith-executed repair needs: while a window
	// is active, no model may load/unload/switch/restart. Enforcement is
	// this decorator over engine.Engine — not a change to the frozen
	// Contract 2 interface — so wrapping once here covers every consumer
	// (sched, httpapi, the profiler, smith's Placer) uniformly. wrappedEng
	// replaces the bare eng value in every Deps struct below that grants
	// mutating access; the two purely-informational reads just above
	// (collector's Slots/CurrentMode) stay pointed at the unwrapped eng —
	// observability must not go dark during a repair, and they never
	// mutate anything. See go/internal/maintenance's package doc for the
	// full design (docs/v5-smith.md's autonomous-remediation plan §1).
	maintGate := startMaintenanceGate(ctx, db, events)
	// Short declaration deliberately, not `var wrappedEng engine.Engine =`:
	// the wrapper satisfies engine.Engine plus the extra FitPlan/SlotStates
	// methods sched.Engine and smith.Placer need, and narrowing the static
	// type to engine.Engine here would make those later assignments fail
	// to compile even though the underlying value still has the methods.
	wrappedEng := maintenance.WrapEngine(maintGate, eng)

	// tsAPI answers tailscaled LocalAPI questions over the unix socket — no
	// `tailscale status` subprocess. Shared between the collector's
	// tailscale_node bookmark-health checks and smith's tailscale_peers
	// check (P6 FR8); a single client, one httpClient() lazily built inside
	// it. Found live while wiring P6: TailscaleOnline was never actually
	// passed into collector.Options before this, so every tailscale_node
	// bookmark had silently gotten no server-side health entry since
	// bookmarks shipped — fixed here as a one-line adjacent correction.
	tsAPI := &collector.TailscaleLocalAPI{}

	// routerSrv is declared here so the collector's SlotErrorCount closure
	// can reference it; it's actually constructed later (a0 router section).
	var routerSrv *router.Server

	// ── Collector (single probe loop) ──────────────────────────────────────
	coll := collector.New(collector.Options{
		Cfg:         getCfg,
		Systemd:     dbusConn,
		Slots:       eng,
		CurrentMode: eng.CurrentMode,
		GPU:         gpu,
		Proc:        proc,
		Hostname:    hostname,
		ExtraUnits:  extraUnits(cfg),
		SpecialistDeclaredBytes: func() int64 {
			if f := specialistDeclared.Load(); f != nil {
				return (*f)()
			}
			return 0
		},
		TailscaleOnline: tsAPI.NodeOnline,
		// SlotErrorCount is the a0 router's per-slot 5xx/transport window —
		// the device-lost early-warning (a wedged llama-server 5xxes every
		// request while /health stays green). routerSrv is constructed later
		// in main; the closure is nil-safe so a collector cycle before the
		// router exists simply reports no slot errors.
		SlotErrorCount: func(port int, windowSeconds int64) (int, int64) {
			if routerSrv == nil {
				return 0, 0
			}
			return routerSrv.SlotErrorCount(port, windowSeconds)
		},
		// CompressorTargets is the store-backed {service: port} map: also
		// doubles as the source unitNames() uses to discover the dynamic
		// compressor-<service> units (Phase 9b create/teardown), so their
		// live Active state shows correctly in GET /api/v1/compressor/config
		// without a second discovery path.
		CompressorTargets: func() map[string]int {
			proxies, err := db.Routing().Proxies(context.Background())
			if err != nil {
				return nil
			}
			out := make(map[string]int, len(proxies))
			for _, p := range proxies {
				out[p.Service] = p.Port
			}
			return out
		},
		// CompressorUnits is the store-backed {service: real unit name} map —
		// a service's real unit isn't always "compressor-<service>" (e.g.
		// "aiand"'s real unit is "headroom-external"), found live 2026-07-28
		// when that mismatch made the dashboard show it inactive despite
		// running fine; see collector.Options.CompressorUnits.
		CompressorUnits: func() map[string]string {
			proxies, err := db.Routing().Proxies(context.Background())
			if err != nil {
				return nil
			}
			out := make(map[string]string, len(proxies))
			for _, p := range proxies {
				out[p.Service] = p.Unit
			}
			return out
		},
		OnTokenSample: func(slot, mode string, promptDelta, predictedDelta int64) {
			// Per-slot token deltas become kind="inference" usage events,
			// aggregated by the metrics/usage read paths (handlers.go).
			rec := store.UsageEvent{
				TS:               time.Now(),
				Kind:             "inference",
				Model:            mode,
				Slot:             slot,
				PromptTokens:     promptDelta,
				CompletionTokens: predictedDelta,
			}
			if err := db.Usage().Record(context.Background(), rec); err != nil {
				log.Printf("forge: usage record: %v", err)
			}
		},
		// OnPrefillSample accumulates real, passively-observed prefill
		// throughput per mode (Compressor local-savings prefill sprint,
		// 2026-08-06 — see docs/progress.md's 2026-08-06 entries and
		// migrations/0031_model_prefill_stats.sql). Keyed by the mode's
		// CURRENT fingerprint (profileRunner reuses the same, already-proven
		// staleness concept the PROFILE track uses) so a config change
		// starts a fresh accumulation instead of blending two different
		// performance regimes together. profileRunner is nil until set below
		// (same forward-reference pattern as engine.Deps.ProfileBytes above);
		// skip silently on that narrow startup window rather than block.
		OnPrefillSample: func(slot, mode string, promptTokens int64, promptSeconds float64) {
			recordPrefillSample(db, getCfg, profileRunner, slot, mode, promptTokens, promptSeconds)
		},
		// OnCompressorSample persists per-proxy Compressor counter deltas
		// (cost/savings sprint Phase 3, 2026-07-30) — the docs/v5-plan.md
		// open question 4 blocker (shared-file-contaminated savings counters)
		// is resolved by scraping the volatile per-process counters instead;
		// see internal/collector/llama.go's scrapeCompressorCounters.
		OnCompressorSample: func(service string, s collector.CompressorSample) {
			recordCompressorSample(db, service, s)
		},
		// OnSlotActivity (Sprint K, 2026-08-05): low-latency push of a
		// slot's busy↔idle edge, on top of statusResponse.slot_activity
		// (the poll/reconnect source of truth). Fires at most once per
		// edge, never once per collector cycle — see
		// collector.reportSlotActivity's doc comment. New event name is a
		// Contract 1 amendment (internal/bus/bus.go).
		OnSlotActivity: func(slot string, active bool) {
			events.Publish("slot:activity", map[string]any{"slot": slot, "active": active})
		},
	})
	notifier.set(coll)
	go coll.Run(ctx)

	// ── Compressor proxy provisioner (Phase 2, docs/v5-headroom-topology.md
	// §5, decided option (a); generalized Sprint 3, docs/v5-headroom-
	// replacement.md) ─────────────────────────────────────────────────────
	// Writes per-instance env files under <StateDir>/compress
	// (testuser-writable, EnvironmentFile=/var/lib/forge/compress/%i.env,
	// matching systemd/forge-compress@.service's own path) and
	// starts/stops/restarts the pre-installed `forge-compress@<service>`
	// template-unit instance over the shared D-Bus adapter — no unit-file
	// authoring at runtime (that unit file + its model/tokenizer/onnxruntime
	// artifacts under /opt/forge/compress/ are a one-time manual root
	// install). Sprint 7 dropped the second Provisioner value this used to
	// coexist with (a headroom-ai-shaped "headroom@" template, from before
	// the Sprint 3 cutover) once Sprint 6 confirmed zero live
	// compressor_proxies rows still pointed at it.
	compressorProvisioner := &compressorctl.Provisioner{
		Systemd:        dbusConn,
		EnvDir:         filepath.Join(cfg.Paths.StateDir, "compress"),
		TemplatePrefix: "forge-compress@",
	}

	// Boot-time reconcile: headroom@<service> instances are deliberately NOT
	// systemd-enabled (they're provisioned/torn down dynamically, never
	// meant to blanket-autostart) — so a host reboot or any restart that
	// takes the whole machine down leaves every registered, non-orphaned
	// proxy row in the store pointing at a unit that never came back, while
	// nothing about the row itself changes. a0 has no way to detect this
	// short of actually trying a request (routing.go's resolveBackend trusts
	// a non-orphaned row unconditionally) — found live 2026-07-29 when a
	// host reboot took down all three real proxies (local/deepseek/aiand)
	// and every single a0 request, local and remote alike, failed with a
	// bare transport error until manually restarted (see progress.md).
	// Restart (not Reconcile) is correct here: it doesn't touch the env
	// file, so it's safe for both this package's template units and any
	// legacy hand-created one — matches POST /api/v1/compressor/restart's own
	// "safe for any unit" contract. Best-effort: a failure here must not
	// block daemon startup.
	reconcileCompressorsOnBoot(ctx, db.Routing(), compressorProvisioner)
	seedCompressorRetired(ctx, db.Settings())

	// ── Profile runner (PROFILE track — docs/v5-profiling-benchmarks.md) ──
	// Created after the engine + collector exist. The engine's ProfileMB
	// closure (above) captures this variable by reference and resolves at
	// call time.
	profileRunner = profile.New(profile.Deps{
		Engine:    wrappedEng,
		Llama:     collector.NewLlamaClient(nil),
		Profiles:  db.ModelProfiles(),
		Publish:   events,
		Cfg:       getCfg,
		Logf:      log.Printf,
		Probe:     func(ctx context.Context) { coll.ProbeNow(ctx) },
		Snapshots: coll,
	})

	// ── Scheduler (in-process, one mutex) ──────────────────────────────────
	scheduler, err := sched.New(sched.Deps{
		PooledConfig:       isPooledConfig,
		Engine:             wrappedEng,
		Source:             coll,
		Sched:              db.Sched(),
		Settings:           db.Settings(),
		MaintenanceBlocked: maintGate.Blocked,
		Fallback: sched.Config{
			IdleUnloadS:            cfg.Scheduler.IdleUnloadS,
			SmallJobTokenThreshold: cfg.Scheduler.SmallJobTokenThreshold,
			PriorityJumpCap:        cfg.Scheduler.PriorityJumpCap,
			ReservationSoonMin:     cfg.Scheduler.ReservationSoonMin,
		},
		// ComfyUI seam (S1): gates (queue-idle probe against smith.comfyui.url)
		// and executes (graceful unit stop) the eviction FitPlan proposes when
		// loaded slots alone cannot free enough memory. The operator opt-out
		// lives in the scheduler config (comfyui_evictable, default on).
		ComfyUI: newComfyUISeam(comfyUnit, comfyURL, dbusConn),
		// RouteSync (a0 model-label sync on load) is left nil: the V5 router
		// catalog reads live collector snapshots, so it does not need the V4
		// push-based _sync_router_route.
	})
	if err != nil {
		log.Fatalf("forge: scheduler: %v", err)
	}

	// providersSvc backs both the dashboard's providers API and smith's
	// handoff candidate health probe below — one instance, one 5-minute
	// TTL cache (providers.New's default), not two independently-caching
	// copies of the same live provider health data.
	providersSvc := providers.New(providers.Deps{
		Catalog:   db.Providers(),
		Offerings: db.Catalog(),
	})

	// webSvc backs smith P5's web research (docs/v5-smith.md §4.8): search
	// via searxng, fetch via firecrawl with a `direct` (guarded, no-API-key)
	// terminus. Constructed before smith so it can be wired into Deps.Web —
	// never Deps.HTTPClient, whose blanket 3s timeout is sized for loopback
	// probes only.
	webSvc := smithweb.New(smithweb.Deps{
		Cache:     smithweb.NewSQLCache(db),
		Settings:  db.Settings(),
		UserAgent: "forge-smith/" + version,
		Logf:      log.Printf,
	})

	// ── Per-slot consumer attribution registry ─────────────────────────────
	// One shared instance for the whole process: the a0 router Marks each
	// foundry_slot attempt with the caller's key-name-derived label, smith
	// Marks its brain slot as "SMITH", and httpapi reads fresh entries back
	// onto /api/v1/status as slot_consumers.
	activityReg := activity.New()

	// Observe-only a0 context-creation ledger (WS-N1): one instance shared by
	// the router (hook) and httpapi (read side). Stops with ctx.
	a0InFlight := inflight.New()
	ctxLedger := ctxledger.New(ctxledger.Config{Sink: db.ContextCreation(), Settings: db.Settings()})
	ctxLedger.Start(ctx)

	// ── Smith (self-diagnosis agent — docs/v5-smith.md) ──────────────────
	// Created after engine + collector + scheduler exist. P1-P2 are the
	// deterministic tier + action model: coded checks, findings persistence,
	// quick/deep sweeps, the SelfContext behind GET /api/v1/smith/status, and
	// the propose/approve/execute action model. P3 adds the reasoning tier —
	// smith calls a0 itself (over loopback, exempted in router.checkAuth; see
	// that function's doc comment) rather than through any dep here — plus
	// ProviderHealth for the self-eviction handoff's remote-probe branch
	// (handoff.go's probeHandoffCandidates). Load/Unload are never called
	// directly; they go through the proposal approval path.
	//
	// ── HF model acquisition (search/preflight/download/registration) ──
	// hfClient's own HTTP client carries NO blanket Timeout (unlike
	// smith.Deps.HTTPClient's 3s loopback-probe client, constructed
	// below) — hfdownload's worker bounds each download attempt itself
	// via context (fileAttemptTimeout, 2h), and the metadata calls
	// (search/tree/card) are always wrapped in a short context by their
	// own callers instead.
	hfClient := &hf.Client{HTTP: &http.Client{}, Token: hf.TokenFunc(db.Settings())}
	// checkBuildBinary is the one build guard (internal/buildguard, ADR-0020 F8) over catalog.build_roots: the builds
	// API, smith's build_refresh and hfdownload's load-mode probe all vet a binary path through it. A setting that
	// exists but cannot be read is an error, not a reason to fall back to the (possibly wider) defaults.
	checkBuildBinary := func(ctx context.Context, path string) (string, error) {
		var configured []string
		raw, err := settings.Get(ctx, "catalog.build_roots")
		switch {
		case err == nil:
			_ = json.Unmarshal(raw, &configured)
		case errors.Is(err, store.ErrNotFound):
		default:
			return "", err
		}
		return buildguard.CheckBinary(path, buildguard.Roots(configured)), nil
	}
	// checkBuildBinaryPath is that guard in the shape a caller that cannot fail takes: the reason a path may not run,
	// or "" when it may. A settings read that fails vetoes the probe rather than defaulting open.
	checkBuildBinaryPath := func(ctx context.Context, path string) string {
		reason, err := checkBuildBinary(ctx, path)
		if err != nil {
			return "catalog.build_roots could not be read: " + err.Error()
		}
		return reason
	}
	hfDownloadSvc := hfdownload.New(hfdownload.Deps{
		Store: db, HF: hfClient, Cfg: getCfg, Source: coll, Publish: events, Logf: log.Printf,
		OnCatalogChange: mergedProvider.Invalidate,
		VetBinaryPath:   checkBuildBinaryPath,
	})
	// Boot reconcile (mirrors the compressor-proxy pattern elsewhere in
	// this file): any job the store still shows "running" can only mean
	// the previous process died mid-download — flip it to paused before
	// anything else touches the queue, rather than leave it claiming a
	// worker is fetching for it when none is.
	if err := hfDownloadSvc.BootReconcile(ctx); err != nil {
		log.Printf("forge: hfdownload boot reconcile: %v", err)
	}

	// specialistProblems is set once the specialist pool exists (it is built after Smith); the specialist_pool check reads it through this pointer.
	var specialistProblems atomic.Pointer[func(context.Context) []smith.SpecialistProblem]
	// The host probes smith's checks use (git, journalctl, binary --version): one command runner, with the smith binary-path rule injected.
	hostProbes := hostprobe.Probes{Run: hostprobe.ExecRunner{}, BinaryPathAllowed: smith.BinaryPathAllowed}
	var speechReport atomic.Pointer[func(context.Context) (speechsetup.Report, error)]
	smithAgent := smith.New(smith.Deps{
		SpeechReport: func(ctx context.Context) (speechsetup.Report, error) {
			f := speechReport.Load()
			if f == nil {
				return speechsetup.Report{}, errors.New("the speech engine pool is not up yet")
			}
			return (*f)(ctx)
		},
		SpecialistProblems: func(ctx context.Context) []smith.SpecialistProblem {
			f := specialistProblems.Load()
			if f == nil {
				return nil
			}
			return (*f)(ctx)
		},
		Store:   db,
		Catalog: db.Catalog(),
		// build_refresh's only Config write: repoint at another build, then clear the merged-config cache like the API's own writes do.
		ConfigBuilds: store.ConfigWrites{Cat: db.Catalog(), Changed: mergedProvider.Invalidate},
		// build_refresh's candidate registration and cleanup retire go through the same guards as the API (the build guard over catalog.build_roots).
		Builds: store.BuildWrites{Cat: db.Catalog(), Changed: mergedProvider.Invalidate,
			CheckBinary: checkBuildBinary},
		Registry:    reg,
		Settings:    settingsvc.Validating(db.Settings()),
		Sched:       scheduler,
		Engine:      wrappedEng,
		Placer:      wrappedEng,
		Activity:    activityReg,
		RestartUnit: dbusConn.Restart,
		Source:      coll,
		Publisher:   events,
		Subscriber:  events,
		Audit:       db.Audit(),
		Cfg:         getCfg,
		Logf:        log.Printf,
		Web:         webSvc,
		HF:          hfClient,
		HFDownload:  hfDownloadSvc,
		TailscalePeers: func(ctx context.Context) ([]collector.Peer, bool) {
			return tsAPI.Peers(ctx)
		},
		BinaryVersion: hostProbes.BinaryVersion,
		// The same build-binary guard the builds API applies (internal/buildguard, ADR-0020 F8).
		CheckBuildBinary: checkBuildBinaryPath,
		// GitAhead is binary_versions' upstream-drift probe: how many
		// commits the source tree is ahead of its configured upstream ref.
		// Read-only git rev-list --count; bounded; no shell.
		GitAhead: hostProbes.GitAhead,
		// GitBehindLog fetches the commit subjects for the same range
		// GitAhead counts — feeds binary_versions' watchlist match.
		GitBehindLog: hostProbes.GitBehindLog,
		// GitLsRemote is the upstream-NIGHTLY tracking probe (P3smith):
		// resolves a fork's upstream HEAD sha (read-only git ls-remote,
		// no shell). The URL arrives from operator settings/DB data, so the
		// implementation re-validates it before exec'ing anything.
		GitLsRemote: hostProbes.GitLsRemote,
		// JournalErrors/KernelJournal are smith's device-lost journal seams
		// (gpu_device_lost check + auto-recovery). The unit list is captured
		// from the live slot config (read via cfgHolder at call time so
		// SIGHUP reloads are honored); explicit -u flags, not a journalctl
		// glob, because a glob only matches currently-active units and would
		// drop a just-unloaded slot's persisted ErrorDeviceLost history.
		JournalErrors: func(ctx context.Context, n int, since time.Time) ([]string, error) {
			var units []string
			if cfg := cfgHolder.Load(); cfg != nil {
				for _, slot := range cfg.Slots {
					units = append(units, slot.Unit)
				}
			}
			return hostProbes.SlotJournalErrors(ctx, n, units, since)
		},
		KernelJournal:       hostProbes.KernelJournal,
		ComfyUI:             &dynamicComfyUIClient{settings: db.Settings()},
		DeleteFile:          smithDeleteFile,
		InstallLauncherFile: smithInstallLauncherFile,
		RunStep:             smithRunStep,
		Maintenance:         maintGate,
		BlockedWorkPath:     smithBlockedWorkPath(ctx, db.Settings()),
		ProviderHealth: func(ctx context.Context, provider string) (string, error) {
			all, err := providersSvc.List(ctx)
			if err != nil {
				return "", err
			}
			for _, p := range all {
				if p.Name == provider {
					return p.Health.State, nil
				}
			}
			return "", fmt.Errorf("provider %q not found", provider)
		},
		CompressorProvisioner: compressorProvisioner,
	})
	// Periodic sweeps (smith.schedule — quick hourly, deep daily by default,
	// both disable-able). Sweeps are pure reads + findings persistence; the
	// loop re-reads the setting each tick so edits take effect without a
	// restart.
	smithAgent.Start(ctx)
	defer smithAgent.Stop()

	// ── Scheduler jobs runner (P3 — cron-style forced loads) ──────────────
	// Fires enabled scheduler_jobs rows whose next_run_at has passed by
	// calling scheduler.EnsureLoaded with requested_by="cron:<name>" — the
	// same entry point a0/MCP/dashboard use, so placement, reservations,
	// and queue attribution all behave identically. AdvanceMissed skips
	// (never replays) runs missed while the daemon was down; Run exits on
	// ctx cancellation for graceful shutdown.
	jobsRunner, err := sched.NewJobsRunner(sched.JobsDeps{
		Ensure: scheduler.EnsureLoaded,
		Jobs:   db.SchedulerJobs(),
		Logf:   log.Printf,
	})
	if err != nil {
		log.Fatalf("forge: scheduler jobs: %v", err)
	}
	jobsRunner.AdvanceMissed(ctx)
	go jobsRunner.Run(ctx)

	// The overnight profiling run idles until an operator arms it.
	profileNight := newProfileNight(db, profileRunner, wrappedEng, coll, maintGate)
	go profileNight.Loop(ctx)

	// ── Auth ───────────────────────────────────────────────────────────────
	auth := authz.New(db)

	// ── Auth v2 (Sprint 0-AUTH Phase A/B/C) ───────────────────────────────
	networkIdentity := newNetworkIdentity(ctx, db.Settings())

	policyStore, webAuthnSvc, recoverySvc := newAuthV2(ctx, db)

	// ── FX / billing-currency cache (Sprint 0 §0.2) ──────────────────────────
	// Daemon-side: fetches live rates (default ECB/Frankfurter, overridable
	// via billing.fx_source_url), caches them in fx_rates, and serves the
	// usage handler's display-currency conversions. Never crashes the daemon
	// on a fetch failure — the last cached rate stays served and ages stale.
	fxCache := fx.New(db.SQL(), db.Settings())
	fxCache.Start(ctx)
	defer fxCache.Close()

	// ── HTTP API + embedded PWA (dashboard) ────────────────────────────────
	// ── Specialist slot pool (ADR-0022) ────────────────────────────────────
	// Always built, so the first slot added to a running daemon takes effect at once; with no slots it answers ErrNoMatch everywhere and the host
	// behaves as one without a pool. The supervisor adopts what is already running, then starts always-on members in the background (smallest
	// first); it never delays serving.
	specialistPool, err := specialist.New(specialist.Deps{
		Cfg:          getCfg,
		Sys:          dbusConn,
		Store:        db.Specialist(),
		EnvDir:       specialistEnvDir,
		Catalog:      catalogResolver{cfg: getCfg, estimate: reg.WeightEstimateBytes},
		SlotLaunches: db.SlotLaunches(),
		ModelRoots:   func() []string { return specialist.ModelRoots(getCfg().Paths.ModelsDir, specialist.EngineRoot) },
		// The same live memory budget the LLM fit gate uses; on a probe error the gate is open and the launcher's own failure handling remains.
		FreeBytes: func() int64 {
			b, err := eng.MemoryBudget()
			if err != nil {
				log.Printf("forge: specialist fit gate: memory budget unreadable (%v); not gating this load", err)
				return math.MaxInt64 / 2
			}
			return b.FreeBytes
		},
		IdleUnload: func() time.Duration { return time.Duration(getCfg().Scheduler.IdleUnloadS) * time.Second },
		// The default engine per kind of speech (plan P1); read per request, so a change in Voice & Speech applies at once.
		SpeechDefaults: func(ctx context.Context) (map[string]string, error) {
			raw, err := db.Settings().Get(ctx, specialist.SpeechDefaultsKey)
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return specialist.ParseSpeechDefaults(raw)
		},
		Logf: log.Printf,
		OnFailed: func(slot, occupant string, attempts int, err error) {
			events.Publish(httpapi.EventNotificationNew, map[string]any{
				"code":    "SPECIALIST_FAILED",
				"subject": slot,
				"message": fmt.Sprintf("specialist %s in %s failed to start %d times in a row: %v", occupant, slot, attempts, err),
			})
		},
	})
	if err != nil {
		log.Fatalf("forge: specialist pool: %v", err)
	}
	reservedFn := specialistPool.ReservedBytes
	specialistReserved.Store(&reservedFn)
	declaredFn := specialistPool.DeclaredBytes
	specialistDeclared.Store(&declaredFn)
	pooledFn := specialistPool.PooledConfig
	pooledConfig.Store(&pooledFn)
	problemsFn := func(ctx context.Context) []smith.SpecialistProblem {
		var out []smith.SpecialistProblem
		for _, p := range specialistPool.AlwaysOnProblems(ctx) {
			out = append(out, smith.SpecialistProblem{Slot: p.Slot, Occupant: p.Occupant, State: p.State, Err: p.Err, Attempts: p.Attempts})
		}
		return out
	}
	specialistProblems.Store(&problemsFn)
	speechFn := func(ctx context.Context) (speechsetup.Report, error) {
		engines, err := db.Specialist().ListOccupants(ctx)
		if err != nil {
			return speechsetup.Report{}, err
		}
		var defaults map[string]string
		raw, err := db.Settings().Get(ctx, specialist.SpeechDefaultsKey)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return speechsetup.Report{}, err
		}
		if err == nil {
			if defaults, err = specialist.ParseSpeechDefaults(raw); err != nil {
				return speechsetup.Report{}, err
			}
		}
		return speechsetup.Inspect(specialist.EngineRoot, engines, defaults, specialistPool.Snapshot(ctx)), nil
	}
	speechReport.Store(&speechFn)
	go specialistPool.Run(ctx)
	log.Printf("forge: specialist pool supervising %d slot(s)", len(cfg.SpecialistSlots)) //nolint:gosec // G706: the only value logged is a count

	api := httpapi.New(httpapi.Deps{
		Specialists:           specialistPool,
		SpecialistStore:       db.Specialist(),
		SpecialistWriter:      db.Specialist(),
		SpecialistCatalog:     catalogResolver{cfg: getCfg, estimate: reg.WeightEstimateBytes},
		Snapshots:             coll,
		Engine:                wrappedEng,
		Sched:                 scheduler,
		Activity:              activityReg,
		Auth:                  auth,
		AuthSetup:             auth,
		Events:                events,
		Publish:               events,
		Config:                getCfg,
		Hostname:              hostname,
		Version:               version,
		Usage:                 db.Usage(),
		Routing:               db.Routing(),
		CompressorProvisioner: compressorProvisioner,
		Settings:              settingsvc.Validating(db.Settings()),
		Audit:                 db.Audit(),
		Sessions:              db.Sessions(),
		Registry:              reg,
		FX:                    fxCache,
		Metrics:               db.Metrics(),
		Compressors:           db.Compressors(),
		SlotStateStore:        db.Sched(),
		Providers:             providersSvc,
		// Sprint 0-AUTH (Phase A): wire the identity provider, policy store,
		// step-up verifier, key manager, and the new store interfaces.
		NetworkIdentity:    networkIdentity,
		IdentityLinks:      db.IdentityLinks(),
		TOTPStore:          db.TOTP(),
		PolicyStore:        policyStore,
		StepUpTTL:          authz.DefaultStepUpTTL,
		NetworkDefaultRole: authz.RoleViewer,
		StepUpVerifier:     auth,
		KeyManager:         auth,
		Keys:               db.Keys(),
		WebAuthnService:    webAuthnSvc,
		RecoveryService:    recoverySvc,
		Profiles:           profileRunner,
		ProfileNight:       profileNight,
		Prober:             func(ctx context.Context) { coll.ProbeNow(ctx) },
		Catalog:            db.Catalog(),
		Llama:              collector.NewLlamaClient(nil),
		PrefillStats:       db.PrefillStats(),
		InvalidateConfig:   mergedProvider.Invalidate,
		Favorites:          db.Favorites(),
		CtxLedger:          ctxLedger,
		InFlight:           a0InFlight,
		Smith:              smithAgent,
		HFDownload:         hfDownloadSvc,
		HFClient:           hfClient,
		Maintenance:        maintGate,
		SlotLog: func(ctx context.Context, unit string, maxLines int) string {
			return journalTail(ctx, unit, time.Time{}, maxLines)
		},
		ReloadConfig: func() {
			reloadConfigFromStore(ctx, db, &cfgHolder)
			mergedProvider.Invalidate()
		},
		// Sprint 12 (was H) Phase 3: the Danger Zone's restart action. Uses
		// the same shared D-Bus connection as everything else in this file
		// — Restart appends ".service" itself, so pass the bare unit name.
		SystemRestart: func(ctx context.Context) error {
			return dbusConn.Restart(ctx, "forge-daemon")
		},
	})
	defer api.Close()

	// Sprint 12 (was H) Phase 2: a fresh process start has, by definition,
	// already picked up every stored infra.* value (they were all just read
	// via config.LoadFromStore above and router.LoadFromStore below) — so
	// any "restart required" signal left over from a previous life is
	// stale. Clear it before serving any request rather than leaving a
	// then-wrong banner up until the next settings save.
	api.ClearRestartRequired(ctx)

	routerCfg, err := router.LoadFromStore(ctx, db)
	if err != nil {
		log.Fatalf("forge: load router config from store: %v", err)
	}
	log.Printf("forge: router config loaded from store (listen_port=%d)", routerCfg.ListenPort)

	// ── a0 router ──────────────────────────────────────────────────────────
	routerSrv = router.NewWithDeps(router.Deps{
		Specialists:  specialistPool,
		Cfg:          routerCfg,
		Auth:         auth,
		Sched:        scheduler,
		Routing:      db.Routing(),
		Settings:     db.Settings(),
		Audit:        db.Audit(),
		Slots:        slotPorts(cfg),
		StoreCatalog: db.Catalog(),
		Usage:        db.Usage(),
		Activity:     activityReg,
		Registry:     reg,
		CtxLedger:    ctxLedger,
		InFlight:     a0InFlight,
		SlotKind:     slotKindLookup(db.SlotLaunches(), time.Now),
	})

	// ── MCP server ─────────────────────────────────────────────────────────
	mcpSrv := mcp.NewWithDeps(mcp.Deps{
		Sched:  scheduler,
		Engine: wrappedEng,
		Auth:   auth,
		// Audit roadmap R3 (docs/v5-mcp-audit.md): MCP fleet mutations are
		// audit-logged with the bearer key name as actor.
		Audit: db.Audit(),
		// Audit roadmap R2: list_models reads the same catalog seam a0's
		// BuildModelsResponse uses, so the two listings cannot drift.
		Catalog: db.Catalog(),
	})

	runServers(ctx, cancel, cfg, db, &cfgHolder, mergedProvider, version, api, routerSrv, mcpSrv)
}

// runServers starts the three Contract-1 listeners (dashboard :5000, a0
// :8085, MCP :8095) and blocks in the signal/shutdown loop until the
// process is asked to exit. SIGINT/SIGTERM shut down gracefully; SIGHUP
// re-reads the store-backed config into cfgHolder (engine + collector pick
// it up on their next cycle) and invalidates the merged-config cache — no
// file to re-parse post-cutover (TOML decommission Phase 3,
// docs/v5-toml-decommission.md §4), so listener/API/router changes still
// need a restart.
func runServers(ctx context.Context, cancel context.CancelFunc, cfg *config.Config, db *store.DB, cfgHolder *atomic.Pointer[config.Config], mergedProvider *mergedConfigProvider, version string, api *httpapi.Server, routerSrv *router.Server, mcpSrv *mcp.Server) {
	dashSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	a0Srv := &http.Server{
		Addr:              cfg.Server.RouterListen,
		Handler:           routerSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	mcpHTTP := &http.Server{
		Addr:              cfg.Server.MCPListen,
		Handler:           mcpSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 3)
	go func() { errCh <- listenAndServe(dashSrv, "dashboard", cfg.Server.Listen) }()
	go func() { errCh <- listenAndServe(a0Srv, "a0", cfg.Server.RouterListen) }()
	go func() { errCh <- listenAndServe(mcpHTTP, "mcp", cfg.Server.MCPListen) }()

	log.Printf("forge %s listening: dashboard=%s a0=%s mcp=%s",
		version, cfg.Server.Listen, cfg.Server.RouterListen, cfg.Server.MCPListen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				reloadConfigFromStore(ctx, db, cfgHolder)
				mergedProvider.Invalidate()
				continue
			}
			log.Printf("forge: %s received, shutting down", sig)
			cancel() // stop the collector loop
			shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer shutCancel()
			_ = dashSrv.Shutdown(shutCtx)
			_ = a0Srv.Shutdown(shutCtx)
			_ = mcpHTTP.Shutdown(shutCtx)
			return
		case err := <-errCh:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("forge: %v", err)
			}
		}
	}
}

// startMaintenanceGate constructs the quiet-host gate (autonomous-remediation
// plan, Sprint 1) and runs its boot reconcile + periodic ticker. A window is
// only unconditionally orphaned if nothing durable claims it. Sprint 2's
// procedure runner (go/internal/smith/procedure.go) can legitimately hold a
// window across a restart — a live (running or awaiting_checkpoint)
// smith_procedure_runs row carrying this exact lease means
// resumeProcedureRuns (called from smithAgent.Start, constructed later in
// main) is about to pick the run back up, so force-exiting the window here
// would pull it out from under that resume. Queried directly against db
// rather than through smithAgent (not constructed yet at this point in
// boot) — smith_procedure_runs is a plain table, no smith-package call
// needed to check it.
func startMaintenanceGate(ctx context.Context, db *store.DB, events *bus.Bus) *maintenance.Gate {
	maintGate := maintenance.New(db.Settings(), events, time.Now, log.Printf)
	if !hasLiveMaintenanceLease(ctx, db, maintGate.Status().LeaseID) {
		maintGate.ReconcileOnBoot(func(reason string) {
			if err := db.Audit().Write(ctx, store.AuditEntry{
				Actor: "system", Action: "maintenance_boot_reconcile", Detail: reason,
			}); err != nil {
				log.Printf("forge: maintenance boot-reconcile audit write: %v", err)
			}
		})
	} else {
		log.Printf("forge: maintenance window survives boot reconcile — a live procedure run holds its lease, resuming shortly")
	}
	maintGate.StartTicker(ctx, time.Minute)
	return maintGate
}

// switchNotifier is a settable engine.SwitchNotifier that forwards to the
// collector once it exists — breaking the engine↔collector build cycle. The
// collector pointer is written once (before the probe loop starts) and read
// from the engine's op goroutines, so an atomic pointer keeps it race-free.
type switchNotifier struct {
	c atomic.Pointer[collector.Collector]
}

func (n *switchNotifier) set(c *collector.Collector) { n.c.Store(c) }

func (n *switchNotifier) NotifySwitchComplete() {
	if c := n.c.Load(); c != nil {
		c.NotifySwitchComplete()
	}
}

// extraUnits lists the always-on auxiliary systemd units the collector probes
// on top of the config-derived slot/mode units: the aux services in [ports]
// (embedding, stt) follow the forge-<key> naming, plus the TTS unit.
func extraUnits(cfg *config.Config) []string {
	var units []string
	for key := range cfg.Ports {
		units = append(units, "forge-"+key)
	}
	if cfg.Server.TTSUnit != "" {
		units = append(units, cfg.Server.TTSUnit)
	}
	return units
}

// comfyUIHelpers builds the two lazily-resolving closures ComfyUI eviction
// (S1) and the a0/smith seams need: unit resolves the operator-configured
// systemd unit name (smith.comfyui.unit), footprint reads that unit's live
// GPU memory footprint via its PID. Both live in the smith.comfyui.*
// settings keys (deployment data — provisioned per install via
// import-local, never compiled in) and resolve lazily so an import-local
// edit takes effect without a restart. Empty unit/URL = the deployment has
// no (configured) ComfyUI and eviction gracefully never proposes it.
func comfyUIHelpers(ctx context.Context, settings store.Settings, sys *engine.DBus, proc collector.Proc) (unit func() string, footprint func() int64, url func() string) {
	settingString := func(key string) string {
		raw, err := settings.Get(ctx, key)
		if err != nil || len(raw) == 0 {
			return ""
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		return v
	}
	unit = func() string { return settingString(smith.SettingComfyUIUnit) }
	url = func() string { return settingString(smith.SettingComfyUIURL) }
	footprint = func() int64 {
		u := unit()
		if u == "" {
			return 0
		}
		probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pid, err := sys.MainPID(probeCtx, u)
		if err != nil || pid == 0 {
			return 0
		}
		return int64(proc.GPUMemoryBytes(int(pid)))
	}
	return unit, footprint, url
}

// newComfyUISeam builds the scheduler's ComfyUI eviction seam (S1): Idle
// gates on a queue-idle probe against smith.comfyui.url, Stop executes a
// graceful unit stop. The operator opt-out lives in the scheduler config
// (comfyui_evictable, default on), not here.
func newComfyUISeam(unit func() string, comfyURL func() string, sys *engine.DBus) *sched.ComfyUISeam {
	return &sched.ComfyUISeam{
		Unit: unit,
		Idle: func(ctx context.Context) (bool, string) {
			url := comfyURL()
			if url == "" {
				return false, "no ComfyUI URL configured"
			}
			q, err := comfyui.NewHTTPClient(url).Queue(ctx)
			if err != nil {
				return false, "queue probe failed (" + err.Error() + ")"
			}
			if len(q.Running) > 0 || len(q.Pending) > 0 {
				return false, "a workflow is queued or running"
			}
			return true, ""
		},
		Stop: func(ctx context.Context) error {
			u := unit()
			if u == "" {
				return fmt.Errorf("no ComfyUI unit configured")
			}
			return sys.Stop(ctx, u)
		},
	}
}

// newAuthV2 constructs Sprint 0-AUTH's Phase B/C pieces beyond
// networkIdentity: the per-page+sub-area assurance PolicyStore (settings KV
// under "auth.policy", JSON {resourceKey: minFactor}, default seed §3.4
// written on first access), the WebAuthn passkey service (the adapters
// bridge store types to the authz interface seams — authz doesn't import
// store to avoid a cycle), and the recovery-code service (128-bit random
// look-up secrets hashed with HMAC-SHA256 keyed by a server pepper — a fast
// approved one-way function, NOT a password KDF; rationale in
// internal/authz/recovery.go and docs/v5-sprint0-auth-design.md). Fatal on a
// recovery-pepper load failure, same as every other boot-time
// log.Fatalf in main.
func newAuthV2(ctx context.Context, db *store.DB) (*authz.PolicyStore, *authz.WebAuthnService, *authz.RecoveryService) {
	policyStore := authz.NewPolicyStore(authz.SettingsAdapter{
		GetFn: func(ctx context.Context, key string) ([]byte, error) {
			return db.Settings().Get(ctx, key)
		},
		SetFn: func(ctx context.Context, key string, value []byte) error {
			return db.Settings().Set(ctx, key, value)
		},
	})

	webAuthnSvc := authz.NewWebAuthnService(
		httpapi.NewWebAuthnCredentialStoreAdapter(db.WebAuthnCredentials()),
		httpapi.NewWebAuthnUserStoreAdapter(db.Users()),
	)

	recoveryPepper, err := loadRecoveryPepper(ctx, db)
	if err != nil {
		log.Fatalf("forge: recovery pepper: %v", err)
	}
	recoverySvc := authz.NewRecoveryService(
		httpapi.NewRecoveryStoreAdapter(db.RecoveryCodes()),
		recoveryPepper,
	)

	return policyStore, webAuthnSvc, recoverySvc
}

// newNetworkIdentity resolves the trusted-network-principal provider
// (Sprint 0-AUTH Phase A/B/C). The provider is selected by the
// "auth.network_provider" setting ("tailscale" | "forward_auth_header" |
// "none"). Default is "tailscale" for tailnet-first deployments; public
// deployments set it to "forward_auth_header" or "none" (Phase C).
func newNetworkIdentity(ctx context.Context, settings store.Settings) authz.NetworkIdentityProvider {
	networkIdentity := authz.NetworkIdentityProvider(authz.NoNetworkIdentity{})
	if raw, err := settings.Get(ctx, "auth.network_provider"); err == nil {
		var providerName string
		if json.Unmarshal(raw, &providerName) == nil {
			switch providerName {
			case "tailscale":
				networkIdentity = &authz.TailscaleIdentityProvider{
					Client: &authz.TailscaleLocalWhoIsClient{},
				}
			case "forward_auth_header":
				// Phase C: read trusted CIDRs + header name from settings.
				headerName := "X-Auth-Request-User"
				trustedCIDRStr := "127.0.0.0/8"
				if raw, err := settings.Get(ctx, "auth.provider.forward_auth_header.header_name"); err == nil {
					var v string
					if json.Unmarshal(raw, &v) == nil && v != "" {
						headerName = v
					}
				}
				if raw, err := settings.Get(ctx, "auth.provider.forward_auth_header.trusted_cidrs"); err == nil {
					var v string
					if json.Unmarshal(raw, &v) == nil && v != "" {
						trustedCIDRStr = v
					}
				}
				networkIdentity = &authz.ForwardAuthHeaderProvider{
					HeaderName:   headerName,
					TrustedCIDRs: authz.ParseCIDRs(trustedCIDRStr),
				}
			case "none":
				// already the default
			}
		}
	} else {
		// No setting → default to tailscale for tailnet-first deployments.
		networkIdentity = &authz.TailscaleIdentityProvider{
			Client: &authz.TailscaleLocalWhoIsClient{},
		}
	}
	return networkIdentity
}

// reconcileCompressorsOnBoot restarts every non-orphaned compressor proxy
// row's unit — see its call site in main for the full incident writeup on
// why this exists. hp may be nil (a fresh install with no routing store
// configured yet); a nil check guards that, same as the original inline
// form.
func reconcileCompressorsOnBoot(ctx context.Context, hp store.Routing, prov *compressorctl.Provisioner) {
	if hp == nil {
		return
	}
	if compressorctl.Retired() {
		// ADR-0017 / contract C7: the compressor is retired from the request path; a boot must not
		// (re)start any forge-compress unit.
		log.Printf("forge: compressor retired; skipping boot reconcile")
		return
	}
	bootCtx, bootCancel := context.WithTimeout(ctx, 30*time.Second)
	proxies, err := hp.Proxies(bootCtx)
	bootCancel()
	if err != nil {
		log.Printf("forge: compressor boot reconcile: list proxies: %v", err)
	}
	for _, row := range proxies {
		if !row.OrphanedAt.IsZero() {
			continue
		}
		restartCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := prov.Restart(restartCtx, row.Unit)
		cancel()
		if err != nil {
			log.Printf("forge: compressor boot reconcile: restart %s (%s): %v", row.Service, row.Unit, err)
		} else {
			log.Printf("forge: compressor boot reconcile: %s (%s) restarted", row.Service, row.Unit)
		}
	}
}

// recordPrefillSample is collector.Options.OnPrefillSample's production
// body: accumulates real, passively-observed prefill throughput per mode
// (Compressor local-savings prefill sprint, 2026-08-06 — see
// docs/progress.md's 2026-08-06 entries and
// migrations/0031_model_prefill_stats.sql). Keyed by the mode's CURRENT
// fingerprint (profileRunner reuses the same, already-proven staleness
// concept the PROFILE track uses) so a config change starts a fresh
// accumulation instead of blending two different performance regimes
// together. profileRunner is nil until main's profile.New call runs (same
// forward-reference pattern as engine.Deps.ProfileBytes) — the caller
// re-reads the variable at call time, so this degrades silently on that
// narrow startup window rather than block.
func recordPrefillSample(db *store.DB, getCfg func() *config.Config, profileRunner *profile.Runner, slot, mode string, promptTokens int64, promptSeconds float64) {
	if profileRunner == nil {
		return
	}
	fp, err := profileRunner.Fingerprint(mode)
	if err != nil {
		log.Printf("forge: prefill sample: fingerprint %q: %v", mode, err)
		return
	}
	m, ok := getCfg().Modes[mode]
	if !ok || m.ConfigID == 0 {
		log.Printf("forge: prefill sample: mode %q has no catalog config_id", mode)
		return
	}
	configID := m.ConfigID
	if err := db.PrefillStats().AddObservation(context.Background(), configID, fp, promptTokens, promptSeconds); err != nil {
		log.Printf("forge: prefill sample: %v", err)
	}
}

// recordCompressorSample is collector.Options.OnCompressorSample's
// production body: persists per-proxy Compressor counter deltas (cost/
// savings sprint Phase 3, 2026-07-30) — the docs/v5-plan.md open question 4
// blocker (shared-file-contaminated savings counters) is resolved by
// scraping the volatile per-process counters instead; see
// internal/collector/llama.go's scrapeCompressorCounters.
func recordCompressorSample(db *store.DB, service string, s collector.CompressorSample) {
	ctx := context.Background()
	now := time.Now()
	proxies, err := db.Routing().Proxies(ctx)
	if err != nil {
		log.Printf("forge: compressor sample: proxies read: %v", err)
		return
	}
	var proxyID int64
	found := false
	for _, p := range proxies {
		if p.Service == service {
			proxyID, found = p.ID, true
			break
		}
	}
	if !found {
		log.Printf("forge: compressor sample: unknown proxy service %q", service)
		return
	}
	var cacheReadSum, uncachedSum int64
	for _, d := range s.CacheReadTokensDelta {
		cacheReadSum += d
	}
	for _, d := range s.UncachedTokensDelta {
		uncachedSum += d
	}
	row := store.CompressorSavingsSampleRow{
		TS: now, ProxyID: proxyID,
		TokensIn: s.TokensInDelta, TokensOut: s.TokensOutDelta, TokensSaved: s.TokensSavedDelta,
		Requests: s.RequestsDelta, RequestsCached: s.RequestsCachedDelta,
		RequestsFailed: s.RequestsFailedDelta, RequestsRateLimited: s.RequestsRateLimitedDelta,
		RequestsTimeout: s.RequestsTimeoutDelta, RequestsCanceled: s.RequestsCanceledDelta,
		FailOpenTotal:   s.FailOpenDelta,
		CacheReadTokens: cacheReadSum, UncachedTokens: uncachedSum,
		CacheBusts: s.CacheBustsDelta, CacheBustTokensLost: s.CacheBustTokensLostDelta,
		TTFBCount: s.TTFBCountDelta, TTFBSumMs: s.TTFBSumMsDelta,
		TTFBMinMs: s.TTFBMinMsSinceStart, TTFBMaxMs: s.TTFBMaxMsSinceStart,
		LatencyCount: s.LatencyCountDelta, LatencySumMs: s.LatencySumMsDelta,
		LatencyMinMs: s.LatencyMinMsSinceStart, LatencyMaxMs: s.LatencyMaxMsSinceStart,
		OverheadCount: s.OverheadCountDelta, OverheadSumMs: s.OverheadSumMsDelta,
		OverheadMinMs: s.OverheadMinMsSinceStart, OverheadMaxMs: s.OverheadMaxMsSinceStart,
		OverheadP50Ms: s.OverheadP50MsRecent, OverheadP90Ms: s.OverheadP90MsRecent, OverheadP99Ms: s.OverheadP99MsRecent,
	}
	var labels []store.CompressorLabelSample
	for provider, d := range s.RequestsByProviderDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "provider", LabelValue: provider,
			Metric: "requests", Delta: d,
		})
	}
	for model, d := range s.RequestsByModelDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "model", LabelValue: model,
			Metric: "requests", Delta: d,
		})
	}
	for outcomeSize, d := range s.MessagesByOutcomeSizeDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "outcome_size", LabelValue: outcomeSize,
			Metric: "messages", Delta: d,
		})
	}
	for provider, d := range s.CacheReadTokensDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "provider", LabelValue: provider,
			Metric: "cache_read_tokens", Delta: d,
		})
	}
	for provider, d := range s.UncachedTokensDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "provider", LabelValue: provider,
			Metric: "uncached_tokens", Delta: d,
		})
	}
	for provider, d := range s.ProviderCacheRequestsDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "provider", LabelValue: provider,
			Metric: "provider_cache_requests", Delta: d,
		})
	}
	for provider, d := range s.ProviderCacheHitRequestsDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "provider", LabelValue: provider,
			Metric: "provider_cache_hit_requests", Delta: d,
		})
	}
	for transform, d := range s.TransformTimingSumDelta {
		d := d
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "transform", LabelValue: transform,
			Metric: "timing_ms_sum", DeltaF: &d,
		})
	}
	for transform, d := range s.TransformTimingCountDelta {
		labels = append(labels, store.CompressorLabelSample{
			TS: now, ProxyID: proxyID, LabelKey: "transform", LabelValue: transform,
			Metric: "timing_ms_count", Delta: d,
		})
	}
	if err := db.Routing().RecordSavingsSample(ctx, row, labels); err != nil {
		log.Printf("forge: compressor sample record: %v", err)
	}
	if err := db.Routing().RecordSavings(ctx, proxyID, now, s.TokensInDelta, s.TokensSavedDelta); err != nil {
		log.Printf("forge: compressor savings record: %v", err)
	}
}

// smithBlockedWorkPath resolves the operator-local blocked-work tracker
// (layer-2 deployment data, two-layer knowledge architecture 2026-08-21):
// the smith.blocked_work.path setting when set, else the conventional
// location under the state dir. Absent file at that path is a valid state
// — the blocked-work KB reads honestly empty (fresh install).
func smithBlockedWorkPath(ctx context.Context, settings store.Settings) string {
	if raw, err := settings.Get(ctx, "smith.blocked_work.path"); err == nil {
		var p string
		if json.Unmarshal(raw, &p) == nil && p != "" {
			return p
		}
	}
	return "/var/lib/forge/smith-blocked-work.md"
}

var systemdUnitRe = regexp.MustCompile(`^[A-Za-z0-9@._-]+$`)

// dynamicComfyUIClient re-resolves smith.comfyui.url from live settings on
// every call rather than binding one *comfyui.HTTPClient at startup — the
// URL is a store-backed setting like everything else smith reads, editable
// without a restart (the same "no hardcoded default anywhere in code"
// posture as smith.model). Building a fresh *comfyui.HTTPClient per call is
// cheap (comfyui_health/comfyui_prune together run at most a handful of
// times per deep sweep, not a hot path) and keeps this a thin adapter
// rather than its own cache-invalidation problem.
type dynamicComfyUIClient struct {
	settings store.Settings
}

func (c *dynamicComfyUIClient) client() *comfyui.HTTPClient {
	url := "http://127.0.0.1:3001"
	if raw, err := c.settings.Get(context.Background(), smith.SettingComfyUIURL); err == nil && len(raw) > 0 {
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			url = s
		}
	}
	return comfyui.NewHTTPClient(url)
}

func (c *dynamicComfyUIClient) Healthy(ctx context.Context) bool { return c.client().Healthy(ctx) }
func (c *dynamicComfyUIClient) Queue(ctx context.Context) (comfyui.QueueResponse, error) {
	return c.client().Queue(ctx)
}
func (c *dynamicComfyUIClient) History(ctx context.Context) (map[string]comfyui.HistoryEntry, error) {
	return c.client().History(ctx)
}
func (c *dynamicComfyUIClient) ObjectInfo(ctx context.Context) (map[string]comfyui.ObjectInfoEntry, error) {
	return c.client().ObjectInfo(ctx)
}

// smithDeleteFile is smith.Deps.DeleteFile's production implementation —
// a thin os.Remove. Deliberately no path validation here: execute.go's
// deleteAllowed already re-validates every path against the live
// smith.comfyui.model_roots setting immediately before this is ever
// called (dispatchDeleteFiles), and duplicating that check here would just
// be a second place for the allowlist to drift out of sync.
func smithDeleteFile(_ context.Context, path string) error {
	return os.Remove(path)
}

// slotLaunchWiring is how the production daemon configures the engine's slot_launches handling (ADR-0026, Phase 1b step e5): the store, and
// enforcement ON. Enforcing means a failure to build or write a slot's launch row fails the load (Result.Success=false, nothing started) instead of
// being logged as a shadow warning, because the slot's shim now launches from that row. Kept as a function so a test pins what main() uses.
func slotLaunchWiring(s engine.SlotLaunchStore) (engine.SlotLaunchStore, bool) {
	return s, true
}

// smithInstallLauncherFile is smith.Deps.InstallLauncherFile's production
// implementation — execute.go's launcherInstallAllowed already re-validated
// path (under /usr/local/lib/forge/, no existing file, a real embedded
// source) immediately before this is ever called, so this only writes.
//
// The write itself is O_EXCL — a second defense against ever overwriting an
// existing file, even under a hypothetical TOCTOU race with
// launcherInstallAllowed's own os.Lstat check.
//
// The SELinux relabel to bin_t (required for systemd to actually exec the
// file, same as every other launcher — CLAUDE.md/docs/pitfalls.md) needs no
// elevated privilege on ForgeHost: `chcon` on a file the calling user already
// owns succeeds unprivileged under the targeted policy's unconfined_u
// (verified live, 2026-09-01 — every other chcon in this repo's deploy
// scripts, migrate-foundry-to-forge.sh and deploy-forge.sh, already relies
// on exactly this, run as plain `testuser` with no sudo). It's still checked for
// a real error rather than swallowed, though — a relabel could still fail
// on a future host with a stricter policy, and this must never report a
// false success on a file that's actually still unexecutable.
func smithInstallLauncherFile(ctx context.Context, path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close %s: %w", path, err)
	}
	cmd := exec.CommandContext(ctx, "chcon", "-u", "system_u", "-r", "object_r", "-t", "bin_t", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("wrote %s but could not set its SELinux context to bin_t (needs privilege this daemon does not have — see smithInstallLauncherFile's doc comment): %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// hasLiveMaintenanceLease reports whether any smith_procedure_runs row is
// still live (running or awaiting_checkpoint) and holds leaseID — see this
// function's one call site in main() for why the boot-time maintenance
// reconcile needs it. "" (no active window at boot) always reports false,
// matching ReconcileOnBoot's own documented no-op-when-inactive behavior.
func hasLiveMaintenanceLease(ctx context.Context, db *store.DB, leaseID string) bool {
	if leaseID == "" {
		return false
	}
	var n int
	err := db.SQL().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM smith_procedure_runs WHERE lease_id = ? AND status IN ('running', 'awaiting_checkpoint')`,
		leaseID).Scan(&n)
	return err == nil && n > 0
}

// smithRunStepEnvBase is the minimal environment every step gets regardless
// of its own Env/EnvPassthrough — enough for a plain binary (git, cmake,
// chcon, coreutils) to run at all. Sprint 6 (autonomous-remediation): this
// replaces unconditionally inheriting forge's full process environment,
// which included secrets such as FORGE_RECOVERY_CODE_PEPPER that a
// step's captured-and-persisted stdout/stderr could then leak.
var smithRunStepEnvBase = []string{"PATH", "HOME", "LANG", "TERM"}

// smithRunStep is smith.Deps.RunStep's production implementation — the
// procedure engine's only exec seam (autonomous-remediation Sprint 2,
// go/internal/smith/procedure.go). Fixed argv, no shell, ever: spec.Argv
// has already been re-validated against procedures.ArgvAllowed by the
// caller immediately before this runs.
//
// spec.Timeout, when > 0, is enforced with a real context.WithTimeout here
// (Sprint 6 fix — three separate doc comments across this codebase used to
// claim this was already bounded; none of them was true, and the run
// context this receives carries no deadline of its own, so a hung step
// used to hang forever while holding any maintenance window its procedure
// had opened). spec.Timeout == 0 runs with no deadline beyond ctx's own,
// same as before this fix — every pre-Sprint-6 procedure's steps declare a
// real Timeout via runProcedureSteps' procedureDefaultStepTimeout fallback,
// so this only changes behavior for a step that would have hung anyway.
//
// The child's environment is smithRunStepEnvBase plus spec.EnvPassthrough
// (named vars inherited from forge's own environment, unread and
// unlogged by smith itself) plus spec.Env's literal fixed values (registry
// data, applied last so they win) — never the daemon's full environment
// (Sprint 6 fix, see smithRunStepEnvBase's doc comment).
//
// Separate stdout/stderr capture so the run journal can show them
// distinctly.
func smithRunStep(ctx context.Context, spec procedures.StepSpec) (procedures.StepResult, error) {
	if len(spec.Argv) == 0 {
		return procedures.StepResult{}, fmt.Errorf("forge: run step: empty argv")
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	start := time.Now()
	cmd := exec.CommandContext(ctx, spec.Argv[0], spec.Argv[1:]...)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	cmd.Env = smithRunStepEnv(spec)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	res := procedures.StepResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	if ctx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("forge: run step %v: timed out after %s: %w", spec.Argv, spec.Timeout, ctx.Err())
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		res.ExitCode = 0
	case errors.As(runErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
	default:
		return res, fmt.Errorf("forge: run step %v: %w", spec.Argv, runErr)
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("forge: run step %v: exit %d", spec.Argv, res.ExitCode)
	}
	return res, nil
}

// smithRunStepEnv builds a step's child environment per smithRunStep's doc
// comment: the fixed minimal base, plus each spec.EnvPassthrough name
// resolved from forge's own environment (skipped, not empty-valued,
// when forge itself doesn't have it set), plus spec.Env's literal
// values applied last so they win over anything inherited.
func smithRunStepEnv(spec procedures.StepSpec) []string {
	out := make([]string, 0, len(smithRunStepEnvBase)+len(spec.EnvPassthrough)+len(spec.Env))
	seen := make(map[string]bool)
	add := func(k, v string) {
		out = append(out, k+"="+v)
		seen[k] = true
	}
	for _, k := range smithRunStepEnvBase {
		if v, ok := os.LookupEnv(k); ok {
			add(k, v)
		}
	}
	for _, k := range spec.EnvPassthrough {
		if seen[k] {
			continue
		}
		if v, ok := os.LookupEnv(k); ok {
			add(k, v)
		}
	}
	for k, v := range spec.Env {
		// Overwrite in place if EnvPassthrough already added k, rather than
		// appending a duplicate entry that Go's exec would silently let the
		// later one win — explicit beats implicit here.
		if seen[k] {
			for i, kv := range out {
				if strings.HasPrefix(kv, k+"=") {
					out[i] = k + "=" + v
				}
			}
			continue
		}
		add(k, v)
	}
	return out
}

// slotPorts maps slot name → raw llama-server port for the router's on-demand
// load pinning (router.Deps.Slots).
func slotPorts(cfg *config.Config) map[string]int {
	out := make(map[string]int, len(cfg.Slots))
	for name, slot := range cfg.Slots {
		out[name] = slot.Port
	}
	return out
}

// recoveryPepperSettingKey is where the auto-generated recovery-code pepper is
// persisted when it is not supplied out-of-band via the environment.
const recoveryPepperSettingKey = "auth.recovery_code_pepper"

// loadRecoveryPepper resolves the server pepper that keys recovery-code HMACs
// (see internal/authz/recovery.go). Resolution order:
//
//  1. FORGE_RECOVERY_CODE_PEPPER env (standard base64). Lets an operator keep
//     the pepper OUT of the state DB — e.g. a systemd credential or a 600
//     EnvironmentFile — the storage separation OWASP's peppering guidance
//     recommends (pepper apart from the hashes).
//  2. A pepper previously persisted in the settings store (base64 value).
//  3. A freshly generated 32-byte pepper on first run, persisted for reuse.
//
// The pepper is defense-in-depth: 128-bit codes hashed with an approved one-way
// function already satisfy NIST SP 800-63B §5.1.2, so persisting it in the state
// DB is acceptable; option 1 exists for operators who want stronger separation.
func loadRecoveryPepper(ctx context.Context, db *store.DB) ([]byte, error) {
	if env := os.Getenv("FORGE_RECOVERY_CODE_PEPPER"); env != "" {
		p, err := base64.StdEncoding.DecodeString(env)
		if err != nil {
			return nil, fmt.Errorf("decode FORGE_RECOVERY_CODE_PEPPER: %w", err)
		}
		return p, nil
	}
	if v, err := db.Settings().Get(ctx, recoveryPepperSettingKey); err == nil {
		p, derr := base64.StdEncoding.DecodeString(string(v))
		if derr != nil {
			return nil, fmt.Errorf("decode stored recovery pepper: %w", derr)
		}
		return p, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	p := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		return nil, fmt.Errorf("generate recovery pepper: %w", err)
	}
	encoded := []byte(base64.StdEncoding.EncodeToString(p))
	if err := db.Settings().Set(ctx, recoveryPepperSettingKey, encoded); err != nil {
		return nil, fmt.Errorf("persist recovery pepper: %w", err)
	}
	return p, nil
}

// reloadConfigFromStore re-reads the store-backed infra config into holder
// on SIGHUP (TOML decommission Phase 3 — no file to re-parse anymore, see
// config.LoadFromStore's doc comment on the SIGHUP semantics change). A read
// failure is logged and the previous config is kept (never crash a running
// daemon over a transient DB hiccup).
func reloadConfigFromStore(ctx context.Context, db *store.DB, holder *atomic.Pointer[config.Config]) {
	cfg, err := config.LoadFromStore(ctx, db)
	if err != nil {
		log.Printf("forge: SIGHUP config reload failed, keeping current config: %v", err)
		return
	}
	holder.Store(cfg)
	log.Printf("forge: SIGHUP config reloaded from store (engine + collector updated; " +
		"listener/API/router changes still need a restart)")
}

// listenAndServe starts srv and logs the listener's actual address. Wraps
// ListenAndServe so the caller gets a meaningful error on port conflict.
func listenAndServe(srv *http.Server, name, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("%s listen %s: %w", name, addr, err)
	}
	log.Printf("forge: %s listener on %s", name, ln.Addr())
	return srv.Serve(ln)
}

// seedCompressorRetired makes the retired state explicit in the store (ADR-0017, contract C7): if
// compressor.passthrough_all has never been written, write true, so the router and the Settings API agree
// without either read path changing its own default. An existing value (true or false) is never overwritten.
func seedCompressorRetired(ctx context.Context, st store.Settings) {
	if st == nil || !compressorctl.Retired() {
		return
	}
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := st.Get(c, "compressor.passthrough_all"); err == nil {
		return
	}
	if err := st.Set(c, "compressor.passthrough_all", []byte("true")); err != nil {
		log.Printf("forge: seed compressor.passthrough_all: %v", err)
	}
}
