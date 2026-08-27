// SPDX-License-Identifier: Apache-2.0

package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/authz"
)

// upstreamError is returned from ModifyResponse when the upstream returns a
// 5xx status. The ReverseProxy forwards it to ErrorHandler, which uses it
// to label the audit entry (and otherwise can't distinguish a 5xx from a
// transport error — both arrive as a plain error).
type upstreamError struct {
	status int
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream returned %d", e.status)
}

// Sprint 4: headers forge-compress sets on every response so a0 can tell
// "the compressor answered, possibly with an error it relayed" from "the
// compressor itself is what's broken" — see cmd/forge-compress/server.go's
// identically-named constants. layerCompressor/layerBackend are the values
// this classification feeds into the all_backends_unavailable body's new
// "layer" field.
const (
	headerForgeCompressReached = "X-Forge-Compress"
	headerForgeCompressError   = "X-Forge-Compress-Error"

	layerCompressor = "compressor"
	layerBackend    = "backend"
)

// chatCompletions implements POST /v1/chat/completions — the a0 hot path
// (Contract 1 §7). Auth is tailnet-conditional (checkAuth). Only model +
// messages are validated; every other field passes through untouched. The
// resolved backend's wire_model overwrites exactly one field in the
// forwarded body (model=wire_model). Streaming is via httputil.ReverseProxy
// with FlushInterval: -1 (no buffering of upstream SSE chunks).
//
// Failover: walk the route's backend chain in order. For each backend: gate
// (skip unhealthy/busy), resolve (atomic base_url/api_key/wire_model),
// attempt via ReverseProxy. On transport error or 5xx (ModifyResponse
// returns an error → ErrorHandler fires BEFORE any bytes are written to the
// client), try the next backend. On 2xx or definitive 4xx, the response is
// committed and streamed — streaming failover is not possible once bytes
// are committed (V4 parity).
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Apply the overall request timeout (request_timeout_s) if configured —
	// unbounded by default, see requestTimeout()'s doc comment. When set,
	// this bounds the whole generation for streaming responses too, matching
	// V4's requests.post(timeout=(connect, request)) semantics.
	if timeout := s.cfg().requestTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		r = r.WithContext(ctx)
	}

	auth := s.checkAuth(r)
	if !auth.ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
		return
	}

	// Attribute this request's effective client address into ctx for the
	// audit trail (auditOutcome reads it back; see clientAddrKey).
	ctx = context.WithValue(ctx, clientAddrKey{},
		authz.EffectiveRemoteAddr(parseRemoteAddr(r.RemoteAddr), r.Header.Get("X-Forwarded-For")).String())

	// Per-slot consumer attribution (status.slot_consumers): derive the
	// caller's human-facing label once up front and stash it in ctx —
	// tryBackends Marks each foundry_slot attempt with it.
	ctx = context.WithValue(ctx, consumerLabelKey{}, s.consumerLabel(r, auth))

	// Read & buffer the body once — used for validation and re-serialised
	// per-backend with wire_model overwritten. Cap at 32 MiB to prevent
	// OOM from oversized chat-completions bodies.
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds 32 MiB limit"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	r.Body = http.NoBody // prevent any downstream reader from hitting EOF noise

	var body map[string]any
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": map[string]string{"body": "must be a valid JSON object"},
		})
		return
	}

	// Validate only model (string, 1–128) + messages (non-empty array).
	// Every other field passes through untouched (Contract 1 §7).
	model, _ := body["model"].(string)
	if len(model) < 1 || len(model) > 128 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": map[string]string{"model": "must be a string of 1-128 characters"},
		})
		return
	}
	messages, _ := body["messages"].([]any)
	if len(messages) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": map[string]string{"messages": "must be a non-empty array"},
		})
		return
	}

	cfg := s.cfg()
	if s.deps.Cfg == nil {
		// Skeleton mode: no router config wired. 503 keeps the healthz path
		// (the only path served by New()) distinct from /v1/*.
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "router not configured"})
		return
	}

	// Local routing resolves by catalog Config identity first, uniformly
	// (ADR-0007) — never by a static router.toml route pinned to a physical
	// slot's port, which is exactly the addressing scheme that let a1's
	// route silently keep serving whatever was on port 8080 under a stale
	// model label (see docs/adr/0007-routing-resolves-by-identity-not-address.md).
	// A model name that isn't a catalog Config at all resolves by wire_model
	// identity against store.Offering next (ADR-0007 §3.4 — the remote-side
	// counterpart to catalogChain). Only a model matching neither falls
	// through to the static router.toml route/backend declarations, which
	// are always empty post-TOML-decommission-cutover — that branch is dead
	// code kept only for a hypothetical future config source, not a real
	// fallback today.
	requestedBy := requestedByHeader(r)
	var chain []*Backend
	catChain, handled, errMsg, loadReason := s.catalogChain(ctx, model, requestedBy)
	switch {
	case handled && catChain != nil:
		chain = catChain
	case handled:
		body := map[string]string{
			"error":   "catalog_load_failed",
			"model":   model,
			"detail":  errMsg,
			"message": unavailableMessageFor(model, "", errMsg),
		}
		if loadReason != "" {
			body["reason"] = string(loadReason)
		}
		writeJSON(w, http.StatusBadGateway, body)
		return
	default:
		offChain, offHandled, offErrMsg := s.offeringChain(ctx, model)
		switch {
		case offHandled && offChain != nil:
			chain = offChain
		case offHandled:
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"error":   "offering_resolve_failed",
				"model":   model,
				"detail":  offErrMsg,
				"message": unavailableMessage(model, ""),
			})
			return
		default:
			route := cfg.RouteFor(model)
			if route == nil {
				writeJSON(w, http.StatusNotFound, map[string]string{
					"error": "model_not_found",
					"model": model,
				})
				return
			}
			// Build the ordered backend chain (primary + fallback).
			chainNames := append([]string{route.Primary}, route.Fallback...)
			for _, name := range chainNames {
				if b := cfg.BackendByName(name); b != nil {
					chain = append(chain, b)
				}
			}
		}
	}
	if len(chain) == 0 {
		// No candidate backend was ever attempted — never a compressor
		// problem (there's nothing to be fronted), so "backend" is
		// unambiguous here, unlike the exhausted-chain case below.
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "all_backends_unavailable",
			"model":   model,
			"message": unavailableMessage(model, layerBackend),
			"layer":   layerBackend,
		})
		return
	}

	cl := &chainLabel{}
	s.tryBackends(ctx, w, r, model, body, chain, 0, cl)
}

// tryBackends attempts the backend at chain[idx], failing over to idx+1 on
// transport error or 5xx. The failover is recursive: ErrorHandler on the
// ReverseProxy calls tryBackends(idx+1). This is safe because ErrorHandler
// fires BEFORE any bytes are written to the ResponseWriter (ModifyResponse
// runs before copyHeader/WriteHeader in Go's httputil.ReverseProxy).
//
// On 2xx or definitive 4xx, the ReverseProxy streams the response to w and
// returns — failover is no longer possible (V4 parity: "streaming failover
// isn't possible once bytes are committed to the client").
func (s *Server) tryBackends(ctx context.Context, w http.ResponseWriter, r *http.Request,
	model string, body map[string]any, chain []*Backend, idx int, cl *chainLabel) {

	if idx >= len(chain) {
		s.auditOutcome(ctx, model, "error", "exhausted:"+cl.String())
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":   "all_backends_unavailable",
			"model":   model,
			"message": unavailableMessage(model, cl.lastLayer),
			"layer":   cl.lastLayer,
		})
		return
	}

	b := chain[idx]
	cfg := s.cfg()

	// Gate: health/busy check for foundry_slot backends.
	// busyMode is read from the store's Settings KV (router.busy_mode) so
	// runtime changes take effect without a config reload.
	busyMode := s.busyMode(ctx)
	reason := slotGate(b, cfg, s.catalog(), busyMode)
	if reason == gateUnhealthy && b.Kind == "foundry_slot" {
		// On-demand load: nothing is currently serving this slot — try
		// loading the requested model via the scheduler before giving up.
		if ok, msg := s.ensureBackendLoaded(ctx, model, b, requestedByHeader(r)); ok {
			reason = slotGate(b, cfg, s.catalog(), busyMode) // re-check after load
		} else {
			cl.add(b.Name + ":load_failed(" + msg + ")")
			s.tryBackends(ctx, w, r, model, body, chain, idx+1, cl)
			return
		}
	}
	if reason != "" {
		cl.add(b.Name + ":skip(" + string(reason) + ")")
		s.tryBackends(ctx, w, r, model, body, chain, idx+1, cl)
		return
	}

	// Resolve the atomic (base_url, api_key, wire_model) triple.
	resolved, err := s.resolveBackend(ctx, b)
	if err != nil {
		cl.add(b.Name + ":resolve_error(" + err.Error() + ")")
		s.tryBackends(ctx, w, r, model, body, chain, idx+1, cl)
		return
	}

	// compressorFronted (Sprint 4; corrected Sprint 8) is true whenever this
	// attempt's real target is behind a forge-compress@* proxy rather than
	// reached directly — resolveBackend now computes this explicitly
	// (ResolvedBackend.CompressorFronted), rather than being inferred from
	// UpstreamOverride != "" the way this used to read: that inference
	// silently misclassified a dedicated per-provider proxy (e.g. deepseek),
	// whose BaseURL is the proxy but which needs no per-request override
	// header, as NOT compressor-fronted. Used below to classify a failure as
	// "compressor" vs "backend" for the consumer-facing 502 body, and
	// (Sprint 8) to decide whether a connection-level failure is eligible
	// for an auto-bypass retry against resolved.DirectUpstreamURL.
	compressorFronted := resolved.CompressorFronted

	// Mutate body: overwrite model with wire_model, and — for remote
	// backends only, gated by the operator's usage.inject_stream_usage
	// setting — inject stream_options.include_usage so a streamed response
	// carries a trailing usage chunk (see internal/router/usage.go; without
	// this, streamed remote spend is structurally unmeasurable). Every other
	// field passes through untouched (re-serialized, matching V4's
	// dict(body) → json=upstream_body behavior).
	requestBody := body
	if b.Kind == "remote" {
		requestBody = applyStreamUsageOptions(body, s.injectStreamUsageEnabled(ctx))
	}
	mutatedBody, err := mutateBody(requestBody, resolved.WireModel)
	if err != nil {
		cl.add(b.Name + ":body_error(" + err.Error() + ")")
		s.tryBackends(ctx, w, r, model, body, chain, idx+1, cl)
		return
	}

	// Per-slot consumer attribution: Mark this foundry_slot attempt's slot
	// with the request's consumer label at START; the completion mark (same
	// label, refreshed timestamp) happens when the upstream body finishes
	// streaming — see markOnCloseBody in ModifyResponse below.
	slotID := ""
	attemptLabel := consumerLabelFromCtx(ctx)
	if b.Kind == "foundry_slot" {
		slotID = s.slotForPort(probePort(b))
		s.markSlot(slotID, attemptLabel)
	}

	// attemptBackend runs one ReverseProxy attempt against targetBase.
	// bypass=true is the Sprint 8 auto-bypass retry: going straight to
	// resolved.DirectUpstreamURL with no x-compress-base-url header — the
	// compressor is skipped entirely for this attempt, not just redirected.
	//
	// FlushInterval: -1 flushes immediately after every write — true
	// streaming, no buffering of upstream SSE chunks (CLAUDE.local.md hard
	// requirement: verify with long generations).
	//
	// URL fields are set explicitly rather than via ProxyRequest.SetURL,
	// which joins the inbound request path onto target's path — since a0's
	// own mount point for this handler IS "/v1/chat/completions" (a fixed
	// pattern, not a prefix), SetURL doubled it to
	// "/v1/chat/completions/v1/chat/completions" on every single call. Every
	// existing test's fake upstream used a catch-all http.HandlerFunc that
	// never asserted on the received path, so this went undetected until a
	// live request against real llama-server/DeepSeek upstreams during the
	// Phase 9b production cutover, both of which returned a real 404 for the
	// doubled path. Same class of bug as the /v1/embeddings passthrough,
	// which explicitly avoided SetURL for this exact reason — never
	// ported to the chat hot path until now.
	var attemptBackend func(w http.ResponseWriter, r *http.Request, targetBase string, bypass bool)
	attemptBackend = func(w http.ResponseWriter, r *http.Request, targetBase string, bypass bool) {
		targetURL, err := url.Parse(targetBase + "/chat/completions")
		if err != nil {
			cl.add(b.Name + ":url_error")
			s.tryBackends(ctx, w, r, model, body, chain, idx+1, cl)
			return
		}
		// Bypassing means we're at the real upstream already — no override
		// header needed (and none of the client's own is let through either,
		// stripped unconditionally below regardless of bypass).
		upstreamOverride := resolved.UpstreamOverride
		if bypass {
			upstreamOverride = ""
		}

		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = targetURL.Scheme
				pr.Out.URL.Host = targetURL.Host
				pr.Out.URL.Path = targetURL.Path
				pr.Out.URL.RawPath = ""
				pr.Out.Host = "" // don't forward our Host header to upstream
				// Security (docs/v5-headroom-topology.md §5b): strip any
				// client-supplied x-compress-base-url unconditionally, on every
				// branch, before possibly setting our own — a0 must never let a
				// caller's own header reach a Compressor proxy verbatim (Compressor
				// itself does no origin validation on this header; the loopback
				// bind on every Compressor proxy port is the only reason this isn't
				// exploitable today, and this rule is what keeps it that way).
				pr.Out.Header.Del("x-compress-base-url")
				if upstreamOverride != "" {
					pr.Out.Header.Set("x-compress-base-url", upstreamOverride)
				}
				if resolved.APIKey != "" {
					pr.Out.Header.Set("Authorization", "Bearer "+resolved.APIKey)
				}
				pr.Out.Header.Set("Content-Type", "application/json")
				pr.Out.Body = io.NopCloser(bytes.NewReader(mutatedBody))
				pr.Out.ContentLength = int64(len(mutatedBody))
			},
			ModifyResponse: func(resp *http.Response) error {
				if resp.StatusCode >= 500 {
					// Returning an error here triggers ErrorHandler, which
					// fails over to the next backend. No bytes have been
					// written to w yet (ModifyResponse runs before
					// copyHeader/WriteHeader in httputil.ReverseProxy).
					if b.Kind == "foundry_slot" {
						s.recordSlotError(b.Port)
					}
					// Layer classification (Sprint 4): forge-compress marks
					// every response it relays with X-Forge-Compress, and a
					// failure it generated itself (never reached the real
					// backend) with X-Forge-Compress-Error instead — see
					// cmd/forge-compress/server.go. A relayed 5xx is a real
					// backend failure, not a compressor one. A bypass attempt
					// (Sprint 8) never went through the compressor at all, so
					// any 5xx it gets is unambiguously the backend's.
					switch {
					case !bypass && compressorFronted && resp.Header.Get(headerForgeCompressError) != "":
						cl.setLayer(layerCompressor)
					case !bypass && compressorFronted && resp.Header.Get(headerForgeCompressReached) != "":
						cl.setLayer(layerBackend)
					case !bypass && compressorFronted:
						// Compressor-fronted but neither header present — an
						// older build without this signal. Ambiguous: don't
						// overwrite with a guess.
					default:
						cl.setLayer(layerBackend)
					}
					return &upstreamError{status: resp.StatusCode}
				}
				if resp.StatusCode >= 400 {
					cl.add(b.Name + ":4xx:" + strconv.Itoa(resp.StatusCode))
				} else if bypass {
					cl.add(b.Name + ":ok(bypassed)")
				} else {
					cl.add(b.Name + ":ok")
				}
				if resp.StatusCode < 400 {
					// Real remote spend (cost/savings sprint Phase 4): only for
					// committed 2xx responses on "remote" backends, never on
					// 4xx (no real usage expected) or foundry_slot (local usage
					// is tracked separately via OnTokenSample). The tap never
					// buffers the streaming body — see usageTap's doc comment.
					if resolved.Provider != "" && s.deps.Usage != nil {
						streaming, _ := body["stream"].(bool)
						captured := resolved
						resp.Body = newUsageTap(resp.Body, streaming, func(buf []byte) {
							s.recordExternalUsage(captured, model, buf, streaming)
						})
					}
					// Consumer attribution completion mark: ReverseProxy
					// always closes the upstream body after copying it, so
					// this fires when the response is fully streamed — both
					// SSE and buffered alike.
					if slotID != "" && attemptLabel != "" {
						resp.Body = &markOnCloseBody{ReadCloser: resp.Body, mark: func() {
							s.markSlot(slotID, attemptLabel)
						}}
					}
				}
				s.auditOutcome(ctx, model, "ok", cl.String())
				return nil
			},
			ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
				// Transport error or ModifyResponse error. No bytes written to
				// rw yet — safe to fail over (or bypass). Classify for the
				// audit detail.
				if b.Kind == "foundry_slot" {
					s.recordSlotError(b.Port)
				}
				var ue *upstreamError
				if errors.As(err, &ue) {
					// Layer already classified in ModifyResponse above — a 5xx
					// only reaches here after that branch already set it.
					cl.add(b.Name + ":5xx:" + strconv.Itoa(ue.status))
					s.tryBackends(ctx, rw, req, model, body, chain, idx+1, cl)
					return
				}
				// A transport error (connection refused/timeout, no response
				// ever parsed) — never an application-level error the
				// compressor itself relayed.
				if !bypass && compressorFronted && resolved.DirectUpstreamURL != "" {
					// Sprint 8 auto-bypass: the compressor itself is
					// unreachable at the connection level — retry this same
					// request directly against the real upstream instead of
					// failing over to the next backend in the chain.
					// Per-request, not sticky: the very next request tries
					// the compressor again, so recovery (e.g. systemd's
					// Restart=on-failure, ~10s) is picked up automatically
					// with no operator action. connect_timeout_s (already
					// applied via s.httpClient()'s dialer, unchanged here)
					// is what keeps a refused/unreachable compressor from
					// costing this attempt more than a few seconds before
					// falling back.
					cl.add(b.Name + ":error(compressor_unreachable),bypassing")
					attemptBackend(rw, req, resolved.DirectUpstreamURL, true)
					return
				}
				cl.add(b.Name + ":error")
				if !bypass && compressorFronted {
					cl.setLayer(layerCompressor)
				} else {
					cl.setLayer(layerBackend)
				}
				s.tryBackends(ctx, rw, req, model, body, chain, idx+1, cl)
			},
			FlushInterval: -1,
			Transport:     s.httpClient().Transport,
		}

		proxy.ServeHTTP(w, r)
	}

	attemptBackend(w, r, resolved.BaseURL, false)
}

// markOnCloseBody wraps an upstream response body so closing it refreshes
// the slot-consumer attribution mark — httputil.ReverseProxy always closes
// the upstream body after copying it to the client, which is exactly
// "response fully streamed" for both SSE and buffered responses.
type markOnCloseBody struct {
	io.ReadCloser
	mark func()
}

func (b *markOnCloseBody) Close() error {
	b.mark()
	return b.ReadCloser.Close()
}

// markSlot records one consumer-attribution mark (nil-registry safe, and a
// no-op without a resolved slot id or label).
func (s *Server) markSlot(slotID, label string) {
	if s.deps.Activity == nil || slotID == "" || label == "" {
		return
	}
	s.deps.Activity.Mark(slotID, label)
}

// mutateBody returns body serialized as JSON with model overwritten to
// wireModel. Every other field passes through. Matches V4's
// `upstream_body = dict(body); upstream_body["model"] = wire_model`.
func mutateBody(body map[string]any, wireModel string) ([]byte, error) {
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["model"] = wireModel
	return json.Marshal(out)
}

// opsConsoleURL is the operator dashboard's browser front (tailscale serve
// HTTPS, see reference_forgehost_live_verification memory) — surfaced to
// consumers in unavailableMessage so a human reading the error has
// somewhere to go, not just a bare code.
const opsConsoleURL = "https://ops.example.ts.net/#console"

// unavailableMessage is a human-readable explanation added alongside (never
// replacing — existing consumers/tests key on the "error" string) the
// "all_backends_unavailable"/"catalog_load_failed"/"offering_resolve_failed"
// codes. Before this, a consumer's only signal was the bare code with no
// indication of what to do about it — found live 2026-07-29 when LibreChat
// surfaced nothing but "all_backends_unavailable" to the end user for a
// model that had, in fact, loaded successfully (the real fault that day was
// unrelated: every Compressor proxy was down after a host reboot, so every
// backend attempt failed at the transport level — see progress.md). This
// message covers every reason a model can end up unusable: it never loaded,
// it loaded but its only backend is unreachable, or the scheduler couldn't
// free a slot for it.
//
// layer (Sprint 4) is layerCompressor, layerBackend, or "" (unclassified —
// e.g. catalog/offering resolution never reached a backend attempt at all).
// It only sharpens the wording; the generic guidance always still applies.
func unavailableMessage(model, layer string) string {
	switch layer {
	case layerCompressor:
		return fmt.Sprintf(
			"%s is not available right now — its Compressor compressor is unreachable, so the request never "+
				"reached the backend at all. Try again in a few minutes, or check %s to see proxy status.",
			model, opsConsoleURL,
		)
	case layerBackend:
		return fmt.Sprintf(
			"%s is not available right now — it may still be loading, failed to load, or its backend is unreachable. "+
				"Try again in a few minutes, or check %s to see status or free up a slot.",
			model, opsConsoleURL,
		)
	default:
		return fmt.Sprintf(
			"%s is not available right now — it may still be loading, failed to load, or its backend is unreachable. "+
				"Try again in a few minutes, or check %s to see status or free up a slot.",
			model, opsConsoleURL,
		)
	}
}

// unavailableMessageFor layers the scheduler's own reason over the generic
// message when the load failure was a MEMORY refusal (S1, feedback F3): the
// operator's ask was that the consumer's error say there wasn't enough VRAM
// and other models couldn't be evicted, rather than a generic "may still be
// loading". detail is the scheduler error text the 502's `detail` field
// carries; matching on its stable prefixes avoids false positives from
// unrelated scheduler errors.
func unavailableMessageFor(model, layer, detail string) string {
	if strings.Contains(detail, "not enough VRAM") {
		return fmt.Sprintf(
			"%s cannot be loaded right now: there is not enough free VRAM, and the models holding the memory could not be evicted at this time. "+
				"The `detail` field says exactly which models blocked it and why. See %s to free a slot manually.",
			model, opsConsoleURL,
		)
	}
	return unavailableMessage(model, layer)
}

// writeJSON writes a JSON response with the given status. Best-effort: if
// encoding fails (shouldn't for our simple payloads), writes the status only.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// parseRemoteAddr extracts the client IP from an http.Request.RemoteAddr
// ("host:port" for TCP). Returns the zero netip.Addr on parse failure —
// which is not loopback, not tailnet, so auth is enforced (fail-closed).
func parseRemoteAddr(remoteAddr string) netip.Addr {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // might be a bare IP without a port
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr
}
