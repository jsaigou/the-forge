// SPDX-License-Identifier: Apache-2.0

package smith

// local_seed.go — the layer-2 provisioning seam of the two-layer knowledge
// architecture (operator directive 2026-08-21):
//
//  1. Product knowledge ships with the binary — runbooks, mechanisms,
//     schemas. Generic by construction; zero deployment data.
//  2. Live-environment data never ships — the mesh inventory, fork
//     recipes, tracked binaries are per-install facts. Migrations create
//     the settings seams EMPTY (0060/0061); a fresh install imports its
//     own values from an operator-maintained local file via
//     `forge smith import-local <file>`.
//
// Import semantics: each section present in the file REPLACES the
// corresponding setting wholesale (an explicit [] clears that registry —
// e.g. a deployment with no build_refresh trees imports
// "build_refresh_forks": [] and every procedurization fails closed, the
// intended posture for trees nobody has reviewed yet). Sections absent
// from the file leave the live setting untouched. Import is idempotent
// and fails closed on malformed JSON, unknown fields (typo guard), or
// entries missing their minimum shape.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/jsaigou/the-forge/internal/smith/web"
	"github.com/jsaigou/the-forge/internal/store"
)

// LocalSeed is the schema of the operator's local seed file. All sections
// are optional; pointer-nil means "absent — leave the live setting alone",
// a non-nil (possibly empty) slice means "replace with exactly this".
// Comment is an optional free-text note (JSON has no comments — the
// shipped example uses it for the schema pointer); ignored on import,
// but unknown field names still fail closed as a typo guard.
type LocalSeed struct {
	Comment           string                         `json:"_comment,omitempty"`
	MeshServices      *[]MeshService                 `json:"mesh_services,omitempty"`
	BuildRefreshForks *[]buildRefreshFork            `json:"build_refresh_forks,omitempty"`
	BinariesTracked   *[]TrackedBinary               `json:"binaries_tracked,omitempty"`
	WebProviders      *map[string]web.ProviderConfig `json:"web_providers,omitempty"`
	ComfyUI           *ComfyUISeed                   `json:"comfyui,omitempty"`
}

// ComfyUISeed is the comfyui section of the local seed file — the
// deployment-local ComfyUI service coordinates. `enabled` is deliberately
// not seedable (it is a generic operator preference, not environment data;
// default true). Unit and URL are required when the section is present.
type ComfyUISeed struct {
	Unit         string   `json:"unit"`
	URL          string   `json:"url"`
	ModelRoots   []string `json:"model_roots,omitempty"`
	WorkflowDirs []string `json:"workflow_dirs,omitempty"`
}

// webProviderSettingKeys maps a seed file's web_providers name to the
// settings key its blob writes. Closed set — an unknown provider name in
// the seed fails closed rather than minting an arbitrary smith.web.* key.
var webProviderSettingKeys = map[string]string{
	"searxng":      web.SettingSearxng,
	"firecrawl":    web.SettingFirecrawl,
	"direct":       web.SettingDirect,
	"customsearch": web.SettingCustomSearch,
	"customfetch":  web.SettingCustomFetch,
}

// ImportLocalSeedSummary reports what an import changed. Counts are -1 for
// sections the file did not carry.
type ImportLocalSeedSummary struct {
	MeshServices      int
	BuildRefreshForks int
	BinariesTracked   int
	WebProviders      int
	ComfyUI           int
}

// ImportLocalSeed validates and applies raw (the local seed file's bytes)
// to the live settings store. Callers: `forge smith import-local` and
// the tests.
func ImportLocalSeed(ctx context.Context, settings store.Settings, raw []byte) (ImportLocalSeedSummary, error) {
	sum := ImportLocalSeedSummary{MeshServices: -1, BuildRefreshForks: -1, BinariesTracked: -1, WebProviders: -1, ComfyUI: -1}
	if settings == nil {
		return sum, fmt.Errorf("smith: import-local: no settings store wired")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var seed LocalSeed
	if err := dec.Decode(&seed); err != nil {
		return sum, fmt.Errorf("smith: import-local: %w", err)
	}
	if seed.MeshServices == nil && seed.BuildRefreshForks == nil && seed.BinariesTracked == nil &&
		seed.WebProviders == nil && seed.ComfyUI == nil {
		return sum, fmt.Errorf("smith: import-local: file carries no recognized section (want mesh_services, build_refresh_forks, binaries_tracked, web_providers, comfyui)")
	}

	// Validate EVERY section before writing ANY: a file whose third
	// section is malformed must not leave the first two half-imported.
	if seed.MeshServices != nil {
		for i, svc := range *seed.MeshServices {
			if svc.Name == "" || svc.Address == "" {
				return sum, fmt.Errorf("smith: import-local: mesh_services[%d] needs both name and address", i)
			}
		}
	}
	if seed.BuildRefreshForks != nil {
		for i, fork := range *seed.BuildRefreshForks {
			if fork.SourceRef == "" {
				return sum, fmt.Errorf("smith: import-local: build_refresh_forks[%d] needs a source_ref", i)
			}
			for backend, flags := range fork.Backends {
				if flags.Backend != backend {
					return sum, fmt.Errorf("smith: import-local: build_refresh_forks[%d] backend %q: backend field %q must equal the map key", i, backend, flags.Backend)
				}
			}
		}
	}
	if seed.BinariesTracked != nil {
		for i, tb := range *seed.BinariesTracked {
			if tb.Name == "" || tb.Kind == "" {
				return sum, fmt.Errorf("smith: import-local: binaries_tracked[%d] needs both name and kind", i)
			}
		}
	}
	if seed.WebProviders != nil {
		for name, cfg := range *seed.WebProviders {
			if _, known := webProviderSettingKeys[name]; !known {
				return sum, fmt.Errorf("smith: import-local: web_providers[%q]: unknown provider (want one of searxng, firecrawl, direct, customsearch, customfetch)", name)
			}
			if cfg.BaseURL == "" {
				continue
			}
			u, err := url.Parse(cfg.BaseURL)
			if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
				return sum, fmt.Errorf("smith: import-local: web_providers[%q]: base_url %q is not an absolute http(s) URL", name, cfg.BaseURL)
			}
		}
	}
	if seed.ComfyUI != nil {
		if seed.ComfyUI.Unit == "" || seed.ComfyUI.URL == "" {
			return sum, fmt.Errorf("smith: import-local: comfyui section needs both unit and url")
		}
		u, err := url.Parse(seed.ComfyUI.URL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			return sum, fmt.Errorf("smith: import-local: comfyui.url %q is not an absolute http(s) URL", seed.ComfyUI.URL)
		}
	}

	if seed.MeshServices != nil {
		if err := setSeedSection(ctx, settings, SettingMeshServices, *seed.MeshServices); err != nil {
			return sum, err
		}
		sum.MeshServices = len(*seed.MeshServices)
	}
	if seed.BuildRefreshForks != nil {
		if err := setSeedSection(ctx, settings, SettingBuildRefreshForks, *seed.BuildRefreshForks); err != nil {
			return sum, err
		}
		sum.BuildRefreshForks = len(*seed.BuildRefreshForks)
	}
	if seed.BinariesTracked != nil {
		if err := setSeedSection(ctx, settings, SettingBinariesTracked, *seed.BinariesTracked); err != nil {
			return sum, err
		}
		sum.BinariesTracked = len(*seed.BinariesTracked)
	}
	if seed.WebProviders != nil {
		for name, cfg := range *seed.WebProviders {
			if err := setSeedSection(ctx, settings, webProviderSettingKeys[name], cfg); err != nil {
				return sum, err
			}
		}
		sum.WebProviders = len(*seed.WebProviders)
	}
	if seed.ComfyUI != nil {
		if err := setSeedSection(ctx, settings, SettingComfyUIUnit, seed.ComfyUI.Unit); err != nil {
			return sum, err
		}
		if err := setSeedSection(ctx, settings, SettingComfyUIURL, seed.ComfyUI.URL); err != nil {
			return sum, err
		}
		roots := seed.ComfyUI.ModelRoots
		if roots == nil {
			roots = []string{}
		}
		if err := setSeedSection(ctx, settings, SettingComfyUIModelRoots, roots); err != nil {
			return sum, err
		}
		dirs := seed.ComfyUI.WorkflowDirs
		if dirs == nil {
			dirs = []string{}
		}
		if err := setSeedSection(ctx, settings, SettingComfyUIWorkflowDirs, dirs); err != nil {
			return sum, err
		}
		sum.ComfyUI = 1
	}

	return sum, nil
}

// setSeedSection marshals section and writes it under key, replacing
// whatever the setting held before ("" for absent — Set upserts).
func setSeedSection(ctx context.Context, settings store.Settings, key string, section any) error {
	raw, err := json.Marshal(section)
	if err != nil {
		return fmt.Errorf("smith: import-local: marshal %s: %w", key, err)
	}
	if err := settings.Set(ctx, key, raw); err != nil {
		return fmt.Errorf("smith: import-local: write %s: %w", key, err)
	}
	return nil
}
