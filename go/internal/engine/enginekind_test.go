// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
)

// Behaviour pinned per engine kind (ADR-0025): what the launch files say, which context a load reports, whether chat-template caps are probed,
// and that a kind this binary does not know is refused before anything is stopped or started.

func kindSvc(kind, backend string, ctx int) config.Service {
	return config.Service{Model: "m.gguf", Alias: "m", Context: ctx, PortRole: "a1", Backend: backend, EngineKind: kind, StartupTimeoutS: 120,
		ExtraArgs: []string{"--parallel", "1"}}
}

// The row built for each kind carries the kind and backend (the shim and the engine's read-backs use them, no env file does any more).
func TestSlotLaunchRow_PerKind(t *testing.T) {
	cases := []struct {
		name, kind, backend, wantKind, wantBackend string
	}{
		{"llamacpp vulkan", "llamacpp", "vulkan", "llamacpp", "vulkan"},
		{"default kind is llamacpp", "", "rocm", "llamacpp", "rocm"},
		{"vllm engine", "vllm", "rocm", "vllm", "rocm"},
		{"legacy backend vllm", "", "vllm", "vllm", "vllm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			row, err := BuildSlotLaunchRow(cfg, "a1", "m", 0, kindSvc(tc.kind, tc.backend, 4096))
			if err != nil {
				t.Fatal(err)
			}
			if row.EngineKind != tc.wantKind || row.Backend != tc.wantBackend {
				t.Errorf("row kind/backend = %q/%q, want %q/%q", row.EngineKind, row.Backend, tc.wantKind, tc.wantBackend)
			}
			if row.NCtx != 4096 || row.ModelPath != cfg.Paths.ResolveModelPath("m.gguf") {
				t.Errorf("row n_ctx/model_path = %d/%q", row.NCtx, row.ModelPath)
			}
		})
	}
}

func TestValidateSlotService_UnknownKindFailsClosed(t *testing.T) {
	err := validateSlotService("a1", kindSvc("bogus", "rocm", 4096))
	if err == nil || !strings.Contains(err.Error(), `unknown engine kind "bogus"`) {
		t.Fatalf("err = %v, want an unknown-engine-kind error", err)
	}
	if err := validateSlotService("../x", kindSvc("llamacpp", "rocm", 4096)); err == nil {
		t.Error("a traversal slot name was accepted")
	}
}

// vLLM reports its configured context without asking /props; llama.cpp reports what /props says (the stub differs from the configured value).
func TestVerifiedContext_PerKind(t *testing.T) {
	cfg := testConfig(t)
	m, _, _ := newTestManager(t, cfg, newFakeSys(), newLlamaStub(t, 8192))
	ctx := context.Background()

	if got := m.verifiedContext(ctx, 8080, kindSvc("llamacpp", "vulkan", 32768)); got != 8192 {
		t.Errorf("llamacpp verifiedContext = %d, want the /props value 8192", got)
	}
	if got := m.verifiedContext(ctx, 8080, kindSvc("vllm", "rocm", 32768)); got != 32768 {
		t.Errorf("vllm verifiedContext = %d, want the configured 32768", got)
	}
	if got := m.verifiedContext(ctx, 8080, kindSvc("", "vllm", 32768)); got != 32768 {
		t.Errorf("legacy backend vllm verifiedContext = %d, want the configured 32768", got)
	}
	if got := m.verifiedContext(ctx, 8080, kindSvc("bogus", "rocm", 32768)); got != 0 {
		t.Errorf("unknown kind verifiedContext = %d, want 0 (never a guess)", got)
	}
}

func TestProbesChatTemplateCaps_PerKind(t *testing.T) {
	m, _, _ := newTestManager(t, testConfig(t), newFakeSys(), nil)
	for _, tc := range []struct {
		svc  config.Service
		want bool
	}{
		{kindSvc("llamacpp", "vulkan", 1), true},
		{kindSvc("", "rocm", 1), true},
		{kindSvc("vllm", "rocm", 1), false},
		{kindSvc("", "vllm", 1), false},
		{kindSvc("bogus", "rocm", 1), false},
	} {
		if got := m.probesChatTemplateCaps(tc.svc); got != tc.want {
			t.Errorf("kind %q backend %q: probe = %v, want %v", tc.svc.EngineKind, tc.svc.Backend, got, tc.want)
		}
	}
}

// An unknown kind is refused by both load paths before any unit is stopped or started.
func TestLoadAndSwitch_UnknownKindRefusedBeforeSystemdTouched(t *testing.T) {
	cfg := testConfig(t)
	cfg.Modes["mystery"] = config.Mode{Services: []config.Service{kindSvc("bogus", "rocm", 4096)}}
	sys := newFakeSys()
	sys.setSeq("forge-a1", st("active", "running"))
	m, _, _ := newTestManager(t, cfg, sys, newLlamaStub(t, 4096))

	for name, res := range map[string]Result{
		"Load":       m.Load(context.Background(), "mystery", "a1"),
		"SwitchMode": m.SwitchMode(context.Background(), "mystery"),
	} {
		if res.Success {
			t.Errorf("%s accepted an unknown engine kind", name)
		}
		if !strings.Contains(res.Message, "unknown engine kind") {
			t.Errorf("%s message = %q, want it to name the unknown engine kind", name, res.Message)
		}
	}
	if len(sys.stopped) != 0 || len(sys.started) != 0 {
		t.Errorf("systemd touched for an unknown kind: stopped=%v started=%v", sys.stopped, sys.started)
	}
}
