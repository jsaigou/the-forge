# Deployment & Environment

> **v0.5 is live.** This file described V4 (Python/Flask). v0.5 is a single Go binary
> (`forge`) serving dashboard (:5000), a0 (:8085), and MCP (:8095) as one process
> (`forge-daemon.service`). The V4 details below are retained as a rollback reference.
> For current v0.5 facts, see `CLAUDE.md` and `AGENTS.md`.

## v0.5 File Locations
- **Binary:** `/opt/forge/forge` (Go, cross-compiled `GOOS=linux GOARCH=amd64`)
- **Config:** `/etc/forge/forge.toml` + `/etc/forge/router.toml` (~~`/etc/forge/models.toml`~~ retired — Phase B moved the registry to the catalog DB)
- **State DB:** `/var/lib/forge/forge.db` (SQLite — sessions, api_keys, slot_state, settings, auth-v2 tables, model_profiles, metric_samples, catalog tables, etc.)
- **PWA build:** embedded in the binary via `go/internal/httpapi/web_dist/` (built from `web/`)

## v0.5 Deploy procedure

Build PWA → cross-compile → ship → fix permissions → restart:

```bash
# 1. Build the PWA (embeds into go/internal/httpapi/web_dist/)
cd web && npm run build

# 2. Cross-compile the daemon (stamp a version)
cd ../go && GOOS=linux GOARCH=amd64 \
  go build -ldflags "-X main.version=v5.0.x-<tag>-$(git rev-parse --short HEAD)" \
  -o forge ./cmd/forge

# 3. Ship to ForgeHost. IMPORTANT: `cat > file` creates mode 0644 (no exec bit)!
#    You MUST chmod +x after, or systemd crash-loops with "Failed at step EXEC
#    ... Permission denied" (learned 2026-07-24 during A1 deploy).
cat forge | tailscale ssh forgehost "cat > /opt/forge/forge.new"

# 4. Backup current binary + DB, fix perms, atomic swap, restart.
tailscale ssh forgehost '
  set -e
  cd /opt/forge
  cp -a forge forge.bak-$(date +%Y%m%d-%H%M%S)
  cp -a /var/lib/forge/forge.db /var/lib/forge/forge.db.bak-$(date +%Y%m%d-%H%M%S)
  chmod +x forge.new
  chcon -t bin_t forge.new    # SELinux (ForgeHost is enforcing)
  mv -f forge.new forge
'
# 5. Restart (runs any pending DB migrations on startup). No sudo needed —
#    polkit permits testuser to restart this unit (confirmed 2026-08-13; plain
#    `sudo` demands a password, so don't use it).
tailscale ssh forgehost 'systemctl restart forge-daemon.service'
```

The binary is owned by `testuser` and the restart is polkit-permitted — no sudo
anywhere in steps 3–5. Backups (`forge.bak-*` / `forge.db.bak-*`) are the
rollback path — to roll back, `mv` the backup over `forge` and restart.

## v0.5 Systemd Units
- `forge-daemon.service`: dashboard (:5000) + a0 router (:8085) + MCP (:8095) — single Go binary
- `forge-a1.service`: Port 8080 — inference slot A1 (renamed 2026-07-29 from `forge-primary.service`, internal key `primary`→`a1`, for consistency with a3/a4 — see `docs/modes.md`)
- `forge-a2.service`: Port 8081 — inference slot A2 (renamed 2026-07-29 from `forge-secondary.service`, internal key `secondary`→`a2`)
- `forge-a3.service`: Port 8087 — inference slot A3 (added 2026-07-21)
- `forge-a4.service`: Port 8088 — inference slot A4 (added 2026-07-21)
- `forge-embedding.service`: CPU-only embedding server, port 8083 (permanent)
- `forge-stt.service`: CPU-only STT server, port 8084 (permanent)
- `forge-tts.service`: Qwen3-TTS server, port 8082 — see `docs/qwen3-tts-voices.md`

---

## V4 File Locations (rollback reference — NOT live)
- **Config:** `/etc/forge/config.toml` (Single source of truth)
- **Engine:** `/opt/forge/engine.py`
- **Dashboard:** `/opt/forge/app.py`
- **Scripts:** `/usr/local/lib/forge/`
- **State:** `/var/lib/forge/current`
- **Python:** `/opt/forge/venv/` (Python 3.14)

## V4 Systemd Units (rollback reference — NOT live)
- `forge-primary.service`: Port 8080 — inference slot a1
- `forge-secondary.service`: Port 8081 — inference slot a2 (dual-model modes only)
- `forge-dashboard.service`: UI and API (gunicorn+gevent, port 5000)
- `forge-router.service`: a0 router, port 8085 — see `docs/llm-router.md`

> Boot-to-default-mode (`forge-boot.service` / `ai-mode boot`) was **retired** several
> major versions ago — models load on demand via the a0 scheduler; no default mode is
> forced at boot.
- `forge-embedding.service`: CPU-only embedding server, port 8083 (permanent, not mode-switched)
- `forge-stt.service`: CPU-only STT server, port 8084 (permanent, not mode-switched)
- `forge-tts.service`: Qwen3-TTS server, port 8082 — see `docs/qwen3-tts-voices.md`
- `forge-tts-backup.service` / `.timer`: weekly rclone backup of TTS voices to Google Drive
- `forge-agent.service`: Forge Node Agent (user unit on remote nodes, e.g. PairNode), port 9200
- `forge-comfyui.service`: ComfyUI service mode, started/stopped via the dashboard Services panel or `forge services`, not mode switching

## Inference Binaries

Two llama-server builds coexist on ForgeHost:

| Backend | Path |
|---|---|
| Vulkan (default) | `/opt/forge/llama.cpp/build-vulkan-new/bin/llama-server` |
| ROCm | `/opt/forge/llama.cpp/build/bin/llama-server` |

The launcher scripts select the binary via `$FORGE_BACKEND` (written by the engine from the mode's `backend` field). The engine writes `FORGE_LLAMA_BIN` from the catalog `builds` table (`binary_path`); the `standard-vulkan` build (id 5) currently points at `build-vulkan-new`.

**RUNPATH gotcha (2026-08-17 incident):** the Vulkan build MUST be compiled with a
`$ORIGIN` install RPATH. The previous `build-vulkan` binaries baked an absolute
`RUNPATH=/opt/forge/llama.cpp-b9859/build-vulkan/bin:` at configure time; when that tree
was renamed to `llama.cpp`, every `forge-a2` start died at exec with
`error while loading shared libraries: libllama-server-impl.so` (exit 127). CMake embeds
the source-tree path by default, so any rebuild that omits the RPATH flags below recreates
the bug on the next tree move. `$ORIGIN` makes the loader resolve sibling libs from the
binary's own `bin/` dir regardless of where the tree lives.

To rebuild Vulkan after a llama.cpp update (build into a NEW dir, never overwrite live):
```bash
cd /opt/forge/llama.cpp
cmake -S . -B build-vulkan-new \
  -DCMAKE_BUILD_TYPE=Release -DGGML_VULKAN=ON -DGGML_HIP=OFF \
  -DGGML_NATIVE=ON -DGGML_OPENMP=ON \
  -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON '-DCMAKE_INSTALL_RPATH=$ORIGIN'
cmake --build build-vulkan-new --target llama-server llama-bench -j32
# verify the $ORIGIN literal survived in the cache:
grep CMAKE_INSTALL_RPATH: build-vulkan-new/CMakeCache.txt   # must show $ORIGIN (not empty/;/...)
env -i HOME=$HOME PATH=/usr/bin:/bin ldd build-vulkan-new/bin/llama-server   # all libs resolve from $ORIGIN
# then repoint the catalog build (UPDATE builds SET binary_path=... WHERE id=5) + SIGHUP forge
```
(`chcon -t bin_t` on the binary is optional — the kintsugi `build-rocm-new` runs fine under
the default `usr_t` context, verified live.)

## CLI (`forge`)

The client CLI/TUI ships inside the `forge` binary (same file as the daemon —
subcommands never boot a daemon). It talks to the running daemon's dashboard API
using a bearer key: set `FORGE_API_KEY`, or put a minted operator key at
`~/.config/forge/cli.key` (`forge keys-export` mints + writes it; requires admin).

| Command | Effect |
|---|---|
| `forge tui` | Full-screen ops console: Overview / Slots / Services / Compressor / Keys / Smith chat |
| `forge status` | One-screen summary (slots, budget, GTT) |
| `forge models` | List catalog configs available to load |
| `forge load <mode> [slot]` | Load a config onto a slot |
| `forge unload <slot>` | Empty a slot |
| `forge services [start\|stop <name>]` | List or toggle infra services |

Environment: `FORGE_API_URL` overrides the default `http://127.0.0.1:5000`
(use the tailnet dashboard URL to administer from a laptop).

## SELinux & Permissions (ForgeHost)
- Use `uv` for all package management.
- After updates, apply `bin_t` context to scripts and venv binaries.
- Do NOT use `restorecon` on `/usr/local/lib/forge/`; it defaults to the wrong type (`lib_t`).
- Inference binaries in `/opt/forge/llama.cpp/build*/bin/` also need `bin_t` before systemd can execute them.
## Installer (`install.sh`) — dual targets

`install.sh` supports two targets via `--target=dev|public` (default **dev**):

- **dev** — reference-host (ForgeHost) oriented: Fedora-only gates, fixed
  `/opt/forge` paths, BIOS/firmware checklist, SELinux relabel notes,
  PairNode/ComfyUI probe, legacy uv strict-mode package checks.
- **public** — generic-host path: same unit installation, but
  reference-host assumptions are guarded/skipped. Requires the hardware/
  software pre-flight to pass unless `--force`. After units install it runs
  the KGC hardware matcher (`installer/kgc-match.py`; a match writes
  `/etc/sysconfig/forge-kgc` and opts the host out of runtime auto-tuning),
  offers to provision mandatory model assets
  (`installer/provision.py --mandatory`), and prints next-steps
  (mint key, dashboard, `forge tui`).

Pre-flight can be run standalone: `installer/preflight.sh [--json]`.
Asset manifest: `installer/assets.manifest.json`; provisioner usage and the
whole flow are documented in `installer/README.md`.
