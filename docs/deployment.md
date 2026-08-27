# Deployment & Environment

The Forge is a single Go binary (`forge`) serving the ops dashboard (`:5000`), the a0 router
(`:8085`), and the MCP control surface (`:8095`) as one process, under one systemd unit
(`forge-daemon.service`).

## File Locations

- **Binary:** `/opt/forge/forge` (Go, typically cross-compiled `GOOS=linux GOARCH=amd64` for a
  remote host)
- **Config:** entirely store-backed - the catalog DB, `infra.*` settings keys, and the auth
  store. There is no config file to hand-edit; use Settings in the dashboard, the
  `/api/v1/catalog/*` and `/api/v1/*/settings` APIs, or `forge config get/set`.
- **State DB:** `/var/lib/forge/forge.db` (SQLite - sessions, API keys, slot state, settings,
  auth tables, model profiles, metric samples, catalog tables, etc.)
- **PWA build:** embedded in the binary via `go/internal/httpapi/web_dist/` (built from `web/`)

## Deploy procedure

Build the PWA → cross-compile → ship → fix permissions → restart:

```bash
# 1. Build the PWA (embeds into go/internal/httpapi/web_dist/)
cd web && npm run build

# 2. Cross-compile the daemon (stamp a version)
cd ../go && GOOS=linux GOARCH=amd64 \
  go build -ldflags "-X main.version=v0.5.x-<tag>-$(git rev-parse --short HEAD)" \
  -o forge ./cmd/forge

# 3. Ship to the host. IMPORTANT: a plain `cat > file` redirect creates a file with mode 0644
#    (no exec bit) - you MUST chmod +x after, or systemd will crash-loop with
#    "Failed at step EXEC ... Permission denied".
cat forge | ssh <host> "cat > /opt/forge/forge.new"

# 4. Back up the current binary + DB, fix perms, atomic swap, restart.
ssh <host> '
  set -e
  cd /opt/forge
  cp -a forge forge.bak-$(date +%Y%m%d-%H%M%S)
  cp -a /var/lib/forge/forge.db /var/lib/forge/forge.db.bak-$(date +%Y%m%d-%H%M%S)
  chmod +x forge.new
  chcon -t bin_t forge.new    # SELinux, only if enforcing
  mv -f forge.new forge
'
# 5. Restart (runs any pending DB migrations on startup).
ssh <host> 'systemctl restart forge-daemon.service'
```

Backups (`forge.bak-*` / `forge.db.bak-*`) are the rollback path - to roll back, move the backup
over `forge` and restart.

If you administer the host over Tailscale, `tailscale ssh <host>` works the same way as a
regular `ssh` once the host is set up as an SSH node; a polkit rule can grant your own user
passwordless restart on `forge-daemon.service` specifically, instead of requiring `sudo` for
every deploy.

## Systemd Units

- `forge-daemon.service`: dashboard (:5000) + a0 router (:8085) + MCP (:8095) - single Go binary
- `forge-a1.service` … `forge-a4.service`: inference slots A1-A4 (default ports 8080/8081/8087/8088)
- `forge-embedding.service`: CPU-only embedding server (permanent, always-on)
- `forge-stt.service`: CPU-only speech-to-text server (permanent, always-on)
- `forge-tts.service`: text-to-speech server

## Inference Binaries

The engine can run more than one llama-server build side by side (e.g. a Vulkan build and a
ROCm build), and selects which one to launch per-config via the catalog's `builds` table
(`binary_path`). The launcher script picks the binary based on the config's `backend` field.

**RPATH gotcha when building llama.cpp from source:** if you build with an absolute install
RPATH baked in at configure time (CMake's default), moving or renaming the source/build tree
later breaks every launch with `error while loading shared libraries` - the loader looks for
sibling libraries at the old absolute path. Build with an `$ORIGIN`-relative RPATH instead so
the binary resolves sibling libraries from its own directory regardless of where the tree lives:

```bash
cmake -S . -B build-vulkan-new \
  -DCMAKE_BUILD_TYPE=Release -DGGML_VULKAN=ON -DGGML_HIP=OFF \
  -DGGML_NATIVE=ON -DGGML_OPENMP=ON \
  -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON '-DCMAKE_INSTALL_RPATH=$ORIGIN'
cmake --build build-vulkan-new --target llama-server llama-bench -j$(nproc)
# verify the $ORIGIN literal survived in the cache:
grep CMAKE_INSTALL_RPATH: build-vulkan-new/CMakeCache.txt   # must show $ORIGIN, not empty
env -i HOME=$HOME PATH=/usr/bin:/bin ldd build-vulkan-new/bin/llama-server   # all libs resolve
```

Build into a new directory rather than overwriting a build currently in use, then repoint the
catalog's `builds.binary_path` and send the daemon a reload once you've verified the new binary.

## CLI (`forge`)

The client CLI/TUI ships inside the `forge` binary (same file as the daemon; subcommands never
boot a daemon). It talks to the running daemon's dashboard API using a bearer key: set
`FORGE_API_KEY`, or put a minted key at `~/.config/forge/cli.key` (`forge keys-export` mints and
writes it; requires admin).

| Command | Effect |
|---|---|
| `forge tui` | Full-screen ops console: Overview / Slots / Services / Compressor / Keys / Smith chat |
| `forge status` | One-screen summary (slots, budget, GTT) |
| `forge models` | List catalog configs available to load |
| `forge load <mode> [slot]` | Load a config onto a slot |
| `forge unload <slot>` | Empty a slot |
| `forge services [start\|stop <name>]` | List or toggle infra services |

Environment: `FORGE_API_URL` overrides the default `http://127.0.0.1:5000` (point it at your
tailnet/LAN dashboard URL to administer remotely).

## SELinux & Permissions

If running on an SELinux-enforcing host:

- Apply the `bin_t` context to inference binaries and any scripts systemd executes, after every
  update.
- Don't rely on `restorecon` for a binary living outside a standard system path - it can assign
  the wrong type (e.g. `lib_t` instead of `bin_t`) for a custom install location; set the
  context explicitly with `chcon` instead.

## Installer (`install.sh`)

`install.sh` supports two targets via `--target=dev|public` (default **dev**):

- **dev** - reference-host oriented: distro-specific gates, fixed local paths, a BIOS/firmware
  checklist, and stricter package-manager checks suited to a known development box.
- **public** - generic-host path: same unit installation, but reference-host assumptions are
  guarded or skipped. Requires the hardware/software pre-flight to pass unless `--force`. After
  units install it runs the hardware matcher (`installer/kgc-match.py`; a match writes a
  sysconfig file and opts the host out of runtime auto-tuning), offers to provision mandatory
  model assets (`installer/provision.py --mandatory`), and prints next steps (mint a key, open
  the dashboard, `forge tui`).

Pre-flight can be run standalone: `installer/preflight.sh [--json]`. Asset manifest:
`installer/assets.manifest.json`; provisioner usage and the whole flow are documented in
`installer/README.md`.

## Network exposure

The dashboard (`Server.Listen`) defaults to `127.0.0.1:5000` - loopback only. The `a0` router
(`Server.RouterListen`) defaults to `:8085` with **no host specified, i.e. all interfaces** - by
design, since it's meant to be reached by other devices on your tailnet. Its auth model assumes
that reachability boundary: requests sourced from a private tailnet address range skip the
bearer-key check entirely (see [docs/llm-router.md](llm-router.md)'s "Tailnet-Conditional Auth").

That's safe on a host that's only reachable over Tailscale. It is **not** safe to expose directly
to the public internet (a cloud VPS with a public IP, a port-forward, etc.) without first putting
a real perimeter in front of it - a firewall restricting :8085 to your tailnet's address range,
or a reverse proxy that terminates TLS and only forwards traffic you trust. If you're not
deploying on a Tailscale-connected host, treat every `a0` request as requiring its bearer key and
firewall the port accordingly.
