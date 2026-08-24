// SPDX-License-Identifier: Apache-2.0

package httpapi

// preflight.go — Sprint 12 (was H) Phase 3: the Danger Zone's real
// validate-before-save check for the boot-critical system group (server
// listen addresses, paths, ports, tailscale hostname). Wired into
// PUT/POST /api/v1/system/settings + /system/preflight via
// Server.systemPreflightFn (set below in NewInfraPreflight, called from
// cmd/forge/main.go once the Server exists).
//
// Every check here answers "can forge reach this" — it runs as the
// daemon's own uid, not the requesting operator's, which is the right
// question to ask (the daemon is what has to open these paths/ports), but
// worth remembering when a check fails on a path the operator can `ls`
// themselves. None of these checks write anything except state_dir's own
// self-cleaning write probe (create-then-immediately-remove) — that probe
// is the only way to honestly answer "is this directory writable," so it
// stays, with a comment explaining why.

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ttsUnitRE = regexp.MustCompile(`^[A-Za-z0-9@._-]+$`)

// hostnameRE is a loose DNS-label check — this is a warn-only cosmetic
// check (see preflightSystem's hostname case), not a hard gate, so it
// doesn't need to be a full RFC 1123 validator.
var hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// preflightSystem implements Server.systemPreflightFn. Wired in
// SetSystemPreflight (httpapi.go) rather than referenced by name directly,
// so a nil Server (Phase 4 stub environment, most unit tests) simply skips
// preflight instead of requiring every test fixture to wire it.
func (s *Server) preflightSystem(ctx context.Context, cand systemSettingsResponse) ([]preflightCheck, map[string]string) {
	var checks []preflightCheck
	fail := map[string]string{}
	add := func(field, level, message string) {
		checks = append(checks, preflightCheck{Field: field, Level: level, Message: message})
		if level == "error" {
			fail[field] = message
		}
	}

	current := s.resolvedSystemSettings(ctx)

	checkDir := func(field, path string, requiredWhenEmpty bool) {
		if path == "" {
			if requiredWhenEmpty {
				add(field, "warn", "not configured")
			}
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			add(field, "error", fmt.Sprintf("cannot stat: %v", err))
			return
		}
		if !info.IsDir() {
			add(field, "error", "exists but is not a directory")
			return
		}
		add(field, "ok", "directory exists")
	}
	checkDir("models_dir", cand.ModelsDir, true)
	checkDir("sysconfig_dir", cand.SysconfigDir, true)
	checkDir("icons_dir", cand.IconsDir, false)

	// state_dir gets a real write probe on top of the dir check — this is
	// the one check in this file with a side effect, and it's
	// self-cleaning by construction (create with O_EXCL, then remove in
	// the same breath) — the only way to honestly answer "is this
	// directory writable by the daemon" short of actually trying.
	if cand.StateDir == "" {
		add("state_dir", "error", "must not be empty")
	} else if info, err := os.Stat(cand.StateDir); err != nil {
		add("state_dir", "error", fmt.Sprintf("cannot stat: %v", err))
	} else if !info.IsDir() {
		add("state_dir", "error", "exists but is not a directory")
	} else {
		probe := filepath.Join(cand.StateDir, ".forge-preflight")
		if f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
			add("state_dir", "error", fmt.Sprintf("not writable by the daemon: %v", err))
		} else {
			_ = f.Close()
			_ = os.Remove(probe)
			add("state_dir", "ok", "directory exists and is writable")
		}
	}

	checkBin := func(field, path string) {
		if path == "" {
			add(field, "warn", "not configured")
			return
		}
		info, err := os.Stat(path)
		if err != nil {
			add(field, "error", fmt.Sprintf("cannot stat: %v", err))
			return
		}
		if info.IsDir() {
			add(field, "error", "is a directory, not a file")
			return
		}
		if info.Mode()&0o111 == 0 {
			add(field, "error", "not executable")
			return
		}
		add(field, "ok", "executable found")
	}
	checkBin("vulkan_bin", cand.VulkanBin)
	checkBin("rocm_bin", cand.RocmBin)

	// Listen addresses: skip the bind probe for anything unchanged — the
	// daemon is already bound to it, so probing would fail every save that
	// doesn't touch listen/router_listen/mcp_listen. A changed address gets
	// a real net.Listen+Close probe, explicitly caveated as "checked now,
	// not guaranteed at restart" (a port free now can be taken later).
	checkAddr := func(field, cur, cnd string) {
		if cnd == "" {
			add(field, "error", "must not be empty")
			return
		}
		if cnd == cur {
			add(field, "ok", "unchanged — already bound")
			return
		}
		ln, err := net.Listen("tcp", cnd)
		if err != nil {
			add(field, "error", fmt.Sprintf("checked now, not at restart: %v", err))
			return
		}
		_ = ln.Close()
		add(field, "ok", "address is free right now (not guaranteed at restart time)")
	}
	checkAddr("listen", current.Listen, cand.Listen)
	checkAddr("router_listen", current.RouterListen, cand.RouterListen)
	checkAddr("mcp_listen", current.MCPListen, cand.MCPListen)

	// Port collisions: every candidate aux port against the 3 listeners and
	// every real slot port (config.Slots — never editable from this
	// endpoint, but a real collision target). Structural key/range
	// validation already happened in buildSystemCandidate; this is the
	// deeper "would this actually work" check.
	used := map[int]string{}
	addUsed := func(addr, label string) {
		if _, ps, err := net.SplitHostPort(addr); err == nil {
			if p, err := strconv.Atoi(ps); err == nil && p > 0 {
				used[p] = label
			}
		}
	}
	addUsed(cand.Listen, "listen")
	addUsed(cand.RouterListen, "router_listen")
	addUsed(cand.MCPListen, "mcp_listen")
	if s.deps.Config != nil {
		if cfg := s.deps.Config(); cfg != nil {
			for name, slot := range cfg.Slots {
				if slot.Port > 0 {
					used[slot.Port] = "slot " + name
				}
			}
		}
	}
	portKeys := make([]string, 0, len(cand.Ports))
	for k := range cand.Ports {
		portKeys = append(portKeys, k)
	}
	for _, k := range portKeys {
		p := cand.Ports[k]
		if owner, dup := used[p]; dup {
			add("ports", "error", fmt.Sprintf("port %d for %q collides with %s", p, k, owner))
			continue
		}
		used[p] = "ports." + k
	}
	if len(portKeys) > 0 {
		if _, failed := fail["ports"]; !failed {
			add("ports", "ok", fmt.Sprintf("%d aux port(s), no collisions", len(portKeys)))
		}
	}

	// db_path: parent directory must exist and be writable (same
	// self-cleaning probe pattern as state_dir); if a file already exists
	// there, sniff its header rather than opening it — a second writer
	// connection to a live SQLite database is exactly the thing to avoid.
	if cand.DBPath == "" {
		add("db_path", "error", "must not be empty")
	} else {
		dir := filepath.Dir(cand.DBPath)
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			add("db_path", "error", fmt.Sprintf("parent directory %q does not exist", dir))
		} else {
			probe := filepath.Join(dir, ".forge-preflight-db")
			if f, err := os.OpenFile(probe, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err != nil {
				add("db_path", "error", fmt.Sprintf("parent directory not writable: %v", err))
			} else {
				_ = f.Close()
				_ = os.Remove(probe)
				if existing, err := os.Open(cand.DBPath); err == nil {
					hdr := make([]byte, 16)
					n, _ := existing.Read(hdr)
					_ = existing.Close()
					if n == 16 && string(hdr) == "SQLite format 3\x00" {
						add("db_path", "ok", "existing database file looks valid")
					} else {
						add("db_path", "warn", "a file exists here but doesn't look like a SQLite database")
					}
				} else {
					add("db_path", "ok", "parent directory is writable; a fresh database will be created here")
				}
			}
		}
	}

	if cand.TTSUnit == "" {
		add("tts_unit", "warn", "not configured")
	} else if !ttsUnitRE.MatchString(cand.TTSUnit) {
		add("tts_unit", "error", "must look like a systemd unit name (letters, digits, @._- — no .service suffix)")
	} else {
		add("tts_unit", "ok", "name looks valid")
	}

	if cand.Hostname != "" {
		if hostnameRE.MatchString(cand.Hostname) {
			add("hostname", "ok", "looks like a valid hostname")
		} else {
			add("hostname", "warn", "doesn't look like a DNS name")
		}
	}

	return checks, fail
}

// checkTrustedCIDRLockout is the highest-value check in this file: it
// answers whether SAVING this trusted_cidrs value would lock the SAVING
// operator out of a forward_auth_header deployment. Called from
// handleAuthConfigPut (auth_handlers.go), not through Server.systemPreflightFn
// — auth.provider.forward_auth_header.* isn't part of the system group's 4
// settings keys, it lives entirely inside auth/config, so it gets its own
// direct call rather than a second preflight endpoint.
//
// authz.ParseCIDRs (identity.go) silently drops any comma-separated entry
// that doesn't parse, so handleAuthConfigPut's structural check (every
// entry parses) already prevents a typo from silently narrowing the trust
// list. This check goes one step further: even a fully well-formed CIDR
// set can still exclude the very operator saving it.
func checkTrustedCIDRLockout(remoteAddr, cidrsRaw string) *preflightCheck {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		// Can't determine the caller's own address (e.g. a unix socket or
		// a test harness's synthetic RemoteAddr) — nothing meaningful to
		// check, don't block the save over it.
		return nil
	}
	for _, part := range strings.Split(cidrsRaw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			continue // already rejected by the structural check upstream
		}
		if prefix.Contains(addr) {
			return &preflightCheck{Field: "trusted_cidrs", Level: "ok", Message: "your own address is covered"}
		}
	}
	return &preflightCheck{
		Field: "trusted_cidrs", Level: "error",
		Message: fmt.Sprintf("this would lock you out — your address (%s) is not covered by any entry", host),
	}
}

// handleSystemRestart — POST /api/v1/system/restart (admin,
// action.system.restart). Restarts forge-daemon.service itself via D-Bus
// (engine.DBus.Restart, wired through Deps.SystemRestart so this package
// doesn't import internal/engine's DBus type directly).
//
// The success signal for the caller is the reconnect (health-poll), not
// this response — the process serving THIS request is the one about to
// die. The sequence is deliberate: write the 202 body, flush it onto the
// wire so it's provably sent, THEN fire the restart from a goroutine on
// context.Background() (never r.Context() — that's canceled the instant
// this handler returns, before the 750ms delay even elapses). The delay
// gives the flushed response a moment to actually leave the socket before
// the process that would write to it disappears. Restarting does not
// unload any model — slots are separate `forge-a*` systemd units, only
// the daemon process itself cycles.
func (s *Server) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if s.deps.SystemRestart == nil {
		writeError(w, http.StatusServiceUnavailable, "restart not available")
		return
	}
	s.audit(r, identity(r).Name, "system_restart", "forge-daemon", "")
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "unit": "forge-daemon.service"})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	restart := s.deps.SystemRestart
	goSafe("system_restart", func() {
		time.Sleep(750 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := restart(ctx); err != nil {
			// Expected in the common case: the restart job succeeds and
			// this very process is killed before RestartUnitContext's
			// result channel ever delivers — the job is owned by PID 1 and
			// completes regardless of whether we're still around to hear
			// about it. This log line only fires on a genuine failure to
			// even SUBMIT the job (e.g. D-Bus unreachable).
			log.Printf("httpapi: system restart: %v", err)
		}
	})
}
