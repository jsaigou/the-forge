// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

var testRoots = []string{"/opt/forge/models", "/opt/forge/qwentts.cpp/models"}

func TestValidate(t *testing.T) {
	good := store.Occupant{Name: "embed", Kind: "llama-server", Params: map[string]string{
		"build": "build-vulkan-plain", "model": "/opt/forge/models/Qwen3-Embedding-0.6B/q8.gguf", "embedding": "true", "ctx": "8192", "ngl": "0"}}
	if err := Validate(good, testRoots); err != nil {
		t.Fatalf("a valid occupant was refused: %v", err)
	}
	bad := map[string]store.Occupant{
		"unknown kind":      {Name: "x", Kind: "bash"},
		"unknown parameter": {Name: "x", Kind: "kokoro", Params: map[string]string{"extra_args": "--evil"}},
		"missing required":  {Name: "x", Kind: "qwen3-asr-server"},
		"path outside":      {Name: "x", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/etc/passwd"}},
		"path traversal":    {Name: "x", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models/../../etc/passwd"}},
		"relative path":     {Name: "x", Kind: "qwen3-asr-server", Params: map[string]string{"model": "models/a.gguf"}},
		"root prefix trick": {Name: "x", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models-evil/a.gguf"}},
		"newline":           {Name: "x", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models/a.gguf\nFORGE_POOL_KIND=x"}},
		"bad int":           {Name: "x", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models/a.gguf", "threads": "4; rm"}},
		"bad word":          {Name: "x", Kind: "llama-server", Params: map[string]string{"build": "build-vulkan-plain", "model": "/opt/forge/models/a.gguf", "alias": "a b"}},
		"bad build":         {Name: "x", Kind: "llama-server", Params: map[string]string{"build": "../../bin", "model": "/opt/forge/models/a.gguf"}},
		"bad bool":          {Name: "x", Kind: "llama-server", Params: map[string]string{"build": "build-vulkan-plain", "model": "/opt/forge/models/a.gguf", "embedding": "yes"}},
	}
	for name, o := range bad {
		if err := Validate(o, testRoots); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRenderEnv(t *testing.T) {
	o := store.Occupant{Name: "asr", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models/qwen3-asr/q8.gguf", "threads": "4"}}
	env, err := RenderEnv(o, "s2", 8101, testRoots)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"FORGE_POOL_KIND=qwen3-asr-server", "FORGE_POOL_SLOT=s2", "FORGE_POOL_OCCUPANT=asr", "FORGE_POOL_PORT=8101",
		"FORGE_POOL_P_MODEL=/opt/forge/models/qwen3-asr/q8.gguf", "FORGE_POOL_P_THREADS=4"} {
		if !strings.Contains(env, want+"\n") {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	// A kind whose application fixes its port gets that port, not the slot's.
	k, err := RenderEnv(store.Occupant{Name: "kokoro", Kind: "kokoro"}, "s3", 8102, testRoots)
	if err != nil || !strings.Contains(k, "FORGE_POOL_PORT=8880\n") {
		t.Errorf("kokoro env = %q, %v", k, err)
	}
	if _, err := RenderEnv(store.Occupant{Name: "x", Kind: "nope"}, "s1", 8100, testRoots); err == nil {
		t.Error("an invalid occupant was rendered")
	}
}

// An unset or "/" model root must not widen the containment check to the whole filesystem.
func TestPathParamRejectsEverythingWhenRootsAreEmptyOrSlash(t *testing.T) {
	for _, roots := range [][]string{{""}, {"/"}, {"", "/", "relative/dir"}, nil} {
		if err := checkParam(ParamPath, "/etc/shadow", roots); err == nil {
			t.Errorf("roots %q accepted /etc/shadow", roots)
		}
	}
	if err := checkParam(ParamPath, "/models/a.gguf", []string{"", "/models"}); err != nil {
		t.Errorf("a real root next to an empty one must still work: %v", err)
	}
}

func TestEnvFileInjectionShapesAreRefused(t *testing.T) {
	roots := []string{"/models"}
	for _, v := range []string{`/models/a"b.gguf`, "/models/a'b.gguf", `/models/a\b.gguf`, "/models/a.gguf "} {
		if err := checkParam(ParamPath, v, roots); err == nil {
			t.Errorf("path %q must be refused", v)
		}
	}
	params := map[string]string{"build": "build-vulkan-plain", "model": "/models/a.gguf", "ctx": "8192", "ngl": "0"}
	if _, err := RenderEnv(store.Occupant{Name: "fine-name", Kind: "llama-server", Params: params}, "a1", 8080, roots); err != nil {
		t.Fatalf("control: a valid occupant must render: %v", err)
	}
	for _, name := range []string{"evil\nFORGE_POOL_KIND=x", "has space", ""} {
		if _, err := RenderEnv(store.Occupant{Name: name, Kind: "llama-server", Params: params}, "a1", 8080, roots); err == nil {
			t.Errorf("occupant name %q must be refused", name)
		}
	}
}
