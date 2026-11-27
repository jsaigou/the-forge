// SPDX-License-Identifier: Apache-2.0

package launch_test

// Golden tests: the builder against the REAL legacy static launchers (testdata/legacy/start-aN.sh, frozen oracles, run under bash). The scripts are run
// unmodified except for two things: the one ARGS_FILE assignment points at a temp file, and `exec` is shadowed by a function that records its
// arguments and the exported environment instead of starting anything. The env file is delivered the way systemd delivers it (as process
// environment, nothing else inherited). The recorded exe/argv/added-environment must equal the builder's byte for byte.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/config"
	"github.com/jsaigou/the-forge/internal/launch"
)

// legacySHA256 pins the frozen oracles (testdata/legacy/README.md). They are the launchers production ran before the generic shim.
var legacySHA256 = map[string]string{
	"a1": "9f90505a9880620cbf69289cc0c6f4af02af6f6c9c4dcdcfedf095a83a69304a",
	"a2": "d96361bfe42e2fb0383a9b436d8aa2bc567de7d655e6e2d36f7ab2cf257ce661",
	"a3": "74d7fed619b8aff5c6a4ea2c71d8160dfd8d41eb78c6d903a79bd86fc6eabbce",
	"a4": "84182d2fd5bea7ab776ba9e53b1b218ec237396901b3e894f5a0dd4d6e38a5e3",
}

// legacyLauncher returns the frozen legacy script for a slot.
func legacyLauncher(t *testing.T, slot string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "legacy", "start-"+slot+".sh"))
	if err != nil {
		t.Fatalf("legacy oracle for %s: %v", slot, err)
	}
	return b
}

func TestLegacyOraclesAreFrozen(t *testing.T) {
	for slot, want := range legacySHA256 {
		sum := sha256.Sum256(legacyLauncher(t, slot))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("testdata/legacy/start-%s.sh sha256 = %s, want %s: the oracles are frozen (see testdata/legacy/README.md)", slot, got, want)
		}
	}
}

// The scripts fall back to a built-in llama-server path when FORGE_LLAMA_BIN is unset. Those are the builds the builder's defaults must equal
// (moved here from smith/launchers when the embedded copies became the shim).
func TestLegacyFallbackPathsAreTheLivePlainBuilds(t *testing.T) {
	re := regexp.MustCompile(`LLAMA_BIN="([^"$]+)"`)
	want := map[string]bool{
		"/opt/forge/llama.cpp-upstream-plain/build-rocm-plain/bin/llama-server":   true,
		"/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server": true,
	}
	for slot := range legacySHA256 {
		paths := re.FindAllStringSubmatch(string(legacyLauncher(t, slot)), -1)
		if len(paths) != 2 {
			t.Errorf("%s: found %d fallback paths, want 2", slot, len(paths))
		}
		for _, m := range paths {
			if !want[m[1]] {
				t.Errorf("%s: fallback %q is not a live plain build", slot, m[1])
			}
		}
	}
}

var slotPorts = map[string]int{"a1": 8080, "a2": 8081, "a3": 8087, "a4": 8088}

func goldenConfig() *config.Config {
	cfg := &config.Config{Paths: config.Paths{ModelsDir: "/opt/forge/models"}, Slots: map[string]config.Slot{}}
	for s, p := range slotPorts {
		cfg.Slots[s] = config.Slot{Unit: "forge-" + s, Port: p, Label: strings.ToUpper(s)}
	}
	return cfg
}

// daemonFiles mirrors the file format the retired engine writer (writeServiceFiles, removed in step d2) produced, which the frozen legacy scripts read.
func daemonFiles(cfg *config.Config, svc config.Service) (envFile, argsFile string) {
	mmproj := ""
	if svc.MMProj != "" {
		mmproj = cfg.Paths.ResolveModelPath(svc.MMProj)
	}
	lines := []string{
		"FORGE_MODEL_PATH=" + cfg.Paths.ResolveModelPath(svc.Model),
		"FORGE_MODEL_ALIAS=" + svc.Alias,
		"FORGE_CONTEXT=" + strconv.Itoa(svc.Context),
		"FORGE_MMPROJ=" + mmproj,
		"FORGE_BACKEND=" + svc.Backend,
	}
	if svc.LlamaBin != "" {
		lines = append(lines, "FORGE_LLAMA_BIN="+svc.LlamaBin)
	}
	return strings.Join(lines, "\n") + "\n", strings.Join(svc.ExtraArgs, "\n") + "\n"
}

type scriptResult struct {
	exe   string
	argv  []string
	added map[string]string
}

func bashAtLeast4(t *testing.T) {
	t.Helper()
	out, err := exec.Command("bash", "-c", "echo ${BASH_VERSINFO[0]}").Output()
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	if v, _ := strconv.Atoi(strings.TrimSpace(string(out))); v < 4 {
		t.Skipf("bash %s: the launchers use mapfile and need bash >= 4", strings.TrimSpace(string(out)))
	}
}

// runLauncher runs start-<slot>.sh. startEnv is the whole environment the script starts with.
func runLauncher(t *testing.T, slot, argsFile string, startEnv []string) scriptResult {
	t.Helper()
	src := legacyLauncher(t, slot)
	orig := "ARGS_FILE=/etc/sysconfig/forge-" + slot + "-args\n"
	if strings.Count(string(src), orig) != 1 {
		t.Fatalf("start-%s.sh: expected exactly one %q line", slot, strings.TrimSpace(orig))
	}
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	if err := os.WriteFile(argsPath, []byte(argsFile), 0o600); err != nil {
		t.Fatal(err)
	}
	outArgv, outEnv := filepath.Join(dir, "argv.out"), filepath.Join(dir, "env.out")
	// exec shadow: record "$@" (exe first) and every exported variable, NUL separated, then stop.
	preamble := "exec() {\n" +
		"  { printf '%s\\0' \"$@\"; } > '" + outArgv + "'\n" +
		"  { for n in $(compgen -e); do printf '%s=%s\\0' \"$n\" \"${!n}\"; done; } > '" + outEnv + "'\n" +
		"  builtin exit 0\n}\n"
	body := strings.Replace(string(src), orig, "ARGS_FILE='"+argsPath+"'\n", 1)
	script := filepath.Join(dir, "launcher.sh")
	if err := os.WriteFile(script, []byte(preamble+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "--noprofile", "--norc", script)
	cmd.Env = startEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher failed: %v\n%s", err, out)
	}
	argvRaw, err := os.ReadFile(outArgv)
	if err != nil {
		t.Fatalf("launcher never reached exec: %v", err)
	}
	envRaw, err := os.ReadFile(outEnv)
	if err != nil {
		t.Fatal(err)
	}
	args := splitNUL(argvRaw)
	start := map[string]string{}
	for _, kv := range startEnv {
		k, v, _ := strings.Cut(kv, "=")
		start[k] = v
	}
	added := map[string]string{}
	for _, kv := range splitNUL(envRaw) {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "PWD", "OLDPWD", "SHLVL", "_":
			continue
		}
		if sv, had := start[k]; !had || sv != v {
			added[k] = v
		}
	}
	return scriptResult{exe: args[0], argv: args[1:], added: added}
}

func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(bytes.TrimSuffix(b, []byte{0}), []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out
}

// systemdEnv is the environment systemd gives the script: the env file's pairs, plus a PATH for grep (the unit has none of the rest).
func systemdEnv(envFile string, extra ...string) []string {
	env := []string{"PATH=/usr/bin:/bin"}
	for _, line := range strings.Split(envFile, "\n") {
		if strings.TrimSpace(line) != "" {
			env = append(env, line)
		}
	}
	return append(env, extra...)
}

type goldenCase struct {
	name   string
	kind   string // the engine kind handed to the builder ("" = default)
	svc    config.Service
	extra  []string // extra process environment (inherited LD_LIBRARY_PATH)
	inheit string   // Input.InheritedLDLibraryPath that corresponds to extra
}

func goldenCases() []goldenCase {
	return []goldenCase{
		{name: "llamacpp vulkan plain", kind: "llamacpp", svc: config.Service{Model: "gemma4.gguf", Alias: "gemma4", Context: 131072, Backend: "vulkan",
			ExtraArgs: []string{"--load-mode", "mmap", "--flash-attn", "on", "--threads", "8", "--parallel", "1", "--ctx-checkpoints", "16", "--jinja"}}},
		{name: "llamacpp rocm plain", kind: "llamacpp", svc: config.Service{Model: "qwen36-35b-a3b.gguf", Alias: "qwen36-35b-a3b", Context: 262144, Backend: "rocm",
			ExtraArgs: []string{"--load-mode", "none", "--cache-type-k", "q8_0", "--cache-type-v", "q8_0", "--batch-size", "2048", "--ubatch-size", "512", "--swa-full"}}},
		{name: "llamacpp vulkan with mmproj", kind: "llamacpp", svc: config.Service{Model: "gemma4-e4b.gguf", Alias: "gemma4-e4b-qat", Context: 32768, Backend: "vulkan",
			MMProj: "mmproj/gemma4-e4b-f16.gguf", ExtraArgs: []string{"--parallel", "1"}}},
		{name: "llamacpp rocm with absolute mmproj", kind: "llamacpp", svc: config.Service{Model: "/srv/m/v.gguf", Alias: "v", Context: 8192, Backend: "rocm",
			MMProj: "/srv/m/v-mmproj.gguf"}},
		{name: "llamacpp FORGE_LLAMA_BIN override beats rocm default", kind: "llamacpp", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 4096, Backend: "rocm",
			LlamaBin: "/opt/forge/llama.cpp-kintsugi/build-rocm/bin/llama-server", ExtraArgs: []string{"--parallel", "1"}}},
		{name: "llamacpp FORGE_LLAMA_BIN override beats vulkan default", kind: "llamacpp", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 4096, Backend: "vulkan",
			LlamaBin: "/opt/forge/llama.cpp-kintsugi/build-vulkan/bin/llama-server"}},
		{name: "llamacpp speculative and template flags", kind: "llamacpp", svc: config.Service{Model: "qwen36-27b.gguf", Alias: "qwen36-27b", Context: 262144, Backend: "rocm",
			ExtraArgs: []string{"--spec-type", "draft-mtp", "--spec-draft-n-max", "3", "--spec-draft-model", "/opt/forge/models/draft.gguf",
				"--chat-template-kwargs", `{"enable_thinking":false}`, "--reasoning-budget", "0", "--mmproj", "/opt/forge/models/extra-mm.gguf"}}},
		{name: "llamacpp empty extra_args (args file is a single newline)", kind: "llamacpp", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 2048, Backend: "vulkan"}},
		{name: "llamacpp extra args with space, equals and leading space", kind: "llamacpp", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 2048, Backend: "vulkan",
			ExtraArgs: []string{"--override-kv=tokenizer.ggml.add_bos_token=bool:false", "--system-prompt", "be brief and kind", " leading-space-kept"}}},
		{name: "llamacpp model path with a space", kind: "llamacpp", svc: config.Service{Model: "my models/with space.gguf", Alias: "spaced model", Context: 2048, Backend: "vulkan"}},
		{name: "llamacpp empty backend defaults to vulkan", kind: "llamacpp", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 2048, Backend: ""}},
		{name: "llamacpp empty kind and backend (rows that predate the column)", kind: "", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 2048}},
		{name: "llamacpp context 0 passes through", kind: "llamacpp", svc: config.Service{Model: "m.gguf", Alias: "m", Context: 0, Backend: "vulkan"}},
		// KNOWN-DEAD PATH PENDING REINVESTIGATION (reproduced verbatim): /opt/rocm-therock-7.13 no longer exists on ForgeHost.
		{name: "vllm KNOWN-DEAD PATH reproduced verbatim (legacy backend vllm)", kind: "vllm", svc: config.Service{Model: "hy-mt2-30b-a3b", Alias: "hy-mt2", Context: 16384,
			Backend: "vllm", ExtraArgs: []string{"--tensor-parallel-size", "1", "--max-num-batched-tokens", "8192"}}},
		{name: "vllm KNOWN-DEAD PATH ignores mmproj and FORGE_LLAMA_BIN like the script", kind: "", svc: config.Service{Model: "hy-mt2-30b-a3b", Alias: "hy-mt2", Context: 16384,
			Backend: "vllm", MMProj: "ignored.gguf", LlamaBin: "/opt/ignored/llama-server"}},
		{name: "vllm KNOWN-DEAD PATH appends the inherited LD_LIBRARY_PATH", kind: "vllm", svc: config.Service{Model: "hy-mt2-30b-a3b", Alias: "hy-mt2", Context: 16384, Backend: "vllm"},
			extra: []string{"LD_LIBRARY_PATH=/usr/local/lib:/opt/other"}, inheit: "/usr/local/lib:/opt/other"},
	}
}

func TestGoldenBuilderEqualsRealLauncher(t *testing.T) {
	bashAtLeast4(t)
	cfg := goldenConfig()
	for _, slot := range []string{"a1", "a2", "a3", "a4"} {
		for _, tc := range goldenCases() {
			t.Run(slot+"/"+tc.name, func(t *testing.T) {
				svc := tc.svc
				svc.PortRole = slot
				envFile, argsFile := daemonFiles(cfg, svc)
				want := runLauncher(t, slot, argsFile, systemdEnv(envFile, tc.extra...))

				in, err := launch.FromService(cfg, slot, svc)
				if err != nil {
					t.Fatal(err)
				}
				in.InheritedLDLibraryPath = tc.inheit
				got, err := launch.Build(tc.kind, in)
				if err != nil {
					t.Fatalf("Build: %v", err)
				}
				if got.Exe != want.exe {
					t.Errorf("exe = %q, script exec'd %q", got.Exe, want.exe)
				}
				if !reflect.DeepEqual(got.Argv, want.argv) {
					t.Errorf("argv differs\n builder: %q\n script:  %q", got.Argv, want.argv)
				}
				if len(got.Env) != len(want.added) || (len(got.Env) > 0 && !reflect.DeepEqual(got.Env, want.added)) {
					t.Errorf("added env differs\n builder: %q\n script:  %q", got.Env, want.added)
				}
				if got.Cwd != "" {
					t.Errorf("cwd = %q: the scripts do not change directory", got.Cwd)
				}
				// The launch must fit a slot_launches row.
				row := got.Row(slot, in.Port)
				row.ConfigName, row.EngineKind = "golden", "llamacpp"
				if bad := row.Validate(); bad != nil {
					t.Errorf("does not fit a slot_launches row: %v", bad)
				}
				// The files the daemon writes carry the same inputs as the daemon's own values.
				fin, other, err := launch.ParseSlotFiles(slot, cfg.Slots[slot].Port, envFile, argsFile)
				if err != nil {
					t.Fatal(err)
				}
				fin.InheritedLDLibraryPath = tc.inheit
				if !reflect.DeepEqual(fin, in) && !(len(fin.ExtraArgs) == 0 && len(in.ExtraArgs) == 0 && eqIgnoringArgs(fin, in)) {
					t.Errorf("ParseSlotFiles(files) != FromService(svc)\n files: %+v\n svc:   %+v", fin, in)
				}
				if len(other) != 0 {
					t.Errorf("unexpected non-FORGE keys %v in a fresh env file", other)
				}
			})
		}
	}
}

func eqIgnoringArgs(a, b launch.Input) bool {
	a.ExtraArgs, b.ExtraArgs = nil, nil
	return reflect.DeepEqual(a, b)
}

// The script hard-codes one port per script; the builder takes the slot's configured port. Prove each script's port is the configured default of
// its slot, and that a differently configured port flows through (the new behaviour: nothing is baked per slot).
func TestPortComesFromConfigNotFromTheSlotName(t *testing.T) {
	bashAtLeast4(t)
	cfg := goldenConfig()
	svc := config.Service{Model: "m.gguf", Alias: "m", Context: 2048, Backend: "vulkan"}
	for slot, port := range slotPorts {
		envFile, argsFile := daemonFiles(cfg, svc)
		res := runLauncher(t, slot, argsFile, systemdEnv(envFile))
		if i := indexOf(res.argv, "--port"); i < 0 || res.argv[i+1] != strconv.Itoa(port) {
			t.Fatalf("%s script port = %v, want %d", slot, res.argv, port)
		}
		l, err := launch.BuildService(cfg, slot, svc)
		if err != nil {
			t.Fatal(err)
		}
		if i := indexOf(l.Argv, "--port"); l.Argv[i+1] != strconv.Itoa(port) {
			t.Errorf("%s builder port = %q, want %d", slot, l.Argv[i+1], port)
		}
	}
	cfg.Slots["a1"] = config.Slot{Unit: "forge-a1", Port: 9191}
	l, err := launch.BuildService(cfg, "a1", svc)
	if err != nil {
		t.Fatal(err)
	}
	if i := indexOf(l.Argv, "--port"); l.Argv[i+1] != "9191" {
		t.Errorf("configured port 9191 not used: %q", l.Argv)
	}
	if _, err := launch.BuildService(cfg, "a9", svc); err == nil {
		t.Error("an unconfigured slot must be refused")
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// The four scripts must differ only in their slot name and port; if anyone edits one script alone this fails.
func TestFourLaunchersDifferOnlyInSlotAndPort(t *testing.T) {
	norm := func(slot string) string {
		b := legacyLauncher(t, slot)
		var keep []string
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "#") {
				continue // header comments name the slot and wording differs
			}
			l = strings.ReplaceAll(l, "forge-"+slot+"-args", "forge-SLOT-args")
			l = strings.ReplaceAll(l, strconv.Itoa(slotPorts[slot]), "PORT")
			keep = append(keep, l)
		}
		return strings.Join(keep, "\n")
	}
	ref := norm("a1")
	for _, s := range []string{"a2", "a3", "a4"} {
		if norm(s) != ref {
			t.Errorf("start-%s.sh differs from start-a1.sh beyond slot name and port", s)
		}
	}
}

// Documents WHY the builder refuses what it refuses: the script silently drops blank args-file lines, so such an extra arg would vanish in production.
func TestScriptDropsBlankArgsLinesWhichIsWhyTheBuilderRefusesThem(t *testing.T) {
	bashAtLeast4(t)
	cfg := goldenConfig()
	svc := config.Service{Model: "m.gguf", Alias: "m", Context: 1, Backend: "vulkan", ExtraArgs: []string{"--a", "", "   ", "--b"}}
	envFile, argsFile := daemonFiles(cfg, svc)
	res := runLauncher(t, "a1", argsFile, systemdEnv(envFile))
	tail := res.argv[len(res.argv)-2:]
	if !reflect.DeepEqual(tail, []string{"--a", "--b"}) || indexOf(res.argv, "") >= 0 {
		t.Fatalf("script kept blank lines: %q", res.argv)
	}
	if _, err := launch.BuildService(cfg, "a1", svc); err == nil {
		t.Error("builder must refuse extra args the args file cannot carry")
	}
}

// Documented deliberate difference: kind vllm with backend rocm (the ADR-0025 spelling) was never launchable by the script, which branches on
// FORGE_BACKEND only and so exec's llama-server with the vLLM model. The builder follows the kind.
func TestVLLMKindWithRocmBackendDiffersFromScriptOnPurpose(t *testing.T) {
	bashAtLeast4(t)
	cfg := goldenConfig()
	svc := config.Service{Model: "hy-mt2", Alias: "hy", Context: 1024, Backend: "rocm", EngineKind: "vllm"}
	envFile, argsFile := daemonFiles(cfg, svc)
	res := runLauncher(t, "a1", argsFile, systemdEnv(envFile))
	if res.exe != launch.DefaultRocmLlamaServer {
		t.Fatalf("script exec'd %q; expected it to ignore the kind and run llama-server", res.exe)
	}
	l, err := launch.BuildService(cfg, "a1", svc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(l.Exe, "/vllm") || l.Argv[0] != "serve" {
		t.Errorf("builder = %q %q, want the vllm branch", l.Exe, l.Argv)
	}
}
