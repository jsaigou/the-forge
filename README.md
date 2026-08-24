<div align="center">

<img src="docs/assets/hero.svg" width="480" alt="The Forge banner" />

# The Forge

**A self-hosted local-inference stack: one Go binary that routes, schedules
and operates LLMs on your own GPU — with an OpenAI-compatible API, an ops
dashboard and an agent control surface.**

[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)
![React](https://img.shields.io/badge/web-React%20%2B%20Vite-61DAFB?logo=react&logoColor=black)
![Platform](https://img.shields.io/badge/platform-Linux%20%2F%20ROCm-red)

</div>

---

## What it is

The Forge turns a single Linux machine with a big unified-memory GPU into a
multi-model inference appliance:

- **`forge` daemon** — a single Go binary serving three surfaces on canonical
  ports: the ops dashboard (`:5000`), an OpenAI-compatible router (`:8085`)
  and an MCP control surface (`:8095`) for agents.
- **On-demand model scheduling** — request a model that isn't loaded and the
  scheduler loads it into one of four llama.cpp slots, evicting by fit when
  memory runs out. Clients never manage slots.
- **Store-backed catalog** — models, configs, variants, benchmarks and
  offerings live in SQLite, editable live over the dashboard/API. No config
  files to drift.
- **Ops dashboard** — slot/bay state, GPU memory budget, actual-vs-configured
  context size, hang alerts, auth policy and API keys.
- **Agent control plane** — MCP tools for pre-warming models, fit checks and
  time-boxed reservations, plus per-key agent identity.

## Architecture

```
                ┌────────────────────────── forge daemon ──────────────────────────┐
 clients        │                                                                  │
 ────────►      │  :8085 router (OpenAI-compatible)   :5000 ops dashboard          │
 OpenAI SDKs    │           │                                │                     │
 agents ─────►  │  scheduler (load / evict / reserve)   catalog store (SQLite)     │
                │           │                                                      │
 MCP ─────────► │  :8095 MCP control surface                                auth    │
                └───────────┼──────────────────────────────────────────┬───────────┘
                            ▼                                          ▼
                 llama.cpp slots (a1..a4)                      systemd units, metrics
```

## Quick start

> Hardware note: The Forge targets Linux + AMD ROCm (tested on Strix Halo
> class APUs with unified memory). Builds are plain Go; other GPUs work
> wherever llama.cpp does.

```bash
git clone https://github.com/jsaigou/the-forge.git && cd the-forge
go build ./go/cmd/forge        # build the daemon
cd web && npm ci && npm run build && cd ..   # build the dashboard assets

sudo ./install.sh              # install units + polkit rules (see docs/deployment.md)
forge status                   # inspect bays, memory budget, queue
```

Then point any OpenAI-compatible client at `http://localhost:8085/v1`.

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
| [docs/design.md](docs/design.md) | system design overview |
| [docs/scheduler.md](docs/scheduler.md) | slot scheduling, reservations, MCP role |
| [docs/llm-router.md](docs/llm-router.md) | router design and failover |
| [docs/deployment.md](docs/deployment.md) | install, units, operations |
| [docs/modes.md](docs/modes.md) | model roster and capabilities |
| [docs/adding-a-model.md](docs/adding-a-model.md) | cataloging a new model |
| [docs/pitfalls.md](docs/pitfalls.md) | operational sharp edges |
| [docs/adr/](docs/adr/) | architecture decision records |

## Status & roadmap

This repository is in **early public release**: the source and installer are
published for review; binaries and model weights are **not** distributed — you
build from source against your own llama.cpp installation. The roadmap lives
in the issue tracker; expect churn in the scheduler and auth surfaces while
the v0.5 line stabilizes.

## License

Apache-2.0 — see [LICENSE](LICENSE).
