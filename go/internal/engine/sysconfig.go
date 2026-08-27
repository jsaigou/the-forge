// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// slotNameRe bounds what may appear in forge-<slot>-{env,args} filenames
// (and systemd unit names): config-supplied slot keys can never turn the
// service-file path into a traversal.
var slotNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// writeServiceFiles writes /etc/sysconfig/forge-<slot>-env (KEY=VALUE) and
// forge-<slot>-args (one arg per line), preserving non-FORGE_ keys in
// the existing env file (LD_LIBRARY_PATH, HSA_*, ROCm vars — port of
// engine._write_service_files). The env files predate V5 on a deployed host;
// migrate-v4 owns first-run seeding.
//
// FOUNDRY_-prefixed lines are dropped, not preserved, even though they don't
// start with FORGE_ — they are the pre-rename name of these exact keys, not
// an unrelated cross-cutting var. Blanket-preserving "everything not
// FORGE_-prefixed" has already caused two incidents from the same rename:
// GGML_CUDA_ENABLE_UNIFIED_MEMORY silently *dropped* when a slot's env file
// was recreated empty instead of migrated (docs/pitfalls.md, "silently
// dropped by the 2026-07-29 slot rename"), and — the reason this exclusion
// exists — stale FOUNDRY_* lines surviving the 2026-08-24 Foundry→Forge
// rename got preserved forever alongside fresh FORGE_* lines, so the
// deployed launcher script (which the redeploy step of that rename forgot
// to update) kept reading the frozen old values while this engine's own
// bookkeeping moved on. See docs/pitfalls.md's FOUNDRY_*/FORGE_* divergence
// entry for the live incident this fixed.
func (m *Manager) writeServiceFiles(slot string, svc config.Service) error {
	if !slotNameRe.MatchString(slot) {
		return fmt.Errorf("writeServiceFiles: invalid slot name %q", slot)
	}
	cfg := m.d.Cfg()
	envPath := filepath.Join(cfg.Paths.SysconfigDir, "forge-"+slot+"-env")
	argsPath := filepath.Join(cfg.Paths.SysconfigDir, "forge-"+slot+"-args")

	// Preserve existing lines that are neither FORGE_ (rewritten fresh below)
	// nor FOUNDRY_ (permanently supplanted — see the doc comment above).
	var preserved []string
	if raw, err := os.ReadFile(envPath); err == nil { //nolint:gosec // slot charset-validated by slotNameRe
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			key, _, _ := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			if !strings.HasPrefix(key, "FORGE_") && !strings.HasPrefix(key, "FOUNDRY_") {
				preserved = append(preserved, line)
			}
		}
	}

	mmproj := ""
	if svc.MMProj != "" {
		mmproj = cfg.Paths.ResolveModelPath(svc.MMProj)
	}
	lines := []string{
		"FORGE_MODEL_PATH=" + cfg.Paths.ResolveModelPath(svc.Model),
		"FORGE_MODEL_ALIAS=" + svc.Alias,
		fmt.Sprintf("FORGE_CONTEXT=%d", svc.Context),
		"FORGE_MMPROJ=" + mmproj,
		"FORGE_BACKEND=" + svc.Backend,
	}
	// Per-service llama-server override (custom builds — kintsugi/puzzle —
	// that carry features upstream llama.cpp main lags on). The launcher
	// honors FORGE_LLAMA_BIN when set, else selects by FORGE_BACKEND.
	// Written only when non-empty so V4's optional-override semantics hold.
	if svc.LlamaBin != "" {
		lines = append(lines, "FORGE_LLAMA_BIN="+svc.LlamaBin)
	}
	lines = append(lines, preserved...)

	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("engine: writing %s: %w", envPath, err)
	}
	args := strings.Join(svc.ExtraArgs, "\n") + "\n"
	if err := os.WriteFile(argsPath, []byte(args), 0o600); err != nil {
		return fmt.Errorf("engine: writing %s: %w", argsPath, err)
	}
	return nil
}

// slotEnv reads a slot's sysconfig env file.
func (m *Manager) slotEnv(slot string) map[string]string {
	return collector.ReadSlotEnv(m.d.Cfg().Paths.SysconfigDir, slot)
}

// inferSlotMode matches a slot's env file to a configured mode by alias or
// model-path suffix (port of engine._infer_slot_mode).
//
// Alias is checked in a full pass across every mode BEFORE modelPath is ever
// consulted. svc.Alias is always set to the exact catalog Config name (see
// cmd/forge/merged_config.go), so it uniquely identifies one mode even when
// same-weights sibling configs (e.g. a "-nothink" flag variant vs. its base
// config) share the same underlying GGUF and would each satisfy the
// modelPath fallback. Interleaving the two checks per-mode, as a single
// combined loop over cfg.Modes did before, let whichever sibling Go's
// randomized map iteration visited first win the modelPath match even when
// a later-visited mode had the real alias match — 2026-08-25 incident: a
// slot running the "-nothink" sibling got reconciled after a daemon restart
// as its base sibling, which hid it from the scheduler's findLoaded and let
// a real second ~25GB GPU load land on another slot for what was actually
// the same config already resident.
func inferSlotMode(cfg *config.Config, env map[string]string) string {
	alias := env["FORGE_MODEL_ALIAS"]
	modelPath := env["FORGE_MODEL_PATH"]
	if alias == "" && modelPath == "" {
		return ""
	}
	if alias != "" {
		for name, mode := range cfg.Modes {
			for _, svc := range mode.Services {
				if svc.Alias == alias {
					return name
				}
			}
		}
	}
	if modelPath != "" {
		for name, mode := range cfg.Modes {
			for _, svc := range mode.Services {
				if svc.Model != "" && strings.HasSuffix(modelPath, svc.Model) {
					return name
				}
			}
		}
	}
	return ""
}
