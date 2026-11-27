// SPDX-License-Identifier: Apache-2.0

// Package launch builds the exact executable, argument vector and added environment of an inference slot's engine process (ADR-0026 section 5,
// Radiance Phase 1b step b). All engine knowledge is here, in Go: the output is stored as a slot_launches row and exec()ed by `forge slot-exec`,
// and nothing is ever interpreted by a shell again.
//
// For the engine kinds that exist today (llama.cpp and vLLM) the builders reproduce byte for byte what the static launcher
// (the legacy per-slot scripts, frozen in testdata/legacy) did with the env file and args file the daemon used to write (retired in Phase 1b step d2; the golden tests still model that file format). The golden tests run the
// real scripts and compare. Where the script is lenient and the builder refuses (an argument that the args file cannot carry, a value the systemd
// env-file parser would rewrite, an unknown backend), the refusal is deliberate and tested; see validate.go.
package launch

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/store"
)

// ErrInvalid wraps every refusal of a bad Input or a bad Launch; the message names the field.
var ErrInvalid = errors.New("launch: invalid input")

var envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Launch is what is executed: Exe (absolute) with Argv (the arguments after argv[0]), the variables to ADD to the inherited environment, and an
// optional working directory (empty: inherit). It fits store.SlotLaunch field for field.
type Launch struct {
	Exe  string
	Argv []string
	Env  map[string]string
	Cwd  string
}

// Input is everything a builder needs, and exactly what the engine already holds when it calls writeServiceFiles.
type Input struct {
	Slot       string // slot name (a1..a4, ...); checked against the slot-name rule
	Port       int    // the slot's port, from config.Slots[slot].Port (never derived from the name here)
	Alias      string // served model name
	Context    int    // configured context (0 passes through: llama.cpp reads it as "model default")
	ModelPath  string // resolved (absolute when the models dir is set)
	MmprojPath string // resolved; empty: none (llama.cpp only)
	Backend    string // vulkan | rocm | "" (= vulkan) for llama.cpp; the legacy "vllm" for vLLM
	Binary     string // the engine binary override (config.Service.LlamaBin / the build's binary_path); empty: the backend default
	ExtraArgs  []string
	// InheritedLDLibraryPath is the LD_LIBRARY_PATH the process would inherit; the vLLM launcher appends it after its own entry. Row env only ADDS
	// variables, so the caller passes what it wants kept (empty: nothing).
	InheritedLDLibraryPath string
	// LaunchEnv is the build row's launch_env (migration 0106): the host environment the engine needs, validated at write time by the build
	// guard. Only a builder whose Spec.UsesLaunchEnv is set reads it; the llama.cpp and vLLM builders are byte-pinned to the legacy scripts and
	// ignore it (the build API refuses a launch_env on those kinds).
	LaunchEnv map[string]string
}

// Default llama-server binaries, exactly the ones start-aN.sh hard-codes (the scripts' fallbacks are pinned by TestLegacyFallbackPathsAreTheLivePlainBuilds).
// config.Paths.VulkanBin/RocmBin are NOT used: the scripts never read them.
const (
	DefaultRocmLlamaServer   = "/opt/forge/llama.cpp-upstream-plain/build-rocm-plain/bin/llama-server"
	DefaultVulkanLlamaServer = "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server"
)

type builder func(Input) (Launch, error)

// checked runs the env-file/args-file input rules (validate.go) before a builder that reproduces a legacy script; those rules exist because the
// script's inputs travelled through an env file and an args file. A builder that never touches those files does its own checks.
func checked(b builder) builder {
	return func(in Input) (Launch, error) {
		if err := checkInput(in); err != nil {
			return Launch{}, err
		}
		return b(in)
	}
}

// builders is keyed by enginekind.Spec.Launch: the dispatch is by the name the kind table gives, never by comparing a kind.
var builders = map[string]builder{
	enginekind.LaunchLlamaCpp:  checked(buildLlamaCpp),
	enginekind.LaunchVLLM:      checked(buildVLLM),
	enginekind.LaunchRadiance:  buildRadiance,
	enginekind.LaunchRadiance2: buildRadiance2,
}

// Build returns the launch for kind. An empty kind is llama.cpp and the legacy backend "vllm" selects vLLM (enginekind.Resolve); an unknown kind
// is enginekind.ErrUnknownKind. A successful result has passed Check.
func Build(kind string, in Input) (Launch, error) {
	spec, err := enginekind.Resolve(kind, in.Backend)
	if err != nil {
		return Launch{}, err
	}
	b, ok := builders[spec.Launch]
	if !ok { // a kind in enginekind with no builder here: fail closed
		return Launch{}, fmt.Errorf("%w %q: no launch builder %q", enginekind.ErrUnknownKind, spec.Name, spec.Launch)
	}
	l, err := b(in)
	if err != nil {
		return Launch{}, err
	}
	if err := l.Check(); err != nil {
		return Launch{}, err
	}
	return l, nil
}

// FromService maps what the daemon has in hand (the config, the slot name, the resolved service) to an Input with no other translation: the port is
// the configured slot's, the model and mmproj paths are resolved the way the daemon always resolved them.
func FromService(cfg *config.Config, slot string, svc config.Service) (Input, error) {
	s, ok := cfg.Slots[slot]
	if !ok {
		return Input{}, fmt.Errorf("%w: slot %q is not configured", ErrInvalid, slot)
	}
	return FromServicePort(cfg, slot, s.Port, svc)
}

// FromServicePort is FromService for a slot that is not in cfg.Slots (a specialist pool slot, ADR-0022: its port is configured under
// cfg.SpecialistSlots and handed in by the caller).
func FromServicePort(cfg *config.Config, slot string, port int, svc config.Service) (Input, error) {
	in := Input{
		Slot:      slot,
		Port:      port,
		Alias:     svc.Alias,
		Context:   svc.Context,
		ModelPath: cfg.Paths.ResolveModelPath(svc.Model),
		Backend:   svc.Backend,
		Binary:    svc.LlamaBin,
		ExtraArgs: svc.ExtraArgs,
		LaunchEnv: svc.LaunchEnv,
	}
	if svc.MMProj != "" {
		in.MmprojPath = cfg.Paths.ResolveModelPath(svc.MMProj)
	}
	return in, nil
}

// BuildService is the daemon-facing entry: FromService, then Build with the service's own engine kind (svc.Engine(), which fails closed on an unknown
// kind exactly as the daemon's load path does).
func BuildService(cfg *config.Config, slot string, svc config.Service) (Launch, error) {
	spec, err := svc.Engine()
	if err != nil {
		return Launch{}, err
	}
	in, err := FromService(cfg, slot, svc)
	if err != nil {
		return Launch{}, err
	}
	return Build(spec.Name, in)
}

// BuildServiceOn is BuildService for a slot whose port the caller supplies (a specialist pool slot).
func BuildServiceOn(cfg *config.Config, slot string, port int, svc config.Service) (Launch, error) {
	spec, err := svc.Engine()
	if err != nil {
		return Launch{}, err
	}
	in, err := FromServicePort(cfg, slot, port, svc)
	if err != nil {
		return Launch{}, err
	}
	return Build(spec.Name, in)
}

// Row copies l into a store row skeleton; the caller fills ConfigID, ConfigName, EngineKind, Backend, paths, NCtx and WrittenBy.
func (l Launch) Row(slot string, port int) store.SlotLaunch {
	return store.SlotLaunch{Slot: slot, Port: port, Exe: l.Exe, Argv: l.Argv, Env: l.Env, Cwd: l.Cwd}
}

// Check is the shared validator every builder output passes (ADR-0026 section 5, "Common rules").
func (l Launch) Check() error {
	if !filepath.IsAbs(l.Exe) || strings.IndexByte(l.Exe, 0) >= 0 {
		return fmt.Errorf("%w: exe %q must be an absolute path without NUL", ErrInvalid, l.Exe)
	}
	if l.Cwd != "" && !filepath.IsAbs(l.Cwd) {
		return fmt.Errorf("%w: cwd %q must be an absolute path", ErrInvalid, l.Cwd)
	}
	for i, a := range l.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return fmt.Errorf("%w: argument %d contains a NUL byte", ErrInvalid, i)
		}
		if secretFlag(a) {
			return fmt.Errorf("%w: argument %d is a flag that carries a secret; secrets never go in argv", ErrInvalid, i)
		}
	}
	for k, v := range l.Env {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("%w: env name %q must match ^[A-Z][A-Z0-9_]*$", ErrInvalid, k)
		}
		if secretEnvName(k) {
			return fmt.Errorf("%w: env name %q looks like a secret; secrets never go in the row", ErrInvalid, k)
		}
		if strings.IndexByte(v, 0) >= 0 {
			return fmt.Errorf("%w: value of %s contains a NUL byte", ErrInvalid, k)
		}
	}
	return nil
}
