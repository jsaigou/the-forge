// SPDX-License-Identifier: Apache-2.0

package router

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/jsaigou/the-forge/internal/authz"
)

// embeddings implements POST /v1/embeddings as a STATIC passthrough to the
// always-on embedding service (Contract 1 §a0 amendment 2026-07-22). Unlike
// the chat hot path it has NO routing, NO failover, NO catalog gating, and
// NO on-demand scheduling — the embedding service is CPU-only and permanent,
// so there is a single fixed upstream (RouterConfig.EmbeddingURL, typically
// http://127.0.0.1:8083/v1). This makes a0 a complete OpenAI provider so a
// consumer pointed at a0 gets chat AND embeddings from one base URL.
//
// Auth is tailnet-conditional, identical to the chat path (checkAuth). The
// request body passes through byte-for-byte — a0 does not inspect, validate,
// or rewrite embeddings requests (no wire_model concept here).
func (s *Server) embeddings(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r).ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
		return
	}

	base := s.cfg().EmbeddingURL
	if base == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "embeddings passthrough not configured (set [router].embedding_url)",
		})
		return
	}

	// Apply the overall request timeout, matching the chat path.
	ctx := r.Context()
	if timeout := s.cfg().requestTimeout(); timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Attribute the effective client address for the audit trail (chat-path
	// parity; auditOutcome reads it back — see clientAddrKey).
	ctx = context.WithValue(ctx, clientAddrKey{},
		authz.EffectiveRemoteAddr(parseRemoteAddr(r.RemoteAddr), r.Header.Get("X-Forwarded-For")).String())

	target, err := url.Parse(base)
	if err != nil || target.Host == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "bad embedding_url"})
		return
	}
	// Fixed upstream path: <base path>/embeddings. We set the URL fields
	// explicitly rather than using ProxyRequest.SetURL, which would join the
	// inbound request path onto the target and double it.
	upstreamPath := strings.TrimRight(target.Path, "/") + "/embeddings"

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			pr.Out.URL.Path = upstreamPath
			pr.Out.URL.RawPath = ""
			pr.Out.Host = "" // don't forward our Host header upstream
		},
		ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, _ error) {
			// Single upstream, no failover — a transport error is a 502.
			s.auditOutcome(ctx, "embeddings", "error", "embeddings:upstream_error")
			writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "embedding upstream unreachable"})
		},
		ModifyResponse: func(resp *http.Response) error {
			label := "ok"
			if resp.StatusCode >= 400 {
				label = "4xx"
			}
			s.auditOutcome(ctx, "embeddings", label, "embeddings:"+label)
			return nil
		},
		FlushInterval: -1,
		Transport:     s.httpClient().Transport,
	}
	proxy.ServeHTTP(w, r)
}
