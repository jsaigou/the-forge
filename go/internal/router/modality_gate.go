// SPDX-License-Identifier: Apache-2.0

package router

// Modality pre-check and hidden-model answer (ADR-0020 F5; docs/api-contract-implementation-plan.md phase 3).
//
// An image sent to a text-only config used to be dispatched (and the model loaded for it) only to come back as a
// llama-server 500/501. The catalog already says what each config can take, so a0 now refuses up front with a 400
// that names the missing modality. A model that exists but is hidden is answered with its own code instead of the
// "model_not_found" an unknown name gets; the status stays 404 (hidden means not routable, ADR-0020 decision 2).

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/jsaigou/the-forge/internal/store"
)

// requestModalities reports which non-text inputs a chat request carries, as catalog modality names.
func requestModalities(body map[string]any) (vision, audio bool) {
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		parts, _ := msg["content"].([]any)
		for _, p := range parts {
			part, _ := p.(map[string]any)
			switch part["type"] {
			case "image_url", "image":
				vision = true
			case "input_audio", "audio_url":
				audio = true
			}
		}
	}
	return vision, audio
}

// targetConfig resolves a requested model name to the catalog config that would serve it: the config itself, or
// an alias's target. Virtual models resolve per call and are not gated.
func (s *Server) targetConfig(ctx context.Context, model string) (store.Config, bool) {
	sc := s.deps.StoreCatalog
	if sc == nil {
		return store.Config{}, false
	}
	if cfg, err := sc.ConfigByName(ctx, model); err == nil {
		return cfg, true
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Config{}, false
	}
	alias, err := sc.ModelAliasByName(ctx, model)
	if err != nil || alias.Visibility == "hidden" {
		return store.Config{}, false
	}
	cfg, err := sc.GetConfig(ctx, alias.ConfigID)
	if err != nil {
		return store.Config{}, false
	}
	return cfg, true
}

// missingModality returns the catalog modality ("vision" or "audio") the request needs and the target config
// cannot take. It fails open: an unresolvable catalog never blocks a request.
func (s *Server) missingModality(ctx context.Context, model string, body map[string]any) (string, bool) {
	vision, audio := requestModalities(body)
	if !vision && !audio {
		return "", false
	}
	cfg, ok := s.targetConfig(ctx, model)
	if !ok || cfg.Visibility == "hidden" {
		return "", false
	}
	return s.missingInputModality(ctx, cfg, vision, audio)
}

// missingInputModality is the check itself, for a caller that has already resolved the config and extracted what the request
// carries (the /v1/systemone body keeps its media in `files`, not in chat messages).
func (s *Server) missingInputModality(ctx context.Context, cfg store.Config, vision, audio bool) (string, bool) {
	if !vision && !audio {
		return "", false
	}
	res, ok := s.resolveConfigModalities(ctx, cfg)
	if !ok {
		return "", false
	}
	has := map[string]bool{}
	for _, m := range res.Enabled {
		has[m] = true
	}
	switch {
	case vision && !has["vision"]:
		return "vision", true
	case audio && !has["audio"]:
		return "audio", true
	}
	return "", false
}

func writeUnsupportedModality(w http.ResponseWriter, model, modality string) {
	input := map[string]string{"vision": "image", "audio": "audio"}[modality]
	writeJSON(w, http.StatusBadRequest, map[string]string{
		"error":    "unsupported_modality",
		"model":    model,
		"modality": modality,
		"message": fmt.Sprintf("%s cannot take %s input: its catalog config has no %s support enabled. "+
			"Use a model that does (see /v1/models architecture.input_modalities), or send text only.", model, input, modality),
	})
}

// isHiddenModel reports whether name is a hidden catalog config or model alias.
func (s *Server) isHiddenModel(ctx context.Context, name string) bool {
	sc := s.deps.StoreCatalog
	if sc == nil {
		return false
	}
	if cfg, err := sc.ConfigByName(ctx, name); err == nil {
		return cfg.Visibility == "hidden"
	}
	if alias, err := sc.ModelAliasByName(ctx, name); err == nil {
		return alias.Visibility == "hidden"
	}
	return false
}
