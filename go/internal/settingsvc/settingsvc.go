// SPDX-License-Identifier: Apache-2.0

// Package settingsvc is the one place that knows which settings keys exist and what shape their values take
// (ADR-0020 F7). `forge config set` used to check only that a value parsed as JSON and write it straight to the
// settings table, so a typo created a key nothing reads and a mis-shaped smith.binaries.tracked broke the next
// reader silently. Callers that write a setting by name go through Check first.
package settingsvc

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// Validator checks one key's JSON value and returns a human-readable reason when it is not acceptable.
type Validator func(raw json.RawMessage) error

// knownKeys are the settings keys some reader in the codebase uses. TestKnownKeysCoverSource keeps this list in
// step with the string literals in the source, so a new key fails the build until it is registered here.
var knownKeys = map[string]bool{}

// knownPrefixes admit open-ended families (ui.*, nfs.*, notes.*) whose members are app-defined.
var knownPrefixes = []string{"ui.", "nfs.", "notes."}

func init() {
	for _, k := range []string{
		"auth.a0_tailnet_bypass", "auth.network_default_role", "auth.network_provider", "auth.policy",
		"auth.provider.forward_auth_header.header_name", "auth.provider.forward_auth_header.trusted_cidrs",
		"auth.recovery_code_pepper", "auth.step_up_ttl_min", "auth.webauthn.rp_id", "auth.webauthn.rp_name",
		"billing.display_currency", "billing.fx_refresh_min", "billing.fx_source_url",
		"catalog.build_roots", "catalog.launch_env_roots", "catalog.require_if_match", "catalog.show_new_downloads",
		"compressor.external_enabled", "compressor.local_enabled", "compressor.passthrough_all",
		"compressor.passthrough_services",
		"dashboard.pages",
		"infra.cost", "infra.external_tools", "infra.memory", "infra.monitor", "infra.paths.rocm_bin", "infra.paths.vulkan_bin", "infra.paths", "infra.ports",
		"infra.router", "infra.scheduler", "infra.server", "infra.service_icons", "infra.service_links", "infra.tailscale",
		"metrics.retention_days", "metrics.sample_interval_s",
		"router.busy_mode", "router.capability_substitution", "router.provider_failover",
		"scheduler.config",
		"usage.inject_stream_usage", "wizard.completed",
		"smith.auto_handoff_cloud", "smith.auto_recover_device_lost", "smith.autonomy", "smith.binaries.enabled",
		"smith.binaries.tracked", "smith.blocked_work.path", "smith.brain_chain", "smith.brain_residency",
		"smith.build_refresh.forks", "smith.build_refresh.watchlist", "smith.comfyui.enabled", "smith.comfyui.keep_files",
		"smith.comfyui.model_roots", "smith.comfyui.unit", "smith.comfyui.url", "smith.comfyui.workflow_dirs",
		"smith.failopen_budget_ms", "smith.handoff_offerings", "smith.language_ja_enabled", "smith.mesh.services",
		"smith.missed_patterns", "smith.model", "smith.retention", "smith.schedule", "smith.self_review", "smith.settings",
		"smith.tailscale.watch_peers", "smith.thresholds", "smith.tools", "smith.turn_budget",
		"smith.web.cache_ttl", "smith.web.customfetch", "smith.web.customsearch", "smith.web.direct", "smith.web.enabled",
		"smith.web.firecrawl.api_key", "smith.web.firecrawl", "smith.web.provider_order", "smith.web.searxng", "smith.web",
	} {
		knownKeys[k] = true
	}
}

// Known reports whether some reader uses key.
func Known(key string) bool {
	if knownKeys[key] {
		return true
	}
	for _, p := range knownPrefixes {
		if strings.HasPrefix(key, p) && len(key) > len(p) {
			return true
		}
	}
	return false
}

// validators holds the shape check for keys where a wrong shape does real harm. A known key without an entry is
// accepted as any valid JSON (its reader decides), which is what every key did before this package existed.
var validators = map[string]Validator{
	"catalog.require_if_match":      boolean,
	"catalog.show_new_downloads":    boolean,
	"compressor.passthrough_all":    boolean,
	"compressor.external_enabled":   boolean,
	"compressor.local_enabled":      boolean,
	"smith.binaries.enabled":        boolean,
	"smith.web.enabled":             boolean,
	"smith.comfyui.enabled":         boolean,
	"smith.language_ja_enabled":     boolean,
	"catalog.build_roots":           absolutePathList,
	"catalog.launch_env_roots":      absolutePathList,
	"smith.binaries.tracked":        trackedBinaries,
	"smith.build_refresh.forks":     buildRefreshForks,
	"smith.build_refresh.watchlist": stringList,
}

// Check validates a write of raw to key. A key nothing reads is refused unless force is set; a value that is not
// JSON, or not the shape the key's reader expects, is refused always (force skips only the unknown-key check).
func Check(key string, raw []byte, force bool) error {
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("value is not valid JSON: %w", err)
	}
	if !Known(key) && !force {
		return fmt.Errorf("unknown settings key %q (no reader uses it; a typo would silently do nothing). Pass --force to write it anyway", key)
	}
	if v, ok := validators[key]; ok {
		if err := v(raw); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	return nil
}

func boolean(raw json.RawMessage) error {
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return fmt.Errorf("must be true or false")
	}
	return nil
}

// absolutePathList is a non-empty array of absolute directories (the roots a catalog build binary may live under).
func absolutePathList(raw json.RawMessage) error {
	var l []string
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("must be a JSON array of absolute directory paths")
	}
	if len(l) == 0 {
		return fmt.Errorf("must not be empty (leave the key unset to use the defaults)")
	}
	for i, p := range l {
		if err := safePath(fmt.Sprintf("entry %d", i), p); err != nil {
			return err
		}
		if filepath.Clean(p) == "/" {
			return fmt.Errorf("entry %d: / would allow every binary on the host", i)
		}
	}
	return nil
}

func stringList(raw json.RawMessage) error {
	var l []string
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("must be a JSON array of strings")
	}
	return nil
}

// safePath is the injection guard shared by every path a setting hands to an exec: absolute, no shell metacharacters.
func safePath(field, p string) error {
	switch {
	case p == "":
		return fmt.Errorf("%s must not be empty", field)
	case !filepath.IsAbs(p):
		return fmt.Errorf("%s must be an absolute path (got %q)", field, p)
	case strings.ContainsAny(p, "\t\n;|&$`"):
		return fmt.Errorf("%s contains disallowed characters", field)
	}
	return nil
}

var trackedKinds = map[string]bool{"llama_build": true, "python_pkg": true, "runtime": true, "service": true}

func trackedBinaries(raw json.RawMessage) error {
	var list []struct {
		Name        string `json:"name"`
		Kind        string `json:"kind"`
		Path        string `json:"path"`
		SourceKind  string `json:"source_kind"`
		SourceRef   string `json:"source_ref"`
		UpstreamRef string `json:"upstream_ref"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return fmt.Errorf("must be a JSON array of {name, kind, path, source_kind, source_ref, upstream_ref}: %w", err)
	}
	seen := map[string]bool{}
	for i, b := range list {
		at := fmt.Sprintf("entry %d", i)
		if b.Name == "" {
			return fmt.Errorf("%s: name must not be empty", at)
		}
		if seen[b.Name] {
			return fmt.Errorf("%s: duplicate name %q", at, b.Name)
		}
		seen[b.Name] = true
		at = fmt.Sprintf("entry %d (%s)", i, b.Name)
		if !trackedKinds[b.Kind] {
			return fmt.Errorf("%s: kind %q must be one of llama_build, python_pkg, runtime, service", at, b.Kind)
		}
		// A service is tracked by name; every other kind is tracked by the file it runs.
		if b.Kind != "service" {
			if err := safePath("path", b.Path); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
		switch b.SourceKind {
		case "":
		case "git":
			if err := safePath("source_ref", b.SourceRef); err != nil {
				return fmt.Errorf("%s: source_kind git needs a source tree: %w", at, err)
			}
		default:
			return fmt.Errorf("%s: source_kind %q must be empty or \"git\"", at, b.SourceKind)
		}
	}
	return nil
}

func buildRefreshForks(raw json.RawMessage) error {
	var list []struct {
		SourceRef   string `json:"source_ref"`
		Remote      string `json:"remote"`
		UpstreamRef string `json:"upstream_ref"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return fmt.Errorf("must be a JSON array of fork recipes: %w", err)
	}
	seen := map[string]bool{}
	for i, f := range list {
		if err := safePath("source_ref", f.SourceRef); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if seen[f.SourceRef] {
			return fmt.Errorf("entry %d: duplicate source_ref %q", i, f.SourceRef)
		}
		seen[f.SourceRef] = true
		if f.Remote == "" || f.UpstreamRef == "" {
			return fmt.Errorf("entry %d (%s): remote and upstream_ref are required", i, f.SourceRef)
		}
	}
	return nil
}

// validating wraps a settings store so every write goes through the shape checks, whichever handler or agent made it.
type validating struct{ inner store.Settings }

// Validating returns inner with Set checked by Check (unknown keys allowed: a typed handler may write a key built at
// runtime; the unknown-key refusal is the CLI's, where a typo is the likely cause). Get is unchanged.
func Validating(inner store.Settings) store.Settings { return validating{inner} }

func (v validating) Get(ctx context.Context, key string) ([]byte, error) {
	return v.inner.Get(ctx, key)
}

func (v validating) Set(ctx context.Context, key string, value []byte) error {
	if err := Check(key, value, true); err != nil {
		return fmt.Errorf("settings: refusing to write %s: %w", key, err)
	}
	return v.inner.Set(ctx, key, value)
}
