# Scheduler

> **v0.5 is live.** This doc describes the V4 Python scheduler (`scheduler.py`). In v0.5, the
> scheduler is an in-process Go component inside `forge` (`internal/sched/`) — no
> `scheduler.py`, no `forge-router.service`, no `forge-mcp.service`. The MCP server is
> served by `forge-daemon.service` on :8095. The concepts, API surface, and behavior
> described below remain accurate; only the implementation language/process model changed.

The memory-management brain for The Forge's four inference bays (A1–A4) and ComfyUI: on-demand
loading, idle auto-unload, priority queue-jump for small jobs, and time-boxed reservations. Backs
both the dashboard (`app.py`, port 5000) and the A0 router (`router_app.py`, port 8085) from a
single shared module — one scheduler, many callers.

**Status (✅ built and live-verified 2026-07-21):** the scheduler (`scheduler.py`),
reservations, A0 wiring, and the MCP server (`mcp_server.py`, port 8095,
`forge-mcp.service`) are all implemented and verified on ForgeHost — see `progress.md`
(Phase 3 and Phase 6 entries) for the build/verification log. This document remains the
design reference.

---

## Where It Lives

`forge/scheduler.py` — a new shared module in `/opt/forge`, imported directly by **both**
`app.py` and `router_app.py`.

This is deliberately *not* a separate service with its own RPC surface. `router_app.py` already
does `import engine` and runs from the same codebase/venv as `app.py` — it's a second systemd
service (`forge-router.service`) sharing the same `/opt/forge` install, not a separate
deployment. `scheduler.py` follows that exact, already-established pattern: a plain Python import,
not a new network hop. Adding RPC (HTTP, a socket, whatever) between the dashboard and A0 just to
share scheduler state would introduce a new failure mode and a new latency source for zero benefit
— both processes can already read and write the same files on the same host.

## The Cross-Process Locking Problem This Surfaces

`engine.py`'s existing slot-state lock (`_state_lock = threading.Lock()`) is **in-process-only**.
That was never actually safe across two OS processes — a `threading.Lock` only excludes other
threads inside the same interpreter — but it didn't matter in practice before this work, because
only the dashboard process ever called `load_to_slot()`/`unload_slot()`. A0 only proxied requests
to already-loaded slots; it never mutated slot state itself.

The scheduler changes that: A0's request path now calls into the same slot-loading logic
(`ensure_loaded()`, eviction, placement) directly, in its own process. Two real OS processes can
now race to load/evict/unload against the same slot state at the same time — a `threading.Lock`
in one process is invisible to the other.

**Fix:** slot-state and new queue-state files move to the `filelock.FileLock` + in-process-lock
pairing `config_writer.py` already uses for `config.toml`/`secrets.toml` (per CLAUDE.md
convention: acquire the filelock first, both locks required, in that order). This is the same
correctness class of bug `config_writer.py` was already built to avoid for config mutation — it
just didn't apply to slot state until a second process started touching it.

## Consumer Model

Ordinary inference consumers (chat completions, embeddings, anything a calling agent sends)
**only ever talk to A0** — never directly to A1–A4's raw `llama-server` ports, and never to the
scheduler or any Forge-specific endpoint. From a consumer's point of view, "my model isn't
loaded right now" is invisible.

When A0 receives a request for a model that isn't currently loaded anywhere, it calls
`scheduler.ensure_loaded(model)`, which:

1. Checks whether the model already fits in a free slot.
2. If not, evicts an idle slot if one is eligible (see below) to make room.
3. Loads the model and blocks the caller's request — holding the consumer's HTTP connection open
   for **up to 150s** — until the load completes or times out.
4. On success, the Flask/gevent proxy call in `router_app.py` proceeds to the now-loaded slot. On
   timeout, it returns a clean error rather than hanging indefinitely.

MCP (below) is a separate, secondary path for agents that want to actively manage the fleet
(pre-warm a model, inspect queue state) — it is not part of this consumer path.

## Eviction Philosophy: On-Demand Only

Nothing is evicted, loaded, or unloaded except in direct response to an actual incoming request.
There are no proactive or background sweeps — no timer that periodically checks for idle slots
and unloads them, no pre-warming at a clock boundary. This holds even for reservations (below):
a reservation's window opening does not itself trigger a load; the load happens lazily on that
window's first real request, whenever that arrives.

**Idle-eviction eligibility:** a slot is only evictable once it has been idle for at least
`idle_unload_s` (default **180s**) *and* something currently needs the memory it holds. Idle time
alone is never sufficient — an idle-but-unneeded slot just stays loaded.

For ordinary on-demand traffic, eviction never touches a busy (non-idle) slot. (Reservations
introduce a second, stronger eviction tier that can — see below.)

### Fit gate accounting (2026-08-22 incident hardening)

`FitPlan`/`MemoryBudget` (`go/internal/engine/memory.go`) gate every load against three
independent checks, because on Strix Halo each memory probe lies differently (GTT misses
HMM/unified pages, fdinfo misses unmapped pages, RSS double-counts GTT-resident mappings):

1. **Additive budget** — `gtt_used + all-slot llama-server RSS` against GTT total. All backends'
   RSS counts since the 2026-08-22 crash (previously rocm slots only, which left vulkan builds'
   footprints invisible). Conservative by design: overlap double-counts rather than under-counts.
2. **Host-headroom cap** — FreeBytes never exceeds kernel-reported MemAvailable minus a 8 GiB
   reserve the host needs to stay responsive (the crash signature was swap-thrash into a KFD
   wedge, not a clean OOM kill).
3. **In-flight reservation** — a load that has started but not yet materialized reserves its full
   estimated footprint, so two back-to-back requests cannot both admit against memory only one
   will get (the exact 2026-08-22 race between sibling GGUF loads).

Every fit decision logs its inputs and verdict to the daemon journal (`fit <mode>: OK|REFUSED …`),
and same-weights sibling loads are gated at `Engine.Load` on proven headroom (ADR-0006 amendment).
A load that polls out its deadline against retryable refusals reports the last blocker in the
timeout error, which a0 surfaces as the 502 `detail`.

## Priority Queue-Jump

An agent attaches a size hint (input token count) when it submits a request. This lets small,
fast requests avoid getting stuck behind large, slow ones in the queue.

- **`small_job_token_threshold`** (default **1500** tokens): a request at or under this size may
  jump ahead of an already-queued job that is much larger. Motivating case: an 8k-token request
  shouldn't sit behind a 150k-token job that will occupy a slot for minutes — the two requests are
  orders of magnitude apart in expected runtime, and making the small one wait is a bad trade for
  everyone.
- This jump rule is **global** — it applies across the whole queue regardless of which agent
  submitted which request, not scoped per-agent.
- **Starvation guard — `priority_jump_cap`** (default **2**): a large job can be jumped by smaller
  jobs at most this many times before it is forced to run next, no matter what else arrives after
  it. This bounds worst-case wait time for large jobs to a small constant number of jumps, rather
  than letting a steady stream of small jobs starve them indefinitely.

## Reservations

`[[reservations]]` entries in `config.toml` reserve a resource for a time window, ahead of
ordinary on-demand traffic.

### Schema

| Field | Type | Meaning |
|---|---|---|
| `label` | string | Human-readable name for the reservation. |
| `model` | string | Model alias to have loaded during the window. |
| `start` | datetime | Window start. |
| `end` | datetime | Window end. |
| `scope` | string | `"bay"` (a specific slot, see `bay` field), `"whole_box"`, or `"comfyui"`. |
| `created_by` | string | `"human"`, or an agent identifier. |
| `allow_agent_reschedule` | bool | Whether an agent other than the creator may modify (move) this reservation. |
| `allow_agent_cancellation` | bool | Whether an agent other than the creator may cancel this reservation. |

A `scope = "bay"` reservation also carries a `bay` field naming the specific slot it reserves.

### Two Eviction Tiers

- **Normal (interactive/on-demand) eviction** — the rule described above: only idle-past-threshold
  slots, and only enough to satisfy the incoming request. Never touches a busy slot.
- **Reservation-triggered eviction** — when a reserved model/resource is *first requested during
  its own active window*, it may forcibly evict **anything**, including busy (non-idle) slots.
  This is still lazy/on-demand — nothing happens at the exact clock boundary the window opens;
  eviction only fires on that first in-window request. In practice this needs no separate
  pre-warming logic, because a batch job naturally sends its first call when it wakes up.

### Protection Rules

- **Soon-window pre-protection:** a slot whose loaded model has a reservation starting within
  `reservation_soon_min` (default **10 minutes**) is shielded from ordinary idle-eviction. This
  avoids a slot getting evicted and then immediately reloaded moments before its reserved window
  opens.
- **Full immunity once active:** once a reserved model/resource is loaded during its active
  window, that slot/resource is fully immune to any eviction — idle or forced — until the window
  ends, even if it sits idle for the entire window.

### Ownership and Edit Permissions

- `created_by` is `"human"` or an agent identifier.
- An agent may always edit or cancel its **own** reservations.
- Editing or cancelling **someone else's** reservation (human- or agent-created) requires the
  matching flag on that reservation: `allow_agent_reschedule` to modify, `allow_agent_cancellation`
  to delete.
- **Defaults:** human-created reservations are locked by default — both flags `false`. This
  reflects that a human setting one up via the UI implies deliberate lock-down intent.
  Agent-created reservations default both flags to `true` (open), since an agent didn't go through
  a UI flow implying the same intent. Human UI-created reservations always win on conflict against
  an agent request, regardless of the flags.

### ComfyUI as a Reservable Resource

ComfyUI (the `creative` service mode, `forge-comfyui.service`, port 3001) is used by agents too,
and is a first-class reservable resource via `scope = "comfyui"` — same eviction-tier and
protection rules as inference slots.

This was originally flagged as a gap in the Phase 2 memory-budget work — `memory_budget()`
appeared to have no visibility into ComfyUI's GPU footprint. **Verified empirically on ForgeHost
(2026-07-21).** `forge-comfyui.service` was started and `rocm-smi`'s GTT counter was watched
directly. `get_metrics()`'s `gtt_used_mb` is a **whole-GPU `rocm-smi` reading**, not scoped to
`llama-server` processes — confirmed it already exceeded tracked `model_weights_mb` (on-disk
weight file sizes for loaded slots) by ~18.8GB during a real ComfyUI generation, i.e. KV cache
overhead plus ComfyUI's own footprint. (Idle ComfyUI — before any workflow runs — uses a
negligible ~4MB, which is correct behavior for a lazy-loading system, not a measurement gap;
that's what made it *look* invisible originally.)

**Correction (2026-07-21, later same day):** the above was verified only with all-Vulkan
inference slots loaded, and that combination genuinely needed no fix — `gtt_used_mb` alone
already covers Vulkan/vLLM slots and ComfyUI together, since none of them are invisible to
rocm-smi. But `get_metrics()`'s `inference_rss_mb` (what `memory_budget()`/`can_fit()` actually
use) was computed as `max(gtt_used_mb, vmrss_used_mb)`, not their sum — and ROCm+unified-memory
slots (`nemotron`/`nemotron-puzzle`, `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON`) are invisible to
`gtt_used_mb` specifically (see `docs/pitfalls.md` "GTT counter blind spot"), so whenever one of
those was loaded — the common case, since ROCm is only used for the largest models — `max()`
picked `vmrss_used_mb` and **silently dropped `gtt_used_mb` entirely, ComfyUI's contribution
included.** Fixed: `inference_rss_mb` is now `gtt_used_mb` (covers everything else, ComfyUI
included, as verified above) **plus** the RSS of only the ROCm+unified-memory slots specifically
(`engine._get_unified_memory_rss_mb()`), added rather than compared — see `progress.md` for the
live-verification log and `scripts/test_comfyui_memory_accounting.py` for a deterministic
reproduction using the real nemotron RSS figures measured on 2026-07-16.

The remaining piece is purely a coordination feature, not a memory-tracking one:
`scope = "comfyui"` reservations exist so agents/humans know not to start a ComfyUI job during
someone else's reserved window. `can_fit()`'s eviction *candidates* remain scoped to the 4
inference slots only — ComfyUI was never an eviction target and doesn't need to become one.

## MCP's Role

MCP is a **secondary control/introspection/negotiation surface** for agents that want to actively
manage the fleet. It is explicitly **not** the mechanism by which ordinary inference traffic gets
a model loaded — that happens automatically at A0 (`ensure_loaded()`, above). An agent using only
chat completions through A0 never needs to touch MCP at all.

MCP is for agents that want to:

- Inspect fleet/queue status before deciding what to do next (what's loaded, what's busy, what
  would fit if requested, current queue depth and position — including other agents' queued work,
  since the priority-jump negotiation in the queue is visible here).
- **Trigger a load directly** (pre-warm a model ahead of sending real inference through A0) —
  this is not a read-only surface.
- Create, list, modify, and cancel reservations, subject to the ownership rules above.

### Tools (`go/internal/mcp`)

Implemented, not planned: the V4 Python server (`forge/mcp_server.py`) is retired; the live
surface is `go/internal/mcp`, served by `forge` on :8095. `GET /v1/tools` (no auth) returns
each tool's name, description, `mutating` flag, and input schema — that discovery listing is the
authoritative wire shape, so consult it rather than assuming one.

| Tool | Purpose |
|---|---|
| `status` | Fleet + queue state: what's loaded, what's busy, queue depth/positions. |
| `can_fit(model)` | Would `model` fit right now? Strictly a read-only fit check — takes only `model`, triggers no load or eviction. |
| `ensure_loaded(model)` | Trigger a load, blocking with a timeout — mirrors what A0 does internally for ordinary traffic, but callable directly. |
| `unload(model)` | Unload a model from its slot. |
| `list_reservations` | List reservations, optionally filtered. |
| `create_reservation(...)` | Create a reservation (schema above). |
| `update_reservation(...)` | Modify a reservation, subject to ownership rules. |
| `cancel_reservation(...)` | Cancel a reservation, subject to ownership rules. |
| `list_models` | Model inventory, read-only: visible local Configs + remote Offerings, sourced from the same catalog seam as a0's `/v1/models` so the two listings can't drift. |

`list_models` was deliberately absent from the original frozen v0.5 surface and was added per
`docs/v5-mcp-audit.md` roadmap R2; the other eight tools are the surface as originally frozen.

All tools are backed by the same in-process Go scheduler (`internal/sched`) that a0 and the
dashboard use — one scheduler, many callers, so an MCP-triggered load and an a0-triggered load
contend for slots through the exact same code path.

### Auth

Key-based, **human-minted** — there is no self-service registration endpoint. A human mints each
key on ForgeHost via `forge mint-key -kind mcp -name <agent>` (one key per (kind, name);
re-minting the same name rotates it, and the token prints once). Keys are
`sk-mcp-<keyid>-<secret>` — the same shape as the `sk-router-*` pattern (see
`docs/llm-router.md`). The key's *name* is the caller identity: it becomes `requested_by`
(loads/unloads) and `created_by` (reservations), and is never taken from the request body.
That identity is what makes ownership meaningful ("whose reservation is this," "which agent
is queued ahead of me") — MCP callers are always identified, never anonymous.

There is **no tailnet bypass** for MCP — deliberately unlike a0. Every `POST /v1/tools/<name>`
must carry a valid bearer key; auth is checked before tool resolution (an unauthenticated probe
gets 401, not 404), and missing auth fails closed. `GET /v1/tools` and `GET /healthz` stay
unauthenticated by design.

## A0's Tailnet-Conditional Auth

Bundled into this phase because it touches `router_app.py`: A0 skips its `sk-router-*` API-key
check for requests sourced from the Tailscale CGNAT range (`100.64.0.0/10`), detected via source
IP. The key is still enforced for any future non-tailnet ingress.

Rationale: any tailnet device can already bypass A0 entirely and hit A1–A4's raw `llama-server`
ports directly (no auth there) or the dashboard (full auth), so requiring a key at A0 specifically
for tailnet-origin traffic is friction without a real security benefit. A0 is tailnet-only today —
no other ingress exists — so in practice this is close to dropping the key requirement outright.
It's scoped defensively for the future instead: when A0 is later exposed via Tailscale Funnel or a
reverse proxy (explicitly planned, not yet built), API keys must be enforced for that non-tailnet
path. Building the conditional check now, rather than removing key enforcement entirely, costs the
same effort and means the auth boundary is already correct when that exposure happens.

### Two access paths (both work with no auth from the tailnet)

A0 is reachable via two paths, and the tailnet check handles both correctly:

1. **Direct HTTP** (`http://forge.example.ts.net:8085/v1` or `http://203.0.113.7:8085/v1`):
   `request.remote_addr` is the real tailnet IP of the caller. No proxy hop. This is the most
   reliable path — no `tailscale serve` dependency, no service-name resolution needed (node names
   resolve from any Tailscale client with MagicDNS).

2. **HTTPS via `tailscale serve`** (`https://a0.example.ts.net/v1`): `tailscale serve`
   terminates TLS and proxies to `localhost:8085`, so `request.remote_addr` is `127.0.0.1`. The
   real client IP is in `X-Forwarded-For`, which `tailscale serve` sets trustworthily (it's the
   proxy, not the client, that writes this header). `_is_tailnet_request()` reads XFF **only** when
   `remote_addr` is loopback — this prevents a direct non-tailnet connection from setting a spoofed
   `X-Forwarded-For: 100.64.x.x` header to bypass the check.

**Host resolution note:** `a0.example.ts.net` is a Tailscale *service* name (resolves to
a service IP like `203.0.113.8`) — a newer Tailscale feature that not all clients resolve
consistently. If the service name doesn't resolve from your host, use the direct HTTP path via the
node name (`forge.example.ts.net`) or the stable tailnet IP (`203.0.113.7`).

## Config: `[scheduler]`

New table in `/etc/forge/config.toml`, all four values tunable and exposed on a Settings section
of the PWA's Scheduling page (round-tripped through `config_writer.py`, `tomlkit` + filelock, per
CLAUDE.md convention).

| Key | Default | Meaning |
|---|---|---|
| `idle_unload_s` | `180` | Seconds a slot must sit idle before it becomes eligible for on-demand eviction. |
| `small_job_token_threshold` | `1500` | Input token count at or under which a request may queue-jump ahead of a much larger queued job. |
| `priority_jump_cap` | `2` | Max number of times a large job can be jumped by smaller jobs before it is forced to run next regardless. |
| `reservation_soon_min` | `10` | Minutes before a reservation's start within which its slot is pre-protected from ordinary idle-eviction. |

## Related

- `docs/llm-router.md` — A0 router this module is wired into (request flow, auth, config schema).
- `docs/design.md` §16 "Open Work" — priority ordering for this and other post-V4 work.
- `CLAUDE.md` — "Critical Hardware / Runtime Facts" for the GTT/unified-memory constraints
  `can_fit()`/`place_model()` must respect regardless of scheduler logic sitting on top.
