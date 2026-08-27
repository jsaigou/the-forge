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

// speech.go implements POST /v1/audio/speech + GET /v1/voices as STATIC
// passthroughs to forge-tts (Tier 1 Sprint 3, a0 TTS passthrough) —
// deliberately the same shape as embeddings.go: no routing, no failover, no
// catalog gating, no on-demand scheduling. forge-tts is an always-on
// service like the embedding service, not a slot, so there is nothing to
// place or evict.
//
// Sequenced after Sprint 2 (Voice & Speech settings) so this respects
// whatever engine resident/available/disabled state the operator has
// configured — that's forge-tts's own concern (internal/tts's dualEngine/
// QwenTTS), unchanged by routing the request through a0 instead of hitting
// forge-tts directly. This does not add authentication forge-tts lacks:
// /v1/audio/speech is unauthenticated at forge-tts's own layer regardless
// of entry point, and a0's checkAuth also admits tailnet addresses with no
// identity — what routing through a0 adds is the audit trail and usage
// attribution every other a0 verb gets, plus a bearer requirement
// off-tailnet. /v1/voices is exposed here as GET only (mux.HandleFunc
// registers only the GET method) — forge-tts's own auth only guards
// /v1/voices* for non-GET methods, so a mutating voices call was never a
// candidate for this passthrough in the first place.
//
// X-Forge-Internal-Token is unconditionally stripped from the inbound
// request before proxying, matching the strip-then-set discipline
// resolveBackend's foundry_slot case uses for x-compress-base-url — a
// consumer must never be able to smuggle a value for a header forge-tts's
// own layer treats as a trust boundary, even though GET requests don't
// currently require one.
func (s *Server) speechProxy(upstreamSuffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAuth(r).ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
			return
		}

		base := s.cfg().TTSURL
		if base == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "TTS passthrough not configured (set [router].tts_url)",
			})
			return
		}

		ctx := r.Context()
		if timeout := s.cfg().requestTimeout(); timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		ctx = context.WithValue(ctx, clientAddrKey{},
			authz.EffectiveRemoteAddr(parseRemoteAddr(r.RemoteAddr), r.Header.Get("X-Forwarded-For")).String())

		target, err := url.Parse(base)
		if err != nil || target.Host == "" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "bad tts_url"})
			return
		}
		upstreamPath := strings.TrimRight(target.Path, "/") + upstreamSuffix

		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.Header.Del("X-Forge-Internal-Token")
				pr.Out.URL.Scheme = target.Scheme
				pr.Out.URL.Host = target.Host
				pr.Out.URL.Path = upstreamPath
				pr.Out.URL.RawPath = ""
				pr.Out.Host = ""
			},
			ErrorHandler: func(rw http.ResponseWriter, _ *http.Request, _ error) {
				s.auditOutcome(ctx, "tts", "error", "tts:upstream_error")
				writeJSON(rw, http.StatusBadGateway, map[string]string{"error": "tts upstream unreachable"})
			},
			ModifyResponse: func(resp *http.Response) error {
				label := "ok"
				if resp.StatusCode >= 400 {
					label = "4xx"
				}
				s.auditOutcome(ctx, "tts", label, "tts:"+label)
				return nil
			},
			FlushInterval: -1,
			Transport:     s.httpClient().Transport,
		}
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}
}
