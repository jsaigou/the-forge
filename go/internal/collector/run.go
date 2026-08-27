// SPDX-License-Identifier: Apache-2.0

package collector

import (
	"context"
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jsaigou/the-forge/internal/config"
)

// BookmarkProbe is one dashboard bookmark with a server-side health check
// (V4 [[ui.bookmarks]] health / health_arg / always_online fields). The
// bookmark list itself is app-mutated state (store settings), so Phase 9
// wires it in as a callback — the collector cannot import the store.
type BookmarkProbe struct {
	Label        string
	Health       string // "systemd_unit" | "tailscale_node" | ""
	HealthArg    string
	AlwaysOnline bool
}

// Options wires a Collector. Cfg and Systemd are required; everything else
// degrades gracefully when nil (fields simply stay empty in the Snapshot),
// which is also how unit tests isolate probes.
type Options struct {
	Cfg     func() *config.Config
	Systemd Systemd

	// Slots is the engine (authoritative slot reconciliation). Nil until
	// Phase 9 wiring: slots then report Mode "" — occupancy unknown.
	Slots SlotStateSource

	// CurrentMode feeds Metrics.Mode (engine.CurrentMode).
	CurrentMode func() string

	GPU         *GPU
	Proc        Proc
	HostSensors HostSensors
	Hostname    string

	// ExtraUnits are probed on top of config-derived units (infra units
	// like forge-tts/-embedding/-stt whose names live outside the V5
	// config schema; Phase 9 wires them).
	ExtraUnits []string

	// TTSEngineUnits returns every currently-configured tts.engines unit
	// (Tier 1 Sprint 2, Voice & Speech settings) — a closure, not a static
	// list like ExtraUnits, so an operator wiring up a new resident engine
	// unit via Settings is watched on the very next collector cycle, not
	// only after a daemon restart (same live-discovery shape as
	// CompressorUnits just below, for the same reason: this is exactly the
	// mechanism meant to close the blind spot where forge-tts-custom/-base
	// crash-looped for 5 days undetected because nothing probed them). Nil
	// = no TTS engine units watched.
	TTSEngineUnits func() []string

	// Bookmarks supplies server-side-health bookmarks (store-backed).
	Bookmarks func() []BookmarkProbe

	// TailscaleOnline answers tailscale_node bookmark checks (see
	// TailscaleLocalAPI). Nil: those bookmarks get no health entry.
	TailscaleOnline func(ctx context.Context, node string) bool

	// CompressorTargets maps compressor service name → local port for the
	// per-proxy savings scrape (store-backed; Phase 9).
	CompressorTargets func() map[string]int

	// CompressorUnits maps compressor service name → real systemd unit name, for
	// unitNames() to watch. Deliberately separate from CompressorTargets (which
	// only the savings scrape needs) rather than assuming a unit is always
	// named "compressor-<service>" — that assumption broke live on ForgeHost: the
	// "aiand" service's real unit is "headroom-external.service" (a provider
	// nickname vs. its actual unit, chosen independently), which the old
	// hardcoded convention could never watch, so it always showed inactive on
	// the dashboard regardless of whether it was really running. Nil or a
	// missing/empty entry for a service falls back to the old convention (see
	// unitNames) rather than watching nothing.
	CompressorUnits func() map[string]string

	// OnTokenSample receives per-slot token deltas (usage recording).
	OnTokenSample func(slot, mode string, promptDelta, predictedDelta int64)

	// OnPrefillSample receives one interval's real measured prefill
	// throughput for a slot — promptTokens processed over promptSeconds of
	// llama-server's own cumulative prompt_seconds_total counter (2026-08-06,
	// Compressor local-savings prefill sprint). Deliberately separate from
	// OnTokenSample: that callback feeds usage_events (billing/accounting)
	// and shouldn't take on a second, unrelated concern. Only fires when
	// promptSeconds > 0 (real prefill work happened this interval) and mode
	// is attributable.
	OnPrefillSample func(slot, mode string, promptTokens int64, promptSeconds float64)

	// OnCompressorSample receives one interval's per-proxy Compressor counter
	// deltas (cost/savings sprint, 2026-07-30). Never called for an interval
	// where every delta is zero — see CompressorSample.AllZero.
	OnCompressorSample func(service string, sample CompressorSample)

	// OnSlotActivity fires on a busy↔idle edge for a slot — never once per
	// cycle regardless of state (Sprint K, 2026-08-05: the naive per-cycle
	// push would flood the SSE bus at 2s resolution for the common case of
	// a slot serving a long streaming response). Fires false once for a
	// slot that drops out of the active set entirely (unloaded, or its
	// /metrics scrape started failing), so a listener never has to infer
	// "gone" from silence.
	OnSlotActivity func(slot string, active bool)

	// BaseURL overrides the per-port probe URL (tests).
	BaseURL func(port int) string

	// DialPort overrides the port-listening probe (tests).
	DialPort func(port int) bool

	// Now overrides the clock (tests).
	Now func() time.Time

	// SlotErrorCount reports recent 5xx/transport failures for a slot port
	// (the a0 router's per-slot error window — see router.SlotErrorCount).
	// A device-lost llama-server 5xxes every request while /health stays
	// green, so this is the early-warning signal the stall detector can't
	// see. nil ⇒ the collector skips the SLOT_ERROR_STORM alert (tests,
	// stub environments). Returns (windowed count, lifetime count).
	SlotErrorCount func(port int, windowSeconds int64) (int, int64)
}

// Collector runs the single probe loop (V5 design decision 2) and publishes
// immutable Snapshots. It implements Source.
type Collector struct {
	o     Options
	llama *LlamaClient

	snap atomic.Pointer[Snapshot]

	// mu serializes cycles (the Run loop vs ProbeNow) and guards the
	// mutable probe state below.
	mu                     sync.Mutex
	hang                   *hangDetector
	lastActivity           map[string]time.Time
	lastTokenTotals        map[string][2]float64
	lastPrefillSeconds     map[string]float64
	lastCompressorCounters map[string]*compressorCounters
	nctxCache              map[string]int   // slot → verified NCtx for the current slot session
	identityCache          map[string]Props // slot → verified ModelAlias/ModelPath for the current slot session
	prevUnits              map[string]UnitState
	rings                  map[string][]float64 // gpu / ram / temp, 120 samples

	// lastBusy backs OnSlotActivity's edge detection (Sprint K). Absent key
	// == never reported, same "unseen" convention as lastActivity.
	lastBusy map[string]bool

	// cycleN counts completed cycle()s. Sprint K (2026-08-05): halving
	// PollIntervalS to sharpen slot-activity resolution doubled the rate of
	// every probe in cycle(), including ones that don't need to sharpen
	// (Compressor /metrics scrapes, Tailscale bookmark health, TCP port
	// dials) — those run every other cycle instead, reusing the prior
	// cycle's result in between (probeUnits' c.prevUnits is the existing
	// precedent for this pattern). Unit states and the inference scrape,
	// the two things this sprint actually wants sharper, run every cycle.
	cycleN             uint64
	prevPorts          map[int]bool
	prevBookmarkHealth map[string]bool

	// cpuJiffies/netCounters back the Phase 4 (2026-08-12) real-utilization
	// CPU% and network throughput readings — both are cumulative kernel
	// counters, meaningless without a diff against the previous cycle. Zero
	// value (valid=false) on a fresh Collector correctly yields nil for the
	// first cycle rather than a bogus rate computed against a zero baseline.
	lastCPUJiffies cpuJiffiesState
	lastNet        netCountersState

	// lastNRestarts tracks each unit's NRestarts as of the previous cycle,
	// for crash detection (notifications sprint). A unit's first sighting
	// only records a baseline — it must NOT emit UNIT_RESTARTED, since a
	// long-running unit can easily have NRestarts > 0 from before forge
	// itself last started, which is not a new event.
	lastNRestarts map[string]uint32
}

// historyRingSize mirrors V4 monitor's deque(maxlen=120).
const historyRingSize = 120

// New builds a Collector. Call Run to start the loop, or ProbeNow for
// one-shot cycles (tests, engine decision points).
func New(o Options) *Collector {
	if o.Cfg == nil {
		panic("collector: Options.Cfg is required")
	}
	if o.Systemd == nil {
		panic("collector: Options.Systemd is required")
	}
	if o.GPU == nil {
		o.GPU = &GPU{}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	cfg := o.Cfg()
	minTPS := float64(cfg.Monitor.HangTPSThousand) / 1000
	c := &Collector{
		o:     o,
		llama: NewLlamaClient(o.BaseURL),
		hang: newHangDetector(
			minTPS,
			time.Duration(cfg.Monitor.HangSustainS)*time.Second,
			time.Duration(cfg.Monitor.SwitchCooldownS)*time.Second,
		),
		lastActivity:           map[string]time.Time{},
		lastTokenTotals:        map[string][2]float64{},
		lastPrefillSeconds:     map[string]float64{},
		lastCompressorCounters: map[string]*compressorCounters{},
		nctxCache:              map[string]int{},
		identityCache:          map[string]Props{},
		prevUnits:              map[string]UnitState{},
		rings:                  map[string][]float64{"gpu": {}, "ram": {}, "temp": {}, "power": {}},
		lastNRestarts:          map[string]uint32{},
		lastBusy:               map[string]bool{},
		prevPorts:              map[int]bool{},
		prevBookmarkHealth:     map[string]bool{},
	}
	return c
}

// Current implements Source. Nil before the first completed cycle.
func (c *Collector) Current() *Snapshot { return c.snap.Load() }

// Run executes the probe loop until ctx is done. The first cycle runs
// immediately so Current() is non-nil as soon as possible.
func (c *Collector) Run(ctx context.Context) {
	interval := time.Duration(c.o.Cfg().Monitor.PollIntervalS) * time.Second
	c.ProbeNow(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.ProbeNow(ctx)
		}
	}
}

// ProbeNow runs one full probe cycle immediately and returns the fresh
// snapshot. Used by the loop itself, and by the engine at decision points
// (memory budgeting) where a stale snapshot (≤PollIntervalS old) could
// mis-place a load.
func (c *Collector) ProbeNow(ctx context.Context) *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	snap := c.cycle(ctx)
	c.snap.Store(snap)
	return snap
}

// NotifySwitchComplete opens the hang-detection cooldown window (crown
// jewels: 120s post-switch — load + initial KV allocation look like stalls).
func (c *Collector) NotifySwitchComplete() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hang.NotifySwitchComplete(c.o.Now())
}

// History returns the gpu/ram/temp sparkline rings (V4 monitor.get_history).
func (c *Collector) History() map[string][]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]float64, len(c.rings))
	for k, v := range c.rings {
		out[k] = append([]float64(nil), v...)
	}
	return out
}

// cycle builds one Snapshot. Callers hold c.mu.
//
// Sprint K (2026-08-05): PollIntervalS halved (4s→2s) to sharpen the
// slot-activity signal, but not every probe here needs to sharpen with it.
// probePorts (sequential TCP dials, 500ms timeout each) and bookmarkHealth
// (can include a Tailscale API call) run every other cycle, reusing the
// prior cycle's result in between — the same staleness contract
// probeUnits' c.prevUnits fallback already has. recordCompressorSavings
// deliberately does NOT get this treatment despite also scraping
// remote-ish endpoints: it's stateful (tracks its own counter deltas
// internally via c.lastCompressorCounters), so skipping a cycle wouldn't
// just serve a slightly-stale cached value like the two above — it would
// silently double the sampling interval of compressor_savings_samples, the same
// table the cost/savings dashboard integrates over. Its own scrapes are
// loopback HTTP to local Compressor proxies (cheap), unlike a real dial
// timeout or an external API call, so there's no cost reason to gate it
// either. Unit states and the inference scrape — what this sprint
// actually wants sharper — still run every cycle.
func (c *Collector) cycle(ctx context.Context) *Snapshot {
	cfg := c.o.Cfg()
	now := c.o.Now()
	c.cycleN++
	slow := c.cycleN%2 == 1 // odd cycles do the slow/remote probes

	units := c.probeUnits(ctx, cfg)
	slots := c.slotAssignments(units)
	inference, scraped := c.scrapeInference(ctx, cfg, units, slots, now)
	metrics := c.buildMetrics(cfg, units, slots)
	alerts := c.collectAlerts(cfg, metrics, scraped, now)
	alerts = append(alerts, c.unitAlerts(units)...)
	alerts = append(alerts, c.collectSlotErrorAlerts(cfg, now)...)
	c.recordCompressorSavings(ctx)
	c.updateRings(metrics)

	ports := c.prevPorts
	if slow {
		ports = c.probePorts(cfg)
		c.prevPorts = ports
	}
	bookmarks := c.prevBookmarkHealth
	if slow {
		bookmarks = c.bookmarkHealth(ctx, units)
		c.prevBookmarkHealth = bookmarks
	}

	snap := &Snapshot{
		TakenAt:        now,
		Hostname:       c.o.Hostname,
		Metrics:        metrics,
		Units:          units,
		Slots:          c.buildSlots(cfg, slots, now),
		Inference:      inference,
		Ports:          ports,
		BookmarkHealth: bookmarks,
		Alerts:         alerts,
		Compressors:    c.buildCompressors(units),
	}
	return snap
}

// buildCompressors builds the per-proxy resource state (Sprint 4) from the
// same store-backed CompressorTargets/CompressorUnits sources unitNames already
// uses for discovery — one source of truth for "which proxies exist".
// Unconditional per proxy: a torn-down or unreachable proxy still gets an
// entry (Up: false), unlike recordCompressorSavings which skips it outright.
func (c *Collector) buildCompressors(units map[string]UnitState) map[string]CompressorState {
	if c.o.CompressorTargets == nil {
		return nil
	}
	targets := c.o.CompressorTargets()
	if len(targets) == 0 {
		return nil
	}
	unitByService := map[string]string{}
	if c.o.CompressorUnits != nil {
		unitByService = c.o.CompressorUnits()
	}
	out := make(map[string]CompressorState, len(targets))
	for service, port := range targets {
		unit := unitByService[service]
		if unit == "" {
			unit = "compressor-" + service
		}
		cs := CompressorState{Unit: unit, Port: port}
		if st, ok := units[unit]; ok {
			cs.Up = st.Active()
			cs.MainPID = st.MainPID
			cs.NRestarts = st.NRestarts
			cs.Result = st.Result
			if st.MainPID > 0 {
				cs.RSSBytes = int64(c.o.Proc.RSSBytes(int(st.MainPID)))
			}
		}
		out[service] = cs
	}
	return out
}

// unitNames derives every unit the collector must watch: slot units, service
// mode units, extra infra units, and systemd-health bookmark args.
func (c *Collector) unitNames(cfg *config.Config) []string {
	seen := map[string]bool{}
	var names []string
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			names = append(names, u)
		}
	}
	for _, slot := range cfg.Slots {
		add(slot.Unit)
	}
	for _, mode := range cfg.Modes {
		add(mode.Unit)
	}
	for _, u := range c.o.ExtraUnits {
		add(u)
	}
	if c.o.TTSEngineUnits != nil {
		for _, u := range c.o.TTSEngineUnits() {
			add(u)
		}
	}
	// Compressor proxy units are dynamic (created/removed at runtime via
	// POST/teardown, Phase 9b) rather than config-derived, so they're
	// discovered from the same store-backed source CompressorTargets already
	// reads for the savings scrape — one source of truth for "which
	// proxies exist" instead of a second discovery path. The real unit name
	// per service comes from CompressorUnits (store.ProxyRow.Unit) — a service
	// name and its unit are chosen independently (e.g. a legacy hand-created
	// unit's real name doesn't follow any template at all), so
	// "forge-compress@<service>" is only a fallback guess (matching the
	// real template every proxy provisions onto since Sprint 3/7) for a
	// service CompressorUnits doesn't have an entry for, never the primary
	// source of truth.
	if c.o.CompressorTargets != nil {
		units := map[string]string{}
		if c.o.CompressorUnits != nil {
			units = c.o.CompressorUnits()
		}
		for service := range c.o.CompressorTargets() {
			if u := units[service]; u != "" {
				add(u)
			} else {
				add("forge-compress@" + service)
			}
		}
	}
	if c.o.Bookmarks != nil {
		for _, b := range c.o.Bookmarks() {
			if b.Health == "systemd_unit" {
				add(b.HealthArg)
			}
		}
	}
	sort.Strings(names)
	return names
}

// probeUnits reads unit states. On probe failure the previous cycle's map is
// reused: stale unit state is safe, but an empty map would make a
// deactivating unit invisible — exactly the state the crown-jewels rule
// exists to protect (slot state must not clear during deactivating).
func (c *Collector) probeUnits(ctx context.Context, cfg *config.Config) map[string]UnitState {
	states, err := c.o.Systemd.UnitStates(ctx, c.unitNames(cfg))
	if err != nil || states == nil {
		return c.prevUnits
	}
	c.attachGPUMemory(cfg, states)
	c.prevUnits = states
	return states
}

// attachGPUMemory fills UnitState.GPUBytes for every non-slot unit whose
// main process is running (S2: Resources-tab attribution). One fdinfo read
// per live unit per cycle — the same Proc.GPUMemoryBytes accounting the slot
// figures use, deduped by drm-client-id — so ComfyUI and the always-on
// services get named as GTT holders instead of disappearing into "free".
// Slot units are skipped: their canonical per-slot figure is populated in
// buildSlots (SlotState.MemoryBytes) and consumers sum the two maps.
func (c *Collector) attachGPUMemory(cfg *config.Config, states map[string]UnitState) {
	slotUnits := map[string]bool{}
	for _, slot := range cfg.Slots {
		slotUnits[slot.Unit] = true
	}
	for name, st := range states {
		if st.MainPID == 0 || slotUnits[name] {
			continue
		}
		st.GPUBytes = int64(c.o.Proc.GPUMemoryBytes(int(st.MainPID)))
		states[name] = st
	}
}

func (c *Collector) slotAssignments(units map[string]UnitState) map[string]SlotAssignment {
	if c.o.Slots == nil {
		return map[string]SlotAssignment{}
	}
	return c.o.Slots.SlotStates(units)
}

// orderedSlots returns config slot names in display order.
func orderedSlots(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Slots))
	for name := range cfg.Slots {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := cfg.Slots[names[i]], cfg.Slots[names[j]]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		return names[i] < names[j]
	})
	return names
}

// activeSlotPorts returns slot → port for slots whose unit is active (port
// of monitor._active_slot_ports, minus the subprocess sweep).
func activeSlotPorts(cfg *config.Config, units map[string]UnitState) map[string]int {
	out := map[string]int{}
	for name, slot := range cfg.Slots {
		if units[slot.Unit].Active() {
			out[name] = slot.Port
		}
	}
	return out
}

// scrapeInference polls each active slot's /metrics ONCE per cycle (the V4
// single-poll fix: hang detection, activity tracking, and token samples all
// consume this one scrape). Returns the Snapshot.Inference map and the raw
// scrapes keyed by port for hang detection.
func (c *Collector) scrapeInference(
	ctx context.Context,
	cfg *config.Config,
	units map[string]UnitState,
	slots map[string]SlotAssignment,
	now time.Time,
) (map[string]SlotInference, map[int]*LlamaMetrics) {
	active := activeSlotPorts(cfg, units)
	inference := map[string]SlotInference{}
	scraped := map[int]*LlamaMetrics{}

	for slot, port := range active {
		m := c.llama.Metrics(ctx, port)
		scraped[port] = m
		if m == nil {
			// Still starting up (or wedged): no activity seed, no NCtx. Not
			// touching c.lastActivity here is deliberate, not a gap: we
			// have no evidence either way this cycle, and the map already
			// holds the last cycle that DID have evidence — nothing to
			// correct. It only gets cleared once the slot goes inactive
			// (below), same as any other missed-scrape cycle.
			continue
		}

		promptDelta, predictedDelta, hasBaseline := c.recordTokenSample(slot, slots[slot].Mode, m)

		// Activity: newly-seen slots count as just-active so a freshly
		// loaded model isn't instantly idle-eviction-eligible. Beyond that,
		// a slot counts as active if either (a) a request is in flight
		// right now (RequestsProcessing), or (b) the cumulative
		// prompt/predicted token counters advanced since the last scrape.
		// (b) matters because (a) alone only catches a request if it
		// happens to still be in flight at the exact moment of a
		// ~2s-interval scrape — a request that both starts and finishes
		// between two polls is invisible to the gauge, so a slot serving a
		// steady stream of short completions could otherwise read "idle
		// 3h" while actively answering. hasBaseline is false on a slot's
		// first observation and on llama.cpp builds without cumulative
		// counters, where the gauge is all there is.
		tokensAdvanced := hasBaseline && (promptDelta > 0 || predictedDelta > 0)
		if _, seen := c.lastActivity[slot]; !seen || m.RequestsProcessing > 0 || tokensAdvanced {
			c.lastActivity[slot] = now
		}
		c.reportSlotActivity(slot, m.RequestsProcessing > 0)

		// NCtx + ModelAlias/ModelPath: verified once per slot session via a
		// single /props fetch, cached until the slot goes inactive (crown
		// jewels: actual context recorded; ModelAlias/ModelPath are the
		// ground truth smith's slot_model_identity check compares against
		// the engine's own configured belief).
		nctx, ok := c.nctxCache[slot]
		identity := c.identityCache[slot]
		if !ok {
			if p, err := c.llama.PropsInfo(ctx, port); err == nil {
				nctx = p.NCtx
				c.nctxCache[slot] = p.NCtx
				identity = p
				c.identityCache[slot] = p
			}
		}

		inference[slot] = SlotInference{
			NCtx:                 nctx,
			ModelAlias:           identity.ModelAlias,
			ModelPath:            identity.ModelPath,
			RequestsProcessing:   int(m.RequestsProcessing),
			PromptTokensTotal:    totalOrZero(m.PromptTotal),
			PredictedTokensTotal: totalOrZero(m.PredictedTotal),
			TokensPerSecond:      math.Max(m.PromptTPS, m.PredictedTPS),
			SlotErrors:           c.slotErrorCount(port),
		}
	}

	// Drop per-slot session state for slots no longer active, so a later
	// reload starts fresh (activity, token baselines, NCtx cache).
	for slot := range c.lastActivity {
		if _, ok := active[slot]; !ok {
			delete(c.lastActivity, slot)
		}
	}
	for slot := range c.lastTokenTotals {
		if _, ok := active[slot]; !ok {
			delete(c.lastTokenTotals, slot)
			delete(c.lastPrefillSeconds, slot)
		}
	}
	for slot := range c.nctxCache {
		if _, ok := active[slot]; !ok {
			delete(c.nctxCache, slot)
		}
	}
	for slot := range c.identityCache {
		if _, ok := active[slot]; !ok {
			delete(c.identityCache, slot)
		}
	}
	for slot := range c.lastBusy {
		if _, ok := active[slot]; !ok {
			// Dropped out of the active set entirely (unloaded, or /metrics
			// went from reachable to not) — report false once rather than
			// leaving a listener's last-known state stuck at true forever.
			c.reportSlotActivity(slot, false)
			delete(c.lastBusy, slot)
		}
	}
	return inference, scraped
}

// slotErrorCount reads the a0 router's per-slot 5xx/transport window for
// port (60s), or 0 when the seam isn't wired.
func (c *Collector) slotErrorCount(port int) int {
	if c.o.SlotErrorCount == nil {
		return 0
	}
	n, _ := c.o.SlotErrorCount(port, 60)
	return n
}

// reportSlotActivity fires OnSlotActivity only on a busy↔idle edge for an
// already-seen slot — never once per cycle regardless of state (which at a
// 2s cadence would flood the SSE bus for the ordinary case of one slot
// serving a long streaming response), and never on first sighting either:
// a slot's starting state is already correct in the polled
// statusResponse.slot_activity for any client connecting/reconnecting, so
// the push channel only needs to carry transitions after that.
func (c *Collector) reportSlotActivity(slot string, busy bool) {
	prev, seen := c.lastBusy[slot]
	c.lastBusy[slot] = busy
	if !seen || prev == busy {
		return
	}
	if c.o.OnSlotActivity != nil {
		c.o.OnSlotActivity(slot, busy)
	}
}

func totalOrZero(v *float64) int64 {
	if v == nil {
		return 0
	}
	return int64(*v)
}

// recordTokenSample computes one slot's token delta against its session
// baseline and hands it to OnTokenSample (port of
// monitor._record_token_samples, including counter-reset detection: a
// counter that went backwards restarted from 0, so the new value IS the
// delta). Also computes the matching real prefill-throughput delta
// (promptTokens / promptSeconds) and hands it to OnPrefillSample — see that
// field's doc comment for why it's a separate callback from OnTokenSample.
//
// Returns the raw prompt/predicted deltas too: scrapeInference reuses them
// as an activity signal, since any token processed during the interval
// proves the slot wasn't idle even when RequestsProcessing read 0 at both
// endpoints. hasBaseline is false on the slot's first observation or on a
// build without cumulative counters, when no delta is computable.
func (c *Collector) recordTokenSample(slot, mode string, m *LlamaMetrics) (promptDelta, predictedDelta int64, hasBaseline bool) {
	if m.PromptTotal == nil || m.PredictedTotal == nil {
		return 0, 0, false // old llama.cpp build without cumulative counters
	}
	prev, seen := c.lastTokenTotals[slot]
	c.lastTokenTotals[slot] = [2]float64{*m.PromptTotal, *m.PredictedTotal}

	var prevSeconds float64
	secondsSeen := false
	if m.PromptSecondsTotal != nil {
		prevSeconds, secondsSeen = c.lastPrefillSeconds[slot]
		c.lastPrefillSeconds[slot] = *m.PromptSecondsTotal
	}

	if !seen {
		return 0, 0, false // first observation — baseline only
	}
	promptDelta = delta(*m.PromptTotal, prev[0])
	predictedDelta = delta(*m.PredictedTotal, prev[1])

	if promptDelta > 0 && mode != "" && c.o.OnPrefillSample != nil && m.PromptSecondsTotal != nil && secondsSeen {
		if secondsDelta := deltaFloat(*m.PromptSecondsTotal, prevSeconds); secondsDelta > 0 {
			c.o.OnPrefillSample(slot, mode, promptDelta, secondsDelta)
		}
	}

	if (promptDelta > 0 || predictedDelta > 0) && mode != "" && c.o.OnTokenSample != nil {
		c.o.OnTokenSample(slot, mode, promptDelta, predictedDelta)
	}
	return promptDelta, predictedDelta, true
}

func delta(current, prev float64) int64 {
	if current >= prev {
		return int64(current - prev)
	}
	return int64(current) // counter reset: restarted from 0, re-accumulated
}

// deltaFloat is delta's float64 counterpart, for prompt_seconds_total (which
// llama-server reports as a whole-second-truncated but still monotonically
// cumulative counter) — same counter-reset semantics.
func deltaFloat(current, prev float64) float64 {
	if current >= prev {
		return current - prev
	}
	return current // counter reset: restarted from 0, re-accumulated
}

// buildMetrics assembles the Metrics block, including the crown-jewels
// additive inference RSS: gtt_used (whole-GPU classic-GTT floor, covering
// Vulkan slots, vLLM, ComfyUI, everything rocm-smi/sysfs can see) PLUS the
// VmRSS of llama-server processes in ROCm+unified-memory slots (invisible
// to the GTT counter). ADD, never max() — max() silently dropped ComfyUI
// whenever a ROCm slot outweighed it (docs/pitfalls.md).
func (c *Collector) buildMetrics(
	cfg *config.Config,
	units map[string]UnitState,
	slots map[string]SlotAssignment,
) Metrics {
	stats := c.o.Proc.Stats()
	gpu := c.o.GPU.Sample()
	sensors := c.o.HostSensors.Sample()

	usedBytes := stats.MemTotalBytes - stats.MemAvailBytes
	var memPct float64
	if stats.MemTotalBytes > 0 {
		memPct = math.Round(usedBytes/stats.MemTotalBytes*1000) / 10
	}

	// CPU.Pct is real jiffy-delta utilization (Phase 4, 2026-08-12) — Load1
	// stays the raw load average, a separate and separately useful figure.
	idleAll, total, jiffiesOK := c.o.Proc.CPUJiffies()
	cpuPct := c.cpuUtilPct(idleAll, total, jiffiesOK)

	rxBytes, txBytes, netOK := c.o.Proc.NetDev()
	netRxRate, netTxRate := c.networkRates(rxBytes, txBytes, netOK, c.o.Now())

	m := Metrics{
		Memory: Memory{
			TotalBytes: int64(math.Round(stats.MemTotalBytes)),
			UsedBytes:  int64(math.Round(usedBytes)),
			AvailBytes: int64(math.Round(stats.MemAvailBytes)),
			Pct:        memPct,
		},
		CPU:              CPU{Load1: stats.Load1, Pct: cpuPct},
		Disk:             sampleDisk(cfg.Paths.ModelsDir),
		Storage:          sampleStorageMounts(cfg.Paths),
		GPUUsePct:        gpu.UsePct,
		GTTUsedBytes:     gpu.GTTUsedBytes,
		GTTTotalBytes:    gpu.GTTTotalBytes,
		TempCelsius:      gpu.TempC,
		GPUJunctionTempC: gpu.JunctionTempC,
		PackagePowerW:    gpu.PackagePowerW,
		CPUPackageTempC:  sensors.CPUPackageTempC,
		NVMeTempC:        sensors.NVMeTempC,
		NetRxBytesPerSec: netRxRate,
		NetTxBytesPerSec: netTxRate,
	}
	if c.o.CurrentMode != nil {
		m.Mode = c.o.CurrentMode()
	}
	if stats.UptimeS > 0 {
		up := int64(stats.UptimeS)
		m.UptimeSeconds = &up
	}

	activeSlotNames := make([]string, 0, len(cfg.Slots))
	for name, slot := range cfg.Slots {
		if units[slot.Unit].Active() {
			activeSlotNames = append(activeSlotNames, name)
		}
	}
	weightsBytes := ActiveWeightsBytes(cfg.Paths.SysconfigDir, cfg.Paths.ModelsDir, activeSlotNames)

	var gttUsed float64
	if gpu.GTTUsedBytes != nil {
		gttUsed = float64(*gpu.GTTUsedBytes)
	}
	unifiedBytes := c.unifiedMemoryRSSBytes(cfg, slots)
	inferenceBytes := gttUsed + unifiedBytes // ADDITIVE — never max()

	if inferenceBytes > 0 {
		v := int64(math.Round(inferenceBytes))
		m.InferenceRSSBytes = &v
	}
	if weightsBytes > 0 {
		v := int64(weightsBytes)
		m.ModelWeightsBytes = &v
	}
	if weightsBytes > 0 && inferenceBytes > 0 {
		if kv := inferenceBytes - float64(weightsBytes); kv > 0 {
			v := int64(math.Round(kv))
			m.KVCacheBytes = &v
		}
	}
	return m
}

// cpuJiffiesState/netCountersState back cpuUtilPct/networkRates — both are
// cumulative-kernel-counter deltas that need the previous cycle's reading,
// not a single point-in-time value.
type cpuJiffiesState struct {
	idleAll, total uint64
	valid          bool
}

type netCountersState struct {
	rxBytes, txBytes uint64
	at               time.Time
	valid            bool
}

// cpuUtilPct computes real CPU utilization from a /proc/stat jiffy-counter
// delta against the previous cycle (Phase 4 collector metrics, 2026-08-12)
// — the pre-existing CPU.Pct was load1/nproc*100 (load average, not
// utilization; Load1 stays available separately). Returns 0 — the
// non-pointer "sampled struct" zero-value convention Memory/CPU/Disk
// already use — on the first cycle (no prior sample), a failed read, or a
// counter reset (rare: host suspend/resume), never a bogus rate computed
// against a stale or zero baseline. Callers hold c.mu (buildMetrics' own
// contract via cycle()).
func (c *Collector) cpuUtilPct(idleAll, total uint64, ok bool) float64 {
	if !ok {
		c.lastCPUJiffies = cpuJiffiesState{}
		return 0
	}
	prev := c.lastCPUJiffies
	c.lastCPUJiffies = cpuJiffiesState{idleAll: idleAll, total: total, valid: true}
	if !prev.valid || total <= prev.total {
		return 0
	}
	totalDelta := total - prev.total
	idleDelta := idleAll - prev.idleAll
	if idleDelta > totalDelta {
		idleDelta = totalDelta
	}
	pct := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	return math.Round(pct*10) / 10
}

// networkRates computes bytes/sec rx/tx rates from a /proc/net/dev counter
// delta against the previous cycle (Phase 4 collector metrics, 2026-08-12
// — network throughput was entirely absent before this). now is the
// collector's own clock (Options.Now) rather than an assumed fixed poll
// interval, since a skipped or slow cycle would otherwise misreport the
// rate. Returns nil, nil on the first cycle, a failed read, or a counter
// reset (interface flap/replug) — never a rate computed against a stale or
// zero baseline.
func (c *Collector) networkRates(rx, tx uint64, ok bool, now time.Time) (rxRate, txRate *float64) {
	if !ok {
		c.lastNet = netCountersState{}
		return nil, nil
	}
	prev := c.lastNet
	c.lastNet = netCountersState{rxBytes: rx, txBytes: tx, at: now, valid: true}
	if !prev.valid || rx < prev.rxBytes || tx < prev.txBytes {
		return nil, nil
	}
	elapsed := now.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return nil, nil
	}
	rxR := float64(rx-prev.rxBytes) / elapsed
	txR := float64(tx-prev.txBytes) / elapsed
	return &rxR, &txR
}

// unifiedMemoryRSSBytes sums llama-server VmRSS for slots whose loaded mode
// uses backend="rocm" (GGML_CUDA_ENABLE_UNIFIED_MEMORY — HMM-backed host RAM
// the GTT counter cannot see). Only those slots: adding any other slot's RSS
// would double-count memory gtt_used already reports (port of
// engine._get_unified_memory_rss_mb, now bytes — A1 retrofit).
func (c *Collector) unifiedMemoryRSSBytes(cfg *config.Config, slots map[string]SlotAssignment) float64 {
	rocmPorts := map[int]bool{}
	for slotName, a := range slots {
		if a.Mode == "" {
			continue
		}
		slot, ok := cfg.Slots[slotName]
		if !ok {
			continue
		}
		mode, ok := cfg.Modes[a.Mode]
		if !ok || len(mode.Services) == 0 {
			continue
		}
		if mode.Services[0].Backend == "rocm" {
			rocmPorts[slot.Port] = true
		}
	}
	if len(rocmPorts) == 0 {
		return 0
	}
	var total float64
	for _, pid := range c.o.Proc.ByComm("llama-server") {
		if port, ok := c.o.Proc.PortArg(pid); ok && rocmPorts[port] {
			total += c.o.Proc.RSSBytes(pid)
		}
	}
	return total
}

// buildSlots merges config slot facts, engine assignments, and activity
// tracking into Snapshot.Slots.
func (c *Collector) buildSlots(
	cfg *config.Config,
	slots map[string]SlotAssignment,
	now time.Time,
) map[string]SlotState {
	out := map[string]SlotState{}
	portPID := c.llamaServerPIDsByPort()
	for _, name := range orderedSlots(cfg) {
		slot := cfg.Slots[name]
		a := slots[name]
		st := SlotState{
			Slot:      name,
			Label:     slot.Label,
			Unit:      slot.Unit,
			Port:      slot.Port,
			Mode:      a.Mode,
			Loading:   a.Loading,
			Unloading: a.Unloading,
		}
		if t, ok := c.lastActivity[name]; ok {
			st.LastActivity = t
		}
		if a.Mode != "" {
			if pid, ok := portPID[slot.Port]; ok {
				st.MemoryBytes = int64(c.o.Proc.GPUMemoryBytes(pid))
			}
		}
		out[name] = st
	}
	return out
}

// llamaServerPIDsByPort maps each running llama-server process to the port
// it was launched with (one pass over /proc, reused for every slot this
// cycle rather than re-scanning per slot).
func (c *Collector) llamaServerPIDsByPort() map[int]int {
	out := map[int]int{}
	for _, pid := range c.o.Proc.ByComm("llama-server") {
		if port, ok := c.o.Proc.PortArg(pid); ok {
			out[port] = pid
		}
	}
	return out
}

// collectAlerts runs hang detection over this cycle's scrapes and adds the
// GTT high-water warning.
func (c *Collector) collectAlerts(
	cfg *config.Config,
	metrics Metrics,
	scraped map[int]*LlamaMetrics,
	now time.Time,
) []Alert {
	var alerts []Alert
	for port, m := range scraped {
		if a := c.hang.Observe(now, port, m); a != nil {
			alerts = append(alerts, *a)
		}
	}
	// Forget stall state for ports that vanished from the scrape set
	// (unit stopped): a stale timer must not fire on a future reload.
	for port := range c.hang.stalledSince {
		if _, ok := scraped[port]; !ok {
			c.hang.Forget(port)
		}
	}

	if metrics.GTTUsedBytes != nil && metrics.GTTTotalBytes != nil && *metrics.GTTTotalBytes > 0 {
		pct := float64(*metrics.GTTUsedBytes) / float64(*metrics.GTTTotalBytes) * 100
		if pct >= float64(cfg.Monitor.GTTWarnPct) {
			alerts = append(alerts, Alert{
				Code: "GTT_HIGH",
				Msg: fmt.Sprintf("GTT pool at %.0f%% (%d/%d bytes) — above the %d%% warning threshold",
					pct, *metrics.GTTUsedBytes, *metrics.GTTTotalBytes, cfg.Monitor.GTTWarnPct),
			})
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].Code != alerts[j].Code {
			return alerts[i].Code < alerts[j].Code
		}
		return alerts[i].Port < alerts[j].Port
	})
	return alerts
}

// collectSlotErrorAlerts raises SLOT_ERROR_STORM when the a0 router has
// recorded ≥3 5xx/transport failures on a slot port within the last 60s.
// This is the device-lost early-warning the stall detector can't see: a
// wedged llama-server returns 5xx on every request while /health stays green
// (2026-08-16 qwen38-27b). Nil SlotErrorCount seam ⇒ no alerts (stub envs).
func (c *Collector) collectSlotErrorAlerts(cfg *config.Config, now time.Time) []Alert {
	if c.o.SlotErrorCount == nil {
		return nil
	}
	const window = 60 // seconds
	const threshold = 3
	var alerts []Alert
	for _, slot := range cfg.Slots {
		n, _ := c.o.SlotErrorCount(slot.Port, window)
		if n >= threshold {
			alerts = append(alerts, Alert{
				Code: "SLOT_ERROR_STORM",
				Port: slot.Port,
				Msg: fmt.Sprintf(
					"slot %s (port %d): %d 5xx/transport failure(s) in the last %ds — possible GPU device-lost (check journalctl -k for 'ring timeout'/'device wedged')",
					slot.Label, slot.Port, n, window),
			})
		}
	}
	return alerts
}

// unitAlerts detects crashes, OOM kills, and systemd-initiated restarts from
// the Service-interface properties UnitStates now carries (notifications
// sprint — no dmesg/kernel access needed; systemd already tracks all three
// unprivileged). Level-triggered like INFERENCE_HANG: a unit whose Result
// stays "oom-kill" keeps reporting UNIT_OOM every cycle until it's
// restarted — that's intentional, downstream dedup (by unit+invocation)
// collapses repeats into one notification with a bumped count, same as the
// hang alert already relies on.
func (c *Collector) unitAlerts(units map[string]UnitState) []Alert {
	var alerts []Alert
	seenThisCycle := make(map[string]bool, len(units))
	for name, st := range units {
		seenThisCycle[name] = true
		switch st.Result {
		case "oom-kill":
			alerts = append(alerts, Alert{
				Code: "UNIT_OOM",
				Unit: name,
				Msg:  fmt.Sprintf("%s was killed by the OOM killer (systemd Result=oom-kill)", name),
			})
		case "core-dump", "signal", "watchdog", "exit-code":
			alerts = append(alerts, Alert{
				Code: "UNIT_CRASH",
				Unit: name,
				Msg: fmt.Sprintf("%s exited abnormally (systemd Result=%s, ExecMainStatus=%d)",
					name, st.Result, st.ExecMainStatus),
			})
		}

		prev, known := c.lastNRestarts[name]
		if known && st.NRestarts > prev {
			alerts = append(alerts, Alert{
				Code: "UNIT_RESTARTED",
				Unit: name,
				Msg:  fmt.Sprintf("%s was restarted by systemd (NRestarts %d → %d)", name, prev, st.NRestarts),
			})
		}
		c.lastNRestarts[name] = st.NRestarts
	}
	// Drop baselines for units that vanished from this cycle's probe set
	// (config changed, unit renamed) so a stale baseline can't produce a
	// bogus restart alert if the name is ever reused.
	for name := range c.lastNRestarts {
		if !seenThisCycle[name] {
			delete(c.lastNRestarts, name)
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].Code != alerts[j].Code {
			return alerts[i].Code < alerts[j].Code
		}
		return alerts[i].Unit < alerts[j].Unit
	})
	return alerts
}

// recordCompressorSavings scrapes each Compressor proxy's own volatile
// per-process /metrics counters (not the shared-file-contaminated
// headroom_persistent_savings_* family — see docs/v5-headroom-topology.md)
// and reports interval deltas plus the latest lifetime min/max gauges.
func (c *Collector) recordCompressorSavings(ctx context.Context) {
	if c.o.CompressorTargets == nil {
		return
	}
	targets := c.o.CompressorTargets()
	for service, port := range targets {
		if port == 0 {
			continue
		}
		cur, ok := c.llama.scrapeCompressorCounters(ctx, port)
		if !ok {
			continue // proxy stopped / unreachable / old build — skip cycle
		}
		prev, seen := c.lastCompressorCounters[service]
		c.lastCompressorCounters[service] = cur
		if !seen {
			continue // baseline only
		}
		sample := CompressorSample{
			TokensInDelta:                 delta(cur.TokensIn, prev.TokensIn),
			TokensOutDelta:                delta(cur.TokensOut, prev.TokensOut),
			TokensSavedDelta:              delta(cur.TokensSaved, prev.TokensSaved),
			RequestsDelta:                 delta(cur.Requests, prev.Requests),
			RequestsCachedDelta:           delta(cur.RequestsCached, prev.RequestsCached),
			RequestsFailedDelta:           delta(cur.RequestsFailed, prev.RequestsFailed),
			RequestsRateLimitedDelta:      delta(cur.RequestsRateLimited, prev.RequestsRateLimited),
			RequestsTimeoutDelta:          delta(cur.RequestsTimeout, prev.RequestsTimeout),
			RequestsCanceledDelta:         delta(cur.RequestsCanceled, prev.RequestsCanceled),
			FailOpenDelta:                 delta(sumLabelValues(cur.FailOpenByReason), sumLabelValues(prev.FailOpenByReason)),
			TTFBCountDelta:                delta(cur.TTFBCount, prev.TTFBCount),
			TTFBSumMsDelta:                deltaF(cur.TTFBSum, prev.TTFBSum),
			LatencyCountDelta:             delta(cur.LatencyCount, prev.LatencyCount),
			LatencySumMsDelta:             deltaF(cur.LatencySum, prev.LatencySum),
			OverheadCountDelta:            delta(cur.OverheadCount, prev.OverheadCount),
			OverheadSumMsDelta:            deltaF(cur.OverheadSum, prev.OverheadSum),
			RequestsByProviderDelta:       diffLabelMap(cur.RequestsByProvider, prev.RequestsByProvider),
			RequestsByModelDelta:          diffLabelMap(cur.RequestsByModel, prev.RequestsByModel),
			CacheReadTokensDelta:          diffLabelMap(cur.CacheReadTokens, prev.CacheReadTokens),
			CacheWriteTokensDelta:         diffLabelMap(cur.CacheWriteTokens, prev.CacheWriteTokens),
			UncachedTokensDelta:           diffLabelMap(cur.UncachedTokens, prev.UncachedTokens),
			ProviderCacheRequestsDelta:    diffLabelMap(cur.ProviderCacheRequests, prev.ProviderCacheRequests),
			ProviderCacheHitRequestsDelta: diffLabelMap(cur.ProviderCacheHitRequests, prev.ProviderCacheHitRequests),
			CacheBustsDelta:               delta(cur.CacheBusts, prev.CacheBusts),
			CacheBustTokensLostDelta:      delta(cur.CacheBustTokensLost, prev.CacheBustTokensLost),
			TransformTimingSumDelta:       diffLabelMapF(cur.TransformTimingSum, prev.TransformTimingSum),
			TransformTimingCountDelta:     diffLabelMap(cur.TransformTimingCount, prev.TransformTimingCount),
		}
		if cur.TTFBCount > 0 {
			min, max := cur.TTFBMin, cur.TTFBMax
			sample.TTFBMinMsSinceStart, sample.TTFBMaxMsSinceStart = &min, &max
		}
		if cur.LatencyCount > 0 {
			min, max := cur.LatencyMin, cur.LatencyMax
			sample.LatencyMinMsSinceStart, sample.LatencyMaxMsSinceStart = &min, &max
		}
		if cur.OverheadCount > 0 {
			min, max := cur.OverheadMin, cur.OverheadMax
			sample.OverheadMinMsSinceStart, sample.OverheadMaxMsSinceStart = &min, &max
		}
		if len(cur.TransformTimingMax) > 0 {
			sample.TransformTimingMaxSinceStart = copyFloatMap(cur.TransformTimingMax)
		}
		if sample.AllZero() {
			continue // idle proxy: nothing happened this interval
		}
		if c.o.OnCompressorSample != nil {
			c.o.OnCompressorSample(service, sample)
		}
	}
	// Hygiene: a proxy torn down (or renamed) since the last cycle must not
	// diff against a stale baseline if it (or a same-named replacement)
	// reappears later — mirrors lastNRestarts' cleanup for vanished units.
	for service := range c.lastCompressorCounters {
		if _, ok := targets[service]; !ok {
			delete(c.lastCompressorCounters, service)
		}
	}
}

// deltaF is delta's float64 counterpart, for sums that don't represent
// whole-token/request counts (TTFB/latency/overhead ms sums).
func deltaF(current, prev float64) float64 {
	if current >= prev {
		return current - prev
	}
	return current // counter reset: restarted from 0, re-accumulated
}

// sumLabelValues sums all values in a label map into a single float64 —
// used to collapse a labelled counter (e.g. compress_failopen_total{reason})
// into one scalar for delta computation.
func sumLabelValues(m map[string]float64) float64 {
	var s float64
	for _, v := range m {
		s += v
	}
	return s
}

// diffLabelMap computes per-key reset-safe deltas between two labelled
// counter snapshots. A key present only in cur is treated as a fresh
// baseline (delta 0, not fabricated) — matches the top-level "first sighting
// records a baseline" rule; a key that vanished from cur (label combination
// no longer emitted) is simply dropped, never reported as a negative delta.
func diffLabelMap(cur, prev map[string]float64) map[string]int64 {
	if len(cur) == 0 {
		return nil
	}
	out := make(map[string]int64, len(cur))
	for k, v := range cur {
		p, ok := prev[k]
		if !ok {
			continue
		}
		d := delta(v, p)
		if d != 0 {
			out[k] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// diffLabelMapF is the float64 counterpart of diffLabelMap, for labelled
// counter deltas that are float-valued (e.g. compress_transform_timing_ms_sum).
func diffLabelMapF(cur, prev map[string]float64) map[string]float64 {
	if len(cur) == 0 {
		return nil
	}
	out := make(map[string]float64, len(cur))
	for k, v := range cur {
		p, ok := prev[k]
		if !ok {
			continue
		}
		d := deltaF(v, p)
		if d != 0 {
			out[k] = d
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// copyFloatMap returns a shallow copy of m, or nil if empty.
func copyFloatMap(m map[string]float64) map[string]float64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
func (c *Collector) probePorts(cfg *config.Config) map[int]bool {
	dial := c.o.DialPort
	if dial == nil {
		dial = func(port int) bool {
			conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 500*time.Millisecond)
			if err != nil {
				return false
			}
			conn.Close()
			return true
		}
	}
	out := map[int]bool{}
	for _, slot := range cfg.Slots {
		out[slot.Port] = dial(slot.Port)
	}
	for _, port := range cfg.Ports {
		out[port] = dial(port)
	}
	return out
}

// bookmarkHealth computes server-side bookmark health (V4
// monitor._check_bookmark_health): always_online → true; systemd_unit →
// unit active; tailscale_node → LocalAPI peer online. Bookmarks with no
// server-side check get no entry (client probes them).
func (c *Collector) bookmarkHealth(ctx context.Context, units map[string]UnitState) map[string]bool {
	out := map[string]bool{}
	if c.o.Bookmarks == nil {
		return out
	}
	for _, b := range c.o.Bookmarks() {
		switch {
		case b.AlwaysOnline:
			out[b.Label] = true
		case b.Health == "systemd_unit" && b.HealthArg != "":
			out[b.Label] = units[b.HealthArg].Active()
		case b.Health == "tailscale_node" && b.HealthArg != "" && c.o.TailscaleOnline != nil:
			out[b.Label] = c.o.TailscaleOnline(ctx, b.HealthArg)
		}
	}
	return out
}

// updateRings appends this cycle's sparkline samples (gpu/ram/temp, 120
// deep). Unlike V4 — which appended literal 0 when a sensor read failed,
// drawing fake dips — failed probes skip the sample.
func (c *Collector) updateRings(m Metrics) {
	push := func(key string, v float64) {
		ring := append(c.rings[key], v)
		if len(ring) > historyRingSize {
			ring = ring[len(ring)-historyRingSize:]
		}
		c.rings[key] = ring
	}
	if m.GPUUsePct != nil {
		push("gpu", *m.GPUUsePct)
	}
	if m.Memory.TotalBytes > 0 {
		push("ram", m.Memory.Pct)
	}
	if m.TempCelsius != nil {
		push("temp", *m.TempCelsius)
	}
	if m.PackagePowerW != nil {
		push("power", *m.PackagePowerW)
	}
}
