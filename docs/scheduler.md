# Scheduler

The memory-management brain for The Forge's four inference bays (A1-A4) and any co-resident
service like ComfyUI: on-demand loading, idle auto-unload, priority queue-jump for small jobs,
and time-boxed reservations. Both the ops dashboard and the a0 router call into the same
in-process Go scheduler (`internal/sched`) - one scheduler, many callers, so a dashboard-
triggered load and an a0-triggered load contend for slots through the exact same code path.

## Consumer Model

Ordinary inference consumers (chat completions, embeddings, anything a calling agent sends)
only ever talk to a0 - never directly to A1-A4's raw inference-server ports, and never to the
scheduler directly. When a0 receives a request for a model that isn't currently loaded anywhere,
it calls into the scheduler's `EnsureLoaded`, which:

1. Checks whether the model already fits in a free slot.
2. If not, evicts an idle slot if one is eligible (see below) to make room.
3. Loads the model and blocks the caller's request - holding the consumer's HTTP connection open
   for up to a configurable timeout (default **320s**) - until the load completes or times out.
4. On success, the request proceeds to the now-loaded slot. On timeout, a 502 is returned with a
   human-readable `message` field (and a structured `reason` code when the timeout was a
   placement refusal) rather than hanging indefinitely.

`GET /v1/load-status?model=<name>` answers "what is `<name>` doing right now" without blocking
or mutating scheduler state, so a second connection can poll load progress while another request
against the same model sits blocked inside `EnsureLoaded`.

MCP (below) is a separate, secondary path for agents that want to actively manage the fleet
(pre-warm a model, inspect queue state) - it is not part of this consumer path.

## Eviction Philosophy: On-Demand Only

Nothing is evicted, loaded, or unloaded except in direct response to an actual incoming request.
There are no proactive or background sweeps - no timer that periodically checks for idle slots
and unloads them, no pre-warming at a clock boundary. This holds even for reservations (below):
a reservation's window opening does not itself trigger a load; the load happens lazily on that
window's first real request, whenever that arrives.

**Idle-eviction eligibility:** a slot is only evictable once it has been idle for at least
`idle_unload_s` (default **180s**) *and* something currently needs the memory it holds. Idle time
alone is never sufficient - an idle-but-unneeded slot just stays loaded.

For ordinary on-demand traffic, eviction never touches a busy (non-idle) slot. (Reservations
introduce a second, stronger eviction tier that can - see below.)

### Fit gate accounting

Every load is gated against several independent checks, because on a unified-memory APU host
each individual memory probe can be misleading in a different way (a raw GTT counter can miss
HMM/unified-memory pages, process RSS can double-count GTT-resident mappings, and so on):

1. **Additive budget** - GTT usage plus the RSS of every loaded slot's inference process,
   across every backend, checked against total GTT. Conservative by design: overlap
   double-counts rather than under-counts.
2. **Host-headroom cap** - free memory never exceeds the kernel's own reported available memory
   minus a fixed reserve the host needs to stay responsive.
3. **In-flight reservation** - a load that has started but not yet materialized reserves its
   full estimated footprint, so two back-to-back requests can't both be admitted against memory
   only one of them will actually get.

Every fit decision is logged with its inputs and verdict. A load that times out against
retryable refusals reports the last blocker in the timeout error, which a0 surfaces in its 502
response.

## Priority Queue-Jump

An agent attaches a size hint (input token count) when it submits a request. This lets small,
fast requests avoid getting stuck behind large, slow ones in the queue.

- **`small_job_token_threshold`** (default **1500** tokens): a request at or under this size may
  jump ahead of an already-queued job that is much larger. Motivating case: a small request
  shouldn't sit behind a huge job that will occupy a slot for minutes - the two are orders of
  magnitude apart in expected runtime, and making the small one wait is a bad trade for everyone.
- This jump rule is **global** - it applies across the whole queue regardless of which agent
  submitted which request, not scoped per-agent.
- **Starvation guard - `priority_jump_cap`** (default **2**): a large job can be jumped by smaller
  jobs at most this many times before it is forced to run next, no matter what else arrives after
  it. This bounds worst-case wait time for large jobs to a small constant number of jumps, rather
  than letting a steady stream of small jobs starve them indefinitely.

## Reservations

Reservations let you reserve a resource (a specific slot, the whole box, or a co-resident
service like ComfyUI) for a time window, ahead of ordinary on-demand traffic - managed via the
dashboard's Scheduling page or the scheduler API/MCP tools.

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

- **Normal (interactive/on-demand) eviction** - the rule described above: only idle-past-threshold
  slots, and only enough to satisfy the incoming request. Never touches a busy slot.
- **Reservation-triggered eviction** - when a reserved model/resource is *first requested during
  its own active window*, it may forcibly evict **anything**, including busy (non-idle) slots.
  This is still lazy/on-demand - nothing happens at the exact clock boundary the window opens;
  eviction only fires on that first in-window request.

### Protection Rules

- **Soon-window pre-protection:** a slot whose loaded model has a reservation starting within
  `reservation_soon_min` (default **10 minutes**) is shielded from ordinary idle-eviction. This
  avoids a slot getting evicted and then immediately reloaded moments before its reserved window
  opens.
- **Full immunity once active:** once a reserved model/resource is loaded during its active
  window, that slot/resource is fully immune to any eviction - idle or forced - until the window
  ends, even if it sits idle for the entire window.

### Ownership and Edit Permissions

- `created_by` is `"human"` or an agent identifier.
- An agent may always edit or cancel its **own** reservations.
- Editing or cancelling **someone else's** reservation (human- or agent-created) requires the
  matching flag on that reservation: `allow_agent_reschedule` to modify, `allow_agent_cancellation`
  to delete.
- **Defaults:** human-created reservations are locked by default - both flags `false`. This
  reflects that a human setting one up via the UI implies deliberate lock-down intent.
  Agent-created reservations default both flags to `true` (open), since an agent didn't go through
  a UI flow implying the same intent. Human UI-created reservations always win on conflict against
  an agent request, regardless of the flags.

### A co-resident service as a reservable resource

A GPU-using service that isn't itself an inference slot (e.g. an image/video generation
service) can be a first-class reservable resource via `scope = "comfyui"` (or an equivalent
scope for your own such service) - same eviction-tier and protection rules as inference slots.
The scheduler's eviction *candidates* stay scoped to the inference slots only; a co-resident
service is a coordination feature (so agents/humans know not to start a job during someone
else's reserved window), not an eviction target itself.

## MCP's Role

MCP is a **secondary control/introspection/negotiation surface** for agents that want to actively
manage the fleet. It is explicitly **not** the mechanism by which ordinary inference traffic gets
a model loaded - that happens automatically at a0 (`EnsureLoaded`, above). An agent using only
chat completions through a0 never needs to touch MCP at all.

MCP is for agents that want to:

- Inspect fleet/queue status before deciding what to do next (what's loaded, what's busy, what
  would fit if requested, current queue depth and position - including other agents' queued work,
  since the priority-jump negotiation in the queue is visible here).
- **Trigger a load directly** (pre-warm a model ahead of sending real inference through a0):
  this is not a read-only surface.
- Create, list, modify, and cancel reservations, subject to the ownership rules above.

### Tools

`GET /v1/tools` (no auth) returns each tool's name, description, `mutating` flag, and input
schema - that discovery listing is the authoritative wire shape, so consult it rather than
assuming one.

| Tool | Purpose |
|---|---|
| `status` | Fleet + queue state: what's loaded, what's busy, queue depth/positions. |
| `can_fit(model)` | Would `model` fit right now? Strictly a read-only fit check - takes only `model`, triggers no load or eviction. |
| `ensure_loaded(model)` | Trigger a load, blocking with a timeout - mirrors what a0 does internally for ordinary traffic, but callable directly. |
| `unload(model)` | Unload a model from its slot. |
| `list_reservations` | List reservations, optionally filtered. |
| `create_reservation(...)` | Create a reservation (schema above). |
| `update_reservation(...)` | Modify a reservation, subject to ownership rules. |
| `cancel_reservation(...)` | Cancel a reservation, subject to ownership rules. |
| `list_models` | Model inventory, read-only: visible local Configs + remote Offerings, sourced from the same catalog seam as a0's `/v1/models` so the two listings can't drift. |

All tools are backed by the same in-process scheduler that a0 and the dashboard use.

### Auth

Key-based, **human-minted** - there is no self-service registration endpoint. A human mints each
key via `forge mint-key -kind mcp -name <agent>` (one key per (kind, name); re-minting the same
name rotates it, and the token prints once). The key's *name* is the caller identity: it becomes
`requested_by` (loads/unloads) and `created_by` (reservations), and is never taken from the
request body. That identity is what makes ownership meaningful ("whose reservation is this,"
"which agent is queued ahead of me") - MCP callers are always identified, never anonymous.

There is **no tailnet bypass** for MCP, deliberately unlike a0 (see below). Every tool call must
carry a valid bearer key; auth is checked before tool resolution, and missing auth fails closed.
`GET /v1/tools` and `GET /healthz` stay unauthenticated by design.

## a0's Tailnet-Conditional Auth

a0 skips its bearer-key check for requests sourced from your tailnet's private address range,
detected via source IP. The key is still enforced for any non-tailnet ingress.

Rationale: on a typical single-host deployment, any device already on your private network can
reach the raw inference-slot ports directly (no auth there) or the dashboard (full auth), so
requiring a key at a0 specifically for same-network traffic is friction without a real security
benefit. This is scoped defensively for the future: once a0 is exposed beyond your private
network (a reverse proxy, a tunnel, etc.), API keys are enforced for that path.

This handles both common access patterns correctly: a direct connection where the real client IP
is visible on the socket, and a connection proxied through a TLS-terminating reverse proxy where
the real client IP arrives via a forwarded-for header. The forwarded header is only trusted when
the immediate connection is from loopback - this prevents a direct, non-proxied connection from
spoofing that header to bypass the check.

## Config

Scheduler settings are tunable live via the dashboard's Scheduling settings page (or
`PUT /api/v1/scheduler/config`) - no restart required.

| Key | Default | Meaning |
|---|---|---|
| `idle_unload_s` | `180` | Seconds a slot must sit idle before it becomes eligible for on-demand eviction. |
| `small_job_token_threshold` | `1500` | Input token count at or under which a request may queue-jump ahead of a much larger queued job. |
| `priority_jump_cap` | `2` | Max number of times a large job can be jumped by smaller jobs before it is forced to run next regardless. |
| `reservation_soon_min` | `10` | Minutes before a reservation's start within which its slot is pre-protected from ordinary idle-eviction. |

## Related

- [docs/llm-router.md](llm-router.md) - a0 router this module is wired into (request flow, auth, config).
- [docs/pitfalls.md](pitfalls.md) - GTT/unified-memory constraints the fit check has to respect
  regardless of scheduler logic sitting on top.
