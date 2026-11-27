// SPDX-License-Identifier: Apache-2.0

package launch_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/enginekind"
	"github.com/jsaigou/the-forge/internal/launch"
)

func okInput() launch.Input {
	return launch.Input{Slot: "a1", Port: 8080, Alias: "m", Context: 4096, ModelPath: "/opt/forge/models/m.gguf", Backend: "vulkan"}
}

func TestBuildRefusals(t *testing.T) {
	long := strings.Repeat("x", 10)
	cases := []struct {
		name   string
		kind   string
		mutate func(*launch.Input)
		is     error // errors.Is target, nil: just any error mentioning want
		want   string
	}{
		{"unknown kind", "bogus", nil, enginekind.ErrUnknownKind, "bogus"},
		{"relative binary override", "llamacpp", func(i *launch.Input) { i.Binary = "bin/llama-server" }, launch.ErrInvalid, "absolute"},
		{"bad slot name", "llamacpp", func(i *launch.Input) { i.Slot = "../a1" }, launch.ErrInvalid, "slot"},
		{"uppercase slot name", "llamacpp", func(i *launch.Input) { i.Slot = "A1" }, launch.ErrInvalid, "slot"},
		{"port zero", "llamacpp", func(i *launch.Input) { i.Port = 0 }, launch.ErrInvalid, "port"},
		{"port too large", "llamacpp", func(i *launch.Input) { i.Port = 65536 }, launch.ErrInvalid, "port"},
		{"negative context", "llamacpp", func(i *launch.Input) { i.Context = -1 }, launch.ErrInvalid, "context"},
		{"empty model", "llamacpp", func(i *launch.Input) { i.ModelPath = "" }, launch.ErrInvalid, "model"},
		{"empty alias", "llamacpp", func(i *launch.Input) { i.Alias = "" }, launch.ErrInvalid, "alias"},
		{"unknown llama.cpp backend", "llamacpp", func(i *launch.Input) { i.Backend = "cuda" }, launch.ErrInvalid, "backend"},
		{"quote in alias", "llamacpp", func(i *launch.Input) { i.Alias = `a"b` }, launch.ErrInvalid, "env file"},
		{"backslash in model", "llamacpp", func(i *launch.Input) { i.ModelPath = `/m\x.gguf` }, launch.ErrInvalid, "env file"},
		{"newline in mmproj", "llamacpp", func(i *launch.Input) { i.MmprojPath = "/a\n/b" }, launch.ErrInvalid, "env file"},
		{"trailing space in model", "llamacpp", func(i *launch.Input) { i.ModelPath = "/m.gguf " }, launch.ErrInvalid, "whitespace"},
		{"empty extra arg", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"--a", ""} }, launch.ErrInvalid, "blank"},
		{"whitespace-only extra arg", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"  "} }, launch.ErrInvalid, "blank"},
		{"newline in extra arg", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"a\nb"} }, launch.ErrInvalid, "newline"},
		{"NUL in extra arg", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"a\x00b"} }, launch.ErrInvalid, "NUL"},
		{"--api-key", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"--api-key", "sk-" + long} }, launch.ErrInvalid, "secret"},
		{"--api-key=value", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"--api-key=sk-" + long} }, launch.ErrInvalid, "secret"},
		{"--api_key_file", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"--api_key_file", "/x"} }, launch.ErrInvalid, "secret"},
		{"--hf-token", "llamacpp", func(i *launch.Input) { i.ExtraArgs = []string{"--hf-token", "hf_" + long} }, launch.ErrInvalid, "secret"},
		{"--password", "vllm", func(i *launch.Input) { i.Backend = "vllm"; i.ExtraArgs = []string{"--password", "p"} }, launch.ErrInvalid, "secret"},
		{"vllm bad slot", "vllm", func(i *launch.Input) { i.Backend = "vllm"; i.Slot = "" }, launch.ErrInvalid, "slot"},
		{"legacy backend vllm with empty alias", "", func(i *launch.Input) { i.Backend = "vllm"; i.Alias = "" }, launch.ErrInvalid, "alias"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := okInput()
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			l, err := launch.Build(tc.kind, in)
			if err == nil {
				t.Fatalf("expected a refusal, got %+v", l)
			}
			if !errors.Is(err, tc.is) {
				t.Errorf("error %v is not %v", err, tc.is)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "sk-") || strings.Contains(err.Error(), "hf_") {
				t.Errorf("error leaks the secret value: %q", err)
			}
		})
	}
}

// Flags that merely contain "token" or "key" as part of a legitimate name are accepted (see secretFlag).
func TestLegitimateFlagsWithTokenOrKeyAreNotRefused(t *testing.T) {
	in := okInput()
	in.Backend = "vllm"
	in.ExtraArgs = []string{"--max-num-batched-tokens", "8192", "--tokenizer", "x", "--cache-type-k", "q8_0", "--override-kv=k=int:1"}
	if _, err := launch.Build("vllm", in); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRefusesBadLaunches(t *testing.T) {
	cases := map[string]launch.Launch{
		"relative exe":      {Exe: "llama-server"},
		"empty exe":         {},
		"relative cwd":      {Exe: "/bin/x", Cwd: "work"},
		"NUL in argv":       {Exe: "/bin/x", Argv: []string{"a\x00"}},
		"lowercase env":     {Exe: "/bin/x", Env: map[string]string{"path": "x"}},
		"secret env name":   {Exe: "/bin/x", Env: map[string]string{"HF_TOKEN": "x"}},
		"api key env name":  {Exe: "/bin/x", Env: map[string]string{"OPENAI_API_KEY": "x"}},
		"password env name": {Exe: "/bin/x", Env: map[string]string{"DB_PASSWORD": "x"}},
		"NUL in env value":  {Exe: "/bin/x", Env: map[string]string{"A": "a\x00"}},
	}
	for name, l := range cases {
		if err := l.Check(); !errors.Is(err, launch.ErrInvalid) {
			t.Errorf("%s: Check = %v, want ErrInvalid", name, err)
		}
	}
	if err := (launch.Launch{Exe: "/bin/x", Argv: []string{"-ngl", "99"}, Env: map[string]string{"HIP_PATH": "/x"}}).Check(); err != nil {
		t.Errorf("good launch refused: %v", err)
	}
}

func TestParseSlotFilesReportsPreservedHostLines(t *testing.T) {
	in, other, err := launch.ParseSlotFiles("a1", 8080,
		"FORGE_MODEL_PATH=/m.gguf\nFORGE_MODEL_ALIAS=m\nFORGE_CONTEXT=4096\nFORGE_MMPROJ=\nFORGE_BACKEND=rocm\nHSA_OVERRIDE_GFX_VERSION=11.5.1\n",
		"--a\n\n  \n--b\n")
	if err != nil {
		t.Fatal(err)
	}
	if in.Context != 4096 || in.Backend != "rocm" || len(in.ExtraArgs) != 2 || other["HSA_OVERRIDE_GFX_VERSION"] != "11.5.1" {
		t.Errorf("parsed %+v other %v", in, other)
	}
	if _, _, err := launch.ParseSlotFiles("a1", 8080, "FORGE_CONTEXT=lots\n", ""); err == nil {
		t.Error("a non-numeric context must be refused")
	}
}
