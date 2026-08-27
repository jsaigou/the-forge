// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/activity"
	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/sched"
	"github.com/jsaigou/the-forge/internal/store"
)

// ResolvedBackend bundles the atomic (base_url, api_key, wire_model) triple
// for one backend record. This is the structural fix for the hermes-agent
// credential-pool bug (NousResearch/hermes-agent#56374) — there is no code
// path that resolves a backend's base_url/credential independently of its
// wire_model. V4 reference: router.ResolvedBackend in forge/router.py.
type ResolvedBackend struct {
	Name      string
	BaseURL   string // e.g. "http://127.0.0.1:8080/v1" or "https://api.deepseek.com/v1"
	APIKey    string // "" for foundry_slot backends (no auth needed locally)
	WireModel string // the model string this backend actually expects
	// Provider is the remote backend's credential name ("" for foundry_slot
	// backends). Surfaced for usage attribution; not used for routing.
	Provider string
	// ProviderID is the real FK to write for usage attribution (Phase 6
	// surrogate-key migration, 0042) — 0 for foundry_slot backends, same
	// "" == "no provider" convention Provider already used.
	ProviderID int64

	// Pricing (cost/savings sprint Phase 4, 2026-07-30) — copied straight
	// from the matching Backend (see Backend's own doc comment); PriceCurrency
	// == "" means "no offering matched, don't compute a cost."
	PriceInPer1M       float64
	PriceOutPer1M      float64
	PriceCachedInPer1M *float64
	PriceCurrency      string
	// UpstreamOverride, when non-empty, is sent as the x-compress-base-url
	// request header (docs/v5-headroom-topology.md §3/§4): the shared local
	// Compressor proxy (BaseURL) honors it to route this specific request to
	// the real slot, chosen per-request rather than baked into the proxy's
	// own fixed upstream. Always server-derived (from the resolved slot's own
	// address) — never anything that could carry request-controlled data; see
	// §5b for why that distinction matters (SSRF hardening on this header).
	// Format: the bare slot ROOT without a /v1 suffix — headroom-ai ≥0.35.0
	// appends /v1 itself (sending /v1 doubles the path; see resolveBackend).
	//
	// Also used on the remote side (Sprint 3, docs/v5-headroom-replacement.md)
	// when a provider is fronted by the shared "external" instance rather
	// than a dedicated per-provider proxy: unlike a dedicated proxy (whose
	// real upstream is baked into its own env file at provision time), the
	// shared instance fronts several providers with different real
	// upstreams, so which one applies must travel with the request — same
	// mechanism, same bare-origin-no-/v1 format, set from provider.TargetURL
	// instead of a slot address. Empty for a dedicated per-provider proxy
	// (its fixed OPENAI_TARGET_API_URL already has the answer) and for the
	// bypassed/no-proxy cases (BaseURL is already the real upstream).
	UpstreamOverride string

	// CompressorFronted (Sprint 8, auto-bypass) reports whether BaseURL
	// actually points at a forge-compress process for this attempt —
	// local shared, remote dedicated, or remote shared "external" — as
	// opposed to already being the real upstream (bypassed, or no proxy
	// linked at all). Deliberately NOT derived from UpstreamOverride != ""
	// the way proxy.go's earlier compressorFronted local variable was: that
	// missed the remote-dedicated-proxy case (BaseURL is the proxy but
	// UpstreamOverride is empty, since a dedicated proxy needs no per-request
	// header — its target is baked into its own env file), which meant a
	// dedicated proxy's transport failure was misclassified as "backend"
	// instead of "compressor". Set explicitly below in both switch cases.
	CompressorFronted bool
	// DirectUpstreamURL is the real backend address to retry against when
	// CompressorFronted is true and the compressor attempt fails at the
	// connection level (Sprint 8) — the slot's own /v1 base for foundry_slot,
	// or provider.TargetURL for remote. Empty whenever CompressorFronted is
	// false (nothing to bypass to that BaseURL doesn't already point at).
	DirectUpstreamURL string
}

// gateReason is the skip reason from slotGate. Empty string = attemptable.
type gateReason string

const (
	gateUnhealthy gateReason = "unhealthy"
	gateBusy      gateReason = "busy"
)

// slotGate returns a skip reason for foundry_slot backends, or "" if the
// backend is attemptable.
//
// "unhealthy" — /health is failing (mid ai-mode switch, crashed, etc.) —
// always skipped immediately; this is the fix for the mode-switch dead
// window.
//
// "busy" — the slot is alive but mid-generation on another request. Only
// gates when busyMode == fail_fast (default "wait": a busy-but-alive slot
// is attempted and the request queues at llama-server's own --parallel 1
// slot).
//
// Remote backends are never gated this way — there is no cheap local
// liveness signal for them; a failed request surfaces directly through the
// proxy's retry handling instead.
func slotGate(b *Backend, cfg *RouterConfig, cat SlotCatalog, busyMode BusyMode) gateReason {
	if cat == nil {
		return ""
	}
	port := probePort(b)
	if port == 0 {
		return "" // remote — no gate
	}
	ttl := cfg.healthTTL()
	if !cat.Probe(port, ttl).Healthy {
		return gateUnhealthy
	}
	if busyMode == BusyFailFast && cat.IsBusy(port, ttl) {
		return gateBusy
	}
	return ""
}

// resolveBackend atomically resolves (base_url, api_key, wire_model) for one
// backend record. Callers must have already checked slotGate for
// foundry_slot backends — this function assumes the slot is attemptable and
// only does resolution.
//
// Compressor passthrough (Phase 8 of V4): if the service is bypassed (globally
// or per-proxy), remote backends route to the provider's real TargetURL
// instead of the compressor proxy base_url. Per docs/llm-router.md "Compressor
// Passthrough" — the get_provider_credential() base_url trap is avoided by
// reading TargetURL only through this gated path.
//
// Local Compressor fronting (docs/v5-headroom-topology.md, added post-§11):
// foundry_slot routes through the shared "local" proxy when
// localCompressorBaseURL reports one (enabled + linked + not bypassed),
// setting UpstreamOverride to the real slot address for the proxy.go Rewrite
// hook to send as x-compress-base-url. This resolves fresh every request —
// same as the slot address itself — so it never pins the proxy to a
// physical slot; see catalogChain's doc comment for why that matters.
func (s *Server) resolveBackend(ctx context.Context, b *Backend) (ResolvedBackend, error) {
	switch b.Kind {
	case "foundry_slot":
		probe := s.catalog().Probe(b.Port, s.cfg().healthTTL())
		wireModel := firstNonEmpty(probe.ModelPath, b.WireModel)
		slotRoot := "http://127.0.0.1:" + strconv.Itoa(b.Port)
		slotBaseURL := slotRoot + "/v1"
		if proxyBaseURL, ok := s.localCompressorBaseURL(ctx); ok {
			return ResolvedBackend{
				Name:    b.Name,
				BaseURL: proxyBaseURL,
				// headroom-ai ≥0.35.0 appends "/v1" to x-compress-base-url
				// itself before forwarding, so send the bare slot ROOT here —
				// the pre-0.35 contract (base WITH /v1, per
				// docs/v5-headroom-topology.md §4) now doubles to
				// /v1/v1/chat/completions and the slot 404s "File Not Found".
				UpstreamOverride: slotRoot,
				WireModel:        wireModel,
				// Sprint 8: the real slot is already known here — reuse it as
				// the auto-bypass target if the shared proxy turns out to be
				// unreachable, rather than re-deriving it in proxy.go.
				CompressorFronted: true,
				DirectUpstreamURL: slotBaseURL,
			}, nil
		}
		return ResolvedBackend{
			Name:      b.Name,
			BaseURL:   slotBaseURL,
			WireModel: wireModel,
		}, nil

	case "remote":
		provider, ok := s.providerCredential(ctx, b.Credential)
		if !ok {
			return ResolvedBackend{}, fmt.Errorf("router: provider %q not found", b.Credential)
		}
		baseURL := b.BaseURL
		upstreamOverride := ""
		// hasProxy tells us whether b.BaseURL (resolved once when
		// offeringChain built this Backend) actually points at a Compressor
		// proxy — dedicated or the shared "external" instance — as opposed
		// to already being the provider's real upstream (no proxy at
		// all). Only when there IS a proxy does a bypass mean anything;
		// re-deriving which service it is here (rather than trusting a
		// value cached on Backend) keeps this fresh every request, same
		// as the foundry_slot case's localCompressorBaseURL call above.
		_, compressorProxy, hasProxy := s.remoteCompressorBaseURL(ctx, provider.ID)
		compressorFronted := false
		switch {
		case hasProxy && s.compressorBypassed(ctx, compressorProxy):
			// Phase 8 passthrough: route to the real upstream, not Compressor.
			if provider.TargetURL != "" {
				baseURL = provider.TargetURL
			}
		case hasProxy && compressorProxy == externalCompressorService:
			// The shared external instance has no single fixed upstream
			// baked into its own env file (unlike a dedicated per-provider
			// proxy) — it fronts several providers with different real
			// targets, so this request must carry its own, the same way
			// the local-fronting path carries a per-request slot address.
			upstreamOverride = strings.TrimSuffix(strings.TrimRight(provider.TargetURL, "/"), "/v1")
			compressorFronted = true
		case hasProxy:
			// A dedicated per-provider proxy: BaseURL (above) is already the
			// proxy's own loopback address, baked in at provision time — no
			// per-request override header needed. Still Compressor-fronted
			// (Sprint 8) even though UpstreamOverride stays empty.
			compressorFronted = true
		}
		// Sprint 8: only meaningful (and only set) when compressorFronted —
		// the bypassed case above already routes BaseURL straight to
		// provider.TargetURL, so there's nothing to bypass to.
		directUpstreamURL := ""
		if compressorFronted {
			directUpstreamURL = provider.TargetURL
		}
		return ResolvedBackend{
			Name:               b.Name,
			BaseURL:            baseURL,
			APIKey:             provider.APIKey,
			WireModel:          b.WireModel,
			Provider:           provider.Name,
			ProviderID:         provider.ID,
			PriceInPer1M:       b.PriceInPer1M,
			PriceOutPer1M:      b.PriceOutPer1M,
			PriceCachedInPer1M: b.PriceCachedInPer1M,
			PriceCurrency:      b.PriceCurrency,
			UpstreamOverride:   upstreamOverride,
			CompressorFronted:  compressorFronted,
			DirectUpstreamURL:  directUpstreamURL,
		}, nil

	default:
		return ResolvedBackend{}, fmt.Errorf("router: unknown backend kind %q", b.Kind)
	}
}

// compressorBypassed reports whether the named Compressor service is bypassed
// (Phase 8 passthrough). Global master switch OR this specific service.
// Per-service isolation: bypassing one proxy leaves every other proxy's
// routing untouched (CLAUDE.local.md hard requirement).
func (s *Server) compressorBypassed(ctx context.Context, service string) bool {
	if s.passthroughAll(ctx) {
		return true
	}
	if service == "" {
		return false
	}
	// V5: per-proxy Passthrough flag on store.Routing.ProxyRow.
	if hp := s.deps.Routing; hp != nil {
		proxies, err := hp.Proxies(ctx)
		if err == nil {
			for _, p := range proxies {
				if p.Service == service && p.Passthrough {
					return true
				}
			}
		}
	}
	// V4 compat: compressor_passthrough_services list in router config (config
	// file fallback when the store isn't wired — skeleton/tests).
	for _, svc := range s.cfg().CompressorPassthroughServices {
		if svc == service {
			return true
		}
	}
	return false
}

// localCompressorService is the store.Routing.ProxyRow.Service name for the
// single shared local proxy (docs/v5-headroom-topology.md §4) — unit
// service "local" (unit forge-compress@local), fronting all 4 A1-A4 slots. A constant, not configurable
// per-Config: the whole point of the "one proxy per scope" topology is one
// process for every local slot, chosen per-request via UpstreamOverride
// rather than one proxy per Config.
const localCompressorService = "local"

// localCompressorEnabled reads compressor.local_enabled from the store's
// Settings KV — deliberately separate from whether a "local" ProxyRow
// exists, so seeding that row (Phase 1's one-time manual verification step)
// doesn't turn routing on by itself. Default false (unset / store not wired),
// matching the passthroughAll/busyMode precedent below.
func (s *Server) localCompressorEnabled(ctx context.Context) bool {
	if st := s.deps.Settings; st != nil {
		raw, err := st.Get(ctx, "compressor.local_enabled")
		if err == nil {
			var v bool
			if err := json.Unmarshal(raw, &v); err == nil {
				return v
			}
		}
	}
	return false
}

// localCompressorBaseURL returns the loopback base URL of the shared local
// Compressor proxy, if local fronting is enabled, the "local" ProxyRow exists,
// and it isn't bypassed (global passthrough_all, or the "local" service
// specifically). ok=false means "route straight to the slot" — the caller's
// existing fallback.
func (s *Server) localCompressorBaseURL(ctx context.Context) (string, bool) {
	if !s.localCompressorEnabled(ctx) {
		return "", false
	}
	if s.compressorBypassed(ctx, localCompressorService) {
		return "", false
	}
	hp := s.deps.Routing
	if hp == nil {
		return "", false
	}
	proxies, err := hp.Proxies(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range proxies {
		if p.Service == localCompressorService && p.OrphanedAt.IsZero() {
			return "http://127.0.0.1:" + strconv.Itoa(p.Port) + "/v1", true
		}
	}
	return "", false
}

// externalCompressorService is the store.Routing.ProxyRow.Service name for
// the single shared "external" proxy (Sprint 3, docs/v5-headroom-replacement.md
// Sprint 3's "Store / provisioning / routing changes" section): one
// forge-compress@external instance fronting every remote provider that
// has no dedicated per-provider proxy of its own. Mirrors localCompressorService's
// "one proxy per scope" topology on the remote side — a constant, not
// configurable per-provider, since the whole point is one process for
// every un-dedicated provider, never one proxy per provider.
const externalCompressorService = "external"

// externalCompressorEnabled reads compressor.external_enabled from the store's
// Settings KV — deliberately separate from whether an "external" ProxyRow
// exists, mirroring localCompressorEnabled's own doc comment (so seeding
// that row doesn't turn routing on by itself). Default false (unset /
// store not wired).
func (s *Server) externalCompressorEnabled(ctx context.Context) bool {
	if st := s.deps.Settings; st != nil {
		raw, err := st.Get(ctx, "compressor.external_enabled")
		if err == nil {
			var v bool
			if err := json.Unmarshal(raw, &v); err == nil {
				return v
			}
		}
	}
	return false
}

// externalCompressorBaseURL returns the loopback base URL of the shared
// "external" Compressor proxy, if external fronting is enabled, the
// "external" ProxyRow exists, and it isn't bypassed (global passthrough_all,
// or the "external" service specifically). ok=false means "no shared
// external proxy available" — callers fall back to whatever they already
// do without one (a per-provider dedicated proxy if linked, or the
// provider's real upstream directly).
func (s *Server) externalCompressorBaseURL(ctx context.Context) (string, bool) {
	if !s.externalCompressorEnabled(ctx) {
		return "", false
	}
	if s.compressorBypassed(ctx, externalCompressorService) {
		return "", false
	}
	hp := s.deps.Routing
	if hp == nil {
		return "", false
	}
	proxies, err := hp.Proxies(ctx)
	if err != nil {
		return "", false
	}
	for _, p := range proxies {
		if p.Service == externalCompressorService && p.OrphanedAt.IsZero() {
			return "http://127.0.0.1:" + strconv.Itoa(p.Port) + "/v1", true
		}
	}
	return "", false
}

// remoteCompressorService returns the Compressor service name (e.g. "deepseek")
// the provider identified by providerID routes through, if any. "" means
// this provider isn't Compressor-fronted (passthrough is meaningless for it).
//
// Looks up the ACTIVE proxy whose provider_id matches — the one real FK that
// replaced the old bidirectional compressor_proxies.provider <->
// router_providers.headroom_proxy string pair (Phase 6 surrogate-key
// migration, 0042). That old pair was two independently-writable columns
// with no FK in either direction; this single lookup is what makes the
// desync it allowed structurally impossible now.
func (s *Server) remoteCompressorService(ctx context.Context, providerID int64) string {
	if providerID == 0 {
		return ""
	}
	hp := s.deps.Routing
	if hp == nil {
		return ""
	}
	proxies, err := hp.Proxies(ctx)
	if err != nil {
		return ""
	}
	for _, p := range proxies {
		if p.ProviderID != nil && *p.ProviderID == providerID && p.OrphanedAt.IsZero() {
			return p.Service
		}
	}
	return ""
}

// providerCredential returns the resolved provider row for a remote
// backend's credential (name). Mirrors V4 auth.get_provider_credential() +
// auth.get_provider_target_url() — but deliberately reads TargetURL (the
// real upstream) only through this gated path, never repurposing any dormant
// base_url field (the docs/llm-router.md trap). A DISABLED provider
// (router_providers.enabled = 0) never resolves — the same "not found"
// treatment as a missing row, so no code path can route through a provider
// the operator switched off. A soft-deleted provider (0042) is likewise
// never returned by Compressor.Providers(), so it resolves the same as
// "not found" too.
func (s *Server) providerCredential(ctx context.Context, name string) (store.ProviderRow, bool) {
	if hp := s.deps.Routing; hp != nil {
		if p, ok, err := hp.ProviderByName(ctx, name); err == nil && ok && p.Enabled {
			return p, true
		}
	}
	return store.ProviderRow{}, false
}

// passthroughAll reads the global compressor.passthrough_all flag from the
// store's Settings KV (V5 home for this app-mutated value). False when the
// store isn't wired (skeleton/tests).
func (s *Server) passthroughAll(ctx context.Context) bool {
	if st := s.deps.Settings; st != nil {
		raw, err := st.Get(ctx, "compressor.passthrough_all")
		if err == nil {
			var v bool
			if err := json.Unmarshal(raw, &v); err == nil {
				return v
			}
		}
	}
	return s.cfg().CompressorPassthroughAll
}

// busyMode reads router.busy_mode from the store's Settings KV, falling back
// to the config-file value when the store isn't wired.
func (s *Server) busyMode(ctx context.Context) BusyMode {
	if st := s.deps.Settings; st != nil {
		raw, err := st.Get(ctx, "router.busy_mode")
		if err == nil {
			var v string
			if err := json.Unmarshal(raw, &v); err == nil {
				switch BusyMode(v) {
				case BusyWait, BusyFailFast:
					return BusyMode(v)
				}
			}
		}
	}
	return s.cfg().BusyMode
}

// requestedByHeader resolves the RequestedBy attribution for a scheduler
// load triggered by this request: X-Forge-Requested-By, when present, lets
// an in-process caller (smith's reasoning tier — docs/v5-smith.md §4.3)
// identify itself in the queue as "smith" instead of the generic "a0", with
// no priority-jump implication (SmallJob is set independently). Absent or
// empty defaults to "a0", the ordinary external-consumer case.
func requestedByHeader(r *http.Request) string {
	if v := r.Header.Get("X-Forge-Requested-By"); v != "" {
		return v
	}
	return "a0"
}

// ensureBackendLoaded is the on-demand load hook for foundry_slot backends
// gated "unhealthy". Finds the Forge mode matching the requested model
// alias and loads it via the scheduler, pinned to this backend's specific
// slot (its port is fixed in router config). See docs/scheduler.md
// "Consumer Model" — this is the mechanism by which ordinary chat-completion
// traffic through A0 gets a model loaded without the calling agent ever
// knowing it wasn't already loaded.
//
// Until Phase 9 integration the scheduler hookup is a stub (sched.Stub),
// which returns success immediately — so the router fronts upstreams
// directly per CLAUDE.local.md.
func (s *Server) ensureBackendLoaded(ctx context.Context, modelName string, b *Backend, requestedBy string) (success bool, message string) {
	sc := s.deps.Sched
	if sc == nil {
		return false, "scheduler not wired"
	}
	targetSlot := s.slotForPort(probePort(b))
	if targetSlot == "" {
		return false, "no slot configured for port " + strconv.Itoa(probePort(b))
	}
	loadCtx, cancel := context.WithTimeout(ctx, s.cfg().ensureLoadedTimeout())
	defer cancel()
	ticket, err := sc.EnsureLoaded(loadCtx, sched.EnsureRequest{
		Model:       modelName,
		RequestedBy: requestedBy,
		TargetSlot:  targetSlot,
	})
	if err != nil {
		return false, err.Error()
	}
	if ticket.Status == "failed" {
		return false, "load failed"
	}
	return true, ""
}

// catalogChain resolves a chat-completion request against a store-backed
// local Config — the *only* local resolution path (ADR-0007): a static
// router.toml route pinned to a physical slot's port can silently keep
// serving whatever's on that port under a stale model label once the
// operator switches what's loaded there (the live a1 bug ADR-0007
// documents), whereas this always resolves fresh against the catalog and
// the scheduler's current placement. TargetSlot is left "" so the scheduler
// places the load on any configured slot (a1-a4) — a0 owns that decision
// dynamically; nothing here pins a model to a physical slot.
//
// handled=false means "not a catalog config either" — the caller falls
// through to the static router.toml route (still the only path for remote
// models, whose wire names were never catalog Configs to begin with; see
// §3.4 — remote migrates to store.Offering in a later phase). handled=true
// with a nil chain means a catalog config was found but couldn't be
// loaded/resolved — the caller should surface errMsg as a 502, alongside
// reason (a stable sched.RefusalReason code, "" when the failure wasn't a
// placement refusal — e.g. a real engine load error) for a consumer to
// switch on instead of parsing errMsg's prose (Sprint 1, a0 load
// visibility). reason is recovered from sched.Scheduler.LoadStatus rather
// than threaded through EnsureLoaded's own (frozen, Contract 2) return
// signature — the outcome ring LoadStatus reads from was just written by
// the very EnsureLoaded call above, so it's always fresh here.
// handled=true with a non-nil chain is a single synthetic foundry_slot
// backend ready for tryBackends, routed straight to the raw slot port.
//
// Deliberately still not binding a proxy to a physical slot here — that
// exact address-based coupling is what ADR-0007 removes at the routing
// layer, and a0's placement is meant to stay free to put any model on any
// slot. Local Compressor fronting (docs/v5-headroom-topology.md, resolving
// §11) is layered in one level down instead, inside resolveBackend's
// foundry_slot case: it routes through one shared local proxy and sets the
// *current* resolved slot as a per-request header
// (ResolvedBackend.UpstreamOverride), so nothing here ever hands the proxy a
// fixed address — this function still just emits a plain foundry_slot
// backend, unchanged.
func (s *Server) catalogChain(ctx context.Context, model string, requestedBy string) (chain []*Backend, handled bool, errMsg string, reason sched.RefusalReason) {
	sc := s.deps.StoreCatalog
	if sc == nil {
		return nil, false, "", ""
	}
	cfg, err := sc.ConfigByName(ctx, model)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, false, "", ""
		}
		return nil, true, "catalog lookup failed: " + err.Error(), ""
	}
	if cfg.Visibility == "hidden" {
		return nil, false, "", ""
	}

	scd := s.deps.Sched
	if scd == nil {
		return nil, true, "scheduler not wired", ""
	}
	loadCtx, cancel := context.WithTimeout(ctx, s.cfg().ensureLoadedTimeout())
	defer cancel()
	ticket, err := scd.EnsureLoaded(loadCtx, sched.EnsureRequest{
		Model:       model,
		RequestedBy: requestedBy,
		TargetSlot:  "",
	})
	if err != nil {
		return nil, true, err.Error(), scd.LoadStatus(model).Reason
	}
	if ticket.Status == "failed" {
		return nil, true, "load failed", scd.LoadStatus(model).Reason
	}
	port := s.deps.Slots[ticket.TargetSlot]
	if port == 0 {
		return nil, true, "no port configured for slot " + ticket.TargetSlot, ""
	}
	return []*Backend{{Name: model, Kind: "foundry_slot", Port: port}}, true, "", ""
}

// offeringChain resolves a remote model against store.Offering (ADR-0007
// §3.4) — the remote-side counterpart to catalogChain. Found live
// 2026-07-28: RouterConfig.Backends/.Routes are always nil once
// router.LoadFromStore replaced router.ParseConfig (TOML decommission
// cutover), which silently broke every remote model (deepseek-v4-pro,
// kimi-k2.7-code, glm-5.2, ...) — they were still listed in /v1/models via
// BuildModelsResponse's Offering loop, but chatCompletions had no path left
// to actually resolve one, since it only ever fell back to the
// now-permanently-empty cfg.RouteFor/BackendByName. §3.4's Offering
// migration was explicitly deferred when routing.go was rewritten for local
// models; this is that migration, done as an incident fix once the gap was
// found rather than a planned session.
//
// Multi-provider selection (2026-08-06): offerings of the same catalog Model
// form one group — the same model on two providers is two distinct
// Offerings, not interchangeable (data residency, cost, reliability), and
// the operator's preference between them is offerings.priority. The request
// matches the group if ANY of its offerings carries the requested
// wire_model (providers name the same model differently — e.g. aiand's
// "zai-org/glm-5.2" vs qwen's "glm-5.2"); the whole group then converges on
// the PRIMARY: the enabled offering of an enabled provider with the lowest
// priority value (ties break by provider name then offering id — settled
// explicitly by GroupOfferingsByModel/SelectOfferingChain in select.go, the
// one place this rule lives; not relied on from ListOfferings' own SQL
// order). Every wire name in the group is
// rewritten to the primary's wire_model at the backend, so what /v1/models
// presents and what actually gets served never disagree. A disabled provider
// simply drops out of the group; if the primary was its offering, the next
// offering in line takes over.
//
// Failover: when router.provider_failover is on, the chain carries the whole
// priority-ordered group and tryBackends' existing transport-error/5xx
// failover retries the next provider; when off (default — matches the
// historical "no fallback chains" posture), the chain is the single primary.
//
// handled=false means "no Offering with this wire_model" — falls through to
// the static router.toml route, same contract as catalogChain. handled=true
// with a nil chain means offerings matched but none is routable (offering
// disabled, provider disabled or missing) — the caller surfaces errMsg as a
// 502.
func (s *Server) offeringChain(ctx context.Context, model string) (chain []*Backend, handled bool, errMsg string) {
	sc := s.deps.StoreCatalog
	if sc == nil {
		return nil, false, ""
	}
	offerings, err := sc.ListOfferings(ctx)
	if err != nil {
		return nil, true, "catalog lookup failed: " + err.Error()
	}

	// Locate the model group by wire_model identity.
	var groupID int64
	for _, o := range offerings {
		if o.WireModel == model {
			groupID = o.ModelID
			break
		}
	}
	if groupID == 0 {
		return nil, false, ""
	}

	// Selection rule shared with BuildModelsResponse + the routing-preview
	// endpoint — see select.go.
	groups := GroupOfferingsByModel(offerings)
	group := SelectOfferingChain(groups[groupID], s.enabledProviderSet(ctx), s.providerFailoverEnabled(ctx))
	if len(group) == 0 {
		return nil, true, "model " + model + " has no enabled offering on an enabled provider"
	}

	for _, o := range group {
		// resolveBackend's "remote" case re-resolves the API key/target
		// itself via Credential; this call is the existence+enablement check.
		provider, ok := s.providerCredential(ctx, o.ProviderName)
		if !ok {
			continue // provider vanished between reads — skip, never route
		}
		baseURL, _, hasProxy := s.remoteCompressorBaseURL(ctx, provider.ID)
		if !hasProxy {
			baseURL = provider.TargetURL // no Compressor proxy (dedicated or shared external) — straight passthrough
		}
		chain = append(chain, &Backend{
			Name: o.ProviderName, Kind: "remote",
			BaseURL: baseURL, WireModel: o.WireModel, Credential: o.ProviderName,
			PriceInPer1M: o.PriceInPer1M, PriceOutPer1M: o.PriceOutPer1M,
			PriceCachedInPer1M: o.PriceCachedInPer1M, PriceCurrency: o.Currency,
		})
	}
	if len(chain) == 0 {
		return nil, true, "model " + model + ": provider credentials unavailable"
	}
	return chain, true, ""
}

// enabledProviderSet returns the ids of providers currently allowed to
// route (router_providers.enabled — 0032; keyed by id since 0042 — a
// soft-deleted provider is already excluded by Compressor.Providers()).
// Missing provider store (skeleton/tests without Compressor wired) yields an
// EMPTY set: routing then fails closed with a clear 502, same as the
// pre-0032 "provider not found" behavior when no provider row matched.
func (s *Server) enabledProviderSet(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	if hp := s.deps.Routing; hp != nil {
		providers, err := hp.Providers(ctx)
		if err == nil {
			for _, p := range providers {
				if p.Enabled {
					out[p.ID] = true
				}
			}
		}
	}
	return out
}

// providerRows returns the current provider rows, or nil when the provider
// store isn't wired (skeleton mode). BuildModelsResponse treats nil as "no
// provider filtering" — with no store there is no enablement state either,
// and listing configured offerings matches the pre-0032 behavior.
func (s *Server) providerRows(ctx context.Context) []store.ProviderRow {
	if hp := s.deps.Routing; hp != nil {
		if providers, err := hp.Providers(ctx); err == nil {
			return providers
		}
	}
	return nil
}

// providerFailoverEnabled reads router.provider_failover from the store's
// Settings KV. Default false: a failed remote request returns a clean error
// rather than silently spending on the next provider — the historical a0
// posture ("no fallback chains", docs/llm-router.md). Enabling it makes
// offeringChain return the whole priority-ordered group, and tryBackends'
// existing transport-error/5xx failover does the rest (4xx stays definitive
// — a provider-side rejection like 401/429 is not retried elsewhere).
func (s *Server) providerFailoverEnabled(ctx context.Context) bool {
	if st := s.deps.Settings; st != nil {
		raw, err := st.Get(ctx, "router.provider_failover")
		if err == nil {
			var v bool
			if err := json.Unmarshal(raw, &v); err == nil {
				return v
			}
		}
	}
	return false
}

// remoteCompressorBaseURL returns the loopback base URL of the Compressor proxy
// fronting providerID's linked provider, if any. "" + false means no proxy
// is linked (or the store isn't wired) — the caller should route straight
// to the provider's real upstream instead.
// remoteCompressorBaseURL returns the loopback base URL a "remote" backend
// for providerID should route through, and which Compressor service name
// that is (for per-service bypass checks — see resolveBackend's "remote"
// case). Tries a per-provider dedicated proxy first (the pre-Sprint-3
// contract: an FK-linked ProxyRow), then falls back to the shared
// "external" instance (Sprint 3) — mirrors localCompressorBaseURL's own
// "resolve fresh every request, one proxy per scope" contract on the
// remote side. ok=false means neither exists: the caller routes straight
// to the provider's real upstream, same as always.
func (s *Server) remoteCompressorBaseURL(ctx context.Context, providerID int64) (baseURL, service string, ok bool) {
	if svc := s.remoteCompressorService(ctx, providerID); svc != "" {
		hp := s.deps.Routing
		if hp != nil {
			if proxies, err := hp.Proxies(ctx); err == nil {
				for _, p := range proxies {
					if p.Service == svc && p.OrphanedAt.IsZero() {
						return "http://127.0.0.1:" + strconv.Itoa(p.Port) + "/v1", svc, true
					}
				}
			}
		}
	}
	if url, ok := s.externalCompressorBaseURL(ctx); ok {
		return url, externalCompressorService, true
	}
	return "", "", false
}

// slotForPort reverse-looks-up which configured slot name owns this port.
// Used to pin an on-demand load to the slot a foundry_slot backend points
// at. Reads from Deps.Slots (the V5 read-only config file's [slots] table,
// passed in by Phase 9 wiring); otherwise "".
func (s *Server) slotForPort(port int) string {
	if s.deps.Slots == nil {
		return ""
	}
	for name, p := range s.deps.Slots {
		if p == port {
			return name
		}
	}
	return ""
}

// chainLabel builds the audit detail string — backend names + coarse status
// labels only, never bodies, prompts, or credentials.
type chainLabel struct {
	attempts []string
	// lastLayer (Sprint 4) is the most recent attempt's failure layer —
	// "compressor" or "backend" — read by the final all_backends_unavailable
	// response to tell a consumer which layer actually broke, instead of
	// the previous bare code. Empty until the first attempt fails.
	lastLayer string
}

func (cl *chainLabel) add(entry string) { cl.attempts = append(cl.attempts, entry) }

func (cl *chainLabel) String() string { return strings.Join(cl.attempts, ",") }

// setLayer records the current attempt's failure layer for the eventual
// error response. Only ever called from a failure path (5xx or transport
// error) — a successful attempt never reaches the final error body, so
// there's nothing to overwrite it with.
func (cl *chainLabel) setLayer(layer string) { cl.lastLayer = layer }

// auditOutcome records a routing decision via store.Audit. Best-effort; nil
// store → no-op. detail is restricted to backend names + coarse status labels
// by construction (see chatCompletions in proxy.go).
// clientAddrKey stashes the request's effective client address (the same
// authz.EffectiveRemoteAddr the tailnet-conditional auth computes) so
// auditOutcome can attribute entries. 2026-08-22 post-mortem gap: the fatal
// sibling loads were unattributable because every router audit row had a
// null remote_addr.
type clientAddrKey struct{}

// consumerLabelKey stashes the request's per-slot consumer-attribution
// label (see consumerLabel) so tryBackends can Mark each foundry_slot
// attempt without the identity being threaded through its signature.
type consumerLabelKey struct{}

func consumerLabelFromCtx(ctx context.Context) string {
	if v, ok := ctx.Value(consumerLabelKey{}).(string); ok {
		return v
	}
	return ""
}

// consumerLabel derives the human-facing consumer label a request's
// foundry_slot attempts are attributed with: the key's operator-chosen
// DisplayName verbatim when set; otherwise activity.DeriveLabel over the
// key name + User-Agent; — tailnet bypass, no key — the effective remote
// address. Requests that identify themselves as smith's
// own reasoning traffic (X-Forge-Requested-By: smith) return "": smith
// marks its brain slot directly as "SMITH" (reasoning.go /
// brain_residency.go), and this router's loopback mark would clobber it.
func (s *Server) consumerLabel(r *http.Request, auth authResult) string {
	if requestedByHeader(r) == "smith" {
		return ""
	}
	if auth.identity.DisplayName != "" {
		return auth.identity.DisplayName
	}
	if auth.identity.Name != "" {
		return activity.DeriveLabel(auth.identity.Name, r.UserAgent())
	}
	return authz.EffectiveRemoteAddr(parseRemoteAddr(r.RemoteAddr), r.Header.Get("X-Forwarded-For")).String()
}

func (s *Server) auditOutcome(ctx context.Context, modelName, result, detail string) {
	if a := s.deps.Audit; a != nil {
		// store.AuditEntry has no Result field; fold result into Detail (JSON),
		// matching the V5 audit schema.
		payload, _ := json.Marshal(map[string]string{
			"result": result,
			"detail": detail,
			"model":  modelName,
		})
		entry := store.AuditEntry{
			TS:     time.Now(),
			Actor:  "router",
			Action: "chat_completion",
			Target: modelName,
			Detail: string(payload),
		}
		if v, ok := ctx.Value(clientAddrKey{}).(string); ok {
			entry.RemoteAddr = v
		}
		_ = a.Write(ctx, entry)
	}
}

// firstNonEmpty returns the first non-empty argument, or "" if all are empty.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// authResult is the outcome of the tailnet-conditional auth check.
type authResult struct {
	identity authz.Identity
	ok       bool
}

// checkAuth implements the tailnet-conditional auth: tailnet-sourced
// requests (authz.IsTailnetAddr(authz.EffectiveRemoteAddr(...))) skip the
// bearer check; non-tailnet requests require a valid sk-router-* token
// verified via authz.Authenticator.VerifyBearerFrom.
//
// The CGNAT/XFF logic lives in internal/authz (frozen, table-tested) — this
// function calls it, never reimplements it (CLAUDE.local.md hard requirement).
func (s *Server) checkAuth(r *http.Request) authResult {
	remoteAddr := parseRemoteAddr(r.RemoteAddr)
	xff := r.Header.Get("X-Forwarded-For")
	effective := authz.EffectiveRemoteAddr(remoteAddr, xff)
	if authz.IsTailnetAddr(effective) {
		return authResult{ok: true}
	}
	// In-process/loopback callers with no XFF are trusted (smith P3 — the
	// reasoning tier calls a0 from inside forge over 127.0.0.1). This is
	// a deliberate loosening, not a no-op: EffectiveRemoteAddr already trusts
	// XFF asserted by a loopback caller, so a local process could already
	// reach a0 unauthenticated by spoofing a CGNAT XFF value — this exemption
	// just makes that same trust boundary explicit for the plain no-XFF case
	// instead of requiring a process to know the spoof. authz.EffectiveRemoteAddr
	// / authz.IsTailnetAddr (the crown-jewels CGNAT/XFF logic) are unchanged.
	if remoteAddr.IsLoopback() && xff == "" {
		return authResult{ok: true, identity: authz.Identity{Name: "loopback"}}
	}
	auth := s.deps.Auth
	if auth == nil {
		// Skeleton mode: no authenticator wired. Permit only healthz
		// (caller is responsible for not routing /v1/* here when Auth is
		// nil). Treat as unauthorized for /v1/* paths.
		return authResult{ok: false}
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return authResult{ok: false}
	}
	token := strings.TrimPrefix(header, "Bearer ")
	id, err := auth.VerifyBearerFrom(r.Context(), effective.String(), token, authz.KindRouter)
	if err != nil {
		return authResult{ok: false}
	}
	return authResult{ok: true, identity: id}
}
