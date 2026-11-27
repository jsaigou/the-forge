// SPDX-License-Identifier: Apache-2.0

package router

// POST /v1/systemone: typed decisions from a native decision model (LFM3 d1 and friends), spec-compatible with the upstream
// TypeSafe System One API (https://docs.typesafe.ai/api) and with llama-server's own /v1/systemone.
//
// The upstream request carries a required `model`, but llama-server IGNORES it in single-model mode: a wrong or nonexistent
// name still gets an answer from whatever is loaded (verified against a loaded d1-2B, 2026-11-02). So a0 cannot lean on the
// slot to reject a bad name; it resolves and validates the name itself, exactly as it does for chat, and then forwards the body
// byte-for-byte (nothing in it is a chat field, and the slot ignores `model` anyway).
//
// What is deliberately different from chat:
//   - One backend, no failover, no capability-tier substitution: decision models are not interchangeable, and a stand-in that
//     cannot answer is worse than an honest error (routeOpts.noSubstitute).
//   - Aliases resolve to their target config, but an alias's request_defaults are chat fields and are NOT applied (the raw
//     body is forwarded). Virtual models are refused: they pick a different chat model per call, which has no meaning here.
//     Remote offerings are not decision models either, so an offering's wire name is simply "not found" on this route.
//   - An upstream 5xx is streamed to the caller instead of triggering failover: llama-server reports request-shaped failures
//     (a question over --ubatch-size tokens, for example) as 500, and "all_backends_unavailable" would hide the real reason.
//   - Streaming does not exist in this API, so the body is read in full for the audit row (input tokens from `usage`).

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/jsaigou/the-forge/internal/authz"
	"github.com/jsaigou/the-forge/internal/store"
)

func (s *Server) systemOne(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	ctx = context.WithValue(ctx, clientAddrKey{},
		authz.EffectiveRemoteAddr(parseRemoteAddr(r.RemoteAddr), r.Header.Get("X-Forwarded-For")).String())
	ctx = s.attributeConsumer(ctx, r, auth)

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
	r.Body = http.NoBody

	var body map[string]any
	if err := decodeBodyExact(bodyBytes, &body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": map[string]string{"body": "must be a valid JSON object"},
		})
		return
	}
	// Validate only what a0 needs to route: model, and that there is something to answer. Everything else (state, the
	// question shapes, files) is the decision model's to judge and is forwarded untouched.
	model, _ := body["model"].(string)
	if len(model) < 1 || len(model) > 128 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": map[string]string{"model": "must be a string of 1-128 characters"},
		})
		return
	}
	if qs, _ := body["questions"].(map[string]any); len(qs) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "validation_failed",
			"fields": map[string]string{"questions": "must be an object with at least one question"},
		})
		return
	}

	if s.deps.Cfg == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "router not configured"})
		return
	}

	cfgRow, ok := s.targetConfig(ctx, model)
	if !ok || cfgRow.Visibility == "hidden" {
		switch {
		case s.isHiddenModel(ctx, model) || (ok && cfgRow.Visibility == "hidden"):
			writeModelNotRoutable(w, model)
		case s.isVirtualModel(ctx, model):
			writeSystemOneVirtualModel(w, model)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "model_not_found", "model": model})
		}
		return
	}

	// Capability: only a config that produces decisions can answer. If the catalog cannot say (read failure), fail open and
	// let the upstream's own 501 speak, as the input gate does.
	if outs, known := s.resolveConfigOutputs(ctx, cfgRow); known && !hasOutput(outs.Enabled, store.OutputDecisions) {
		writeNotADecisionModel(w, model)
		return
	}
	if vision, audio := systemOneMedia(body); vision || audio {
		if mod, missing := s.missingInputModality(ctx, cfgRow, vision, audio); missing {
			writeUnsupportedModality(w, model, mod)
			return
		}
	}

	cl := &chainLabel{}
	// The success audit row is written from a callback using this ctx; without the action it is filed as a chat completion.
	auditCtx := withRouteOpts(ctx, routeOpts{auditAction: "systemone"})
	ctx = withRouteOpts(ctx, routeOpts{
		upstreamPath:       "/systemone",
		rawBody:            bodyBytes,
		noSubstitute:       true,
		passUpstreamErrors: true,
		auditAction:        "systemone",
		onSuccessBody: func(buf []byte) {
			s.auditOutcome(auditCtx, model, "ok", cl.String()+","+systemOneUsageDetail(buf))
		},
	})

	catChain, handled, errMsg, loadReason, _ := s.catalogChain(ctx, model, requestedByHeader(r), body)
	switch {
	case handled && catChain != nil:
		defer finishChain(catChain)
		s.tryBackends(ctx, w, r, model, body, catChain, 0, cl)
	case handled:
		resp := map[string]string{
			"error":   "catalog_load_failed",
			"model":   model,
			"detail":  errMsg,
			"message": unavailableMessageFor(model, "", errMsg),
		}
		if loadReason != "" {
			resp["reason"] = string(loadReason)
		}
		writeJSON(w, http.StatusBadGateway, resp)
	default:
		// targetConfig found it a moment ago; the catalog changed underneath us.
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "model_not_found", "model": model})
	}
}

// systemOneUsageDetail reads the token usage out of a decision response for the audit row. A body that does not parse is
// recorded as such rather than dropped: a missing figure should be visible, not look like zero.
func systemOneUsageDetail(buf []byte) string {
	var resp struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(buf, &resp); err != nil {
		return "usage=unparsed(" + err.Error() + ")"
	}
	return "input_tokens=" + strconv.Itoa(resp.Usage.InputTokens) + ",output_tokens=" + strconv.Itoa(resp.Usage.OutputTokens)
}

// isVirtualModel reports whether name is a visible virtual model.
func (s *Server) isVirtualModel(ctx context.Context, name string) bool {
	sc := s.deps.StoreCatalog
	if sc == nil {
		return false
	}
	vm, err := sc.VirtualModelByName(ctx, name)
	return err == nil && vm.Visibility != "hidden"
}

// writeModelNotRoutable is the 404 a hidden catalog name gets (same body chat writes inline).
func writeModelNotRoutable(w http.ResponseWriter, model string) {
	writeJSON(w, http.StatusNotFound, map[string]string{
		"error": "model_not_routable",
		"model": model,
		"message": model + " exists in the catalog but is hidden, so a0 does not serve it. " +
			"Unhide it (visibility) to route it, or test it directly against its slot's port.",
	})
}
