// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jsaigou/the-forge/internal/collector"
	"github.com/jsaigou/the-forge/internal/config"
)

// writeServiceFiles writes /etc/sysconfig/forge-<slot>-env (KEY=VALUE) and
// forge-<slot>-args (one arg per line), preserving non-FORGE_ keys in
// the existing env file (LD_LIBRARY_PATH, HSA_*, ROCm vars — port of
// engine._write_service_files). The env files predate V5 on a deployed host;
// migrate-v4 owns first-run seeding.
func (m *Manager) writeServiceFiles(slot string, svc config.Service) error {
	cfg := m.d.Cfg()
	envPath := filepath.Join(cfg.Paths.SysconfigDir, "forge-"+slot+"-env")
	argsPath := filepath.Join(cfg.Paths.SysconfigDir, "forge-"+slot+"-args")

	// Preserve existing non-FORGE_ vars in their current order.
	var preserved []string
	if raw, err := os.ReadFile(envPath); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			key, _, _ := strings.Cut(line, "=")
			if !strings.HasPrefix(strings.TrimSpace(key), "FORGE_") {
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

	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("engine: writing %s: %w", envPath, err)
	}
	args := strings.Join(svc.ExtraArgs, "\n") + "\n"
	if err := os.WriteFile(argsPath, []byte(args), 0o644); err != nil {
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
func inferSlotMode(cfg *config.Config, env map[string]string) string {
	alias := env["FORGE_MODEL_ALIAS"]
	modelPath := env["FORGE_MODEL_PATH"]
	if alias == "" && modelPath == "" {
		return ""
	}
	for name, mode := range cfg.Modes {
		for _, svc := range mode.Services {
			if alias != "" && svc.Alias == alias {
				return name
			}
			if modelPath != "" && svc.Model != "" && strings.HasSuffix(modelPath, svc.Model) {
				return name
			}
		}
	}
	return ""
}
