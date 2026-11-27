// SPDX-License-Identifier: Apache-2.0

// Package enginekind is the single dispatch point for "which inference engine is this?" (ADR-0025).
//
// A Build's `backend` (vulkan | rocm) says which GPU path a binary needs; the engine kind says what software it is and therefore how Forge
// launches it, decides it is ready, reads its metrics and accounts for its context. The kind comes from the catalog `engines` row
// (`engines.kind`), never from the backend. Everything that used to compare `backend == "vllm"` asks the Spec for the kind instead.
//
// Adding a kind is one entry in the table below plus its adapters; an unknown kind is an error (Lookup/Resolve), never a silent default.
package enginekind

import (
	"errors"
	"fmt"
	"sort"
)

// Kind names. These are the values stored in engines.kind.
const (
	LlamaCpp = "llamacpp"
	VLLM     = "vllm"
	Radiance = "radiance"
	// Radiance2 is Radiance 2.x: the same engine behind the same HTTP surface, but it takes its settings from a YAML file (`radiance --config FILE`)
	// and refuses command-line flags (upstream 2.0.0), so its launch differs. Everything else about the kind is Radiance's.
	Radiance2 = "radiance2"
)

// Names of the metrics readers and context-ledger parsers a Spec points at. They are declarative until each reader
// is selected through the Spec (Phase 1b); the tests pin them so a change is deliberate.
const (
	MetricsLlamaCppPrometheus = "llamacpp-prometheus"
	LedgerLlamaCppTimings     = "llamacpp-timings"

	// Radiance names. The readers behind MetricsRadiancePrometheus and LedgerOpenAICachedTokens are built by the metrics/ledger step; the
	// Spec only names them (and the series the generic code must read), so no reader is selected through them yet.
	MetricsRadiancePrometheus = "radiance-prometheus"
	LedgerOpenAICachedTokens  = "openai-cached-tokens"
)

// Launch builder names (internal/launch dispatches on Spec.Launch, never on a kind).
const (
	LaunchLlamaCpp = "llamacpp"
	LaunchVLLM     = "vllm"
	LaunchRadiance = "radiance"
	// LaunchRadiance2 builds the same flag list as LaunchRadiance and stores it unchanged (so launch-check, the spec hash and drift detection mean the
	// same thing); `forge slot-exec` turns that list into the YAML file the 2.x binary reads at the moment of launch.
	LaunchRadiance2 = "radiance2"
)

// Series names the kind table points at.
const (
	SeriesLlamaCppBusy       = "llamacpp:requests_processing"
	SeriesVLLMRunning        = "vllm:num_requests_running"
	SeriesRadianceKVMaxToken = "radiance:kv_cache_max_tokens"
)

// ErrUnknownKind is returned (wrapped) for a kind with no entry in the table.
var ErrUnknownKind = errors.New("unknown engine kind")

// Spec is everything Forge needs to know about an engine kind. It is plain data so the table is the documentation.
type Spec struct {
	// Name is the stored kind (engines.kind).
	Name string
	// ProcessNames are the process names (comm) of the engine's servers, used to find orphans after a unit stop.
	ProcessNames []string
	// ReadinessPath is the HTTP path polled for readiness.
	ReadinessPath string
	// ContextFromProps: the actual n_ctx is read from /props after start (and compared with the configured one). When false the
	// configured context is trusted as is.
	ContextFromProps bool
	// ChatTemplateCapsFromProps: chat_template_caps are captured from /props after a successful load.
	ChatTemplateCapsFromProps bool
	// Profilable: the PROFILE harness can measure this kind.
	Profilable bool
	// BinaryRequired: a Build of this kind needs a binary_path (otherwise the engine is a service, not a file).
	BinaryRequired bool
	// Metrics and Ledger name the metrics reader and the context-creation parser for this kind.
	Metrics string
	Ledger  string

	// Launch names the launch builder in internal/launch (ADR-0026 section 5). Empty is a kind with no builder: the launch is refused.
	Launch string
	// UsesLaunchEnv: the kind's builder puts the build row's launch_env into the process environment. A build of any other kind may not
	// carry one (a value that would be silently ignored is refused at write time).
	UsesLaunchEnv bool
	// ContextFromModels: the configured context is read from /v1/models max_model_len (the engine reports the requested value; it does not
	// negotiate one). Used when ContextFromProps is false.
	ContextFromModels bool
	// ContextCapacitySeries is the /metrics gauge holding the tokens of context the engine can actually keep resident. Where set, the actual
	// context of a load is the smaller of the configured value and this capacity (the engine does not lower the context itself, it only
	// logs a warning when its KV pool is smaller).
	ContextCapacitySeries string
	// ChatTemplateCapsFromModels: when /props is not served, chat-template capabilities come from /v1/models "capabilities".
	ChatTemplateCapsFromModels bool
	// NeedsCuratedMemory: the engine pre-allocates its pools from its launch tuple, so file size says nothing about its footprint. The fit
	// gate refuses a load unless a curated safe_memory_bytes benchmark recorded for exactly this config's tuple exists (fail closed).
	NeedsCuratedMemory bool
	// BusySeries is the /metrics series holding the number of requests being processed (what a router asks to see whether a slot is busy).
	// Empty: not declared (the llama.cpp series is what the readers use today).
	BusySeries string
}

var table = map[string]Spec{
	LlamaCpp: {
		Name:                      LlamaCpp,
		ProcessNames:              []string{"llama-server"},
		ReadinessPath:             "/health",
		ContextFromProps:          true,
		ChatTemplateCapsFromProps: true,
		Profilable:                true,
		BinaryRequired:            true,
		Metrics:                   MetricsLlamaCppPrometheus,
		Ledger:                    LedgerLlamaCppTimings,
		Launch:                    LaunchLlamaCpp,
		BusySeries:                SeriesLlamaCppBusy,
	},
	VLLM: {
		Name:          VLLM,
		ProcessNames:  []string{"vllm"},
		ReadinessPath: "/health",
		// vLLM has no /props: the configured context is reported (V4 parity), no chat-template probe, no PROFILE support yet.
		// Its /metrics are read by the llama.cpp reader, which finds none of its series (unchanged behaviour); a vLLM reader is future work.
		Metrics: MetricsLlamaCppPrometheus,
		Ledger:  LedgerLlamaCppTimings,
		Launch:  LaunchVLLM,
	},
	Radiance:  radianceSpec(Radiance, LaunchRadiance),
	Radiance2: radianceSpec(Radiance2, LaunchRadiance2),
}

// radianceSpec is the Spec of a Radiance-family kind. The 1.17 and 2.x kinds share everything (readiness, context, metrics, memory fit, launch
// environment) except the name stored in engines.kind and the launch builder, so they come from one description and cannot drift apart.
func radianceSpec(name, launch string) Spec {
	return Spec{
		Name:          name,
		ProcessNames:  []string{"radiance"},
		ReadinessPath: "/health", // 200 ok; 503 stopping or stuck (the device advanced nothing for a minute) is not ready
		// /props is served by the fork's source but the evaluation says the deployed binary does not: the context is read from /v1/models
		// (the configured value) and the capacity from /metrics, and the chat-template probe falls back to /v1/models capabilities.
		ContextFromProps:           false,
		ContextFromModels:          true,
		ContextCapacitySeries:      SeriesRadianceKVMaxToken,
		ChatTemplateCapsFromProps:  true,
		ChatTemplateCapsFromModels: true,
		Profilable:                 false,
		BinaryRequired:             true,
		Metrics:                    MetricsRadiancePrometheus,
		Ledger:                     LedgerOpenAICachedTokens,
		Launch:                     launch,
		UsesLaunchEnv:              true,
		NeedsCuratedMemory:         true,
		BusySeries:                 SeriesVLLMRunning,
	}
}

// IsRadiance reports whether a stored kind is any Radiance-family engine (1.17 or 2.x). Code that treats "a Radiance engine" the same, whatever its
// launch form, asks this instead of comparing against one kind name. An empty or unknown kind is not Radiance.
func IsRadiance(kind string) bool { return kind == Radiance || kind == Radiance2 }

// Lookup returns the Spec for a stored kind. An empty kind is the default, llama.cpp: rows that predate the column and file-based
// configs carry none. Any other name that is not in the table is ErrUnknownKind.
func Lookup(kind string) (Spec, error) {
	if kind == "" {
		kind = LlamaCpp
	}
	s, ok := table[kind]
	if !ok {
		return Spec{}, fmt.Errorf("%w %q (known: %v)", ErrUnknownKind, kind, Kinds())
	}
	return s, nil
}

// Resolve is Lookup for a launch spec: the engine row's kind plus the Build's backend. The one legacy rule lives here: a Build whose
// backend is the old value "vllm" is a vLLM engine whatever its engines row says (backend used to mean both things), so existing
// catalogs behave as before. New code never writes backend "vllm".
func Resolve(kind, backend string) (Spec, error) {
	if backend == "vllm" && (kind == "" || kind == LlamaCpp) {
		kind = VLLM
	}
	return Lookup(kind)
}

// Kinds lists the known kinds, sorted.
func Kinds() []string {
	out := make([]string, 0, len(table))
	for k := range table {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ProcessNames is every known engine's process names, for sweeps that must not care which engine owns a port.
func ProcessNames() []string {
	var out []string
	seen := map[string]bool{}
	for _, k := range Kinds() {
		for _, n := range table[k].ProcessNames {
			if !seen[n] { // the Radiance kinds share one process name
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	return out
}
