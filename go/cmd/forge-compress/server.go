// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/compress"
)

// server is the OpenAI-compatible reverse proxy: one shared instance fronts
// either the local A1-A4 slots (per-request target chosen via
// x-compress-base-url, exactly like headroom-ai's own contract — see
// routing.go's resolveBackend in internal/router) or one shared "external"
// instance's single configured remote provider fallback.
type server struct {
	cfg     config
	engine  *compress.Engine
	metrics *metrics
	client  *http.Client
	// compressSlots bounds concurrent compression passes (see
	// config.MaxInflight). Buffered channel as a weighted semaphore.
	compressSlots chan struct{}
}

func newServer(cfg config, engine *compress.Engine, m *metrics) *server {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	maxInflight := cfg.MaxInflight
	if maxInflight < 1 {
		maxInflight = 2
	}
	return &server{
		cfg:           cfg,
		engine:        engine,
		metrics:       m,
		compressSlots: make(chan struct{}, maxInflight),
		client: &http.Client{
			// Deliberately no overall response timeout — a large local
			// model's generation can legitimately run well past any fixed
			// bound (see this repo's laguna-s-21 reliability fix: a flat
			// request timeout severed real, healthy long generations). A
			// hung client/upstream is caught by request cancellation
			// (ctx.Done via r.Context()) the same way the a0 router
			// relies on, not a client-side deadline here.
			Transport: &http.Transport{
				DialContext:       dialer.DialContext,
				ForceAttemptHTTP2: true,
			},
		},
	}
}

func (s *server) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/healthz", s.handleHealthz)
	return mux
}

func (s *server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = s.metrics.WriteTo(w)
}

// resolveUpstream determines this request's real target: x-compress-base-url
// when present (the local-fronting contract — see routing.go's
// resolveBackend UpstreamOverride doc comment; this binary appends /v1
// itself, matching headroom-ai >=0.35.0's contract other code already
// depends on), otherwise this instance's own fixed OPENAI_TARGET_API_URL
// (the shared "external" instance's single configured provider fallback).
func (s *server) resolveUpstream(r *http.Request) (string, error) {
	if base := r.Header.Get("x-compress-base-url"); base != "" {
		return strings.TrimRight(base, "/") + "/v1", nil
	}
	if s.cfg.TargetAPIURL != "" {
		return s.cfg.TargetAPIURL, nil
	}
	return "", errNoUpstream
}

var errNoUpstream = &upstreamConfigError{}

type upstreamConfigError struct{}

func (*upstreamConfigError) Error() string {
	return "no x-compress-base-url header and no OPENAI_TARGET_API_URL configured"
}

// X-Forge-Compress headers let a0's router (internal/router/proxy.go)
// tell "the compressor answered, the response just happens to be an
// error" from "the compressor itself is the thing that's broken", without
// guessing from status code alone. A relayed upstream response (even a
// relayed 5xx) carries only headerCompressReached — this binary did its
// job. A failure this binary generated itself instead of relaying also
// carries headerCompressError naming which of its own code paths failed.
const (
	headerCompressReached = "X-Forge-Compress"
	headerCompressError   = "X-Forge-Compress-Error"
)

// writeCompressError sets headerCompressError before delegating to
// http.Error, which calls WriteHeader — headers must be set first.
func writeCompressError(w http.ResponseWriter, reason, message string, status int) {
	w.Header().Set(headerCompressError, reason)
	http.Error(w, message, status)
}

func (s *server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	reqStart := time.Now()
	s.metrics.requests.add(1)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	upstreamBase, err := s.resolveUpstream(r)
	if err != nil {
		s.metrics.requestsFailed.add(1)
		writeCompressError(w, "no_upstream", err.Error(), http.StatusBadGateway)
		return
	}
	targetURL, err := url.Parse(upstreamBase + "/chat/completions")
	if err != nil {
		s.metrics.requestsFailed.add(1)
		writeCompressError(w, "bad_upstream_url", "invalid upstream URL", http.StatusBadGateway)
		return
	}

	rawBody, err := io.ReadAll(r.Body)
	if err != nil {
		s.metrics.requestsFailed.add(1)
		writeCompressError(w, "bad_request", "failed to read request body", http.StatusBadRequest)
		return
	}

	var body map[string]any
	mutatedBody := rawBody
	if len(rawBody) > 0 && json.Unmarshal(rawBody, &body) == nil {
		// Bound the memory-heavy phase (raw body + parsed map + tokenizer
		// windows are all live simultaneously here). Waiting is tied to the
		// request's own ctx: a disconnected client never holds a slot. The
		// RELAY phase below is deliberately unbounded — streaming bytes
		// costs nothing.
		select {
		case s.compressSlots <- struct{}{}:
			defer func() { <-s.compressSlots }()
		case <-r.Context().Done():
			s.metrics.requestsCanceled.add(1)
			return
		}
		compressStart := time.Now()
		budget := time.Duration(s.cfg.FailOpenBudgetMS) * time.Millisecond
		originalTokens, compressedTokens := compressMessages(s.engine, body, budget)
		s.metrics.overhead.observe(msSince(compressStart))
		s.metrics.tokensInput.add(originalTokens)
		if originalTokens > compressedTokens {
			s.metrics.tokensSaved.add(originalTokens - compressedTokens)
		}
		if reencoded, err := json.Marshal(body); err == nil {
			mutatedBody = reencoded
		}
		// A malformed body (not valid JSON, or valid JSON that isn't the
		// expected {"messages": [...]} shape) falls through unmutated —
		// compression is best-effort, never a request-blocking
		// requirement; the upstream gets exactly what the client sent.
		if model, ok := body["model"].(string); ok {
			s.metrics.requestsByModel.add(model, 1)
		}
	}

	var headersAt time.Time
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = targetURL.Scheme
			pr.Out.URL.Host = targetURL.Host
			pr.Out.URL.Path = targetURL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = ""
			// x-compress-base-url is this hop's own internal routing
			// signal (a0 -> this proxy) — meaningless to, and not this
			// binary's business to leak to, the real upstream it's about
			// to call.
			pr.Out.Header.Del("x-compress-base-url")
			// Authorization is forwarded verbatim, never touched — this is
			// what makes the shared "external" instance provider-agnostic
			// by construction: a0 already resolved the real per-request
			// provider.APIKey before calling us, we just relay it (Sprint
			// 3's Architecture section, "Forward Authorization verbatim").
			pr.Out.Header.Set("Content-Type", "application/json")
			pr.Out.Body = io.NopCloser(bytes.NewReader(mutatedBody))
			pr.Out.ContentLength = int64(len(mutatedBody))
		},
		ModifyResponse: func(resp *http.Response) error {
			headersAt = time.Now()
			// This binary reached the upstream and is relaying its real
			// response verbatim — including a relayed 5xx, which is an
			// upstream/backend failure, not a compressor failure.
			resp.Header.Set(headerCompressReached, "1")
			if resp.StatusCode >= 400 {
				s.metrics.requestsFailed.add(1)
			}
			return nil
		},
		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			// A canceled request means the client disconnected — not a
			// compressor failure, and must not count as one (previously
			// misattributed: every ErrorHandler invocation incremented
			// requestsFailed regardless of cause).
			switch {
			case errors.Is(err, context.Canceled):
				s.metrics.requestsCanceled.add(1)
			case isTimeoutErr(err):
				s.metrics.requestsFailed.add(1)
				s.metrics.requestsTimeout.add(1)
			default:
				s.metrics.requestsFailed.add(1)
			}
			log.Printf("forge-compress: upstream error for %s: %v", targetURL, err)
			writeCompressError(rw, "upstream_unreachable", "upstream request failed", http.StatusBadGateway)
		},
		// FlushInterval: -1 flushes immediately after every write — true
		// streaming, no buffering of upstream SSE chunks (matches the a0
		// router's own hard requirement for this exact reason; see
		// proxy.go's comment in internal/router).
		FlushInterval: -1,
		Transport:     s.client.Transport,
	}
	proxy.ServeHTTP(w, r)

	s.metrics.latency.observe(msSince(reqStart))
	if !headersAt.IsZero() {
		s.metrics.ttfb.observe(float64(headersAt.Sub(reqStart).Milliseconds()))
	}
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}

// isTimeoutErr reports whether err (or something it wraps) is a network
// timeout — dial timeout, or the client hitting r.Context()'s deadline
// mid-request (this binary sets no fixed response timeout of its own, see
// newServer's doc comment, so a timeout here means the caller's).
func isTimeoutErr(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return errors.Is(err, context.DeadlineExceeded)
}
