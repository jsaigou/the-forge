<div align="center">

<img src="docs/assets/hero.svg" width="100%" alt="The Forge: a self-hosted, multi-model LLM inference daemon. One Go binary that schedules and routes local LLMs across your GPU's memory budget." />

[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)
![React](https://img.shields.io/badge/web-React%20%2B%20Vite-61DAFB?logo=react&logoColor=black)
![Platform](https://img.shields.io/badge/platform-Linux%20%2F%20ROCm-red)

</div>

**A self-hosted inference mesh built specifically for AMD's unified-memory APUs** — Ryzen AI
Max+ 395 ("Strix Halo") and the class of hardware it defines: one chip, no discrete GPU, and up
to 128 GB of RAM the CPU and GPU both draw from. The Forge turns that unusual memory model into
a real multi-model serving platform: several LLMs loaded and queued concurrently, a router that
fails over to cloud providers when it makes sense, and an in-process agent that watches the
hardware and fixes what it can.

---

## Why Strix Halo needs a different kind of inference server

Strix Halo doesn't behave like the machines most inference tooling was written for. There's no
discrete VRAM; the GPU draws from the same unified pool as the OS, through GTT (Graphics
Translation Table) allocation that most llama.cpp-based tools were never tuned against. Getting
it right in practice means:

- **ROCm vs. Vulkan is a real, per-model decision**, not a toggle you set once. Vulkan tops out
  around 63 GB; ROCm with `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` reaches the full ~120 GB GTT
  pool, but costs more to launch and needs its own memory accounting.
- **The kernel can silently reduce your context window** if it can't find a contiguous GTT
  allocation, and nothing in a stock llama-server deployment tells you that happened.
- **One box, several models you actually want loaded at once** (a coding model, a small router
  brain, an image pipeline) means real memory budgeting and eviction, not "restart the server to
  switch models."

The Forge exists because none of the mainstream self-hosting stacks (Ollama, plain llama.cpp,
text-generation-webui) treat any of that as a first-class problem. It does.

## What it actually does

| | |
|---|---|
| **On-demand multi-model scheduler** | Four inference slots share the unified memory pool. Models load on first request and evict on idle, with a real byte-level memory budget, not a fixed model-per-process assignment. |
| **`a0`, an OpenAI-compatible router** | One endpoint in front of everything: your local slots *and* remote providers, with per-provider failover, usage tracking, and real cost accounting, not an estimate. |
| **Smith, an in-process ops agent** | A set of deterministic health checks (GPU hangs, silent context reduction, drifted builds, orphaned services) run continuously. Smith doesn't just alert: it can propose a fix, walk you through the runbook, or execute the safe ones itself under an explicit autonomy policy. |
| **A real context-compression proxy** | `forge-compress`, a native Go/ONNX proxy, cuts prompt-cache misses and remote-API token spend for both local and cloud-routed traffic, with real, measured savings, not a marketing number. |
| **Live-editable model catalog** | Models, quantizations, pricing, and benchmarks live in a SQLite store, editable through the UI or a CRUD API. No config files to hand-edit and redeploy for. |
| **One binary** | Dashboard, router, MCP server, and scheduler are one Go process. No Python service sprawl, no `docker-compose` of eight containers to keep in sync. |

## How it works

<img src="docs/assets/architecture.svg" width="100%" alt="Architecture: OpenAI SDKs, agents and MCP clients call the forge daemon's router, dashboard and MCP surfaces, which share one scheduler over a SQLite catalog store and load models onto llama.cpp slots a1 through a4." />

**Reference deployment:** an AMD Ryzen AI Max+ 395-class APU (gfx1151) with 128 GB unified RAM
and roughly 120 GB addressable GTT, on Linux with SELinux enforcing. Any Strix Halo (or
comparable unified-memory AMD APU) box should work.

## Repository layout

```
go/
  cmd/forge/           — the single `forge` binary: daemon (dashboard :5000, a0 :8085,
                         MCP :8095) + client CLI/TUI (status/models/load/unload/tui)
  cmd/forge-compress/  — native context-compression proxy
  cmd/forge-tts/       — TTS launcher
  internal/engine/     — llama-server slot lifecycle (systemd + dbus)
  internal/sched/      — on-demand scheduler: placement, eviction, reservations
  internal/router/     — a0: routing, provider failover, streaming proxy
  internal/httpapi/    — dashboard REST API + SSE bus
  internal/smith/      — the ops agent: deterministic checks + LLM reasoning + procedures
  internal/store/      — SQLite catalog/settings/auth store + migrations
  internal/compress/   — context-compression engine (ONNX scorer)
web/                   — React PWA dashboard (Vite; built into the Go binary)
systemd/               — forge-daemon, forge-a1..a4 slots, always-on service units
polkit/                — passwordless unit-management grants
scripts/               — deploy tooling, install/upgrade script
docs/                  — architecture, runbooks, ADRs
```

## Configuration

Everything is **store-backed**: there are no TOML/YAML config files to hand-edit and redeploy.

- **Model catalog** (models, variants, configs, offerings, benchmarks) lives in the SQLite DB,
  edited live via Settings → Catalog or `POST/PUT /api/v1/catalog/*`.
- **Infra config** (ports, slots, scheduler, cost, router timeouts) lives in a settings KV,
  edited via `forge config get/set` or the Settings API.
- **Auth** (users, API keys, WebAuthn, recovery codes) lives in the auth store (Settings →
  Security).

### Adding a model

Models → **Add Model** searches Hugging Face directly, ranks a repo's real GGUF files against
the live memory budget, and runs pre-flight checks (disk headroom, required backend, an
existing-file conflict) before you commit. Download is real and resumable — pause/resume pick up
from the on-disk offset rather than restarting, and a sharded repo downloads as one job. Once
verified, the model registers itself into the catalog automatically (Model, Variant, Artifact,
Config — sensible defaults, `--parallel 1`, the right backend for its size); the new Config lands
`unverified`/`hidden` until you promote it. Gated repos need a token first (Settings → Security →
HuggingFace access token).

The whole flow is also exposed as smith tools (`hf_search`, `hf_preflight`, `download_status`,
and a `download_start` that only ever *proposes* a job — nothing downloads until you approve
it), so you can ask smith to find and queue a model instead of using the search UI yourself. See
[docs/adding-a-model.md](docs/adding-a-model.md) for the full walkthrough.

## Installation

> Hardware note: The Forge targets Linux + AMD ROCm (tested on Strix Halo class APUs with
> unified memory). Builds are plain Go; other GPUs work wherever llama.cpp does.

```bash
git clone https://github.com/jsaigou/the-forge.git && cd the-forge
go build ./go/cmd/forge        # build the daemon
cd web && npm ci && npm run build && cd ..   # build the dashboard assets

sudo ./install.sh                 # fresh install, strict dependency-age checks on
sudo ./install.sh --upgrade       # in-place upgrade (deploys current units + web assets)
sudo ./install.sh --dry-run       # show every platform/dependency check, make no changes
```

`install.sh` checks the platform (kernel, Strix Halo CPU/GPU, ROCm, Vulkan) and deploys
application files and systemd units. Nothing is written to a config file, since there isn't one.
See [docs/deployment.md](docs/deployment.md) for the full step list.

## Usage

```bash
forge status                     # slots, memory budget, GTT
forge models                     # list loadable catalog configs
forge load <config-name> [slot]  # default slot if omitted
forge unload a3
forge tui                        # full-screen ops console (Overview/Slots/Services/Compressor/Keys/Smith)
```

Models live in the scheduler's on-demand pool: the router auto-loads whatever's requested (the
first response pays the load cost), and idle slots evict when memory is needed. The dashboard
(port 5000) exposes the same actions plus the full catalog.

Then point any OpenAI-compatible client at `http://localhost:8085/v1`.

### Verify a config loaded correctly

```bash
curl -sf http://localhost:8080/props | jq .default_generation_settings.n_ctx
```

Always worth checking after a switch — see [docs/pitfalls.md](docs/pitfalls.md) for why this
isn't paranoia.

## Key operational facts

The full list lives in [docs/pitfalls.md](docs/pitfalls.md). The ones that bite hardest:

- **`GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON`** is required for models larger than ~63 GB on the ROCm
  backend. Without it, ROCm is capped at ~63 GB regardless of a larger GTT pool.
- **`--parallel` must always be explicit.** llama-server's default will OOM at large context, and
  setting it above 1 *splits* your configured context across workers rather than multiplying
  capacity — an easy, silent footgun.
- **`TimeoutStopSec` must be ≥ 300.** Large models (80 GB+) take minutes to unload; the 60s
  systemd default causes restart loops.
- **Always verify `n_ctx` after a config switch.** The GTT allocator can silently fall back to a
  smaller context, and the dashboard is the only place that reports the *actual* loaded value.

## Features

- OpenAI-compatible `/v1` chat/completions routing across up to four slots
- On-demand loading with blocking hold (~150 s) instead of hard failures
- Fit checks and read-only capacity planning (`can_fit`)
- Time-boxed reservations with per-agent attribution
- Store-backed model catalog with live CRUD APIs
- Tailscale-aware auth (conditional bypass, per-policy), API keys, TOTP/passkeys
- Always-on sidecars: embeddings, speech-to-text, text-to-speech

## Documentation

| Doc | Contents |
|---|---|
| [docs/scheduler.md](docs/scheduler.md) | slot scheduling, reservations, MCP role |
| [docs/llm-router.md](docs/llm-router.md) | router design and failover |
| [docs/deployment.md](docs/deployment.md) | install, units, operations |
| [docs/modes.md](docs/modes.md) | model configuration concepts |
| [docs/adding-a-model.md](docs/adding-a-model.md) | cataloging a new model |
| [docs/pitfalls.md](docs/pitfalls.md) | operational sharp edges |
| [docs/adr/](docs/adr/) | architecture decision records |

## Status & roadmap

This repository is in **early public release**: the source and installer are published for
review; binaries and model weights are **not** distributed — you build from source against your
own llama.cpp installation. The roadmap lives in the issue tracker; expect churn in the scheduler
and auth surfaces while the v0.5 line stabilizes.

## License

Apache-2.0 — see [LICENSE](LICENSE).
