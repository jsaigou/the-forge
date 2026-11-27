// SPDX-License-Identifier: Apache-2.0

package speechsetup

// The installer behind `forge speech install` (docs/speech-setup-plan.md section 5.2). It runs as the service user, never as root, and works in
// stages that can each be run again: check, fetch, build, weights, smoke, activate. Everything is made in a staging directory and moved into place only
// after a trial run produced sound. An engine that is already complete is adopted: its files are verified and nothing is rebuilt. It never overwrites
// an existing engine directory unless asked to replace it, and then it keeps the previous one. It does not run the package manager.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stages, in order.
const (
	StageCheck    = "check"
	StageFetch    = "fetch"
	StageBuild    = "build"
	StageWeights  = "weights"
	StageSmoke    = "smoke"
	StageActivate = "activate"
)

// Stages lists the stages in the order they run.
var Stages = []string{StageCheck, StageFetch, StageBuild, StageWeights, StageSmoke, StageActivate}

const (
	stagingName = ".staging"
	builtMark   = ".built" // in the staged engine directory: the digest of the lock the environment was built from
	triedMark   = ".tried" // the trial run passed with what is staged
)

// ErrCannotInstall: the recipe can only be registered from files that are already there.
var ErrCannotInstall = errors.New("this engine cannot be installed by the Forge yet; it can only be registered from files that are already on the server")

// Installer makes engines under Root.
type Installer struct {
	Root    string
	Jobs    int  // compile jobs; 0 = half the processors
	Replace bool // replace an existing engine directory, keeping it beside the new one
	DryRun  bool // say what each stage would do
	// Say reports progress: a message key and its values.
	Say func(key string, args ...any)

	// What the host provides. Nil means the real thing; tests substitute them.
	LookPath  func(string) (string, error)
	Command   func(ctx context.Context, dir string, env []string, out io.Writer, name string, args ...string) error
	Client    *http.Client
	FreeBytes func(dir string) (int64, error)
	Trial     func(ctx context.Context, dir string, t Trial, log io.Writer) error
	Distro    func() string // "dnf", "apt" or ""
}

// Outcome is what an installation did.
type Outcome struct {
	Adopted   bool // the engine was complete already
	Activated bool // a newly made engine was moved into place
	// Previous is where a replaced engine directory was kept.
	Previous string
}

// MissingToolsError lists the programs the build needs and cannot find.
type MissingToolsError struct{ Advice []string }

func (e *MissingToolsError) Error() string {
	return "programs needed for the build are missing:\n  " + strings.Join(e.Advice, "\n  ")
}

func (in *Installer) say(key string, args ...any) {
	if in.Say != nil {
		in.Say(key, args...)
	}
}

func (in *Installer) staging(r Recipe) string   { return path.Join(in.Root, stagingName, r.ID) }
func (in *Installer) stagedDir(r Recipe) string { return path.Join(in.staging(r), r.Setup.Dir) }
func (in *Installer) liveDir(r Recipe) string   { return path.Join(in.Root, r.Setup.Dir) }

// Install makes r's engine. only, when not empty, runs that single stage.
func (in *Installer) Install(ctx context.Context, r Recipe, only string) (Outcome, error) {
	if only != "" && !contains(Stages, only) {
		return Outcome{}, fmt.Errorf("unknown stage %q (stages: %s)", only, strings.Join(Stages, ", "))
	}
	if !in.Replace {
		if len(r.Check(in.Root)) == 0 {
			// Complete already: verify what can be verified, and leave it as it is.
			if err := in.verifyPinned(r, in.Root); err != nil {
				return Outcome{}, fmt.Errorf("%s is already there but does not match this recipe (%w); use --replace to make it again", r.ID, err)
			}
			in.say("cli.speech.install.adopted", r.ID)
			return Outcome{Adopted: true}, nil
		}
	}
	if r.Setup == nil {
		return Outcome{}, ErrCannotInstall
	}
	if !in.Replace {
		if _, err := os.Lstat(in.liveDir(r)); err == nil {
			return Outcome{}, fmt.Errorf("%s exists but is not a complete %s engine; use --replace to make it again (the directory is kept beside the new one)", in.liveDir(r), r.ID)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Outcome{}, err
		}
	}
	steps := map[string]func(context.Context, Recipe) error{
		StageCheck: in.check, StageFetch: in.fetch, StageBuild: in.build, StageWeights: in.weights, StageSmoke: in.smoke,
	}
	var out Outcome
	for _, stage := range Stages {
		if only != "" && stage != only {
			continue
		}
		in.say("cli.speech.install.stage", stage)
		if in.DryRun {
			for _, line := range in.plan(r, stage) {
				in.say("cli.speech.install.would", line)
			}
			continue
		}
		if stage == StageActivate {
			previous, err := in.activate(r)
			if err != nil {
				return out, fmt.Errorf("%s: %w", stage, err)
			}
			out.Activated, out.Previous = true, previous
			continue
		}
		if err := steps[stage](ctx, r); err != nil {
			return out, fmt.Errorf("%s: %w", stage, err)
		}
	}
	return out, nil
}

// plan describes a stage for --dry-run.
func (in *Installer) plan(r Recipe, stage string) []string {
	s := r.Setup
	switch stage {
	case StageCheck:
		names := make([]string, 0, len(s.Commands))
		for _, c := range s.Commands {
			names = append(names, c.Name)
		}
		return []string{fmt.Sprintf("require %s and %.1f GB free under %s", strings.Join(names, ", "), float64(s.DiskBytes)/1e9, in.Root)}
	case StageFetch:
		return []string{"write " + strings.Join(sortedKeys(s.Sources), ", ") + " to " + in.stagedDir(r)}
	case StageBuild:
		return []string{fmt.Sprintf("make a Python %s environment in %s from the locked package list", s.Python, path.Join(in.stagedDir(r), s.Venv))}
	case StageWeights:
		var out []string
		for _, f := range r.Files {
			if u := s.URLs[f.Path]; u != "" {
				out = append(out, fmt.Sprintf("download %s (%d bytes, checked by SHA-256)", u, f.Size))
			}
		}
		return out
	case StageSmoke:
		return []string{"start the engine on a spare port and require sound for each trial sentence"}
	case StageActivate:
		return []string{"move " + in.stagedDir(r) + " to " + in.liveDir(r)}
	}
	return nil
}

func (in *Installer) distro() string {
	if in.Distro != nil {
		return in.Distro()
	}
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return packageTool(string(raw))
}

// packageTool reads ID and ID_LIKE from an os-release file.
func packageTool(osRelease string) string {
	ids := ""
	for _, line := range strings.Split(osRelease, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && (k == "ID" || k == "ID_LIKE") {
			ids += " " + strings.Trim(v, `"'`)
		}
	}
	for _, id := range strings.Fields(ids) {
		switch id {
		case "fedora", "rhel", "centos":
			return "dnf"
		case "debian", "ubuntu":
			return "apt"
		}
	}
	return ""
}

func (in *Installer) check(_ context.Context, r Recipe) error {
	look := in.LookPath
	if look == nil {
		look = exec.LookPath
	}
	var advice []string
	tool := in.distro()
	for _, c := range r.Setup.Commands {
		if _, err := look(c.Name); err == nil {
			continue
		}
		switch {
		case tool == "dnf" && c.DNF != "":
			advice = append(advice, c.Name+": sudo dnf install "+c.DNF)
		case tool == "apt" && c.APT != "":
			advice = append(advice, c.Name+": sudo apt install "+c.APT)
		case c.Hint != "":
			advice = append(advice, c.Name+": see "+c.Hint)
		default:
			advice = append(advice, c.Name+": install it with this system's package manager")
		}
	}
	if len(advice) > 0 {
		return &MissingToolsError{Advice: advice}
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		in.say("cli.speech.install.untested", runtime.GOARCH)
	}
	if err := os.MkdirAll(in.staging(r), 0o700); err != nil {
		return fmt.Errorf("this user cannot write under %s: %w", in.Root, err)
	}
	free := in.FreeBytes
	if free == nil {
		free = freeDisk
	}
	n, err := free(in.staging(r))
	if err != nil {
		return fmt.Errorf("read the free disk space: %w", err)
	}
	if n < r.Setup.DiskBytes {
		return fmt.Errorf("not enough disk space under %s: %.1f GB free, %.1f GB needed", in.Root, float64(n)/1e9, float64(r.Setup.DiskBytes)/1e9)
	}
	return nil
}

func freeDisk(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil //nolint:unconvert,gosec // the field types differ between platforms; a disk does not overflow int64
}

func (in *Installer) fetch(_ context.Context, r Recipe) error {
	for _, name := range sortedKeys(r.Setup.Sources) {
		body, err := assets.ReadFile(r.Setup.Sources[name])
		if err != nil {
			return err
		}
		dst := path.Join(in.stagedDir(r), name)
		if err := os.MkdirAll(path.Dir(dst), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(dst, body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) build(ctx context.Context, r Recipe) error {
	s := r.Setup
	lock, err := assets.ReadFile(s.Lock)
	if err != nil {
		return err
	}
	dir := in.stagedDir(r)
	venv := path.Join(dir, s.Venv)
	digest := sha256.Sum256(lock)
	want := hex.EncodeToString(digest[:])
	if got, err := os.ReadFile(path.Join(dir, builtMark)); err == nil && string(got) == want { //nolint:gosec // G304: a path under the engine root
		if _, err := os.Stat(path.Join(venv, "bin", "python")); err == nil {
			in.say("cli.speech.install.built_already")
			return nil
		}
	}
	// A new environment invalidates an earlier trial.
	for _, p := range []string{venv, path.Join(dir, builtMark), path.Join(dir, triedMark)} {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	lockFile := path.Join(in.staging(r), "requirements.lock")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(lockFile, lock, 0o600); err != nil {
		return err
	}
	jobs := in.Jobs
	if jobs <= 0 {
		jobs = max(1, runtime.NumCPU()/2)
	}
	// The interpreter and the cache live under the root, so the engine depends on nothing in anyone's home directory.
	env := append(os.Environ(),
		"UV_PYTHON_INSTALL_DIR="+path.Join(in.Root, "python"),
		"UV_CACHE_DIR="+path.Join(in.Root, ".cache", "uv"),
		// Fedora's uv is set to fetch an interpreter only when told to; this build needs exactly that one.
		"UV_PYTHON_DOWNLOADS=automatic",
		"CMAKE_BUILD_PARALLEL_LEVEL="+strconv.Itoa(jobs),
		"MAKEFLAGS=-j"+strconv.Itoa(jobs),
	)
	logPath := path.Join(in.staging(r), "build.log")
	in.say("cli.speech.install.log", logPath)
	logFile, err := os.Create(logPath) //nolint:gosec // G304: a path under the engine root
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }() // the log is a by-product: a close error changes nothing about the build
	run := in.Command
	if run == nil {
		run = runCommand
	}
	for _, args := range [][]string{
		{"venv", "--python", s.Python, "--python-preference", "only-managed", "--relocatable", venv},
		{"pip", "install", "--python", path.Join(venv, "bin", "python"), "--require-hashes", "-r", lockFile},
	} {
		if err := run(ctx, dir, env, logFile, "uv", args...); err != nil {
			return fmt.Errorf("uv %s failed: %w\n%s", args[0], err, tail(logPath))
		}
	}
	return os.WriteFile(path.Join(dir, builtMark), []byte(want), 0o600)
}

func runCommand(ctx context.Context, dir string, env []string, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the program and its arguments come from the compiled recipes
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, out, out
	return cmd.Run()
}

// tail is the end of a log, for an error message.
func tail(p string) string {
	raw, err := os.ReadFile(p) //nolint:gosec // G304: the installer's own log
	if err != nil {
		return "(the log could not be read: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}

func fileMatches(p string, f File) error {
	h, err := os.Open(p) //nolint:gosec // G304: a path under the engine root, from a compiled recipe
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }() // read only
	sum := sha256.New()
	n, err := io.Copy(sum, h)
	if err != nil {
		return err
	}
	if n != f.Size {
		return fmt.Errorf("%s: %d bytes, expected %d", p, n, f.Size)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != f.SHA256 {
		return fmt.Errorf("%s: its SHA-256 is %s, expected %s", p, got, f.SHA256)
	}
	return nil
}

// verifyPinned reads every pinned file of r under root and compares its digest.
func (in *Installer) verifyPinned(r Recipe, root string) error {
	for _, f := range r.Files {
		if f.SHA256 == "" {
			continue
		}
		in.say("cli.speech.install.verifying", path.Base(f.Path))
		if err := fileMatches(path.Join(root, f.Path), f); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) weights(ctx context.Context, r Recipe) error {
	for _, f := range r.Files {
		url := r.Setup.URLs[f.Path]
		if url == "" {
			continue
		}
		dst := path.Join(in.staging(r), f.Path)
		if fileMatches(dst, f) == nil {
			in.say("cli.speech.install.have", path.Base(f.Path))
			continue
		}
		if err := os.MkdirAll(path.Dir(dst), 0o700); err != nil {
			return err
		}
		// A good copy in the engine being replaced needs no second download.
		if live := path.Join(in.Root, f.Path); fileMatches(live, f) == nil {
			if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if err := os.Link(live, dst); err == nil {
				in.say("cli.speech.install.reused", path.Base(f.Path))
				continue
			}
		}
		in.say("cli.speech.install.downloading", path.Base(f.Path), fmt.Sprintf("%.0f", float64(f.Size)/1e6))
		if err := in.download(ctx, url, dst, f); err != nil {
			return err
		}
	}
	return nil
}

// download fetches url to dst through dst.part, continuing a partial file, and keeps it only if its size and digest are the pinned ones.
func (in *Installer) download(ctx context.Context, url, dst string, f File) error {
	part := dst + ".part"
	var have int64
	if st, err := os.Stat(part); err == nil {
		have = st.Size()
	}
	if have > f.Size {
		have = 0
	}
	if have < f.Size {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if have > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(have, 10)+"-")
		}
		client := in.Client
		if client == nil {
			client = http.DefaultClient
		}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("download %s: %w", path.Base(dst), err)
		}
		defer func() { _ = resp.Body.Close() }() // the copy below reports a broken body
		flags := os.O_WRONLY | os.O_CREATE
		switch {
		case resp.StatusCode == http.StatusPartialContent && have > 0:
			flags |= os.O_APPEND
		case resp.StatusCode == http.StatusOK:
			flags |= os.O_TRUNC // the server sent the whole file
		default:
			return fmt.Errorf("download %s: the server answered %s", path.Base(dst), resp.Status)
		}
		out, err := os.OpenFile(part, flags, 0o600) //nolint:gosec // G304: a path under the engine root, from a compiled recipe
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, io.LimitReader(resp.Body, f.Size+1))
		if err := out.Close(); err != nil && copyErr == nil {
			copyErr = err
		}
		if copyErr != nil {
			return fmt.Errorf("download %s was interrupted (run it again to continue): %w", path.Base(dst), copyErr)
		}
	}
	if st, err := os.Stat(part); err == nil && st.Size() < f.Size {
		return fmt.Errorf("download %s is incomplete: %d of %d bytes (run it again to continue)", path.Base(dst), st.Size(), f.Size)
	}
	if err := fileMatches(part, f); err != nil {
		if rm := os.Remove(part); rm != nil {
			return errors.Join(err, rm)
		}
		return fmt.Errorf("the download did not match its pin and was removed: %w", err)
	}
	return os.Rename(part, dst)
}

func (in *Installer) smoke(ctx context.Context, r Recipe) error {
	dir := in.stagedDir(r)
	if problems := r.Check(in.staging(r)); len(problems) > 0 {
		return (&FilesMissingError{Recipe: r.ID, Problems: problems})
	}
	logPath := path.Join(in.staging(r), "smoke.log")
	in.say("cli.speech.install.log", logPath)
	logFile, err := os.Create(logPath) //nolint:gosec // G304: a path under the engine root, from a compiled recipe
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }() // by-product, as the build log
	trial := in.Trial
	if trial == nil {
		trial = in.runTrial
	}
	if err := trial(ctx, dir, r.Setup.Trial, logFile); err != nil {
		return fmt.Errorf("%w\n%s", err, tail(logPath))
	}
	return os.WriteFile(path.Join(dir, triedMark), []byte("ok\n"), 0o600)
}

const trialStart = 3 * time.Minute

func (in *Installer) runTrial(ctx context.Context, dir string, t Trial, log io.Writer) error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return err
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	program := path.Join(dir, t.Program)
	cmd := exec.CommandContext(runCtx, program, path.Join(dir, t.Argument)) //nolint:gosec // G204: both come from the compiled recipe
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, log, log
	cmd.Env = append(os.Environ(), t.PortEnv+"="+strconv.Itoa(port))
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the engine: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		stop()
		<-exited //nolint:errcheck // the engine was just killed on purpose: its exit error is expected
	}()
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	client := &http.Client{Timeout: 2 * time.Minute}
	deadline := time.Now().Add(trialStart)
	for {
		select {
		case err := <-exited:
			exited <- err
			return fmt.Errorf("the engine stopped while starting: %v", err)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+t.Health, nil)
		if err != nil {
			return err
		}
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close() // only the status matters
			if resp.StatusCode/100 == 2 {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the engine did not answer within %s", trialStart)
		}
	}
	for _, s := range t.Speech {
		seconds, err := speak(ctx, client, base, s)
		if err != nil {
			return fmt.Errorf("the %s trial sentence: %w", s.Name, err)
		}
		in.say("cli.speech.install.heard", s.Name, fmt.Sprintf("%.1f", seconds))
	}
	if t.Check != "" {
		run := in.Command
		if run == nil {
			run = runCommand
		}
		if err := run(ctx, dir, os.Environ(), log, program, "-c", t.Check); err != nil {
			return fmt.Errorf("the engine's self-check failed: %w", err)
		}
	}
	return nil
}

func speak(ctx context.Context, client *http.Client, base string, s TrialRequest) (float64, error) {
	body, err := json.Marshal(map[string]string{"input": s.Input, "voice": s.Voice, "language": s.Language})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }() // the read below reports a broken body
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("the engine answered %s", resp.Status)
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, err
	}
	seconds, level, err := wavLevel(audio)
	if err != nil {
		return 0, err
	}
	if seconds < 0.3 || seconds > 30 {
		return 0, fmt.Errorf("%.2f seconds of audio is not a spoken sentence", seconds)
	}
	if level < 0.003 {
		return 0, fmt.Errorf("the audio is silent")
	}
	return seconds, nil
}

// wavLevel reads a mono or interleaved WAV (16-bit integers or 32-bit floats) and returns its length and its RMS level, full scale = 1.
func wavLevel(b []byte) (seconds, level float64, err error) {
	if len(b) < 12 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return 0, 0, errors.New("the engine did not answer with a WAV file")
	}
	var format, channels, bits uint16
	var rate uint32
	for off := 12; off+8 <= len(b); {
		id, size := string(b[off:off+4]), int(binary.LittleEndian.Uint32(b[off+4:off+8]))
		body := b[off+8:]
		if size >= 0 && size < len(body) {
			body = body[:size]
		}
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return 0, 0, errors.New("the WAV header is cut short")
			}
			format, channels = binary.LittleEndian.Uint16(body), binary.LittleEndian.Uint16(body[2:])
			rate, bits = binary.LittleEndian.Uint32(body[4:]), binary.LittleEndian.Uint16(body[14:])
		case "data":
			if rate == 0 || channels == 0 {
				return 0, 0, errors.New("the WAV has no format")
			}
			var sum float64
			var n int
			switch {
			case format == 1 && bits == 16:
				for i := 0; i+2 <= len(body); i += 2 {
					v := float64(int16(binary.LittleEndian.Uint16(body[i:]))) / 32768 //nolint:gosec // reinterpreting a sample
					sum, n = sum+v*v, n+1
				}
			case format == 3 && bits == 32:
				for i := 0; i+4 <= len(body); i += 4 {
					v := float64(math.Float32frombits(binary.LittleEndian.Uint32(body[i:])))
					sum, n = sum+v*v, n+1
				}
			default:
				return 0, 0, fmt.Errorf("unsupported WAV format %d with %d bits", format, bits)
			}
			if n == 0 {
				return 0, 0, errors.New("the WAV holds no audio")
			}
			return float64(n) / float64(channels) / float64(rate), math.Sqrt(sum / float64(n)), nil
		}
		off += 8 + size + size%2
		if size < 0 {
			break
		}
	}
	return 0, 0, errors.New("the WAV holds no audio")
}

// activate moves the staged engine into place.
func (in *Installer) activate(r Recipe) (previous string, err error) {
	staged, live := in.stagedDir(r), in.liveDir(r)
	if _, err := os.Stat(path.Join(staged, triedMark)); err != nil {
		return "", fmt.Errorf("the staged engine has not passed its trial run (stage %s)", StageSmoke)
	}
	if problems := r.Check(in.staging(r)); len(problems) > 0 {
		return "", &FilesMissingError{Recipe: r.ID, Problems: problems}
	}
	if _, err := os.Lstat(live); err == nil {
		if !in.Replace {
			return "", fmt.Errorf("%s exists; use --replace", live)
		}
		previous = live + ".prev-" + time.Now().UTC().Format("20060102-150405")
		if err := os.Rename(live, previous); err != nil {
			return "", err
		}
		in.say("cli.speech.install.kept", previous)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	for _, mark := range []string{builtMark, triedMark} {
		if err := os.Remove(path.Join(staged, mark)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return previous, err
		}
	}
	if err := os.Rename(staged, live); err != nil {
		if previous != "" {
			if back := os.Rename(previous, live); back != nil {
				return previous, errors.Join(err, back)
			}
			previous = ""
		}
		return previous, err
	}
	if err := os.RemoveAll(in.staging(r)); err != nil {
		in.say("cli.speech.install.leftover", in.staging(r), err.Error())
	}
	in.say("cli.speech.install.activated", live)
	return previous, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
