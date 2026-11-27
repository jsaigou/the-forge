// SPDX-License-Identifier: Apache-2.0

package speechsetup

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const modelBody = "pretend model weights, long enough to be fetched in two pieces"

// testRecipe is a small engine with one built program, one source and one downloaded file.
func testRecipe(url string) Recipe {
	sum := sha256.Sum256([]byte(modelBody))
	return Recipe{
		ID: "toy", Label: "Toy", Files: []File{
			{Path: "toy/venv/bin/python", Executable: true},
			{Path: "toy/server.py"},
			{Path: "toy/models/m.bin", Size: int64(len(modelBody)), SHA256: hex.EncodeToString(sum[:])},
		},
		Setup: &Setup{
			Dir: "toy", Commands: []Command{{Name: "uv", DNF: "uv", Hint: "https://example.invalid/uv"}, {Name: "c++", DNF: "gcc-c++", APT: "g++"}},
			DiskBytes: 1000, Sources: map[string]string{"server.py": "kokoro/server.py"}, Python: "3.12", Venv: "venv", Lock: "kokoro/requirements.lock",
			URLs: map[string]string{"toy/models/m.bin": url},
		},
	}
}

type host struct {
	t        *testing.T
	in       *Installer
	r        Recipe
	commands []string
	trials   int
	trialErr error
	served   []string // the Range header of every download
}

func newHost(t *testing.T) *host {
	t.Helper()
	h := &host{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.served = append(h.served, r.Header.Get("Range"))
		http.ServeContent(w, r, "m.bin", time.Time{}, strings.NewReader(modelBody))
	}))
	t.Cleanup(srv.Close)
	h.r = testRecipe(srv.URL + "/m.bin")
	h.in = &Installer{
		Root:      t.TempDir(),
		LookPath:  func(string) (string, error) { return "/usr/bin/x", nil },
		FreeBytes: func(string) (int64, error) { return 1 << 40, nil },
		Distro:    func() string { return "dnf" },
		Command: func(_ context.Context, _ string, env []string, _ io.Writer, name string, args ...string) error {
			h.commands = append(h.commands, name+" "+args[0])
			if args[0] == "venv" {
				venv := args[len(args)-1]
				if !slices.Contains(env, "UV_PYTHON_INSTALL_DIR="+path.Join(h.in.Root, "python")) || !slices.Contains(env, "UV_PYTHON_DOWNLOADS=automatic") {
					return errors.New("the interpreter must be fetched into the root")
				}
				if err := os.MkdirAll(path.Join(venv, "bin"), 0o755); err != nil {
					return err
				}
				return os.WriteFile(path.Join(venv, "bin", "python"), nil, 0o755)
			}
			return nil
		},
		Trial: func(context.Context, string, Trial, io.Writer) error { h.trials++; return h.trialErr },
	}
	return h
}

func (h *host) install(stage string) (Outcome, error) {
	h.commands, h.trials, h.served = nil, 0, nil
	return h.in.Install(context.Background(), h.r, stage)
}

func (h *host) must(stage string) Outcome {
	h.t.Helper()
	out, err := h.install(stage)
	if err != nil {
		h.t.Fatalf("install %q: %v", stage, err)
	}
	return out
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestInstall_BuildsInStagingThenMovesIntoPlaceAndIsIdempotent(t *testing.T) {
	h := newHost(t)
	out := h.must("")
	if !out.Activated || out.Adopted || out.Previous != "" || h.trials != 1 || !slices.Equal(h.commands, []string{"uv venv", "uv pip"}) {
		t.Fatalf("first run: %+v trials=%d %v", out, h.trials, h.commands)
	}
	if got := h.r.Check(h.in.Root); len(got) != 0 {
		t.Fatalf("not complete: %+v", got)
	}
	if exists(path.Join(h.in.Root, stagingName, "toy")) || exists(path.Join(h.in.Root, "toy", builtMark)) || exists(path.Join(h.in.Root, "toy", triedMark)) {
		t.Error("staging and its marks must not remain")
	}
	// Again: adopted, nothing built or fetched.
	if out = h.must(""); !out.Adopted || out.Activated || len(h.commands) != 0 || h.trials != 0 || len(h.served) != 0 {
		t.Fatalf("second run: %+v %v", out, h.commands)
	}
}

func TestInstall_NeverOverwritesWithoutReplace(t *testing.T) {
	h := newHost(t)
	foreign := path.Join(h.in.Root, "toy", "mine.txt")
	if err := os.MkdirAll(path.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.install(""); err == nil || !strings.Contains(err.Error(), "--replace") || len(h.commands) != 0 {
		t.Fatalf("foreign directory: %v %v", err, h.commands)
	}
	h.in.Replace = true
	out := h.must("")
	if !out.Activated || out.Previous == "" || !exists(path.Join(out.Previous, "mine.txt")) || exists(foreign) {
		t.Fatalf("replace keeps the old directory: %+v", out)
	}
	// A complete engine whose weights differ from the pin is not adopted either.
	h.in.Replace = false
	if err := os.WriteFile(path.Join(h.in.Root, "toy", "models", "m.bin"), []byte(strings.Repeat("x", len(modelBody))), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := h.install(""); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("changed weights: %v", err)
	}
}

func TestInstall_AFailedTrialLeavesNothingInPlaceAndResumes(t *testing.T) {
	h := newHost(t)
	h.trialErr = errors.New("silent")
	if _, err := h.install(""); err == nil || !strings.Contains(err.Error(), "smoke: silent") {
		t.Fatalf("trial: %v", err)
	}
	if exists(path.Join(h.in.Root, "toy")) {
		t.Fatal("an engine that failed its trial must not be in place")
	}
	if _, err := h.install(StageActivate); err == nil || !strings.Contains(err.Error(), "trial") {
		t.Fatalf("activate without a passed trial: %v", err)
	}
	h.trialErr = nil
	if out := h.must(""); !out.Activated || len(h.commands) != 0 || len(h.served) != 0 {
		t.Fatalf("the second run reuses what was built and fetched: %+v %v %v", out, h.commands, h.served)
	}
}

func TestInstall_DownloadsResumeAndMustMatchTheirPin(t *testing.T) {
	h := newHost(t)
	part := path.Join(h.in.Root, stagingName, "toy", "toy", "models", "m.bin.part")
	if err := os.MkdirAll(path.Dir(part), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, []byte(modelBody[:10]), 0o644); err != nil {
		t.Fatal(err)
	}
	h.must(StageWeights)
	if !slices.Equal(h.served, []string{"bytes=10-"}) {
		t.Fatalf("resume: %v", h.served)
	}
	if got, err := os.ReadFile(strings.TrimSuffix(part, ".part")); err != nil || string(got) != modelBody {
		t.Fatalf("joined: %q %v", got, err)
	}

	h = newHost(t)
	h.r.Files[2].SHA256 = strings.Repeat("0", 64)
	if _, err := h.install(StageWeights); err == nil || !strings.Contains(err.Error(), "did not match its pin") {
		t.Fatalf("bad digest: %v", err)
	}
	left, err := os.ReadDir(path.Join(h.in.Root, stagingName, "toy", "toy", "models"))
	if err != nil || len(left) != 0 {
		t.Fatalf("a bad download must not stay: %v %v", left, err)
	}
}

func TestInstall_CheckSaysHowToGetWhatIsMissing(t *testing.T) {
	h := newHost(t)
	h.in.LookPath = func(name string) (string, error) { return "", exec.ErrNotFound }
	for distro, want := range map[string][]string{
		"dnf": {"sudo dnf install uv", "sudo dnf install gcc-c++"},
		"apt": {"https://example.invalid/uv", "sudo apt install g++"},
		"":    {"https://example.invalid/uv", "package manager"},
	} {
		h.in.Distro = func() string { return distro }
		var missing *MissingToolsError
		if _, err := h.install(""); !errors.As(err, &missing) {
			t.Fatalf("%q: %v", distro, err)
		}
		for _, w := range want {
			if !strings.Contains(missing.Error(), w) {
				t.Errorf("%q: %q lacks %q", distro, missing.Error(), w)
			}
		}
	}
	h = newHost(t)
	h.in.FreeBytes = func(string) (int64, error) { return 10, nil }
	if _, err := h.install(""); err == nil || !strings.Contains(err.Error(), "disk space") || len(h.commands) != 0 {
		t.Fatalf("disk: %v", err)
	}
}

func TestInstall_DryRunAndRefusals(t *testing.T) {
	h := newHost(t)
	var said []string
	h.in.DryRun = true
	h.in.Say = func(key string, args ...any) { said = append(said, key) }
	if out := h.must(""); out.Activated || out.Adopted || len(h.commands) != 0 {
		t.Fatalf("dry run: %+v", out)
	}
	if left, err := os.ReadDir(h.in.Root); err != nil || len(left) != 0 || len(said) < 6 {
		t.Fatalf("a dry run writes nothing: %v %v %v", left, err, said)
	}
	h.in.DryRun = false
	if _, err := h.install("nonsense"); err == nil {
		t.Error("unknown stage")
	}
	// An engine the Forge cannot build is still adopted when all its files are there.
	h = newHost(t)
	h.must("")
	h.r.Setup = nil
	if out, err := h.install(""); err != nil || !out.Adopted {
		t.Errorf("complete, no setup: %+v %v", out, err)
	}
	h.in.Root = t.TempDir()
	if _, err := h.install(""); !errors.Is(err, ErrCannotInstall) {
		t.Errorf("absent, no setup: %v", err)
	}
}

// Every recipe the installer can build must be complete in itself.
func TestSetupsAreComplete(t *testing.T) {
	built := 0
	for _, r := range Recipes() {
		s := r.Setup
		if s == nil {
			continue
		}
		built++
		for _, name := range s.Sources {
			if b, err := assets.ReadFile(name); err != nil || len(b) == 0 {
				t.Errorf("%s: source %s: %v", r.ID, name, err)
			}
		}
		lock, err := assets.ReadFile(s.Lock)
		if err != nil || strings.Count(string(lock), "--hash=sha256:") < 50 {
			t.Errorf("%s: the lock must pin by hash: %v", r.ID, err)
		}
		for _, f := range r.Files {
			if !strings.HasPrefix(f.Path, s.Dir+"/") {
				t.Errorf("%s: %s lies outside %s, which is all that is moved into place", r.ID, f.Path, s.Dir)
			}
			rel := strings.TrimPrefix(f.Path, s.Dir+"/")
			_, source := s.Sources[rel]
			fromVenv := strings.HasPrefix(rel, s.Venv+"/")
			if got := boolCount(source, fromVenv, s.URLs[f.Path] != ""); got != 1 {
				t.Errorf("%s: %s must come from exactly one of a source, the environment or a download (%d)", r.ID, f.Path, got)
			}
			if s.URLs[f.Path] != "" && (f.Size == 0 || !strings.HasPrefix(s.URLs[f.Path], "https://")) {
				t.Errorf("%s: %s is downloaded without a pin or over plain HTTP", r.ID, f.Path)
			}
		}
	}
	if built == 0 {
		t.Fatal("no recipe can be installed")
	}
}

func boolCount(b ...bool) (n int) {
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

func testWAV(seconds float64, amplitude float64) []byte {
	const rate = 8000
	n := int(seconds * rate)
	b := append([]byte("RIFF....WAVEfmt "), 16, 0, 0, 0, 1, 0, 1, 0)
	b = binary.LittleEndian.AppendUint32(b, rate)
	b = binary.LittleEndian.AppendUint32(b, rate*2)
	b = append(b, 2, 0, 16, 0)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(n*2))
	for i := range n {
		b = binary.LittleEndian.AppendUint16(b, uint16(int16(amplitude*32767*math.Sin(float64(i)/5))))
	}
	return b
}

func TestWavLevel(t *testing.T) {
	seconds, level, err := wavLevel(testWAV(1.5, 0.5))
	if err != nil || math.Abs(seconds-1.5) > 0.001 || level < 0.3 || level > 0.4 {
		t.Fatalf("%.3f %.3f %v", seconds, level, err)
	}
	if _, level, err = wavLevel(testWAV(1, 0)); err != nil || level != 0 {
		t.Fatalf("silence: %v %v", level, err)
	}
	for name, b := range map[string][]byte{"not a wav": []byte(`{"error":"x"}`), "no data": testWAV(1, 1)[:36], "empty": nil} {
		if _, _, err := wavLevel(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The real trial run against a stand-in engine: it must start it on the port it chose, hear every sentence, run the self-check and stop it.
func TestRunTrial(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	if err := os.WriteFile(path.Join(dir, "good.wav"), testWAV(1, 0.5), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path.Join(dir, "silent.wav"), testWAV(1, 0), 0o644); err != nil {
		t.Fatal(err)
	}
	server := `import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.end_headers()
    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        data = open("silent.wav" if body["voice"] == "mute" else "good.wav", "rb").read()
        self.send_response(200); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", int(os.environ["TOY_PORT"])), H).serve_forever()
`
	if err := os.WriteFile(path.Join(dir, "server.py"), []byte(server), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(python, path.Join(dir, "python")); err != nil {
		t.Fatal(err)
	}
	in := &Installer{}
	trial := Trial{Program: "python", Argument: "server.py", PortEnv: "TOY_PORT", Health: "/health", Speech: []TrialRequest{{Name: "one", Input: "hi", Voice: "v"}}, Check: "pass"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := in.runTrial(ctx, dir, trial, io.Discard); err != nil {
		t.Fatalf("good engine: %v", err)
	}
	bad := trial
	bad.Check = "raise SystemExit(3)"
	if err := in.runTrial(ctx, dir, bad, io.Discard); err == nil || !strings.Contains(err.Error(), "self-check") {
		t.Errorf("self-check: %v", err)
	}
	bad = trial
	bad.Speech = []TrialRequest{{Name: "two", Input: "hi", Voice: "mute"}}
	if err := in.runTrial(ctx, dir, bad, io.Discard); err == nil || !strings.Contains(err.Error(), "silent") {
		t.Errorf("silent: %v", err)
	}
	bad = trial
	bad.Argument = "missing.py"
	if err := in.runTrial(ctx, dir, bad, io.Discard); err == nil || !strings.Contains(err.Error(), "stopped while starting") {
		t.Errorf("crash: %v", err)
	}
}

func TestPackageTool(t *testing.T) {
	for release, want := range map[string]string{
		"ID=fedora\n": "dnf", "ID=ubuntu\nID_LIKE=debian\n": "apt", `ID="rocky"` + "\n" + `ID_LIKE="rhel centos fedora"`: "dnf",
		"ID=arch\n": "", "": "",
	} {
		if got := packageTool(release); got != want {
			t.Errorf("%s: %q, want %q", strconv.Quote(release), got, want)
		}
	}
}
