// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// SlotProbe is the result of probing a foundry_slot backend's raw
// llama-server port. It mirrors V4 router_catalog._probe_slot's return shape.
// Never carries an error — any failure is reported as healthy=false so the
// router's skip-on-failure logic takes over (the mode-switch dead-window
// fix).
type SlotProbe struct {
	Healthy   bool
	NCtx      int
	ModelPath string
}

// SlotCatalog answers health/busy/wire-model questions about forge-local
// slots. In V4 this is router_catalog.py (TTL-cached HTTP probing of
// /health + /props + /metrics). In V5 the collector (Phase 2, track A) owns
// all probing per design decision 2 — but internal/router is not permitted
// to import internal/collector (Contract 2 ownership table), so this
// interface is the seam. Phase 9 swaps the default ttlCatalog below for a
// collector-snapshot-backed implementation.
type SlotCatalog interface {
	// Probe returns the cached-or-fresh probe for a slot's raw port.
	Probe(port int, ttl time.Duration) SlotProbe
	// IsBusy reports whether the slot is mid-generation
	// (llamacpp:requests_processing > 0 via /metrics). Only consulted when
	// busy_mode == fail_fast.
	IsBusy(port int, ttl time.Duration) bool
}

// ttlCatalog is the default SlotCatalog: V4-style on-demand HTTP probing with
// a short TTL cache so the /v1/models list and the router's per-request gate
// don't double-probe during a mode-switch dead window.
type ttlCatalog struct {
	client  *http.Client
	cacheMu sync.Mutex
	health  map[int]slotCacheEntry
	busyMu  sync.Mutex
	busy    map[int]busyCacheEntry
}

type slotCacheEntry struct {
	expires time.Time
	probe   SlotProbe
}

type busyCacheEntry struct {
	expires time.Time
	busy    bool
}

const (
	healthTimeout  = 2 * time.Second // matches engine.py's /health poll timeout
	propsTimeout   = 5 * time.Second // matches engine.py's /props timeout
	metricsTimeout = 3 * time.Second // matches monitor.py's /metrics poll timeout
)

// newTTLCatalog returns a SlotCatalog that probes llama-server directly.
func newTTLCatalog(client *http.Client) *ttlCatalog {
	if client == nil {
		client = &http.Client{}
	}
	return &ttlCatalog{
		client: client,
		health: make(map[int]slotCacheEntry),
		busy:   make(map[int]busyCacheEntry),
	}
}

func (c *ttlCatalog) Probe(port int, ttl time.Duration) SlotProbe {
	now := time.Now()
	c.cacheMu.Lock()
	if e, ok := c.health[port]; ok && e.expires.After(now) {
		c.cacheMu.Unlock()
		return e.probe
	}
	c.cacheMu.Unlock()

	probe := c.probeSlot(port)

	c.cacheMu.Lock()
	c.health[port] = slotCacheEntry{expires: now.Add(ttl), probe: probe}
	c.cacheMu.Unlock()
	return probe
}

func (c *ttlCatalog) IsBusy(port int, ttl time.Duration) bool {
	now := time.Now()
	c.busyMu.Lock()
	if e, ok := c.busy[port]; ok && e.expires.After(now) {
		c.busyMu.Unlock()
		return e.busy
	}
	c.busyMu.Unlock()

	busy := c.probeBusy(port)

	c.busyMu.Lock()
	c.busy[port] = busyCacheEntry{expires: now.Add(ttl), busy: busy}
	c.busyMu.Unlock()
	return busy
}

// probeSlot queries /health then /props on a forge-local llama.cpp slot.
// Never returns an error — any failure (connection refused during a mode
// switch, timeout, malformed body) is reported as unhealthy so the router's
// skip-on-failure logic takes over.
func (c *ttlCatalog) probeSlot(port int) SlotProbe {
	if !c.slotHealthy(port) {
		return SlotProbe{}
	}
	nCtx, modelPath := c.slotProps(port)
	return SlotProbe{Healthy: true, NCtx: nCtx, ModelPath: modelPath}
}

// slotHealthy returns true if /health responds with {"status": "ok"} (or
// empty body — vLLM returns {} on a 200). Any failure is false.
func (c *ttlCatalog) slotHealthy(port int) bool {
	raw, err := c.getRaw(port, "/health", healthTimeout)
	if err != nil {
		return false
	}
	var status struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(raw, &status)
	// Empty/missing status = ok (vLLM); explicit non-ok = unhealthy.
	return status.Status == "" || status.Status == "ok"
}

// slotProps reads /props for n_ctx + model_path. Best-effort: /health
// already confirmed the slot is up; /props failure leaves them zero/empty.
func (c *ttlCatalog) slotProps(port int) (nCtx int, modelPath string) {
	raw, err := c.getRaw(port, "/props", propsTimeout)
	if err != nil {
		return 0, ""
	}
	var top struct {
		NCtx       int    `json:"n_ctx"`
		ModelPath  string `json:"model_path"`
		DefaultGen struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	_ = json.Unmarshal(raw, &top)
	if top.NCtx == 0 {
		nCtx = top.DefaultGen.NCtx
	} else {
		nCtx = top.NCtx
	}
	return nCtx, top.ModelPath
}

// probeBusy returns true if llama-server is currently processing a request.
// Same signal monitor.py's hang detector uses (llamacpp:requests_processing
// from /metrics). Any failure is reported as "not busy" (fail toward the
// existing wait-mode behavior) — a probe failure here is a /health problem,
// already handled separately.
func (c *ttlCatalog) probeBusy(port int) bool {
	raw, err := c.getRaw(port, "/metrics", metricsTimeout)
	if err != nil {
		return false
	}
	return parsePromScalar(string(raw), "llamacpp:requests_processing") > 0
}

func (c *ttlCatalog) getRaw(port int, path string, timeout time.Duration) ([]byte, error) {
	// Use a shallow copy so we don't mutate the shared client's Timeout.
	cl := *c.client
	cl.Timeout = timeout
	resp, err := cl.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errStatus(resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// errStatus wraps a non-200 status code from a probe.
type errStatus int

func (e errStatus) Error() string { return "router: probe HTTP " + strconv.Itoa(int(e)) }

// parsePromScalar returns the first scalar value for a metric name in
// Prometheus text output. Identical to monitor.py's _parse_prom_scalar and
// router_catalog._parse_prom_scalar — duplicated rather than imported since
// the monitor pulls in dashboard-only state.
func parsePromScalar(text, name string) float64 {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, name+" ") || strings.HasPrefix(line, name+"{") {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			if v, err := strconv.ParseFloat(fields[len(fields)-1], 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// ModelsResponse is the OpenAI-shaped /v1/models payload.
type ModelsResponse struct {
	Object string       `json:"object"`
	Data   []ModelEntry `json:"data"`
}

// ModelEntry is one entry in the /v1/models list.
type ModelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created"`
	OwnedBy       string `json:"owned_by"`
	ContextLength int    `json:"context_length,omitempty"`
}

// BuildModelsResponse builds an OpenAI-shaped /v1/models payload — one entry
// per model/Config, regardless of current load/health state (F1 fix: a
// consumer must be able to *see* a configured-but-unloaded model in order to
// request it and trigger the on-demand load path — see ensureBackendLoaded
// in routing.go). Entirely store-backed (TOML decommission Phase 3,
// docs/v5-toml-decommission.md §6): the file-route listing (RouterConfig.Routes
// + a live slot probe for each one) was deleted once `routing_routes` became
// the one source of local/remote model identity — there's no second source
// left to dedup against. storeCat nil (store unwired) yields an empty list.
//
// Multi-provider dedup (2026-08-06): offerings of the same catalog Model are
// ONE entry — the same model on two providers must not show up twice (the
// glm-5.2-on-aiand-and-qwen case). The entry is the group's PRIMARY: the
// enabled offering of an enabled provider with the lowest priority value,
// mirroring offeringChain exactly, so what's listed and what gets served
// never disagree (the primary's wire_model is the listed ID; the other
// providers' wire names still route as aliases converging on the same
// primary). providers nil (provider store unwired) skips the enablement
// filter — pre-0032 behavior for skeleton mode.
func BuildModelsResponse(ctx context.Context, storeCat store.Catalog, providers []store.ProviderRow) ModelsResponse {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	data := make([]ModelEntry, 0)
	seen := map[string]bool{}

	// Store-backed Offerings (MODEL CATALOG Phase 2). Selection (which
	// offering is each model group's primary) is shared with offeringChain
	// and the routing-preview endpoint — see select.go — so what's listed
	// here and what actually gets served can never disagree.
	if storeCat != nil {
		offerings, err := storeCat.ListOfferings(ctx)
		if err == nil {
			enabledByID := map[int64]bool{}
			if providers == nil {
				// Skeleton/test mode: no provider store wired — pre-0032
				// behavior was to skip the enablement filter entirely.
				// SelectOfferingChain has no permissive default (it fails
				// closed, correctly, for live routing), so this caller
				// builds its own "everyone enabled" map instead.
				for _, o := range offerings {
					enabledByID[o.ProviderID] = true
				}
			} else {
				for _, p := range providers {
					if p.Enabled {
						enabledByID[p.ID] = true
					}
				}
			}

			groups := GroupOfferingsByModel(offerings)
			// Model listing order: first-encountered model id in
			// ListOfferings' own row order (lowest-priority-offering-first
			// globally) — preserves the pre-extraction listing order.
			var groupOrder []int64
			seenGroup := map[int64]bool{}
			for _, o := range offerings {
				if !seenGroup[o.ModelID] {
					seenGroup[o.ModelID] = true
					groupOrder = append(groupOrder, o.ModelID)
				}
			}

			now := time.Now().Unix()
			for _, modelID := range groupOrder {
				primary := SelectOfferingChain(groups[modelID], enabledByID, false)
				if len(primary) == 0 {
					continue
				}
				o := primary[0]
				if seen[o.WireModel] {
					continue
				}
				entry := ModelEntry{
					ID:      o.WireModel,
					Object:  "model",
					Created: now,
					OwnedBy: o.ProviderName,
				}
				if o.ContextLength > 0 {
					entry.ContextLength = o.ContextLength
				}
				data = append(data, entry)
				seen[o.WireModel] = true
			}
		}
	}

	// Store-backed local Configs (a0 local-config visibility fix). Every
	// visible catalog Config is listed by name regardless of load state —
	// the F1 on-demand-load intent applies here too: a consumer must be
	// able to see a configured-but-unloaded local model to request it and
	// trigger the on-demand load path (see catalogChain in routing.go).
	// ContextLength comes straight from the Config (not a slot probe) since
	// the config may not be loaded anywhere right now.
	if storeCat != nil {
		configs, err := storeCat.ListConfigs(ctx)
		if err == nil {
			now := time.Now().Unix()
			for _, c := range configs {
				if c.Visibility == "hidden" {
					continue
				}
				if seen[c.Name] {
					continue
				}
				entry := ModelEntry{
					ID:      c.Name,
					Object:  "model",
					Created: now,
					OwnedBy: "forge-local",
				}
				if c.NCtx > 0 {
					entry.ContextLength = c.NCtx
				}
				data = append(data, entry)
				seen[c.Name] = true
			}
		}
	}

	return ModelsResponse{Object: "list", Data: data}
}

// probePort is the port that reflects a foundry_slot backend's actual slot
// liveness — the raw llama-server port.
func probePort(b *Backend) int {
	if b == nil {
		return 0
	}
	switch b.Kind {
	case "foundry_slot":
		return b.Port
	default:
		return 0 // remote — no probe
	}
}
