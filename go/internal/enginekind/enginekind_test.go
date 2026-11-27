// SPDX-License-Identifier: Apache-2.0

package enginekind

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The table is the contract for each kind; a change to any row here is a behaviour change and must be deliberate.
func TestSpecPerKind(t *testing.T) {
	want := map[string]Spec{
		LlamaCpp: {Name: "llamacpp", ProcessNames: []string{"llama-server"}, ReadinessPath: "/health", ContextFromProps: true,
			ChatTemplateCapsFromProps: true, Profilable: true, BinaryRequired: true,
			Metrics: "llamacpp-prometheus", Ledger: "llamacpp-timings", Launch: "llamacpp", BusySeries: "llamacpp:requests_processing"},
		VLLM: {Name: "vllm", ProcessNames: []string{"vllm"}, ReadinessPath: "/health",
			Metrics: "llamacpp-prometheus", Ledger: "llamacpp-timings", Launch: "vllm"},
		Radiance: {Name: "radiance", ProcessNames: []string{"radiance"}, ReadinessPath: "/health",
			ChatTemplateCapsFromProps: true, ChatTemplateCapsFromModels: true, ContextFromModels: true,
			ContextCapacitySeries: "radiance:kv_cache_max_tokens", BinaryRequired: true, NeedsCuratedMemory: true, UsesLaunchEnv: true,
			Metrics: "radiance-prometheus", Ledger: "openai-cached-tokens", Launch: "radiance", BusySeries: "vllm:num_requests_running"},
		Radiance2: {Name: "radiance2", ProcessNames: []string{"radiance"}, ReadinessPath: "/health",
			ChatTemplateCapsFromProps: true, ChatTemplateCapsFromModels: true, ContextFromModels: true,
			ContextCapacitySeries: "radiance:kv_cache_max_tokens", BinaryRequired: true, NeedsCuratedMemory: true, UsesLaunchEnv: true,
			Metrics: "radiance-prometheus", Ledger: "openai-cached-tokens", Launch: "radiance2", BusySeries: "vllm:num_requests_running"},
	}
	for kind, w := range want {
		got, err := Lookup(kind)
		if err != nil {
			t.Fatalf("Lookup(%q): %v", kind, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("Lookup(%q) = %+v, want %+v", kind, got, w)
		}
	}
	if got := Kinds(); !reflect.DeepEqual(got, []string{"llamacpp", "radiance", "radiance2", "vllm"}) {
		t.Errorf("Kinds() = %v: adding a kind must update this test and ADR-0025", got)
	}
}

func TestEmptyKindIsLlamaCpp(t *testing.T) {
	s, err := Lookup("")
	if err != nil || s.Name != LlamaCpp {
		t.Fatalf("Lookup(\"\") = %+v, %v; want llamacpp", s, err)
	}
}

func TestUnknownKindFailsClosed(t *testing.T) {
	for _, kind := range []string{"bogus", "LlamaCpp", "llama.cpp", "Radiance"} {
		_, err := Lookup(kind)
		if !errors.Is(err, ErrUnknownKind) {
			t.Fatalf("Lookup(%q) err = %v, want ErrUnknownKind", kind, err)
		}
		if !strings.Contains(err.Error(), kind) || !strings.Contains(err.Error(), "llamacpp") {
			t.Errorf("error %q should name the kind and the known ones", err)
		}
		if _, err := Resolve(kind, "rocm"); !errors.Is(err, ErrUnknownKind) {
			t.Errorf("Resolve(%q, rocm) err = %v, want ErrUnknownKind", kind, err)
		}
	}
}

// The legacy backend "vllm" keeps meaning the vLLM engine, but only where the engines row has not said otherwise.
func TestResolveLegacyBackend(t *testing.T) {
	for _, tc := range []struct{ kind, backend, want string }{
		{"", "vulkan", LlamaCpp},
		{"", "rocm", LlamaCpp},
		{LlamaCpp, "rocm", LlamaCpp},
		{VLLM, "rocm", VLLM},
		{"", "vllm", VLLM},
		{LlamaCpp, "vllm", VLLM},
		{VLLM, "vllm", VLLM},
	} {
		s, err := Resolve(tc.kind, tc.backend)
		if err != nil || s.Name != tc.want {
			t.Errorf("Resolve(%q,%q) = %q, %v; want %q", tc.kind, tc.backend, s.Name, err, tc.want)
		}
	}
	// An unknown kind is not rescued by a legacy backend.
	if _, err := Resolve("bogus", "vllm"); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("Resolve(bogus, vllm) err = %v, want ErrUnknownKind", err)
	}
}

// Orphan sweeps look for every engine's processes; the order is the one the sweep always used.
func TestProcessNames(t *testing.T) {
	if got := ProcessNames(); !reflect.DeepEqual(got, []string{"llama-server", "radiance", "vllm"}) {
		t.Errorf("ProcessNames() = %v", got)
	}
}

// Only the kinds that need a curated, tuple-bound memory figure and a launch environment say so; nothing else changes behaviour.
func TestKindCapabilityFlagsAreRadianceOnly(t *testing.T) {
	for _, kind := range Kinds() {
		s, err := Lookup(kind)
		if err != nil {
			t.Fatal(err)
		}
		want := IsRadiance(kind)
		if s.NeedsCuratedMemory != want || s.UsesLaunchEnv != want {
			t.Errorf("%s: NeedsCuratedMemory=%v UsesLaunchEnv=%v, want both %v", kind, s.NeedsCuratedMemory, s.UsesLaunchEnv, want)
		}
		if s.Launch == "" {
			t.Errorf("%s has no launch builder name", kind)
		}
	}
}

// The two Radiance kinds are one engine with two launch forms: they must differ only in name and launch builder.
func TestRadianceKindsDifferOnlyInNameAndLaunch(t *testing.T) {
	a, _ := Lookup(Radiance)
	b, _ := Lookup(Radiance2)
	a.Name, a.Launch, b.Name, b.Launch = "", "", "", ""
	if !reflect.DeepEqual(a, b) {
		t.Errorf("radiance and radiance2 differ beyond name and launch:\n%+v\n%+v", a, b)
	}
	if !IsRadiance(Radiance) || !IsRadiance(Radiance2) || IsRadiance("") || IsRadiance(LlamaCpp) || IsRadiance("radiance3") {
		t.Error("IsRadiance must be true for exactly the two Radiance kinds")
	}
}
