// SPDX-License-Identifier: Apache-2.0

// Package specialist runs the specialist slot pool (ADR-0022): a second pool of generic slots for small, specialised, often always-on models and
// services (embedding, TTS, STT, alignment, decisions), apart from the LLM slots.
//
// Occupants are REGISTERED LAUNCH TEMPLATES. This file is the registry: for each launch kind it knows which binary may run, the one directory that
// binary must live under, which parameters are allowed and what they may contain. It renders those into a slot's env file. It never produces, accepts
// or stores a command line; the static launcher (systemd/start-pool.sh) maps the kind to the binary again and refuses anything outside its root, so a
// bad database row cannot become arbitrary execution. The two tables are kept in step by launcher_test.go, which feeds every kind's rendered env
// through the launcher's dry run.
package specialist

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jsaigou/the-forge/internal/store"
)

// ParamType says what a parameter value may look like.
type ParamType int

const (
	ParamPath  ParamType = iota // absolute, clean, under a model root
	ParamInt                    // decimal, 0 <= n <= 1<<31
	ParamBool                   // "true" or "false"
	ParamWord                   // [A-Za-z0-9._:-]{1,64}
	ParamBuild                  // a llama.cpp build directory name: build-[a-z0-9-]+
)

// ParamSpec describes one allowed parameter of a kind.
type ParamSpec struct {
	Type     ParamType
	Required bool
}

// Kind is one registered launch template.
type Kind struct {
	Name string
	// Binary is the absolute path the launcher runs; it must be under Root. For llama-server the build directory is a parameter and Binary is
	// the pattern <Root>/<build>/bin/llama-server, so Binary is left empty and the launcher derives it.
	Binary string
	Root   string
	// FixedPort is set when the application chooses its own port (Kokoro's server.py hard-codes 8880). 0 means the slot's port is passed.
	FixedPort int
	// HealthPath is probed with GET until it answers 2xx.
	HealthPath    string
	HealthTimeout time.Duration
	Params        map[string]ParamSpec
	// VerifyContext: after the health check, compare /props' n_ctx with the requested context and fail if the kernel silently reduced it
	// (repo rule: always verify n_ctx after a load). llama-server kinds only.
	VerifyContext bool
}

// EngineRoot is the directory the speech engines are installed under. The launcher (systemd/start-pool.sh) accepts nothing outside it, and the engine
// recipes (internal/speechsetup) are written relative to it. The public export rewrites it.
const EngineRoot = "/opt/forge"

// ModelRoots are the directories a pool engine's model files may live under: the configured language model directory, and the two places speech
// weights are installed under the engine root.
func ModelRoots(modelsDir, engineRoot string) []string {
	out := []string{path.Join(engineRoot, "models"), path.Join(engineRoot, "qwentts.cpp", "models")}
	if modelsDir != "" && modelsDir != out[0] {
		out = append([]string{modelsDir}, out...)
	}
	return out
}

// KindZonos3Server is the launch kind of the ZONOS3-Mini clone engine.
const KindZonos3Server = "zonos3-server"

var kinds = map[string]Kind{
	"llama-server": {
		Name: "llama-server", Root: "/opt/forge/llama.cpp-upstream-plain", HealthPath: "/health", HealthTimeout: 90 * time.Second,
		Params: map[string]ParamSpec{
			"build":     {ParamBuild, true},
			"model":     {ParamPath, true},
			"mmproj":    {ParamPath, false},
			"alias":     {ParamWord, false},
			"ctx":       {ParamInt, false},
			"batch":     {ParamInt, false},
			"ubatch":    {ParamInt, false},
			"threads":   {ParamInt, false},
			"ngl":       {ParamInt, false},
			"parallel":  {ParamInt, false},
			"embedding": {ParamBool, false},
		},
	},
	KindCatalogConfig: {
		// Hosts a catalog config: the launch (binary, model, context, args) is resolved from the catalog at every start, see catalog.go.
		Name: KindCatalogConfig, Root: llamaRoot, HealthPath: "/health", HealthTimeout: 180 * time.Second, VerifyContext: true,
		Params: map[string]ParamSpec{"config": {ParamWord, true}},
	},
	"qwen-tts-server": {
		Name: "qwen-tts-server", Binary: "/opt/forge/qwentts.cpp/build/tts-server", Root: "/opt/forge/qwentts.cpp/build",
		HealthPath: "/health", HealthTimeout: 90 * time.Second,
		Params: map[string]ParamSpec{"model": {ParamPath, true}, "codec": {ParamPath, true}, "alias": {ParamWord, false}},
	},
	// zonos3-server is Zyphra's ZONOS3-Mini runtime (zonos3.cpp, ggml/Vulkan): a clone-only speech engine. The backbone
	// is the first positional argument of the binary, the companions are flags; none of them is a command line. GET /v2/capabilities answers once the
	// backbone and decoder are loaded and warmed.
	KindZonos3Server: {
		Name: KindZonos3Server, Binary: "/opt/forge/zonos3.cpp/build/zonos3-server", Root: "/opt/forge/zonos3.cpp/build",
		HealthPath: "/v2/capabilities", HealthTimeout: 120 * time.Second,
		Params: map[string]ParamSpec{"model": {ParamPath, true}, "zac": {ParamPath, true}, "spk_encoder": {ParamPath, true},
			"zac_encoder": {ParamPath, false}, "ssl_teacher": {ParamPath, false},
			// Alignment heads for the languages that were measured (docs/zonos-style-tags-plan.md): with them the engine places control spans by where the model
			// is reading, without them by an estimate that starts words late and leaks. en_us and en_gb share the English file.
			"laci_en": {ParamPath, false}, "laci_ja": {ParamPath, false}},
	},
	"qwen3-asr-server": {
		Name: "qwen3-asr-server", Binary: "/opt/forge/qwen3-asr-src/build/qwen3-asr-server", Root: "/opt/forge/qwen3-asr-src/build",
		HealthPath: "/health", HealthTimeout: 60 * time.Second,
		Params: map[string]ParamSpec{"model": {ParamPath, true}, "threads": {ParamInt, false}, "max_upload_mb": {ParamInt, false}},
	},
	"aligner": {
		Name: "aligner", Binary: "/opt/forge/venv-qwen3-aligner/bin/gunicorn", Root: "/opt/forge/venv-qwen3-aligner/bin",
		HealthPath: "/health", HealthTimeout: 180 * time.Second,
		Params: map[string]ParamSpec{"workers": {ParamInt, false}, "timeout": {ParamInt, false}},
	},
	"kokoro": {
		Name: "kokoro", Binary: "/opt/forge/kokoro/venv312/bin/python", Root: "/opt/forge/kokoro",
		FixedPort: 8880, HealthPath: "/health", HealthTimeout: 120 * time.Second,
		Params: map[string]ParamSpec{},
	},
}

// KindByName returns a registered kind.
func KindByName(name string) (Kind, bool) { k, ok := kinds[name]; return k, ok }

// KindNames lists the registered kinds, sorted.
func KindNames() []string {
	out := make([]string, 0, len(kinds))
	for n := range kinds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

var (
	wordRE  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	buildRE = regexp.MustCompile(`^build-[a-z0-9-]{1,48}$`)
)

// Validate checks an occupant against its kind: the kind is registered, every parameter is allowed and well formed, required ones are present,
// and every path lies under one of modelRoots. It is the gate before an occupant is stored or rendered.
func Validate(o store.Occupant, modelRoots []string) error {
	k, ok := kinds[o.Kind]
	if !ok {
		return fmt.Errorf("occupant %q: unknown launch kind %q (registered: %s)", o.Name, o.Kind, strings.Join(KindNames(), ", "))
	}
	if !wordRE.MatchString(o.Name) {
		return fmt.Errorf("occupant name %q is not 1-64 characters of letters, digits and . _ : - (it is written into an EnvironmentFile)", o.Name)
	}
	for name := range o.Params {
		if _, ok := k.Params[name]; !ok {
			return fmt.Errorf("occupant %q: kind %s has no parameter %q", o.Name, k.Name, name)
		}
	}
	for name, spec := range k.Params {
		v, present := o.Params[name]
		if !present || v == "" {
			if spec.Required {
				return fmt.Errorf("occupant %q: kind %s requires parameter %q", o.Name, k.Name, name)
			}
			continue
		}
		if err := checkParam(spec.Type, v, modelRoots); err != nil {
			return fmt.Errorf("occupant %q: parameter %q: %w", o.Name, name, err)
		}
	}
	return nil
}

func checkParam(t ParamType, v string, modelRoots []string) error {
	if strings.ContainsAny(v, "\x00\n\r") {
		return fmt.Errorf("contains a control character")
	}
	switch t {
	case ParamInt:
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 1<<31 {
			return fmt.Errorf("%q is not a whole number in 0..2^31", v)
		}
	case ParamBool:
		if v != "true" && v != "false" {
			return fmt.Errorf("%q is not true or false", v)
		}
	case ParamWord:
		if !wordRE.MatchString(v) {
			return fmt.Errorf("%q is not 1-64 characters of letters, digits and . _ : -", v)
		}
	case ParamBuild:
		if !buildRE.MatchString(v) {
			return fmt.Errorf("%q is not a build directory name (build-<letters, digits, dashes>)", v)
		}
	case ParamPath:
		if !path.IsAbs(v) || path.Clean(v) != v {
			return fmt.Errorf("%q must be an absolute, clean path", v)
		}
		if strings.ContainsAny(v, "\"'\\") || v != strings.TrimSpace(v) {
			return fmt.Errorf("%q must not contain quotes, backslashes or surrounding whitespace (it is written into an EnvironmentFile)", v)
		}
		for _, r := range modelRoots {
			r = strings.TrimRight(r, "/")
			// An unset ModelsDir arrives as "" and "/" trims to "": either would make HasPrefix(v, "/") accept every absolute path.
			if r == "" || !path.IsAbs(r) {
				continue
			}
			if strings.HasPrefix(v, r+"/") {
				return nil
			}
		}
		return fmt.Errorf("%q is not under a model root (%s)", v, strings.Join(modelRoots, ", "))
	}
	return nil
}

// RenderEnv is the content of a slot's env file for an occupant (KEY=VALUE lines, sorted, no secrets). The launcher is the only reader. Values
// were validated and contain no newline, quote handling is unnecessary for systemd's EnvironmentFile because the character set is restricted,
// but paths may contain spaces, so values are written as-is up to end of line, which is how EnvironmentFile reads them.
func RenderEnv(o store.Occupant, slot string, slotPort int, modelRoots []string) (string, error) {
	if err := Validate(o, modelRoots); err != nil {
		return "", err
	}
	k := kinds[o.Kind]
	port := slotPort
	if k.FixedPort != 0 {
		port = k.FixedPort
	}
	lines := []string{
		"FORGE_POOL_KIND=" + k.Name,
		"FORGE_POOL_SLOT=" + slot,
		"FORGE_POOL_OCCUPANT=" + o.Name,
		"FORGE_POOL_PORT=" + strconv.Itoa(port),
	}
	for name, v := range o.Params {
		if v != "" {
			lines = append(lines, "FORGE_POOL_P_"+strings.ToUpper(name)+"="+v)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}
