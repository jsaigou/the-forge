// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/engine"
	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/specialist"
	"github.com/jsaigou/the-forge/internal/store"
)

// catalogResolver resolves a catalog config for the specialist pool from the SAME merged config the LLM slots launch from (ADR-0022): the model
// and mmproj resolved against the models dir, the llama-server the launcher would pick for the config's backend (or the config's own build), the
// context, and the config's own extra args. The pool therefore launches exactly what an LLM slot would.
type catalogResolver struct {
	cfg      func() *config.Config
	estimate func(configID int64) (int64, bool) // the registry's curated memory figure, when there is one
}

// defaultLlamaBin mirrors start-aN.sh's choice when a config names no build of its own.
func defaultLlamaBin(backend string) string {
	if backend == "rocm" {
		return "/opt/forge/llama.cpp-upstream-plain/build-rocm-plain/bin/llama-server"
	}
	return "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server"
}

func (r catalogResolver) service(name string) (config.Mode, config.Service, error) {
	cfg := r.cfg()
	mode, ok := cfg.Modes[name]
	if !ok {
		return config.Mode{}, config.Service{}, fmt.Errorf("no catalog config named %q", name)
	}
	if mode.Type == "service" || len(mode.Services) == 0 {
		return config.Mode{}, config.Service{}, fmt.Errorf("%q is not a model config", name)
	}
	return mode, mode.Services[0], nil
}

func (r catalogResolver) LaunchSpec(_ context.Context, name string) (specialist.LaunchSpec, error) {
	_, svc, err := r.service(name)
	if err != nil {
		return specialist.LaunchSpec{}, err
	}
	cfg := r.cfg()
	// An engine whose launch is a slot_launches row (Radiance) is hosted through the row and `forge slot-exec`, not through the llama-server launcher.
	if eng, err := svc.Engine(); err != nil {
		return specialist.LaunchSpec{}, err // an unknown engine kind fails closed, exactly as an LLM load does
	} else if eng.Launch == enginekind.LaunchRadiance {
		spec := specialist.LaunchSpec{Row: true, Engine: eng.Name, Bin: svc.LlamaBin, Model: cfg.Paths.ResolveModelPath(svc.Model), Alias: svc.Alias,
			Backend: svc.Backend, Ctx: svc.Context, Args: svc.ExtraArgs}
		if svc.MMProj != "" {
			spec.MMProj = cfg.Paths.ResolveModelPath(svc.MMProj)
		}
		return spec, nil
	}
	backend := svc.Backend
	if backend == "" {
		backend = "vulkan"
	}
	bin := svc.LlamaBin
	if bin == "" {
		bin = defaultLlamaBin(backend)
	}
	spec := specialist.LaunchSpec{Bin: bin, Model: cfg.Paths.ResolveModelPath(svc.Model), Alias: svc.Alias, Backend: backend, Ctx: svc.Context, Args: svc.ExtraArgs}
	if svc.MMProj != "" {
		spec.MMProj = cfg.Paths.ResolveModelPath(svc.MMProj)
	}
	return spec, nil
}

// LaunchRow builds the slot_launches row for a config hosted on a pool slot: the same builder the LLM slots use, with the pool slot's port.
func (r catalogResolver) LaunchRow(_ context.Context, name, slot string, port int) (store.SlotLaunch, error) {
	mode, svc, err := r.service(name)
	if err != nil {
		return store.SlotLaunch{}, err
	}
	return engine.BuildSlotLaunchRowOn(r.cfg(), slot, port, name, mode.ConfigID, svc)
}

func (r catalogResolver) EstimateBytes(_ context.Context, name string) int64 {
	mode, svc, err := r.service(name)
	if err != nil {
		return 0
	}
	if r.estimate != nil && mode.ConfigID != 0 {
		if b, ok := r.estimate(mode.ConfigID); ok && b > 0 {
			return b
		}
	}
	// No curated figure: the weight files on disk (the engine's own fallback), with a margin for the KV cache and buffers.
	cfg := r.cfg()
	var total int64
	for _, f := range []string{svc.Model, svc.MMProj} {
		if f == "" {
			continue
		}
		if fi, err := os.Stat(cfg.Paths.ResolveModelPath(f)); err == nil {
			total += fi.Size()
		}
	}
	return total + total/5
}
