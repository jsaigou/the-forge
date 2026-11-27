// SPDX-License-Identifier: Apache-2.0

// Package hostprobe is the daemon's read-only look at its own host: the version of a binary, how far a git checkout is behind its upstream,
// what a remote's HEAD is, and the tail of the systemd and kernel journals. Smith's checks use it through func seams (smith.Deps), so the
// probes themselves are tested here against a fake command runner instead of the real git and journalctl.
//
// Every probe runs a fixed argv with no shell, validates the operator-supplied values that become argv (paths, refs, URLs, unit names) before
// anything is executed, and runs under a timeout. The package imports nothing internal.
package hostprobe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Runner runs one command and returns its combined stdout and stderr. It is the seam between the probes and the host: ExecRunner is the real
// adapter, tests supply a fake.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// DefaultTimeout bounds a probe whose caller gave the context no deadline of its own.
const DefaultTimeout = 30 * time.Second

// ExecRunner runs commands with os/exec. A context that already has a deadline keeps it; otherwise Timeout (DefaultTimeout when zero) applies.
type ExecRunner struct{ Timeout time.Duration }

// Run executes name with args (no shell) and returns the combined output.
func (r ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		t := r.Timeout
		if t <= 0 {
			t = DefaultTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if env := commandEnv(name); env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.CombinedOutput()
}

// commandEnv is what a command gets on top of the daemon's environment. git never prompts for credentials (a prompt would hang a probe until its
// timeout) and may only use the https transport: no ext::, file:// or ssh:// helpers, whatever a repository's config or a URL asks for.
func commandEnv(name string) []string {
	if name == "git" {
		return []string{"GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https"}
	}
	return nil
}

// Probes are the host probes over one Runner.
type Probes struct {
	Run Runner
	// BinaryPathAllowed vets a binary path before it is executed (smith's rule; injected because this package imports nothing internal).
	BinaryPathAllowed func(path string) (bool, string)
}

func (p Probes) runner() Runner {
	if p.Run == nil {
		return ExecRunner{}
	}
	return p.Run
}

var (
	gitRefRe      = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	systemdUnitRe = regexp.MustCompile(`^[A-Za-z0-9@._-]+$`)
)

// BinaryVersion runs "<path> --version" and returns what it printed. llama-server writes its banner to stderr, so a non-zero exit with output
// still counts as an answer.
func (p Probes) BinaryVersion(ctx context.Context, path string) (string, error) {
	if p.BinaryPathAllowed == nil {
		return "", errors.New("hostprobe: no binary path check configured")
	}
	if ok, reason := p.BinaryPathAllowed(path); !ok {
		return "", fmt.Errorf("hostprobe: binary path %q not allowed: %s", path, reason)
	}
	out, err := p.runner().Run(ctx, path, "--version")
	if err != nil && len(out) == 0 {
		return "", fmt.Errorf("hostprobe: exec %s --version: %w", path, err)
	}
	return string(out), nil
}

// checkGitRootRef validates the two operator-config values that become git argv: root must be an absolute path and ref must look like a ref
// name, so neither can ever parse as an option to the git subprocess.
func checkGitRootRef(root, ref string) error {
	if root == "" || ref == "" {
		return errors.New("hostprobe: git requires root and ref")
	}
	if !strings.HasPrefix(root, "/") || strings.ContainsAny(root, "\t\n\r\x00") {
		return fmt.Errorf("hostprobe: git root %q must be a clean absolute path", root)
	}
	if !gitRefRe.MatchString(ref) {
		return fmt.Errorf("hostprobe: git ref %q contains disallowed characters", ref)
	}
	return nil
}

// GitAhead counts the commits upstream ref has that root's HEAD lacks ("git rev-list --count HEAD..ref"). A non-zero count means the tracked
// build lags its upstream. Any error (no such ref, not a git repo, git missing) is returned: callers treat it as "unmeasurable".
func (p Probes) GitAhead(ctx context.Context, root, ref string) (int, error) {
	if err := checkGitRootRef(root, ref); err != nil {
		return 0, err
	}
	out, err := p.runner().Run(ctx, "git", "-C", root, "rev-list", "--count", "HEAD.."+ref)
	if err != nil {
		return 0, fmt.Errorf("hostprobe: git rev-list in %s: %w: %s", root, err, strings.TrimSpace(string(out)))
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("hostprobe: parse git ahead count %q: %w", strings.TrimSpace(string(out)), err)
	}
	return n, nil
}

// GitBehindLog returns the commit subject lines of the same HEAD..ref range GitAhead counts, at most maxN of them (default 50): enough to match
// a keyword watchlist, not a changelog.
func (p Probes) GitBehindLog(ctx context.Context, root, ref string, maxN int) ([]string, error) {
	if err := checkGitRootRef(root, ref); err != nil {
		return nil, err
	}
	if maxN <= 0 {
		maxN = 50
	}
	out, err := p.runner().Run(ctx, "git", "-C", root, "log", "--format=%s", "-n", strconv.Itoa(maxN), "HEAD.."+ref)
	if err != nil {
		return nil, fmt.Errorf("hostprobe: git log in %s: %w: %s", root, err, strings.TrimSpace(string(out)))
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			subjects = append(subjects, line)
		}
	}
	return subjects, nil
}

// GitLsRemote resolves url's HEAD commit with a read-only "git ls-remote". The URL comes from operator-editable settings and database rows, so
// it is validated (ValidateUpstreamURL) before anything is executed.
func (p Probes) GitLsRemote(ctx context.Context, rawURL string) (string, error) {
	if ok, reason := ValidateUpstreamURL(rawURL); !ok {
		return "", fmt.Errorf("hostprobe: git ls-remote url not allowed: %s", reason)
	}
	// Redirects are not followed (a redirect could lead to a host the validation never saw) and only https is allowed, whatever git is told.
	out, err := p.runner().Run(ctx, "git", "-c", "http.followRedirects=false", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always",
		"ls-remote", rawURL, "HEAD")
	if err != nil {
		return "", fmt.Errorf("hostprobe: git ls-remote: %w: %s", err, strings.TrimSpace(string(out)))
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	sha, _, ok := strings.Cut(line, "\t")
	if !ok || sha == "" {
		return "", fmt.Errorf("hostprobe: git ls-remote output unparseable: %q", strings.TrimSpace(string(out)))
	}
	return sha, nil
}

// ValidateUpstreamURL is the one rule for a git upstream URL that smith or the daemon will hand to git. The URL comes from operator-editable
// settings and database rows, so it is vetted before anything is executed: https only, no credentials in the URL, no shell metacharacters, and
// a host that is not the daemon's own machine or network (localhost, loopback, link-local, private and Tailscale ranges, or a numeric form of
// one). Hostnames are not resolved here, so a name that resolves inward is not caught.
func ValidateUpstreamURL(raw string) (ok bool, reason string) {
	if raw == "" {
		return false, "empty url"
	}
	if !strings.HasPrefix(raw, "https://") {
		return false, "only https git URLs are allowed"
	}
	if strings.ContainsAny(raw, "\t\n\r ;|&$`\"'\\") {
		return false, "url contains disallowed characters"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false, "url does not parse"
	}
	if u.User != nil {
		return false, "credentials in the url are not allowed"
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return false, "url has no host"
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false, "host is the local machine"
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || inCGNAT(ip) {
			return false, "host is on a local or private network"
		}
		return true, ""
	}
	if numericHostRe.MatchString(host) {
		return false, "numeric hosts must be written as a plain IP address"
	}
	return true, ""
}

// numericHostRe matches the shorthand forms of an IP address (decimal, hex, octal, dotted partials) that git or curl would still resolve to an
// address but net.ParseIP does not recognize.
var numericHostRe = regexp.MustCompile(`^(0x[0-9a-f]+|[0-9]+)(\.(0x[0-9a-f]+|[0-9]+))*$`)

// inCGNAT reports 100.64.0.0/10, the shared range Tailscale assigns to a tailnet.
func inCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1]&0xC0 == 64
}

// RedactURL returns raw with any credentials removed, for logs and findings. Validation refuses credentials, but a URL that fails validation is
// exactly the one that reaches an error message.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	return u.String()
}

func sinceArgs(since time.Time) []string {
	if since.IsZero() {
		return nil
	}
	return []string{"--since", since.Format("2006-01-02 15:04:05")}
}

func splitLines(out []byte) []string {
	return strings.Split(strings.TrimRight(string(out), "\n"), "\n")
}

// SlotJournalErrors reads the last n lines (default 300) of the journals of the given systemd units. Units are passed as explicit -u flags, not
// a glob, because a glob only matches active units and would drop a just-unloaded slot's history. There is no priority filter: llama-server logs
// its device-lost lines at info priority. Malformed unit names are skipped; with no usable unit the answer is empty. A journalctl that fails and prints
// nothing is an error, not an empty answer: the caller records that the read failed instead of reporting a clean journal. since, when
// non-zero, bounds how far back the read reaches.
func (p Probes) SlotJournalErrors(ctx context.Context, n int, units []string, since time.Time) ([]string, error) {
	if n <= 0 {
		n = 300
	}
	args := append([]string{"--no-pager", "-n", strconv.Itoa(n)}, sinceArgs(since)...)
	seen := map[string]bool{}
	for _, u := range units {
		if u != "" && systemdUnitRe.MatchString(u) && !strings.HasPrefix(u, "-") && !seen[u] {
			seen[u] = true
			args = append(args, "-u", u)
		}
	}
	if len(seen) == 0 {
		return nil, nil
	}
	out, err := p.runner().Run(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("hostprobe: journalctl for %d unit(s): %w", len(seen), err)
	}
	return splitLines(out), nil
}

// KernelJournal reads the last n lines (default 300) of the kernel journal (journalctl -k): where amdgpu logs the ring timeouts that mark a
// wedged GPU.
func (p Probes) KernelJournal(ctx context.Context, n int, since time.Time) ([]string, error) {
	if n <= 0 {
		n = 300
	}
	args := append([]string{"-k", "--no-pager", "-n", strconv.Itoa(n)}, sinceArgs(since)...)
	out, err := p.runner().Run(ctx, "journalctl", args...)
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("hostprobe: journalctl -k: %w", err)
	}
	return splitLines(out), nil
}
