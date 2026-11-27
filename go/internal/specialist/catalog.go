// SPDX-License-Identifier: Apache-2.0

package specialist

// catalog.go: hosting a CATALOG config in the specialist pool (ADR-0022, step 2 of the residency plan). An occupant of kind `catalog-config` names a
// catalog config; at every start the daemon resolves it exactly as it does for an LLM slot (model, mmproj, llama-server binary, context, the
// config's own args) and renders the launcher's env file plus an args file. A catalog model is either a specialist or an LLM-slot model, never
// both: the scheduler and the engine refuse to load a pooled config into an LLM slot, and a0 routes it to the pool.
//
// Trust model: the args are the catalog's own (admin-written, step-up gated) and the LLM slots already run them, so the pool runs them on the same
// terms. What is checked here and again in start-pool.sh: the binary is a llama-server under the install root, the model and mmproj are paths
// under the model roots, and the args contain none of the flags the pool owns.

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// KindCatalogConfig is the occupant kind that hosts a catalog config.
const KindCatalogConfig = "catalog-config"

// launcherKindCatalog is the launcher's name for the same thing (start-pool.sh).
const launcherKindCatalog = "catalog-llama"

// launcherKindLaunch is the launcher's name for a catalog config whose launch is a slot_launches row (ADR-0026): the engine kinds that are not
// llama.cpp, today Radiance. The launcher runs one fixed command, `forge slot-exec <slot>`; the daemon wrote the row, and slot-exec re-checks the
// executable against the catalog's build roots. All engine knowledge stays in Go (internal/launch).
const launcherKindLaunch = "catalog-launch"

// SlotLaunchStore is the part of the store's slot_launches API the pool uses to host a row-launched engine (store.DB.SlotLaunches() satisfies it).
type SlotLaunchStore interface {
	Put(ctx context.Context, l store.SlotLaunch) (int64, error)
	Get(ctx context.Context, slot string) (store.SlotLaunch, error)
	Disarm(ctx context.Context, slot string, generation int64) (bool, error)
}

// llamaRoot is the one directory a pooled llama-server may live under (the same root the plain llama-server kind uses).
const llamaRoot = "/opt/forge/llama.cpp-upstream-plain"

// LaunchSpec is a catalog config resolved to what a llama-server launch needs.
type LaunchSpec struct {
	// Row marks an engine whose launch is a slot_launches row run by `forge slot-exec` (Radiance), not a llama-server command line the launcher
	// builds. Bin is then the build's binary (informational: the row and slot-exec's build-root check are the walls), Args are the config's own flags,
	// and the real launch comes from CatalogResolver.LaunchRow.
	Row     bool
	Engine  string // the engine kind's name, set with Row
	Bin     string // absolute llama-server path
	Model   string // absolute model path
	MMProj  string // absolute mmproj path, or ""
	Alias   string // the config's name
	Backend string // vulkan | rocm
	Ctx     int
	Args    []string // the config's own extra args, one element per argument
}

// CatalogResolver resolves a catalog config for the pool. The daemon implements it from the merged catalog config, so the pool launches what an LLM
// slot would launch.
type CatalogResolver interface {
	LaunchSpec(ctx context.Context, config string) (LaunchSpec, error)
	// EstimateBytes is the config's expected memory use (curated figure, else weight size), or 0 when unknown.
	EstimateBytes(ctx context.Context, config string) int64
	// LaunchRow builds the slot_launches row for a Row spec on the given pool slot and port (the same builder the LLM slots use).
	LaunchRow(ctx context.Context, config, slot string, port int) (store.SlotLaunch, error)
}

// poolOwnedFlags are the flags the pool sets itself or refuses to pass through: they would conflict with the slot's own launch, expose the process
// beyond loopback, or move it off its port. start-pool.sh keeps the same list (a test feeds each through the launcher).
var poolOwnedFlags = []string{
	"--model", "-m", "--alias", "--host", "--port", "--ctx-size", "-c", "--n-gpu-layers", "-ngl", "--mmproj", "--metrics",
	"--api-key", "--api-key-file", "--ssl-key-file", "--ssl-cert-file", "--path",
}

// ValidateLaunchSpec checks a resolved catalog config against the pool's rules. modelRoots are the directories model files may live under.
func ValidateLaunchSpec(spec LaunchSpec, modelRoots []string) error {
	if spec.Row {
		return validateRowSpec(spec, modelRoots)
	}
	if !path.IsAbs(spec.Bin) || path.Clean(spec.Bin) != spec.Bin || path.Base(spec.Bin) != "llama-server" || !strings.HasPrefix(spec.Bin, llamaRoot+"/") {
		return fmt.Errorf("its llama-server (%s) must be a clean path to llama-server under %s", spec.Bin, llamaRoot)
	}
	if spec.Backend != "vulkan" && spec.Backend != "rocm" {
		return fmt.Errorf("backend %q is not supported in the pool (vulkan and rocm llama-server builds only)", spec.Backend)
	}
	if err := checkParam(ParamPath, spec.Model, modelRoots); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	if spec.MMProj != "" {
		if err := checkParam(ParamPath, spec.MMProj, modelRoots); err != nil {
			return fmt.Errorf("mmproj: %w", err)
		}
	}
	if err := checkParam(ParamWord, spec.Alias, modelRoots); err != nil {
		return fmt.Errorf("alias: %w", err)
	}
	if spec.Ctx <= 0 {
		return fmt.Errorf("context size %d is not positive", spec.Ctx)
	}
	for _, a := range spec.Args {
		if strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("an argument contains a control character")
		}
		for _, f := range poolOwnedFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("the config's args contain %s, which the specialist pool sets itself (for --mmproj, attach the mmproj to the config instead)", f)
			}
		}
	}
	return nil
}

// validateRowSpec checks what the pool can know about a row-launched engine. The launch itself (executable, flags, environment) is built and checked by
// internal/launch and written as a row; slot-exec checks the executable against catalog.build_roots again. Here: the model is under the model roots,
// the alias is a plain word, the context is positive, and no argument carries a control character.
func validateRowSpec(spec LaunchSpec, modelRoots []string) error {
	if spec.Engine == "" {
		return fmt.Errorf("the engine kind of a row-launched config is not set")
	}
	if !path.IsAbs(spec.Bin) || path.Clean(spec.Bin) != spec.Bin {
		return fmt.Errorf("its binary (%s) must be a clean absolute path", spec.Bin)
	}
	if err := checkParam(ParamPath, spec.Model, modelRoots); err != nil {
		return fmt.Errorf("model: %w", err)
	}
	if err := checkParam(ParamWord, spec.Alias, modelRoots); err != nil {
		return fmt.Errorf("alias: %w", err)
	}
	if spec.Ctx <= 0 {
		return fmt.Errorf("context size %d is not positive", spec.Ctx)
	}
	for _, a := range spec.Args {
		if strings.ContainsAny(a, "\x00\n\r") {
			return fmt.Errorf("an argument contains a control character")
		}
	}
	return nil
}

// RenderCatalogEnv is the launcher env file for a catalog-config occupant, and RenderCatalogArgs the args file (one argument per line).
func RenderCatalogEnv(o store.Occupant, slot string, port int, spec LaunchSpec) string {
	if spec.Row {
		// Nothing about the engine travels in the env file: the launcher runs `forge slot-exec <slot>` and the row carries the launch.
		return strings.Join([]string{
			"FORGE_POOL_KIND=" + launcherKindLaunch,
			"FORGE_POOL_SLOT=" + slot,
			"FORGE_POOL_OCCUPANT=" + o.Name,
			"FORGE_POOL_PORT=" + strconv.Itoa(port),
		}, "\n") + "\n"
	}
	lines := []string{
		"FORGE_POOL_KIND=" + launcherKindCatalog,
		"FORGE_POOL_SLOT=" + slot,
		"FORGE_POOL_OCCUPANT=" + o.Name,
		"FORGE_POOL_PORT=" + strconv.Itoa(port),
		"FORGE_POOL_P_BIN=" + spec.Bin,
		"FORGE_POOL_P_MODEL=" + spec.Model,
		"FORGE_POOL_P_ALIAS=" + spec.Alias,
		"FORGE_POOL_P_CTX=" + strconv.Itoa(spec.Ctx),
	}
	if spec.MMProj != "" {
		lines = append(lines, "FORGE_POOL_P_MMPROJ="+spec.MMProj)
	}
	return strings.Join(lines, "\n") + "\n"
}

// RenderCatalogArgs is the args file content: one argument per line, exactly as start-aN.sh reads its args file.
func RenderCatalogArgs(spec LaunchSpec) string {
	if spec.Row || len(spec.Args) == 0 { // a row-launched engine's flags are in the row, not in an args file
		return ""
	}
	return strings.Join(spec.Args, "\n") + "\n"
}
