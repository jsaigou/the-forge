// SPDX-License-Identifier: Apache-2.0

package router

// Output-capability gates (docs/lfm3-capability-plan.md, ADR-0021). The modality gate (modality_gate.go) answers "can this
// config READ what the request carries"; these answer "can this config PRODUCE what the endpoint returns". A native decision
// model (d1) answers typed questions on /v1/systemone and cannot chat; a chat model cannot answer /v1/systemone (llama-server
// would 501). Refusing up front, from the catalog, is the same idea as the modality pre-check: no model load for a request
// that can only fail.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// errCodeUnsupportedCapability is the stable code for "this model cannot do what this endpoint does".
const errCodeUnsupportedCapability = "unsupported_capability"

// resolveConfigOutputs is resolveConfigModalities' output-side counterpart: what this config can actually produce, via the
// same store resolver the dashboard and /v1/models use, so they can never disagree. ok is false when the catalog can't answer
// (callers then fail open, exactly as the input gate does: the upstream still rejects what it cannot serve).
func (s *Server) resolveConfigOutputs(ctx context.Context, c store.Config) (store.OutputModalityResolution, bool) {
	cat := s.deps.StoreCatalog
	if cat == nil {
		return store.OutputModalityResolution{}, false
	}
	res, err := store.ResolveConfig(ctx, store.CatalogLookup(cat), c)
	if err != nil {
		log.Printf("router: capability gate: %v (failing open)", err)
		return store.OutputModalityResolution{}, false
	}
	return res.Output, true
}

func hasOutput(outs []string, want string) bool {
	for _, o := range outs {
		if o == want {
			return true
		}
	}
	return false
}

// decisionOnlyChat reports whether model resolves to a config that produces decisions and no text, i.e. a chat request to
// it cannot be answered. Hidden, unknown and virtual names are not gated here (their own answers apply).
func (s *Server) decisionOnlyChat(ctx context.Context, model string) bool {
	cfg, ok := s.targetConfig(ctx, model)
	if !ok || cfg.Visibility == "hidden" {
		return false
	}
	res, ok := s.resolveConfigOutputs(ctx, cfg)
	if !ok {
		return false
	}
	return hasOutput(res.Enabled, store.OutputDecisions) && !hasOutput(res.Enabled, store.OutputText)
}

func writeChatToDecisionModel(w http.ResponseWriter, model string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":      errCodeUnsupportedCapability,
		"model":      model,
		"capability": "chat",
		"message": fmt.Sprintf("%s is a decision model: it answers typed questions on POST /v1/systemone and cannot generate chat text. "+
			"Use a chat model for /v1/chat/completions (see /v1/models), or send this request to /v1/systemone.", model),
	})
}

func writeNotADecisionModel(w http.ResponseWriter, model string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":      errCodeUnsupportedCapability,
		"model":      model,
		"capability": "decisions",
		"message": fmt.Sprintf("%s is not a decision model, so it cannot answer /v1/systemone. "+
			"List the decision models with GET /v1/models?output=decisions.", model),
	})
}

func writeSystemOneVirtualModel(w http.ResponseWriter, model string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":      errCodeUnsupportedCapability,
		"model":      model,
		"capability": "decisions",
		"message": fmt.Sprintf("%s is a virtual model, which resolves to a different chat model on every call. "+
			"/v1/systemone needs a specific decision model: pick one from GET /v1/models?output=decisions.", model),
	})
}

// systemOneMedia reports which non-text inputs a /v1/systemone body carries, as catalog modality names. Media travel as
// data URLs in `files` (and its alias `images`); the MIME type in the URL says what it is. Anything that is not a data URL
// string (a remote URL, a malformed entry) is left for the upstream to judge: a0 only refuses what it can prove.
func systemOneMedia(body map[string]any) (vision, audio bool) {
	for _, key := range []string{"files", "images"} {
		items, _ := body[key].([]any)
		for _, it := range items {
			u, _ := it.(string)
			switch {
			case strings.HasPrefix(u, "data:image/"), strings.HasPrefix(u, "data:video/"):
				vision = true
			case strings.HasPrefix(u, "data:audio/"):
				audio = true
			}
		}
	}
	return vision, audio
}
