# a0 Router

A thin OpenAI-compatible routing/failover proxy (port 8085) that sits between an external AI
agent and every model backend - the Forge's own dynamic slots and any remote OpenAI-compatible
providers you've configured (DeepSeek, GLM, etc.). It owns 100% of the routing/failover decision
so a calling agent never has to reconcile multiple providers' credentials itself. The router
itself is consumer-agnostic; pointing a new agent/tool at it is a config-only change on the
consumer's side, never a router change.

Served by `forge-daemon.service` (the same Go binary as the dashboard and MCP surface) - no
separate process.

## Why This Exists

Two problems motivated building this rather than pointing agents directly at a single backend
with client-side fallback:

1. **A model-switch dead window.** Loading a different model onto a slot briefly takes the old
   server down before the new one is up. A request landing in that window gets
   connection-refused, and a naive client's fast-fail retry can trip an unrelated fallback path
   even though nothing is actually wrong.
2. **Credential/model mismatches under client-side fallback.** Letting each calling agent own
   its own multi-provider fallback logic means every agent has to get the "restore the right
   credential *and* the right model name together" invariant right independently - and it's easy
   to get subtly wrong in a way that sends the wrong model name to the wrong endpoint.

Rather than trust every calling agent to solve this correctly, the router absorbs all
multi-provider awareness itself, on infrastructure you control, and enforces one structural
invariant (below) that makes the second problem impossible by construction.

## Design Invariant

A request matches a **route** (keyed by logical model name) → an ordered list of **backend**
records → each backend bundles `(how to reach it, wire model name, credential)` as one atomic
unit. There is no code path that resolves a backend's address/credential independently of its
wire model name - a backend switch always carries its model name and credential with it.

## Configuration

Routing topology (backends, routes, provider credentials) is entirely store-backed - manage it
via Settings → Routing / Provider Keys in the dashboard, or the corresponding `/api/v1/*` APIs.
There is no file to hand-edit.

A locally-served model's route is resolved fresh per request against the live catalog rather
than a static route table: any visible catalog Config is reachable by name even if nothing is
currently loaded, and the scheduler places the on-demand load on whichever slot is free.

## Request Flow - `POST /v1/chat/completions`

1. Bearer-token auth (or the tailnet-conditional bypass, below). Fail → 401.
2. Validate the request body (passthrough of unrecognized fields, not a strict schema - the
   router forwards what it doesn't need to understand).
3. Match `model` against a configured route, or (if no static route matches) look it up as a
   catalog Config by name. A hidden Config, or no match at all, falls through to a
   `model_not_found` 404. A visible catalog match triggers an on-demand load via the scheduler
   (unpinned - the scheduler places it on whichever slot is free), and a load failure surfaces as
   a 502 with a `catalog_load_failed` code (the model is known, just unavailable right now).
4. Per backend in the resolved chain: skip it if currently gated as unhealthy/busy (see below);
   otherwise resolve the atomic `(address, credential, wire model name)` triple, rewrite the
   request's `model` field to the wire name, and forward the request with a bounded retry on
   connection error/timeout/5xx. On a 2xx or a definitive 4xx, return that response and stop -
   once bytes are already streaming to the client, failing over mid-stream isn't possible.
5. Chain exhausted → 502 `all_backends_unavailable`.
6. Every terminal outcome is audited - backend names and coarse status labels only, never
   request/response bodies, prompts, or credentials.

Every 502 in this flow carries a human-readable `message` field alongside its machine-readable
`error` code, so a consumer application has something reasonable to show a user rather than a
bare code string.

### Busy-Slot Behavior

A local slot's health endpoint is a liveness signal, not a readiness signal - it reports healthy
even mid-generation on another request. Two configurable behaviors (Settings → Routing):

- **`wait`** (default) - a busy-but-healthy slot is attempted anyway; the request queues at the
  inference server's own slot and the router waits. Unbounded by default, since a flat timeout
  applied uniformly regardless of a model's actual speed can sever a legitimately long-running
  generation mid-stream; still configurable per-deployment if a hard ceiling is wanted.
- **`fail_fast`** - a busy slot is treated as unavailable immediately. Without a fallback chain,
  the router returns an error immediately; with fallbacks configured, it skips to the next
  backend.

An **unhealthy** slot (mid model-switch, crashed, connection refused) is always skipped
immediately regardless of this setting - only the busy-but-alive case is gated by it.

## `/v1/models`

Merged and deduplicated by model name from two sources:

1. **Remote-provider offerings** you've configured - one entry per catalog model; when several
   providers offer the same model, only the selected primary is listed (see Multi-Provider
   Selection below).
2. **Local catalog Configs** with `visibility = "visible"` - listed by name regardless of
   whether anything is currently loaded, so a locally cataloged model is discoverable and
   requestable even before its first on-demand load.

## Multi-Provider Selection

The same model can be offered by more than one provider. Those are distinct offerings of one
catalog model - never silently interchangeable (cost, data residency, reliability can all
differ), so your preference between them is explicit:

- **Offering priority** - lowest value wins; a default value means "no preference." Managed in
  Settings → Catalog → Offerings.
- **Provider enabled/disabled** - disable a provider without deleting it; its offerings drop out
  of selection, `/v1/models`, and request routing, while its credentials and any linked
  configuration are preserved so re-enabling restores it exactly. Toggle in Settings → Provider
  Keys.
- **Primary** - a model group's enabled offering of an enabled provider with the lowest priority
  value. `/v1/models` lists exactly the primary, and routing resolves to it.
- **Aliases** - providers can name the same model differently; a request matching any offering's
  wire name resolves the whole group to the primary and rewrites the wire name accordingly, so
  what's listed and what's served never disagree.
- **Provider failover** (Settings → Routing, default off) - with it on, a failed primary
  (transport error/5xx) falls over to the next offering of the same model in priority order; with
  it off, the error surfaces instead of silently spending on the next provider. Definitive 4xx
  responses never fail over. Local-slot chains are unaffected either way.
- A model whose offerings are all disabled (or whose providers all are) is listed nowhere and
  answers a structured 502 rather than a silent 404.

## Static Passthrough Endpoints

A few endpoints are deliberately *not* routed/failed-over the way chat completions are, because
their backing service is a single, fixed, always-on process rather than something the scheduler
loads and unloads:

- **`POST /v1/embeddings`** - forwarded byte-for-byte to a fixed embedding-service URL. No
  failover, no health-gating, no on-demand load.
- **`POST /v1/audio/speech`, `GET /v1/voices`** - same shape, forwarded to a fixed
  text-to-speech-service URL. `/v1/voices` is exposed **GET only** through this path; anything
  mutating stays behind the TTS service's own internal auth.

Both use the same tailnet-conditional auth as the chat path, and both strip any inbound header
the backing service treats as an internal trust boundary before forwarding, so a client can never
smuggle a value for it.

## Tailnet-Conditional Auth

a0 skips its bearer-key check for requests sourced from your tailnet's private address range,
enforcing the key for everything else. See [docs/scheduler.md](scheduler.md)'s "a0's
Tailnet-Conditional Auth" section for the full rationale and how it handles both a direct
connection and one proxied through a TLS-terminating reverse proxy.

## Compression Passthrough

If you run a compression proxy in front of local slots and/or remote providers to reduce
prompt-cache misses and token spend, the router can be told - globally or per-service - to
bypass it and hit the real upstream directly, without tearing down or restarting the proxy. This
is useful both as an operator override and as an automatic fallback when a compression proxy
itself becomes unreachable, so a proxy outage degrades to "uncompressed but working" rather than
failing every request through it.
