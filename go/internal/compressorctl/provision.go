// SPDX-License-Identifier: Apache-2.0

// Package compressorctl implements the proxy provisioning/teardown lifecycle
// for forge-compress instances (docs/v5-headroom-replacement.md Sprint 3,
// generalizing the Phase 2 mechanism docs/v5-headroom-topology.md §5 built
// for headroom-ai's now-retired `headroom@.service` template). A one-time,
// by-hand root install of the `forge-compress@.service` systemd template
// (EnvironmentFile=/var/lib/forge/compress/%i.env, testuser-writable) plus the
// existing polkit grant (forge-compress@* rides the pre-existing
// `50-forge.rules` glob — confirmed live, Sprint 6) lets forge (still
// running as testuser, no privilege escalation) write env files to that
// testuser-writable directory and start/stop/restart the resulting
// `forge-compress@<service>.service` instance over the existing D-Bus
// adapter. This package does not author the unit file itself, only the
// per-instance env file.
package compressorctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/jsaigou/the-forge/internal/store"
)

// Systemd is the subset of engine.Systemd (plus Restart, added to *engine.DBus
// for this package — docs/v5-headroom-topology.md Phase 2) that provisioning
// needs. A real systemd "restart" job is used rather than a manual
// Stop-then-Start sequence from this package, to avoid a start/stop-job race
// on the proxy's listening port.
type Systemd interface {
	Start(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
}

// defaultTemplatePrefix is the systemd template every Provisioner drives.
// Sprint 7 (docs/v5-headroom-replacement.md) dropped the dual-template split
// that existed during the Sprint 3 migration window (a second, headroom-ai-
// shaped "headroom@" template) once Sprint 6 confirmed zero live proxy rows
// still pointed at it — every real proxy has run on forge-compress@ since
// the Sprint 3/5 cutover.
const defaultTemplatePrefix = "forge-compress@"

// templatePrefix returns p.TemplatePrefix, defaulting to
// defaultTemplatePrefix when unset.
func (p *Provisioner) templatePrefix() string {
	if p.TemplatePrefix == "" {
		return defaultTemplatePrefix
	}
	return p.TemplatePrefix
}

// UnitName returns the systemd unit instance for a compressor proxy service
// name under this Provisioner's own template (e.g. "deepseek" ->
// "forge-compress@deepseek"). Matches store.ProxyRow.Unit for any proxy
// provisioned by this Provisioner. Used only when assigning a *new* row's
// Unit field — once a row exists, its stored Unit is the ground truth; see
// isTemplateUnit.
func (p *Provisioner) UnitName(service string) string { return p.templatePrefix() + service }

// isTemplateUnit reports whether unit is one of THIS Provisioner's own
// template instances (its own templatePrefix), as opposed to a legacy
// hand-created unit whose config is baked into its own unit file's
// `Environment=` lines, not read from this package's EnvironmentFile
// mechanism at all. Only template instances of THIS Provisioner have an env
// file it can write/remove, and only their target can be changed by
// rewriting it — Reconcile refuses on anything else rather than silently
// restarting a unit without actually changing what it points at.
func (p *Provisioner) isTemplateUnit(unit string) bool {
	return strings.HasPrefix(unit, p.templatePrefix())
}

// instanceOf returns the %i instance name systemd substitutes into
// EnvironmentFile=.../%i.env for one of this Provisioner's own template
// instances (e.g. "forge-compress@deepseek" -> "deepseek"). Only
// meaningful when isTemplateUnit is true.
func (p *Provisioner) instanceOf(unit string) string {
	return strings.TrimPrefix(unit, p.templatePrefix())
}

// kompressIntraThreads mirrors the fixed value baked into every hand-created
// proxy unit on ForgeHost today (confirmed live 2026-07-28 via `systemctl cat`
// on headroom-a1/headroom-deepseek/headroom-external, the legacy units that
// predated this package) — not configurable per-instance, so it is not a
// store.ProxyRow field.
const kompressIntraThreads = 16

// Provisioner writes per-instance env files and starts/stops the
// corresponding template-unit instance. It does no unit-file authoring and
// requires no privilege beyond the existing forge-*.service polkit grant.
type Provisioner struct {
	Systemd Systemd
	// EnvDir is the testuser-writable directory backing the template's
	// EnvironmentFile=%i.env (e.g. /var/lib/forge/compress).
	EnvDir string
	// TemplatePrefix selects which systemd template this Provisioner
	// drives — "forge-compress@" (default, when unset). See
	// templatePrefix's doc comment.
	TemplatePrefix string
}

func (p *Provisioner) envPathForUnit(unit string) string {
	return filepath.Join(p.EnvDir, p.instanceOf(unit)+".env")
}

// writeEnv renders row as its template instance's EnvironmentFile. Mode
// 0600: the file carries COMPRESS_PROXY_TOKEN, a secret (never logged —
// see store.ProxyRow.Token's own doc comment). Callers must only call this
// for a row whose Unit isTemplateUnit — a legacy hand-created unit's real
// config lives in its own unit file's `Environment=` lines and does not
// read this directory at all.
func (p *Provisioner) writeEnv(row store.ProxyRow) error {
	if row.Token == "" {
		return fmt.Errorf("compressorctl: proxy %q: no token set", row.Service)
	}
	if err := os.MkdirAll(p.EnvDir, 0o700); err != nil {
		return fmt.Errorf("compressorctl: env dir: %w", err)
	}
	content := fmt.Sprintf(
		"OPENAI_TARGET_API_URL=%s\nCOMPRESS_ONNX_INTRA_THREADS=%d\nCOMPRESS_PROXY_TOKEN=%s\nCOMPRESS_PORT=%d\n",
		row.TargetURL, kompressIntraThreads, row.Token, row.Port,
	)
	if err := os.WriteFile(p.envPathForUnit(row.Unit), []byte(content), 0o600); err != nil {
		return fmt.Errorf("compressorctl: write env: %w", err)
	}
	return nil
}

// Provision writes row's env file and (re)starts row.Unit. row.Unit is
// meant to be a fresh `forge-compress@<service>` instance (UnitName) —
// Provision is intended for a brand-new proxy, never a pre-existing legacy
// unit. Callers are responsible for persisting row via store.Routing.SaveProxy
// — this method only touches the filesystem and systemd, never the store.
//
// Uses systemd's Restart rather than Start (real bug, found live 2026-08-19
// migrating "local" onto forge-compress@: that unit name was already
// running — a manually-bootstrapped instance from an earlier smoke test,
// out-of-band from the store — so Start's job on an already-active unit was
// a silent no-op; writeEnv's fresh port/token were written to disk but never
// picked up by the running process, leaving the store's row pointing at a
// port nothing actually listened on). Restart starts a not-yet-running
// template instance exactly like Start would, and additionally guarantees a
// unit that happens to already be running under this name (in violation of
// the "brand-new proxy" assumption above) picks up the just-written env
// file instead of silently keeping its old one.
func (p *Provisioner) Provision(ctx context.Context, row store.ProxyRow) error {
	if !p.isTemplateUnit(row.Unit) {
		return fmt.Errorf("compressorctl: provision %q: unit %q is not a %s template instance", row.Service, row.Unit, p.templatePrefix())
	}
	if err := p.writeEnv(row); err != nil {
		return err
	}
	if err := p.Systemd.Restart(ctx, row.Unit); err != nil {
		return fmt.Errorf("compressorctl: start %s: %w", row.Unit, err)
	}
	return nil
}

// Reconcile rewrites row's env file (target URL/port/token may have
// changed) and restarts row.Unit so the change takes effect. Used when an
// already-provisioned proxy's upstream changes while still linked.
//
// Refuses on a legacy hand-created unit (row.Unit not a
// forge-compress@ instance): that unit's config is baked into its own
// unit file's `Environment=` lines, not read from this package's
// EnvironmentFile mechanism, so rewriting an env file here would restart
// the process while silently leaving its actual target unchanged — a
// correctness trap worse than refusing outright. Retargeting a legacy proxy
// needs its unit file edited directly (or the proxy torn down and
// re-linked, which provisions a fresh template instance in its place).
func (p *Provisioner) Reconcile(ctx context.Context, row store.ProxyRow) error {
	if !p.isTemplateUnit(row.Unit) {
		return fmt.Errorf("compressorctl: reconcile %q: unit %q is a legacy hand-created unit — its target can't be changed by rewriting an env file it doesn't read; edit its unit file directly, or tear it down and re-link to provision a managed replacement", row.Service, row.Unit)
	}
	if err := p.writeEnv(row); err != nil {
		return err
	}
	if err := p.Systemd.Restart(ctx, row.Unit); err != nil {
		return fmt.Errorf("compressorctl: restart %s: %w", row.Unit, err)
	}
	return nil
}

// Restart restarts unit without touching any env file (POST
// /api/v1/compressor/restart, Contract 1 §2 #19) — safe for any unit, legacy
// or template, since D-Bus Restart only needs the real unit name (callers
// pass store.ProxyRow.Unit, never a name reconstructed from Service).
func (p *Provisioner) Restart(ctx context.Context, unit string) error {
	if err := p.Systemd.Restart(ctx, unit); err != nil {
		return fmt.Errorf("compressorctl: restart %s: %w", unit, err)
	}
	return nil
}

// Teardown stops unit and, if it's one of this package's template
// instances, removes its env file (best-effort — the unit is already
// stopped either way, so a failed remove is not fatal). Safe for any unit,
// legacy or template; a legacy unit never had an env file to remove.
// Callers are responsible for marking/deleting the store.ProxyRow (pass
// store.ProxyRow.Unit, never a name reconstructed from Service).
func (p *Provisioner) Teardown(ctx context.Context, unit string) error {
	if err := p.Systemd.Stop(ctx, unit); err != nil {
		return fmt.Errorf("compressorctl: stop %s: %w", unit, err)
	}
	if p.isTemplateUnit(unit) {
		_ = os.Remove(p.envPathForUnit(unit))
	}
	return nil
}

// GenerateToken returns a fresh random COMPRESS_PROXY_TOKEN in the same
// shape as the hand-created proxies' existing tokens (a bare high-entropy
// value; forge-compress doesn't require a fixed prefix).
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("compressorctl: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// DefaultBasePort is the first port considered when allocating a new
// provider proxy — one past the four ports hand-assigned before this
// package existed (a1=8788, a2=8789 [dead/reclaimed as "local"],
// deepseek=8790, external=8791; confirmed live 2026-07-28).
const DefaultBasePort = 8792

// AllocatePort returns the lowest port >= base not already used by existing.
func AllocatePort(existing []store.ProxyRow, base int) int {
	used := make(map[int]bool, len(existing))
	for _, p := range existing {
		used[p.Port] = true
	}
	port := base
	for used[port] {
		port++
	}
	return port
}

// serviceNameRE mirrors validate.go's serviceRE (^[a-z][a-z0-9_-]+$) so this
// package can derive a safe name without importing httpapi.
var serviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]+$`)

// DeriveServiceName slugifies a provider display name into a compressor
// proxy service name: lowercase, "&"→"and", drop every non-[a-z0-9_-] char,
// strip a leading non-letter (service must start [a-z]), truncate to ≤64,
// then dedupe against the taken set by appending -2, -3, … . Returns "" if
// the name can't be made valid. Mirrors the FE suggestion in
// Compression.tsx so a provider's proxy and its icon resolve from the same
// identity (operator feedback 2026-08-14: the proxy's display label should
// match the provider's display name, but the proxy itself still needs a
// regex-safe service name).
func DeriveServiceName(name string, taken func(string) bool) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, "&", "and")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	s = b.String()
	for len(s) > 0 && !(s[0] >= 'a' && s[0] <= 'z') {
		s = s[1:]
	}
	if s == "" {
		return ""
	}
	if len(s) > 60 {
		s = s[:60]
	}
	if !serviceNameRE.MatchString(s) {
		return ""
	}
	candidate := s
	for i := 2; taken(candidate); i++ {
		candidate = s + "-" + strconv.Itoa(i)
		if len(candidate) > 64 {
			candidate = s[:60] + "-" + strconv.Itoa(i)
		}
		if i > 999 {
			return ""
		}
	}
	return candidate
}
