// SPDX-License-Identifier: Apache-2.0

package router

// specialist.go routes a function (embeddings, transcription) to the specialist slot pool when the pool has a member for it (ADR-0022, slice 5).
//
// Nothing changes until the pool is configured: with no pool wired, or no member named and no default for the function, viaPool reports "not
// handled" and the caller does exactly what it did before (the static embeddings passthrough; the catalog audio adapter, then a 404). A pool that
// cannot place a request answers FAST with a stable code and never blocks for minutes: 503 specialist_pool_full or specialist_unavailable.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// SpecialistPool is what the router needs from the pool (implemented by *specialist.Manager).
type SpecialistPool interface {
	Resolve(ctx context.Context, function, model string) (specialist.Target, error)
	// ResolveConfig answers a request for a catalog config the pool hosts (ErrNoMatch when it does not): a pooled config has exactly one owner
	// and is never loaded into an LLM slot (ADR-0022).
	ResolveConfig(ctx context.Context, config string) (specialist.Target, error)
}

// Stable error codes for a pool that cannot answer.
const (
	errCodePoolFull    = "specialist_pool_full"
	errCodeUnavailable = "specialist_unavailable"
)

// leaseRenewEvery is how often a request that is still open renews its lease (well inside the pool's 10 minute lease), so a response that streams for
// longer than the lease keeps its engine. A variable so tests can shorten it.
var leaseRenewEvery = 3 * time.Minute

// renewWhileOpen renews tg's lease until the returned stop function is called. A target without a lease (a test fake) needs no renewing.
// The period is read here rather than inside the goroutine: stopping only closes a channel, so nothing later joins the goroutine, and a
// read it made after the request was served has no happens-before edge to anything. Read by the caller, it is ordered by the request.
func renewWhileOpen(tg specialist.Target) (stop func()) {
	if tg.Renew == nil {
		return func() {}
	}
	every := leaseRenewEvery
	done := make(chan struct{})
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if !tg.Renew() {
					return // the lease is gone (released or the engine was reloaded): nothing left to protect
				}
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// viaPool sends the request to the pool member for function when there is one. It reports whether it answered (including an error answer); false
// means the pool has nothing for this request and the caller continues as before. body is the request body, already read; contentType is its type.
func (s *Server) viaPool(ctx context.Context, w http.ResponseWriter, r *http.Request, function, model, upstreamPath string, body []byte, contentType string) bool {
	if s.deps.Specialists == nil {
		return false
	}
	tg, err := s.deps.Specialists.Resolve(ctx, function, model)
	if errors.Is(err, specialist.ErrNoMatch) {
		return false
	}
	if err != nil {
		s.writePoolError(ctx, w, function, err)
		return true
	}
	// The lease taken with the target keeps the member from being evicted while this request runs, whatever the function; Finish gives it back when the
	// response has ended (also when the client went away or the upstream failed: ServeHTTP returns either way). A long response renews it.
	defer tg.Finish()
	stopRenew := renewWhileOpen(tg)
	defer stopRenew()
	target, perr := url.Parse(tg.BaseURL)
	if perr != nil || target.Host == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "bad specialist address"})
		return true
	}
	actx := withRouteOpts(ctx, routeOpts{auditAction: "specialist"})
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.URL.Path, pr.Out.URL.RawPath = target.Scheme, target.Host, upstreamPath, ""
			pr.Out.Host = ""
			pr.Out.Header.Del("Authorization") // the caller's a0 credential is not the member's business
			if contentType != "" {
				pr.Out.Header.Set("Content-Type", contentType)
			}
			pr.Out.Body = io.NopCloser(bytes.NewReader(body))
			pr.Out.ContentLength = int64(len(body))
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, _ error) {
			s.auditOutcome(actx, tg.Occupant, "error", function+":upstream_error")
			writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "specialist upstream unreachable", "model": tg.Occupant})
		},
		ModifyResponse: func(resp *http.Response) error {
			label := "ok"
			if resp.StatusCode >= 400 {
				label = "4xx"
			}
			s.auditOutcome(actx, tg.Occupant, label, function+":"+label+" slot="+tg.Slot)
			return nil
		},
		FlushInterval: -1,
		Transport:     s.httpClient().Transport,
	}
	proxy.ServeHTTP(w, r)
	return true
}

func (s *Server) writePoolError(ctx context.Context, w http.ResponseWriter, function string, err error) {
	var full *specialist.PoolFullError
	var unavail *specialist.UnavailableError
	switch {
	case errors.As(err, &full):
		body := map[string]any{"error": errCodePoolFull, "function": function, "model": full.Occupant, "holders": full.Holders, "message": full.Error()}
		if full.RetryAfter > 0 {
			secs := int((full.RetryAfter + time.Second - 1) / time.Second)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			body["retry_after_s"] = secs
		}
		s.auditOutcome(ctx, full.Occupant, "error", function+":"+errCodePoolFull)
		writeJSON(w, http.StatusServiceUnavailable, body)
	case errors.As(err, &unavail):
		s.auditOutcome(ctx, unavail.Occupant, "error", function+":"+errCodeUnavailable)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errCodeUnavailable, "function": function, "model": unavail.Occupant, "message": unavail.Error()})
	default:
		s.auditOutcome(ctx, "", "error", function+":pool_error")
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errCodeUnavailable, "function": function, "message": err.Error()})
	}
}

// peekModel returns the `model` of a JSON request body ("" when there is none or it is not JSON).
func peekModel(body []byte) string {
	var b struct {
		Model string `json:"model"`
	}
	if decodeBodyExact(body, &b) != nil {
		return ""
	}
	return b.Model
}

// align is POST /v1/align: forced alignment (word timestamps for caption sync) through the specialist pool. There is no older route to fall back
// to, so with no pool member for alignment it answers 503 specialist_unavailable. The body is the aligner's own JSON and is forwarded untouched.
func (s *Server) align(w http.ResponseWriter, r *http.Request) {
	ctx, cancel, ok := s.audioRequestCtx(w, r)
	if !ok {
		return
	}
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, maxTranscriptionBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body exceeds 64 MiB limit"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if s.viaPool(ctx, w, r, store.FnAlignment, peekModel(raw), "/v1/align", raw, r.Header.Get("Content-Type")) {
		return
	}
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": errCodeUnavailable, "function": store.FnAlignment,
		"message": "no specialist-pool member is set for alignment"})
}
