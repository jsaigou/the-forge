// SPDX-License-Identifier: Apache-2.0

package specialist

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jsaigou/the-forge/internal/store"
)

// launcherDryRun renders the occupant's env exactly as the daemon would and feeds it to the launcher's dry run.
func launcherDryRun(t *testing.T, o store.Occupant, extra ...string) (out string, code int) {
	t.Helper()
	envText, err := RenderEnv(o, "s1", 8100, []string{"/opt/forge/models", "/opt/forge/qwentts.cpp/models"})
	if err != nil {
		t.Fatalf("RenderEnv: %v", err)
	}
	cmd := exec.Command("bash", "../../../systemd/start-pool.sh", "s1")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin", "FORGE_POOL_DRYRUN=1"}
	for _, l := range strings.Split(strings.TrimSpace(envText), "\n") {
		cmd.Env = append(cmd.Env, l)
	}
	cmd.Env = append(cmd.Env, extra...)
	b, err := cmd.CombinedOutput()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Skipf("bash is not runnable here: %v", err)
	}
	return string(b), code
}

// Every kind the Go registry can render must be one the launcher can start, and the command it builds must be the one the systemd units run today.
func TestLauncher_EveryRegisteredKindStarts(t *testing.T) {
	cases := map[string]struct {
		o    store.Occupant
		want []string
	}{
		"llama-server": {store.Occupant{Name: "embed", Kind: "llama-server", Params: map[string]string{"build": "build-vulkan-plain",
			"model": "/opt/forge/models/Qwen3-Embedding-0.6B/Qwen3-Embedding-0.6B-Q8_0.gguf", "embedding": "true", "ctx": "8192", "batch": "8192", "ubatch": "8192", "threads": "4", "ngl": "0"}},
			[]string{"/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", "-m /opt/forge/models/Qwen3-Embedding-0.6B/Qwen3-Embedding-0.6B-Q8_0.gguf",
				"-c 8192", "-b 8192", "-ub 8192", "-t 4", "-ngl 0", "--embedding", "--host 127.0.0.1", "--port 8100"}},
		"qwen-tts-server": {store.Occupant{Name: "custom", Kind: "qwen-tts-server", Params: map[string]string{"model": "/opt/forge/qwentts.cpp/models/qwen-talker-1.7b-customvoice-Q8_0.gguf",
			"codec": "/opt/forge/qwentts.cpp/models/qwen-tokenizer-12hz-Q8_0.gguf", "alias": "customvoice"}},
			[]string{"/opt/forge/qwentts.cpp/build/tts-server", "--model /opt/forge/qwentts.cpp/models/qwen-talker-1.7b-customvoice-Q8_0.gguf", "--codec ", "--alias customvoice", "--port 8100"}},
		"zonos3-server": {store.Occupant{Name: "clone", Kind: "zonos3-server", Params: map[string]string{"model": "/opt/forge/models/zonos3-mini/backbone-ptq.gguf",
			"zac": "/opt/forge/models/zonos3-mini/zac-keep2.gguf", "spk_encoder": "/opt/forge/models/zonos3-mini/spk-encoder.gguf",
			"zac_encoder": "/opt/forge/models/zonos3-mini/zac-encoder.gguf", "ssl_teacher": "/opt/forge/models/zonos3-mini/ssl-teacher.gguf",
			"laci_en": "/opt/forge/models/zonos3-mini/laci/zonos3_mini_heads.json", "laci_ja": "/opt/forge/models/zonos3-mini/laci/zonos3_mini_heads_ja.json"}},
			[]string{"/opt/forge/zonos3.cpp/build/zonos3-server /opt/forge/models/zonos3-mini/backbone-ptq.gguf", "--zac /opt/forge/models/zonos3-mini/zac-keep2.gguf",
				"--spk-encoder /opt/forge/models/zonos3-mini/spk-encoder.gguf", "--zac-encoder ", "--ssl-teacher ",
				"--laci-heads en_us=/opt/forge/models/zonos3-mini/laci/zonos3_mini_heads.json", "--laci-heads en_gb=/opt/forge/models/zonos3-mini/laci/zonos3_mini_heads.json",
				"--laci-heads ja=/opt/forge/models/zonos3-mini/laci/zonos3_mini_heads_ja.json", "--host 127.0.0.1", "--port 8100", "--gpu"}},
		"qwen3-asr-server": {store.Occupant{Name: "asr", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models/qwen3-asr/qwen3-asr-0.6b-q8_0.gguf", "threads": "4", "max_upload_mb": "512"}},
			[]string{"/opt/forge/qwen3-asr-src/build/qwen3-asr-server", "-m /opt/forge/models/qwen3-asr/qwen3-asr-0.6b-q8_0.gguf", "--max-upload-mb 512", "-t 4", "--convert", "--port 8100"}},
		"aligner": {store.Occupant{Name: "align", Kind: "aligner", Params: map[string]string{"workers": "1", "timeout": "1800"}},
			[]string{"/opt/forge/venv-qwen3-aligner/bin/gunicorn", "-w 1", "--timeout 1800", "-k sync", "-b 127.0.0.1:8100", "aligner_server:app"}},
	}
	for kind, c := range cases {
		out, code := launcherDryRun(t, c.o)
		if code != 0 {
			t.Errorf("%s: launcher exit %d: %s", kind, code, out)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: command lacks %q:\n%s", kind, w, out)
			}
		}
	}
	// Kokoro's port is fixed by its server.py: the daemon renders 8880 and the launcher accepts exactly that.
	out, code := launcherDryRun(t, store.Occupant{Name: "kokoro", Kind: "kokoro"})
	if code != 0 || !strings.Contains(out, "/opt/forge/kokoro/venv312/bin/python /opt/forge/kokoro/server.py") || !strings.Contains(out, "cwd=/opt/forge/kokoro") {
		t.Errorf("kokoro: %d %s", code, out)
	}
	// Every kind in the registry has a case here, so a kind added to kinds.go without a launcher branch fails this test.
	for _, k := range KindNames() {
		if _, ok := cases[k]; !ok && k != "kokoro" && k != KindCatalogConfig { // catalog-config has its own tests below
			t.Errorf("registry kind %q has no launcher case in this test", k)
		}
	}
}

// The launcher is the second wall: it refuses what the Go validation would have, if the env file were written by something else.
func TestLauncher_RefusesWhatItShould(t *testing.T) {
	good := func() store.Occupant {
		return store.Occupant{Name: "asr", Kind: "qwen3-asr-server", Params: map[string]string{"model": "/opt/forge/models/a.gguf"}}
	}
	run := func(extra ...string) (string, int) { return launcherDryRun(t, good(), extra...) }
	for name, extra := range map[string][]string{
		"unknown kind":              {"FORGE_POOL_KIND=bash"},
		"a later KIND wins nothing": {"FORGE_POOL_KIND=qwen3-asr-server; touch /tmp/x"},
		"model outside the install": {"FORGE_POOL_P_MODEL=/etc/passwd"},
		"model with traversal":      {"FORGE_POOL_P_MODEL=/opt/forge/models/../../etc/passwd"},
		"non-numeric thread count":  {"FORGE_POOL_P_THREADS=4;id"},
		"missing model":             {"FORGE_POOL_P_MODEL="},
		"bad port":                  {"FORGE_POOL_PORT=80;id"},
	} {
		if out, code := run(extra...); code != 2 {
			t.Errorf("%s: exit %d, want 2: %s", name, code, out)
		}
	}
	if out, code := launcherDryRun(t, store.Occupant{Name: "k", Kind: "kokoro"}, "FORGE_POOL_PORT=8100"); code != 2 {
		t.Errorf("kokoro on a port other than 8880: exit %d: %s", code, out)
	}
	if out, code := launcherDryRun(t, store.Occupant{Name: "e", Kind: "llama-server", Params: map[string]string{"build": "build-vulkan-plain", "model": "/opt/forge/models/a.gguf"}},
		"FORGE_POOL_P_BUILD=../../../bin"); code != 2 {
		t.Errorf("a build name that climbs out of the root: exit %d: %s", code, out)
	}
}

// runLauncherCheck sources the launcher (its test hook stops after the helper definitions) and calls one of its on-disk checks.
func runLauncherCheck(t *testing.T, fn, bin, root string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", `FORGE_POOL_SOURCE_ONLY=1 FORGE_POOL_KIND=kokoro FORGE_POOL_PORT=8880 source ../../../systemd/start-pool.sh s1 && `+fn+` "$1" "$2"`, "bash", bin, root)
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(b), ee.ExitCode()
	} else if err != nil {
		t.Skipf("bash is not runnable here: %v", err)
	}
	return string(b), 0
}

// venvFixture builds <root>/venv/bin/python -> <home alias> -> <real interpreter dir>, with a pyvenv.cfg whose home names the alias, the way uv
// lays out a virtualenv. It returns the root, the interpreter symlink, and the directory the cfg declares.
func venvFixture(t *testing.T) (root, bin, homeAlias string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	interp := filepath.Join(base, "uv", "cpython-3.12.12", "bin")
	homeAlias = filepath.Join(base, "uv", "cpython-3.12", "bin") // uv's version alias: cpython-3.12 -> cpython-3.12.12
	for _, d := range []string{filepath.Join(root, "venv", "bin"), interp, filepath.Join(base, "uv", "cpython-3.12")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(interp, "python3.12"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(base, "uv", "cpython-3.12"))
	if err := os.Symlink(filepath.Join(base, "uv", "cpython-3.12.12"), filepath.Join(base, "uv", "cpython-3.12")); err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(root, "venv", "bin", "python")
	if err := os.Symlink(filepath.Join(homeAlias, "python3.12"), bin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "venv", "pyvenv.cfg"), []byte("home = "+homeAlias+"\nimplementation = CPython\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, bin, homeAlias
}

// The Kokoro failure (2026-11-02): a venv interpreter is a symlink out of the install root by design. It is accepted only into the directory
// the venv's own pyvenv.cfg declares, and nothing else loosens.
func TestLauncher_VenvInterpreterAcceptedOnlyIntoItsDeclaredHome(t *testing.T) {
	root, bin, home := venvFixture(t)
	if out, code := runLauncherCheck(t, "check_venv_bin", bin, root); code != 0 {
		t.Fatalf("a venv python resolving into its declared home was refused (%d): %s", code, out)
	}
	// The plain check still refuses the same symlink: the loosening is confined to the venv function.
	if out, code := runLauncherCheck(t, "check_bin", bin, root); code != 2 || !strings.Contains(out, "resolves outside") {
		t.Errorf("check_bin must still refuse a symlink out of the root: %d %s", code, out)
	}

	cfg := filepath.Join(root, "venv", "pyvenv.cfg")
	refuse := func(name, cfgBody string) {
		t.Helper()
		if err := os.WriteFile(cfg, []byte(cfgBody), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, code := runLauncherCheck(t, "check_venv_bin", bin, root); code != 2 {
			t.Errorf("%s: exit %d, want 2: %s", name, code, out)
		}
	}
	refuse("cfg declares a different home", "home = /usr/bin\n")
	refuse("cfg declares a relative home", "home = bin\n")
	refuse("cfg declares a home that climbs", "home = "+home+"/../../..\n")
	refuse("cfg has no home line", "implementation = CPython\n")
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	if out, code := runLauncherCheck(t, "check_venv_bin", bin, root); code != 2 || !strings.Contains(out, "no pyvenv.cfg") {
		t.Errorf("no pyvenv.cfg: %d %s", code, out)
	}
}

// A patch upgrade moves the real interpreter directory; the venv's cfg names the version alias, which is repointed. It must keep working.
func TestLauncher_VenvInterpreterSurvivesAnInterpreterPatchUpgrade(t *testing.T) {
	root, bin, _ := venvFixture(t)
	target, err := os.Readlink(bin) // <base>/uv/cpython-3.12/bin/python3.12
	if err != nil {
		t.Fatal(err)
	}
	uvDir := filepath.Dir(filepath.Dir(filepath.Dir(target))) // <base>/uv
	newReal := filepath.Join(uvDir, "cpython-3.12.13", "bin")
	if err := os.MkdirAll(newReal, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newReal, "python3.12"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(uvDir, "cpython-3.12")
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(uvDir, "cpython-3.12.13"), alias); err != nil {
		t.Fatal(err)
	}
	if out, code := runLauncherCheck(t, "check_venv_bin", bin, root); code != 0 {
		t.Errorf("the venv python was refused after a patch upgrade of its interpreter (%d): %s", code, out)
	}
}

// Without a venv: the path as written must still be under the root, and a missing or non-executable file is refused.
func TestLauncher_VenvCheckKeepsThePathAndExecutableRules(t *testing.T) {
	root, bin, _ := venvFixture(t)
	if out, code := runLauncherCheck(t, "check_venv_bin", "/usr/bin/python3", root); code != 2 {
		t.Errorf("a path outside the root: %d %s", code, out)
	}
	if out, code := runLauncherCheck(t, "check_venv_bin", root+"/venv/bin/../../../etc/passwd", root); code != 2 {
		t.Errorf("a path with ..: %d %s", code, out)
	}
	if out, code := runLauncherCheck(t, "check_venv_bin", filepath.Join(root, "venv", "bin", "nope"), root); code != 2 {
		t.Errorf("a missing binary: %d %s", code, out)
	}
	_ = bin
}

// catalogDryRun renders a catalog-config occupant exactly as the daemon does (env + args file) and feeds both to the launcher's dry run.
func catalogDryRun(t *testing.T, spec LaunchSpec, extraEnv ...string) (string, int) {
	t.Helper()
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "s1.args")
	if err := os.WriteFile(argsFile, []byte(RenderCatalogArgs(spec)), 0o600); err != nil {
		t.Fatal(err)
	}
	envText := RenderCatalogEnv(store.Occupant{Name: "d1"}, "s1", 8100, spec)
	cmd := exec.Command("bash", "../../../systemd/start-pool.sh", "s1")
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin", "FORGE_POOL_DRYRUN=1", "FORGE_POOL_ARGS_FILE=" + argsFile}
	for _, l := range strings.Split(strings.TrimSpace(envText), "\n") {
		cmd.Env = append(cmd.Env, l)
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(b), ee.ExitCode()
	} else if err != nil {
		t.Skipf("bash is not runnable here: %v", err)
	}
	return string(b), 0
}

// The catalog's own launch, as the LLM slots run it, plus what the pool adds (loopback, its port, -ngl 99, --metrics).
func TestLauncher_CatalogLlamaBuildsTheSameLaunchAnLLMSlotWould(t *testing.T) {
	out, code := catalogDryRun(t, LaunchSpec{
		Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/d1-2B/d1-2B-Q8_0.gguf",
		MMProj: "/opt/forge/models/d1-2B/mmproj-F16.gguf", Alias: "d1-2b", Backend: "vulkan", Ctx: 128000,
		Args: []string{"--load-mode", "none", "--jinja", "--flash-attn", "on", "--threads", "16", "--parallel", "1", "--batch-size", "16384", "--ubatch-size", "16384",
			"--chat-template-kwargs", `{"enable_thinking": false}`},
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", "--model /opt/forge/models/d1-2B/d1-2B-Q8_0.gguf",
		"--alias d1-2b", "--host 127.0.0.1", "--port 8100", "--ctx-size 128000", "-ngl 99", "--metrics", "--mmproj /opt/forge/models/d1-2B/mmproj-F16.gguf",
		"--load-mode none", "--jinja", "--flash-attn on", "--threads 16", "--parallel 1", "--batch-size 16384", "--ubatch-size 16384",
		`--chat-template-kwargs {"enable_thinking": false}`} {
		if !strings.Contains(out, want) {
			t.Errorf("command lacks %q:\n%s", want, out)
		}
	}
}

// The launcher refuses every flag the pool owns, even if the daemon-side check were bypassed (the second wall), and the two lists agree.
func TestLauncher_CatalogLlamaRefusesEveryPoolOwnedFlag(t *testing.T) {
	good := LaunchSpec{Bin: "/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/llama-server", Model: "/opt/forge/models/m.gguf", Alias: "m", Backend: "vulkan", Ctx: 4096}
	for _, f := range poolOwnedFlags {
		for _, arg := range []string{f, f + "=x"} {
			spec := good
			spec.Args = []string{"--jinja", arg, "value"}
			if out, code := catalogDryRun(t, spec); code != 2 || !strings.Contains(out, "pool owns") {
				t.Errorf("launcher accepted pool-owned flag %q: exit %d %s", arg, code, out)
			}
		}
	}
	// Control: the same spec without the flag starts.
	spec := good
	spec.Args = []string{"--jinja"}
	if out, code := catalogDryRun(t, spec); code != 0 {
		t.Errorf("control failed: %d %s", code, out)
	}
	// Anything else about the env is still checked.
	for name, extra := range map[string][]string{
		"binary is not llama-server": {"FORGE_POOL_P_BIN=/opt/forge/llama.cpp-upstream-plain/build-vulkan-plain/bin/bash"},
		"model outside the install":  {"FORGE_POOL_P_MODEL=/etc/passwd"},
		"alias with a space":         {"FORGE_POOL_P_ALIAS=a b"},
		"context not a number":       {"FORGE_POOL_P_CTX=4096;id"},
	} {
		if out, code := catalogDryRun(t, good, extra...); code != 2 {
			t.Errorf("%s: exit %d: %s", name, code, out)
		}
	}
}
