// SPDX-License-Identifier: Apache-2.0

// Package collector owns ALL system probing in forge (V5 design decision 2,
// docs/v5-plan.md). One goroutine loop touches systemd (D-Bus), sysfs, /proc,
// GGUF files, and llama-server /health /props /metrics on a fixed cadence and
// publishes an immutable Snapshot. HTTP handlers, the scheduler, and the
// router only ever read snapshots — nothing outside this package probes.
//
// Contract 2 (docs/v5-go-contracts.md): the types in this file are frozen.
// Additive changes only, signed off by the Phase 9 integration owner.
//
// A1 amendment (2026-07-24, models.toml→DB sprint): the memory fields on
// Metrics / Memory / Disk were renamed in place from *MB to *Bytes and now
// carry the kernel's native unit. This is a contract change — the JSON wire
// keys change from *_mb to *_bytes (httpapi/shapes.go) and the PWA types
// move with them. Signed off during the planning session (see
// docs/v5-modes-config-editable.md §"Post-MODEL-CATALOG sprint" principle 1).
package collector

import "time"

// Snapshot is the immutable result of one collector cycle. The collector
// builds a fresh Snapshot each cycle and swaps the pointer; consumers must
// never mutate a Snapshot or anything reachable from it. Maps are shared —
// treat them as read-only.
type Snapshot struct {
	TakenAt  time.Time
	Hostname string

	// Metrics mirrors GET /api/v1/metrics (Contract 1).
	Metrics Metrics

	// Units maps systemd unit name (without ".service") to its observed
	// state. Includes inference slots, compressor proxies, service modes,
	// infra units, and bookmark health_arg units.
	Units map[string]UnitState

	// Slots maps slot name (config-driven; default a1/a2/a3/a4)
	// to occupancy. A slot whose unit is still `deactivating` is NOT empty —
	// see the crown-jewels list in docs/v5-plan.md.
	Slots map[string]SlotState

	// Inference maps slot name to the last llama-server /metrics + /props
	// scrape for the slot, when one is loaded and responding.
	Inference map[string]SlotInference

	// Ports maps configured port number to "something is listening".
	Ports map[int]bool

	// BookmarkHealth maps bookmark label to computed health for bookmarks
	// with a server-side health check (systemd_unit / tailscale_node).
	BookmarkHealth map[string]bool

	// Alerts carries active monitor alerts (hang detection, GTT warnings).
	Alerts []Alert

	// Compressors maps Compressor proxy service name (the same keys as
	// CompressorTargets/CompressorUnits) to its collector-observed resource
	// state (Sprint 4, resource-bounding + monitoring). Sourced entirely
	// from systemd + /proc, unconditionally every cycle — unlike
	// recordCompressorSavings' CompressorSample, which is skipped on a failed
	// /metrics scrape or an idle interval, exactly the moments a process
	// health signal matters most.
	Compressors map[string]CompressorState
}

// CompressorState is one Compressor-shaped proxy's (headroom@* or
// forge-compress@*) observed resource state: is its unit active, what's
// its main process's RSS, and has systemd restarted it. Not part of
// Contract 2's original set — added additively for Sprint 4.
type CompressorState struct {
	Unit      string
	Port      int
	Up        bool
	MainPID   uint32
	RSSBytes  int64
	NRestarts uint32
	Result    string
}

// Metrics mirrors the GET /api/v1/metrics response body. Pointer fields are
// nullable in the JSON contract ("may be null" — sensor absent or probe
// failed this cycle).
//
// A1 (bytes retrofit, 2026-07-24): every memory field is now bytes (the
// kernel/sysfs native unit). Display boundaries scale to MB/GB. The field
// names carry the unit explicitly to prevent conflation.
type Metrics struct {
	Mode   string
	Memory Memory
	CPU    CPU
	Disk   Disk

	GPUUsePct     *float64
	GTTUsedBytes  *int64
	GTTTotalBytes *int64
	TempCelsius   *float64
	UptimeSeconds *int64

	// PackagePowerW is the amdgpu PPT rail in watts (nil = sensor absent).
	// NOT wall power — see config.Cost.OverheadW/PSUEfficiency, which
	// approximate the rest-of-system draw and PSU loss this reading
	// excludes. Additive Contract 2 change, cost/savings sprint 2026-07-30.
	PackagePowerW *float64

	// InferenceRSSBytes = gtt_used + unified_rss(ROCm-unified slots only).
	// ADD, never max() — see docs/pitfalls.md and the crown-jewels list.
	InferenceRSSBytes *int64
	ModelWeightsBytes *int64
	KVCacheBytes      *int64

	// NetRxBytesPerSec/NetTxBytesPerSec (Phase 4 collector metrics,
	// 2026-08-12) are computed from a diff of two /proc/net/dev cumulative
	// counter reads — nil on the collector's first cycle (no prior sample
	// to diff against) or if the probe failed, never a false 0.
	NetRxBytesPerSec *float64
	NetTxBytesPerSec *float64

	// Storage is per-mount storage (root/models/state — see
	// sampleStorageMounts) additive alongside the original single-volume
	// Disk field above, which stays wired to ModelsDir for back-compat.
	Storage []StorageMount

	// GPUJunctionTempC/CPUPackageTempC/NVMeTempC (Phase 4 collector
	// metrics) are additional hwmon channels beyond the original GPU edge
	// TempCelsius above — nil when the sensor/driver isn't present.
	GPUJunctionTempC *float64
	CPUPackageTempC  *float64
	NVMeTempC        *float64
}

type Memory struct {
	TotalBytes int64
	UsedBytes  int64
	AvailBytes int64
	Pct        float64
}

type CPU struct {
	Load1 float64
	Pct   float64
}

// Disk mirrors the models/data volume free/total/used/pct sample (Sprint 0
// §0.4, BE-1) — the frozen metricsDisk JSON shape (httpapi/shapes.go). Zero
// value when the probe fails or Paths.ModelsDir is unset; disk is a
// non-pointer field like Memory/CPU (the contract emits the zero value, not
// null, when unsampled — see shapes.go's metricsResponse doc comment).
type Disk struct {
	TotalBytes int64
	FreeBytes  int64
	UsedBytes  int64
	Pct        float64
}

// StorageMount is one named mount's Disk sample (Phase 4 collector metrics,
// 2026-08-12 — see sampleStorageMounts's doc comment for which paths are
// sampled and why).
type StorageMount struct {
	Name string
	Path string
	Disk Disk
}

// UnitState is one systemd unit's observed state via D-Bus.
type UnitState struct {
	// ActiveState is systemd's ActiveState: active, inactive, activating,
	// deactivating, failed, reloading.
	ActiveState string
	// SubState is systemd's SubState (running, exited, stop-sigterm, ...).
	SubState string
	// Since is when the unit entered ActiveState.
	Since time.Time

	// Result, NRestarts, ExecMainStatus, and InvocationID are Service-
	// interface properties (notifications sprint, 2026-07-29 — crash/OOM
	// detection). They're absent (zero value) for non-service unit types.
	// Result mirrors `systemctl show -p Result`: "success", "exit-code",
	// "signal", "core-dump", "watchdog", "oom-kill", "start-limit-hit", etc.
	Result string
	// NRestarts is systemd's own restart counter for the unit (bumped by
	// Restart= policy, not by anything forge does). A cycle-over-cycle
	// increase means systemd restarted the unit on its own since we last
	// looked — see the Collector's lastNRestarts tracking in run.go.
	NRestarts uint32
	// ExecMainStatus is the exit code of the unit's main process from its
	// last run (only meaningful once Result is non-empty).
	ExecMainStatus int32
	// InvocationID is systemd's per-invocation UUID (hex-encoded), unique
	// to one run of the unit. Distinguishes "still the same failed
	// invocation we already reported" from "this is a new run that also
	// happened to fail" without needing our own generation counter.
	InvocationID string
	// MainPID is the unit's current main process PID (0 if not running).
	// Sourced from the same Service-interface GetAllPropertiesContext call
	// as Result/NRestarts/ExecMainStatus — no extra D-Bus round trip.
	// Added for compressor health sampling (Sprint 4): forge-compress@*
	// units have no --port cmdline flag to grep for (their port comes from
	// the COMPRESS_PORT env var), so the existing Proc.PortArg-based PID
	// lookup used for llama-server slots doesn't find them; MainPID is a
	// direct, per-unit source of truth instead.
	MainPID uint32
	// GPUBytes is the main process's real GPU memory footprint (VRAM+GTT,
	// Proc.GPUMemoryBytes fdinfo accounting, deduped by drm-client-id).
	// Populated for every watched unit with a live MainPID (S2: Resources-
	// tab attribution — with all slots unloaded this is what names ComfyUI
	// and the always-on services as the holders of "missing" GTT). 0 when
	// the unit isn't running or nothing is measurable. Slot units carry it
	// too, but the canonical per-slot figure stays SlotState.MemoryBytes.
	GPUBytes int64
}

// Active reports whether the unit is fully active.
func (u UnitState) Active() bool { return u.ActiveState == "active" }

// Deactivating reports whether the unit is still shutting down. Slot state
// must not clear, and no load may be placed, while this is true
// (TimeoutStopSec=300-class unloads take minutes).
func (u UnitState) Deactivating() bool { return u.ActiveState == "deactivating" }

// SlotState is one inference slot's occupancy as observed by the collector.
type SlotState struct {
	Slot  string // slot name, e.g. "a1"
	Label string // display label, e.g. "A1"
	Unit  string // systemd unit name without .service
	Port  int

	// Mode is the mode loaded in this slot, "" when empty. Remains set
	// while the old unit is deactivating.
	Mode string

	// Loading/Unloading are non-nil while a transition is in progress.
	Loading   *Transition
	Unloading *Transition

	// LastActivity is the last time the slot served tokens (drives
	// idle-aware eviction). Zero when unknown.
	LastActivity time.Time

	// MemoryBytes is this slot's real live GPU memory footprint (VRAM+GTT,
	// via Proc.GPUMemoryBytes on the slot's llama-server PID) — includes KV
	// cache, activation buffers, everything the process actually holds, not
	// just its curated "weights on disk" estimate. Zero when Mode is empty
	// or the process/fdinfo couldn't be read (non-amdgpu, PID not found).
	MemoryBytes int64
}

// Transition mirrors the slot_loading/slot_unloading entries in the
// GET /api/v1/status response.
type Transition struct {
	Mode      string
	StartedAt time.Time
}

// SlotInference is one llama-server scrape for a loaded slot.
type SlotInference struct {
	// NCtx is the *actual* context from /props
	// (default_generation_settings.n_ctx) — the kernel may silently lower
	// it; always record and compare against configured (crown jewels).
	NCtx int

	// ModelAlias/ModelPath are the actually-running llama-server process's
	// own self-report from /props (top-level model_alias/model_path) — the
	// ground truth for what's really loaded, independent of the engine's
	// env-file-derived belief. Verified once per slot session, same as
	// NCtx (see run.go's nctxCache). Empty when unscraped or the field is
	// absent (older llama.cpp builds without model_alias in /props).
	ModelAlias string
	ModelPath  string

	RequestsProcessing   int
	PromptTokensTotal    int64
	PredictedTokensTotal int64
	TokensPerSecond      float64

	// SlotErrors is the a0 router's 5xx/transport failure count for this
	// slot within the last 60s (router.SlotErrorCount). A device-lost slot
	// 5xxes every request while /health stays green, so this is the
	// "health-green but actually dead" indicator gpu_hang can't see.
	// 0 when the router seam isn't wired.
	SlotErrors int
}

// Alert mirrors the alerts entries in GET /api/v1/status.
type Alert struct {
	Code string
	Msg  string
	Port int    // 0 when not port-scoped
	Unit string // "" when not unit-scoped (notifications sprint)
}
