<div align="center">

<img src="docs/assets/hero.svg" width="100%" alt="The Forge: a self-hosted, multi-model LLM inference daemon. One Go binary that schedules and routes local LLMs across your GPU's memory budget." />

[![License](https://img.shields.io/badge/License-Apache--2.0-blue.svg)](LICENSE)
![Go](https://img.shields.io/badge/Go-1.22%2B-00ADD8?logo=go&logoColor=white)
![React](https://img.shields.io/badge/web-React%20%2B%20Vite-61DAFB?logo=react&logoColor=black)
![Platform](https://img.shields.io/badge/platform-Linux%20%2F%20ROCm-red)

</div>

---

## How it works

<img src="docs/assets/architecture.svg" width="100%" alt="Architecture: OpenAI SDKs, agents and MCP clients call the forge daemon's router, dashboard and MCP surfaces, which share one scheduler over a SQLite catalog store and load models onto llama.cpp slots a1 through a4." />

The Forge is one Go binary, not a pile of services to keep in sync:

- **On-demand scheduling** — a request for a model that isn't loaded triggers a
  load into one of four llama.cpp slots; the scheduler evicts by memory fit
  when it doesn't have room, so clients never manage slots by hand.
- **Store-backed catalog** — models, configs, variants, benchmarks and
  offerings live in SQLite, editable live over the dashboard or API. No config
  files to hand-edit and redeploy for.
- **Three surfaces, one process** — an OpenAI-compatible router (`:8085`), an
  ops dashboard (`:5000`) for slot/GPU/auth state, and an MCP control surface
  (`:8095`) so agents can pre-warm models, check fit, or hold a time-boxed
  reservation.

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
| [docs/scheduler.md](docs/scheduler.md) | slot scheduling, reservations, MCP role |
| [docs/llm-router.md](docs/llm-router.md) | router design and failover |
| [docs/deployment.md](docs/deployment.md) | install, units, operations |
| [docs/modes.md](docs/modes.md) | model configuration concepts |
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
