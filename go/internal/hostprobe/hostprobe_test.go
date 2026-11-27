// SPDX-License-Identifier: Apache-2.0

package hostprobe

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeRunner records every command and answers from a script; any command it was not given an answer for fails the test, so a probe can't run
// something unexpected.
type fakeRunner struct {
	t     *testing.T
	calls [][]string
	out   []byte
	err   error
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return f.out, f.err
}

func newProbes(t *testing.T, out string, err error) (Probes, *fakeRunner) {
	f := &fakeRunner{t: t, out: []byte(out), err: err}
	return Probes{Run: f, BinaryPathAllowed: func(p string) (bool, string) { return strings.HasPrefix(p, "/opt/"), "must be under /opt" }}, f
}

func TestBinaryVersion(t *testing.T) {
	ctx := context.Background()
	p, f := newProbes(t, "version: 4711\n", nil)
	if got, err := p.BinaryVersion(ctx, "/opt/llama/bin/llama-server"); err != nil || got != "version: 4711\n" {
		t.Fatalf("version: %q %v", got, err)
	}
	if !reflect.DeepEqual(f.calls, [][]string{{"/opt/llama/bin/llama-server", "--version"}}) {
		t.Errorf("ran %v", f.calls)
	}
	// A path the check refuses is never executed.
	if _, err := p.BinaryVersion(ctx, "/tmp/evil"); err == nil || len(f.calls) != 1 {
		t.Errorf("disallowed path: %v, calls %v", err, f.calls)
	}
	// llama-server prints its banner to stderr and may exit non-zero: output wins; no output is an error.
	p, _ = newProbes(t, "banner", errors.New("exit status 1"))
	if got, err := p.BinaryVersion(ctx, "/opt/x"); err != nil || got != "banner" {
		t.Errorf("non-zero exit with output: %q %v", got, err)
	}
	p, _ = newProbes(t, "", errors.New("exit status 1"))
	if _, err := p.BinaryVersion(ctx, "/opt/x"); err == nil {
		t.Error("non-zero exit with no output must be an error")
	}
	if _, err := (Probes{Run: &fakeRunner{}}).BinaryVersion(ctx, "/opt/x"); err == nil {
		t.Error("no path check configured must refuse, not run")
	}
}

func TestGitAheadAndBehindLog(t *testing.T) {
	ctx := context.Background()
	p, f := newProbes(t, "7\n", nil)
	if n, err := p.GitAhead(ctx, "/opt/src/llama.cpp", "origin/master"); err != nil || n != 7 {
		t.Fatalf("ahead: %d %v", n, err)
	}
	want := []string{"git", "-C", "/opt/src/llama.cpp", "rev-list", "--count", "HEAD..origin/master"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv %v, want %v", f.calls[0], want)
	}
	p, _ = newProbes(t, "fix a\n\nadd b\n", nil)
	subj, err := p.GitBehindLog(ctx, "/opt/src/llama.cpp", "origin/master", 0)
	if err != nil || !reflect.DeepEqual(subj, []string{"fix a", "add b"}) {
		t.Errorf("log: %v %v", subj, err)
	}
	// The values that become argv are vetted first: nothing runs for a bad root or ref.
	p, f = newProbes(t, "0", nil)
	for name, args := range map[string][2]string{
		"relative root": {"src/llama", "origin/master"},
		"newline root":  {"/opt/a\nb", "origin/master"},
		"option as ref": {"/opt/src", "--upload-pack=x y"},
		"empty ref":     {"/opt/src", ""},
	} {
		if _, err := p.GitAhead(ctx, args[0], args[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused value reached git: %v", f.calls)
	}
	p, _ = newProbes(t, "not a number", nil)
	if _, err := p.GitAhead(ctx, "/opt/src", "main"); err == nil {
		t.Error("unparseable count must be an error")
	}
	p, _ = newProbes(t, "fatal: bad revision", errors.New("exit status 128"))
	if _, err := p.GitAhead(ctx, "/opt/src", "main"); err == nil || !strings.Contains(err.Error(), "bad revision") {
		t.Errorf("git failure should carry git's message: %v", err)
	}
}

func TestGitLsRemote(t *testing.T) {
	ctx := context.Background()
	p, f := newProbes(t, "abc123def\tHEAD\n", nil)
	if sha, err := p.GitLsRemote(ctx, "https://github.com/ggml-org/llama.cpp"); err != nil || sha != "abc123def" {
		t.Fatalf("ls-remote: %q %v", sha, err)
	}
	want := []string{"git", "-c", "http.followRedirects=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"ls-remote", "https://github.com/ggml-org/llama.cpp", "HEAD"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv %v, want %v", f.calls[0], want)
	}
	f.calls = nil
	for _, bad := range []string{"", "ext::sh -c id", "file:///etc", "ssh://x/y", "https://x/y z", "https://x/$(id)", "-oProxyCommand=x"} {
		if _, err := p.GitLsRemote(ctx, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused URL reached git: %v", f.calls)
	}
	p, _ = newProbes(t, "garbage with no tab", nil)
	if _, err := p.GitLsRemote(ctx, "https://github.com/x/y"); err == nil {
		t.Error("unparseable output must be an error")
	}
}

func TestJournalProbes(t *testing.T) {
	ctx := context.Background()
	since := time.Date(2026, 11, 4, 10, 30, 0, 0, time.UTC)

	p, f := newProbes(t, "line1\nline2\n", nil)
	got, err := p.SlotJournalErrors(ctx, 0, []string{"forge-a1", "forge-a1", "bad unit", "-x", "forge-a2@x"}, since)
	if err != nil || !reflect.DeepEqual(got, []string{"line1", "line2"}) {
		t.Fatalf("slot journal: %v %v", got, err)
	}
	want := []string{"journalctl", "--no-pager", "-n", "300", "--since", "2026-11-04 10:30:00", "-u", "forge-a1", "-u", "forge-a2@x"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Errorf("argv %v, want %v", f.calls[0], want)
	}
	// No usable unit: nothing is run.
	f.calls = nil
	if got, err := p.SlotJournalErrors(ctx, 10, []string{"bad unit"}, time.Time{}); got != nil || err != nil || len(f.calls) != 0 {
		t.Errorf("no units: %v %v %v", got, err, f.calls)
	}

	p, f = newProbes(t, "ring comp_1.0 timeout\n", nil)
	if got, err := p.KernelJournal(ctx, 5, time.Time{}); err != nil || !reflect.DeepEqual(got, []string{"ring comp_1.0 timeout"}) {
		t.Fatalf("kernel: %v %v", got, err)
	}
	if !reflect.DeepEqual(f.calls[0], []string{"journalctl", "-k", "--no-pager", "-n", "5"}) {
		t.Errorf("argv %v", f.calls[0])
	}
}

// ExecRunner gives a probe a deadline when the caller supplied none, and leaves the caller's own deadline alone.
func TestExecRunnerAppliesADefaultTimeout(t *testing.T) {
	start := time.Now()
	_, err := ExecRunner{Timeout: 100 * time.Millisecond}.Run(context.Background(), "sleep", "5")
	if err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("a hung command was not stopped by the default timeout: %v after %v", err, time.Since(start))
	}
	out, err := ExecRunner{Timeout: time.Nanosecond}.Run(contextWithDeadline(t), "echo", "hi")
	if err != nil || strings.TrimSpace(string(out)) != "hi" {
		t.Errorf("the caller's own deadline must win over the default: %q %v", out, err)
	}
}

func contextWithDeadline(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestValidateUpstreamURL(t *testing.T) {
	good := []string{
		"https://github.com/ggml-org/llama.cpp",
		"https://github.com/jsaigou/llama.cpp.git",
		"https://gitlab.example.org:8443/group/repo",
		"https://1.1.1.1/x",
		"https://[2606:4700:4700::1111]/x",
	}
	for _, u := range good {
		if ok, why := ValidateUpstreamURL(u); !ok {
			t.Errorf("%s refused: %s", u, why)
		}
	}
	bad := map[string]string{
		"":                                        "empty",
		"http://github.com/x/y":                   "cleartext",
		"HTTPS://github.com/x/y":                  "uppercase scheme (fail closed)",
		"ext::sh -c id":                           "ext transport",
		"file:///etc/passwd":                      "file",
		"ssh://git@github.com/x/y":                "ssh",
		"https://user:pw@github.com/x/y":          "credentials",
		"https://token@github.com/x/y":            "credentials (user only)",
		"https://github.com/x y":                  "space",
		"https://github.com/$(id)":                "shell metacharacter",
		"https://localhost/x":                     "localhost",
		"https://app.localhost/x":                 "localhost subdomain",
		"https://127.0.0.1/x":                     "loopback",
		"https://[::1]/x":                         "ipv6 loopback",
		"https://0.0.0.0/x":                       "unspecified",
		"https://169.254.169.254/latest":          "cloud metadata (link-local)",
		"https://10.1.2.3/x":                      "private",
		"https://192.168.0.5/x":                   "private",
		"https://172.16.9.9/x":                    "private",
		"https://100.64.1.2/x":                    "tailnet (CGNAT)",
		"https://100.127.255.254/x":               "tailnet (CGNAT, top of range)",
		"https://[fe80::1]/x":                     "ipv6 link-local",
		"https://[fd00::1]/x":                     "ipv6 unique local",
		"https://2130706433/x":                    "decimal loopback",
		"https://0x7f000001/x":                    "hex loopback",
		"https://127.1/x":                         "shorthand loopback",
		"https:///x":                              "no host",
		"https://localhost./x":                    "trailing-dot localhost",
		"https://forge.example.ts.net/x": "", // a name is not resolved here; this is allowed (see the doc comment)
	}
	for u, why := range bad {
		if why == "" {
			continue
		}
		if ok, _ := ValidateUpstreamURL(u); ok {
			t.Errorf("%q accepted (%s)", u, why)
		}
	}
	if ok, _ := ValidateUpstreamURL("https://100.128.0.1/x"); !ok {
		t.Error("100.128.0.1 is outside the CGNAT range and public")
	}
}

func TestRedactURL(t *testing.T) {
	if got := RedactURL("https://user:secret@github.com/x/y"); strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Errorf("credentials survived: %s", got)
	}
	if got := RedactURL("https://github.com/x/y"); got != "https://github.com/x/y" {
		t.Errorf("a clean URL changed: %s", got)
	}
	if got := RedactURL("://bad"); strings.Contains(got, "bad") && got == "://bad" {
		t.Errorf("an unparseable URL was echoed: %s", got)
	}
}

func TestGitCommandsNeverPromptAndOnlyUseHTTPS(t *testing.T) {
	env := commandEnv("git")
	want := map[string]bool{"GIT_TERMINAL_PROMPT=0": true, "GIT_ALLOW_PROTOCOL=https": true}
	for _, e := range env {
		delete(want, e)
	}
	if len(want) != 0 {
		t.Errorf("git env is missing %v (got %v)", want, env)
	}
	if env := commandEnv("journalctl"); env != nil {
		t.Errorf("other commands should inherit the environment untouched: %v", env)
	}
}

// A journal read that fails is an error. Reporting it as an empty journal would read as "nothing wrong" to the check that asked.
func TestJournalProbesReportAFailedRead(t *testing.T) {
	ctx := context.Background()
	p, _ := newProbes(t, "", errors.New("exit status 1"))
	if got, err := p.KernelJournal(ctx, 10, time.Time{}); err == nil || got != nil {
		t.Errorf("kernel journal failure: %v %v", got, err)
	}
	if got, err := p.SlotJournalErrors(ctx, 10, []string{"forge-a1"}, time.Time{}); err == nil || got != nil {
		t.Errorf("slot journal failure: %v %v", got, err)
	}
	// Partial output with a non-zero exit is still output.
	p, _ = newProbes(t, "line\n", errors.New("exit status 1"))
	if got, err := p.KernelJournal(ctx, 10, time.Time{}); err != nil || len(got) != 1 {
		t.Errorf("output with a non-zero exit: %v %v", got, err)
	}
}
