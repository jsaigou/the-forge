// SPDX-License-Identifier: Apache-2.0

package launch_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/launch"
)

var radArgs = []string{"--tp", "1", "--kv-cache-dtype", "fp8", "--max-num-seqs", "4", "--num-speculative-tokens", "4"}

func radInput() launch.Input {
	return launch.Input{Slot: "a2", Port: 8081, Alias: "qwen36-rad", Context: 262144, ModelPath: "/opt/forge/models/m.rad",
		Binary: "/opt/radiance/src/build/bin/radiance", ExtraArgs: append([]string{}, radArgs...),
		LaunchEnv: map[string]string{"RADIANCE_HOME": "/opt/radiance/src/build/radiance_home", "RADIANCE_KERNELS": "libhalo"}}
}

// The seqs/throughput study server flags (--tp 1 --kv-cache-dtype fp8 --max-num-seqs N --max-model-len L --num-speculative-tokens 4; the studies do
// not record host/port/served-name, those were harness choices) are reproduced flag for flag. The owned differences are what Forge supplies:
// --model path, --served-model-name, --host 127.0.0.1, --port, and the order (owned flags first, then the config's flags verbatim).
func TestRadianceMatchesStudyFlags(t *testing.T) {
	l, err := launch.Build("radiance", radInput())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--model", "/opt/forge/models/m.rad", "--served-model-name", "qwen36-rad", "--host", "127.0.0.1", "--port", "8081",
		"--max-model-len", "262144", "--tp", "1", "--kv-cache-dtype", "fp8", "--max-num-seqs", "4", "--num-speculative-tokens", "4"}
	if !reflect.DeepEqual(l.Argv, want) {
		t.Errorf("argv = %v\nwant   %v", l.Argv, want)
	}
	if l.Exe != "/opt/radiance/src/build/bin/radiance" {
		t.Errorf("exe = %s", l.Exe)
	}
	// Env is the build's launch_env and nothing else (no PATH, LD_LIBRARY_PATH, ROCM_PATH invented).
	if !reflect.DeepEqual(l.Env, map[string]string{"RADIANCE_HOME": "/opt/radiance/src/build/radiance_home", "RADIANCE_KERNELS": "libhalo"}) {
		t.Errorf("env = %v", l.Env)
	}
	in := radInput()
	l2, _ := launch.Build("radiance", in)
	l2.Env["X"] = "y"
	if _, ok := in.LaunchEnv["X"]; ok {
		t.Error("builder aliased the input env map")
	}
}

func TestRadianceNoEnvWithoutLaunchEnv(t *testing.T) {
	in := radInput()
	in.LaunchEnv = nil
	l, err := launch.Build("radiance", in)
	if err != nil || len(l.Env) != 0 {
		t.Fatalf("env = %v, err = %v; want empty", l.Env, err)
	}
}

func TestRadianceRefusals(t *testing.T) {
	without := func(flag string) []string {
		var out []string
		a := radArgs
		for i := 0; i < len(a); i++ {
			if a[i] == flag {
				i++
				continue
			}
			out = append(out, a[i])
		}
		return out
	}
	cases := []struct {
		name   string
		mutate func(*launch.Input)
		want   string
	}{
		{"no max-num-seqs", func(i *launch.Input) { i.ExtraArgs = without("--max-num-seqs") }, "--max-num-seqs is required"},
		{"no speculation depth", func(i *launch.Input) { i.ExtraArgs = without("--num-speculative-tokens") }, "--num-speculative-tokens is required"},
		{"no tp", func(i *launch.Input) { i.ExtraArgs = without("--tp") }, "--tp is required"},
		{"no extra args", func(i *launch.Input) { i.ExtraArgs = nil }, "is required"},
		{"seqs zero", func(i *launch.Input) { i.ExtraArgs = append(without("--max-num-seqs"), "--max-num-seqs", "0") }, "positive integer"},
		{"seqs word", func(i *launch.Input) { i.ExtraArgs = append(without("--max-num-seqs"), "--max-num-seqs", "four") }, "positive integer"},
		{"seqs missing value", func(i *launch.Input) { i.ExtraArgs = append(without("--max-num-seqs"), "--max-num-seqs") }, "needs a value"},
		{"seqs twice", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--max-num-seqs", "2") }, "more than once"},
		{"flag=value", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--kernels=libhalo") }, "two elements"},
		{"owned model", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--model", "/x.rad") }, "set by Forge"},
		{"owned host", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--host", "0.0.0.0") }, "set by Forge"},
		{"owned port", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--port", "1") }, "set by Forge"},
		{"owned max-model-len", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--max-model-len", "0") }, "set by Forge"},
		{"owned served name", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--served-model-name", "x") }, "set by Forge"},
		{"api key", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--api-key", "k") }, "set by Forge"},
		{"api key env", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--api-key-env", "K") }, "set by Forge"},
		{"radiance home flag", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--radiance-home", "/x") }, "set by Forge"},
		{"whitespace token", func(i *launch.Input) { i.ExtraArgs = append(i.ExtraArgs, "--kernels", "a b") }, "one token"},
		{"leading value", func(i *launch.Input) { i.ExtraArgs = append([]string{"4"}, i.ExtraArgs...) }, "start with a flag"},
		{"context zero", func(i *launch.Input) { i.Context = 0 }, "context"},
		{"relative model", func(i *launch.Input) { i.ModelPath = "org/repo" }, "absolute"},
		{"no binary", func(i *launch.Input) { i.Binary = "" }, "binary"},
		{"relative binary", func(i *launch.Input) { i.Binary = "bin/radiance" }, "absolute"},
		{"secret env name", func(i *launch.Input) { i.LaunchEnv = map[string]string{"HF_TOKEN": "x"} }, "secret"},
		{"bad slot", func(i *launch.Input) { i.Slot = "A1" }, "slot"},
		{"port zero", func(i *launch.Input) { i.Port = 0 }, "port"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := radInput()
			tc.mutate(&in)
			_, err := launch.Build("radiance", in)
			if !errors.Is(err, launch.ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want ErrInvalid containing %q", err, tc.want)
			}
		})
	}
}

func TestRadianceSpeculationAutoAndGPUCapAllowed(t *testing.T) {
	in := radInput()
	in.ExtraArgs = []string{"--tp", "1", "--max-num-seqs", "2", "--num-speculative-tokens", "auto", "--gpu-memory-mib", "40000"}
	if _, err := launch.Build("radiance", in); err != nil {
		t.Fatalf("a card-budget flag is part of the tuple, not refused: %v", err)
	}
}

func TestMemoryTupleCanonical(t *testing.T) {
	a := launch.NewMemoryTuple(1, 2, 3, 4096, []string{"--tp", "1", "--max-num-seqs", "4"})
	b := launch.NewMemoryTuple(1, 2, 3, 4096, []string{"--max-num-seqs", "4", "--tp", "1"})
	if a.Key() != b.Key() {
		t.Errorf("flag order must not matter: %s vs %s", a.Key(), b.Key())
	}
	c := launch.NewMemoryTuple(1, 2, 3, 4096, []string{"--tp", "1", "--max-num-seqs", "8"})
	if a.Key() == c.Key() {
		t.Error("different seqs must differ")
	}
	p, err := launch.ParseMemoryTuple(a.Key())
	if err != nil || p.Key() != a.Key() {
		t.Errorf("round trip: %v %v", p, err)
	}
	if _, err := launch.ParseMemoryTuple(`{"build_id":1,"typo":2}`); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := launch.ParseMemoryTuple(`{"build_id":0,"variant_id":1,"weight_artifact_id":1,"n_ctx":1,"args":[]}`); err == nil {
		t.Error("zero id accepted")
	}
}

// The Radiance 2.x kind stores the same flag list as 1.17 (the spec hash, launch-check and drift detection are defined over it); only
// `forge slot-exec` turns it into YAML. The row must therefore be byte-identical to the 1.17 one.
func TestRadiance2StoresTheSameFlagListAsRadiance(t *testing.T) {
	a, err := launch.Build("radiance", radInput())
	if err != nil {
		t.Fatal(err)
	}
	b, err := launch.Build("radiance2", radInput())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("radiance2 launch differs from radiance:\n%+v\n%+v", b, a)
	}
}

// Everything the 1.17 builder refuses, the 2.x builder refuses too, and so does an argument list that could not be written as a settings file.
func TestRadiance2Refusals(t *testing.T) {
	cases := map[string]func(*launch.Input){
		"owned flag in extra_args": func(in *launch.Input) { in.ExtraArgs = append(in.ExtraArgs, "--port", "9") },
		"missing required --tp": func(in *launch.Input) {
			in.ExtraArgs = []string{"--max-num-seqs", "4", "--num-speculative-tokens", "2"}
		},
		"flag=value spelling": func(in *launch.Input) { in.ExtraArgs = append(in.ExtraArgs, "--seed=1") },
		"not a flag name":     func(in *launch.Input) { in.ExtraArgs = append(in.ExtraArgs, "--Bad_Flag", "1") },
		"newline in a value":  func(in *launch.Input) { in.ExtraArgs = append(in.ExtraArgs, "--probe", "a\nb") },
		"duplicate flag":      func(in *launch.Input) { in.ExtraArgs = append(in.ExtraArgs, "--tp", "2") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := radInput()
			mutate(&in)
			if _, err := launch.Build("radiance2", in); !errors.Is(err, launch.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}
