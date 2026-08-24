# a0 Router

A thin OpenAI-compatible routing/failover proxy running on ForgeHost (port 8085, `svc:a0` →
`a0.example.ts.net`) that sits between an external AI agent and every model
backend — Forge's own dynamic slots (a1/a2) and remote OpenAI-compatible providers (DeepSeek,
later possibly GLM/Gemini). It owns 100% of the routing/failover decision so the calling agent
never has to reconcile multiple providers' credentials itself. Originally built for Hermes
(Nous Research's `hermes-agent`) — Hermes is being phased out project-wide in favor of OpenCode,
which is now the primary consumer (see "Hermes Cutover" below). The router itself is
consumer-agnostic; swapping the calling agent is a config-only change on the consumer's side,
not a router change.

**Status:** ✅ Live in v0.5. The a0 router is served by `forge-daemon.service` (Go binary,
port 8085) — no separate `forge-router.service` or `router_app.py`. Hermes was wired
(2026-07-10) and is now being phased out in favor of OpenCode (both route through a0
as of 2026-07-21). Consumer-facing API shapes are unchanged from V4.

**⚠️ STALE — much of the detail below describes the retired V4 Python mechanism, not
current v0.5 code (flagged 2026-07-29, Headroom Phase 2 wrap-up).** `router.py`,
`engine.py`, `config_writer.py`, `monitor.py`, `auth.py`, and `/etc/forge/config.toml`/
`secrets.toml` are all deleted (TOML decommission, 2026-07-28 — see CLAUDE.md). In
particular: **`forge_slot_proxied`, `a1-via-headroom`/`a2-via-headroom`, and the
Route-Label-Sync mechanism below are 100% dead code in the Go router** — confirmed
nothing ever constructs a `forge_slot_proxied` Backend (ADR-0007 replaced address-pinned
local routing with `catalogChain`, resolved fresh per request against the store catalog).
Headroom fronting for local slots now works completely differently: one shared
`headroom@local` proxy fronts all of A1–A4, with the real slot address sent per-request via
an `x-headroom-base-url` header — see **`docs/v5-headroom-topology.md`** (the current
source of truth) for the full design and `internal/router/routing.go`'s `resolveBackend`
for the code. The **Headroom Passthrough** section below is closer to current but its
Python function names (`_headroom_bypassed`, `auth.get_headroom_proxy_targets`, etc.) are
V4; the Go equivalents are `headroomBypassed`/`localHeadroomBaseURL`/`remoteHeadroomBaseURL`
in `routing.go`. All three real Headroom proxies (`local`/`deepseek`/`aiand`) now run as
`headroom@<service>` systemd template instances (`internal/headroom.Provisioner`), not the
hand-created `headroom-a1`/`headroom-a2`/`headroom-deepseek`/`headroom-external` units this
doc names below — those four legacy units are fully deleted from ForgeHost as of Phase 2.

---

## Why This Exists

Two real bugs surfaced from Hermes (Nous Research's `hermes-agent`, external, not this repo)
talking directly to `a1` with `deepseek` as its own client-side fallback:

1. **Forge mode-switch dead window** — a slot load (`forge load <mode> [slot]`) stops the old
   model and starts the
   new one, taking ~10-25s during which port 8080 is down. A request landing in that window
   gets connection-refused/502, and Hermes' client-side retry gives up fast and fails over to
   deepseek even though nothing is actually wrong with Forge.
2. **Hermes credential-pool bug** — `restore_primary_runtime()` in hermes-agent re-selects a
   credential from a shared pool with no provider check, so after any fallback it can silently
   swap in DeepSeek's real `base_url`/`api_key` while leaving `model="ornith-35b"` attached —
   sending the wrong model name to the wrong endpoint. Reported upstream as
   [NousResearch/hermes-agent#56374](https://github.com/NousResearch/hermes-agent/issues/56374).
   Not fixable via Hermes config, and patching hermes-agent's vendored source wasn't viable
   given how fast that project's commits move.

Rather than patch a fast-moving upstream dependency, the router absorbs all multi-provider
awareness itself, on infrastructure we control.

## Security Rationale

Evaluated adopting an existing gateway (LiteLLM, Bifrost, Portkey, TensorZero) before building
custom. Ruled out: LiteLLM had a real March-2026 PyPI supply-chain compromise (backdoored
versions live ~40 min after a CI credential breach) plus a CVSS 9.3 SQL injection in its
DB-backed key-verification path; TensorZero is archived/unmaintained; Portkey was just acquired
by Palo Alto Networks with unclear self-hosting direction. Bifrost (Go, compiled binary, no
known CVEs) was the best third-party option, but for a 2-provider routing need, a custom proxy
has a smaller attack surface than any of them: **zero new third-party dependencies** (Flask,
gunicorn, gevent, `requests`, `tomlkit`, `argon2-cffi` are already vetted and deployed in this
repo's venv) and **no internal database** (the LiteLLM SQLi's entire bug class doesn't exist
here — topology lives in `config.toml`, credentials in `secrets.toml`, nothing else).

## Design Invariant

A request matches a **route** (keyed by logical model name) → an ordered list of **backend**
names → each backend record bundles `(how to reach it, wire model name, credential)` as one
atomic unit. There is no code path that resolves a backend's `base_url`/credential
independently of its `wire_model` — this is the structural fix for bug #2 above.

## Files

| File | Role |
|---|---|
| `forge/router_app.py` | Flask app + gunicorn entrypoint (`router_app:app`). Routes: `POST /v1/chat/completions`, `GET /v1/models`, `GET /healthz`. Bearer `sk-router-*` auth, no sessions/CSRF. |
| `forge/router.py` | Routing/failover engine — route lookup, health/busy gating, atomic backend resolution, bounded retry, streaming passthrough. |
| `forge/router_catalog.py` | Live `/health`+`/props`+`/metrics` probing (TTL-cached) for Forge-local slots; static entries for remote providers. |
| `forge/gunicorn-router.conf.py` | Single gevent worker, same reasoning as the dashboard's `gunicorn.conf.py`. |
| `systemd/forge-router.service` | Clone of `forge-dashboard.service`'s pattern. |

Extended (not new modules): `forge/auth.py` (router credential storage — see below),
`forge/validators.py` (`ChatCompletionRequest`, `RouterSettingsRequest`), `forge/app.py`
(dashboard-side `GET/PUT /api/v1/router/settings`), `forge/templates/settings.html`
(Services tab — busy-mode toggle).

## Config Schema (`/etc/forge/config.toml`)

```toml
[router]
listen_port             = 8085
connect_timeout_s       = 5
request_timeout_s       = 0     # unbounded by default since 2026-07-30 — see Busy-Slot Behavior below
health_ttl_s            = 4
max_retries_per_backend = 1
ensure_loaded_timeout_s = 320
busy_mode                = "wait"   # "wait" | "fail_fast" — see Busy-Slot Behavior below
headroom_passthrough_all      = false  # Phase 8: global bypass — see "Headroom Passthrough" below
headroom_passthrough_services = []     # e.g. ["external"] — per-proxy bypass, service names

[[router.backends]]
name = "a1"
kind = "forge_slot"       # dynamic: health/busy-probed, wire_model read live from /props
port = 8080

[[router.backends]]
name       = "deepseek-v4-pro"   # static remote provider, routed through headroom-deepseek
kind       = "remote"
base_url   = "http://localhost:8790/v1"
wire_model = "deepseek-chat"
credential = "deepseek"          # -> secrets.toml [[router_providers]] name="deepseek"

[[router.routes]]
model    = "gemma4-26b-mtp"     # auto-synced to real model alias on mode switch
primary  = "a1-via-headroom"    # forge_slot_proxied — probed via raw a1, routed via headroom-a1

[[router.routes]]
model    = "deepseek-v4-pro"
primary  = "deepseek-v4-pro"
```

Current live config has no `fallback` chains on any route — each model resolves to exactly
one backend. Adding GLM/Gemini later is one new `[[router.backends]]` (kind="remote") + one
`[[router_providers]]` secret + one `[[router.routes]]` — no new code.

## `forge_slot_proxied` — Headroom-Fronted Slots

**Status (2026-07-10): live in production.** Both `a1` and `a2` use this backend kind,
routing through `headroom-a1` (port 8788) and `headroom-a2` (port 8789) respectively.
DeepSeek models use `remote` backends pointed at `headroom-deepseek` (port 8790).

A second `forge_slot`-like kind for a1/a2 backends fronted by **Headroom**, a
context-compression proxy that sits in front of a slot's raw `llama-server` and compresses what's
sent within the context window (it does not change `n_ctx` itself). Each backend of this kind
carries two ports instead of one:

```toml
[[router.backends]]
name        = "a1-via-headroom"    # pseudo-slot name, stable across mode switches
kind        = "forge_slot_proxied"
probe_port  = 8080                 # raw llama-server slot — health/busy gate + wire_model/n_ctx probe
route_port  = 8788                 # headroom-a1's port — where the actual request is sent
credential  = ""                   # optional; loopback calls to Headroom don't require one today
```

Key points:

- **Health/busy gating and `wire_model`/`context_length` probing always target `probe_port`** (the
  raw llama-server slot), never `route_port`. Headroom's own `/health`/`/metrics` report Headroom's
  state, not the underlying llama-server's, so probing the raw port keeps this gate exactly as
  accurate as plain `forge_slot`.
- **The actual chat request is sent to `route_port`** — `_resolve_backend` builds
  `base_url=http://127.0.0.1:{route_port}/v1` after confirming `probe_port` is healthy.
- `credential` is optional here: a0 and Headroom both run on ForgeHost, so this is a loopback call and
  Headroom's inbound-auth check exempts loopback callers entirely. The field exists for
  defense-in-depth if Headroom is ever bound to a non-loopback interface.
- `/v1/models` reports `context_length` from `probe_port`'s `/props`, same reasoning as above.

### Route-Label Sync on Mode Switch

Because a `forge_slot_proxied` backend's `name` (e.g. `a1-via-headroom`) is a fixed pseudo-slot
name rather than the model alias, `engine.py`'s `load_to_slot()` calls a new
`_sync_router_route(slot, alias)` helper after every successful load, which calls
`config_writer.set_router_route_model(primary_backend, model)` to update the matching
`[[router.routes]]` entry's `model` field to the alias that was just loaded — otherwise
LibreChat's model picker would keep showing whatever model was loaded last. This is best-effort:
a0 resolves the real `wire_model` live per-request regardless of this label, so a sync failure
never blocks a mode switch — it only means the picker shows a stale name until the next
successful sync. `config_writer.remove_router_route(model_name)` removes a route entry by its
`model` key (companion function, same module).

## Headroom Passthrough (Phase 8)

`[router].headroom_passthrough_all` (global) and `.headroom_passthrough_services` (per-service,
e.g. `["external"]`) let a consumer bypass Headroom compression entirely and hit the real upstream
directly — flips live, no proxy teardown or restart. Both are consulted per-request inside
`_resolve_backend()`, gated by `_headroom_bypassed(service, router_cfg)`:

- **`forge_slot_proxied`** backends: bypassed ⇒ `base_url` becomes `http://127.0.0.1:{probe_port}/v1`
  (the raw llama-server slot) instead of `route_port` (the Headroom proxy). The service name for
  a given `route_port` is resolved via `auth.get_headroom_proxy_targets()` (port → service reverse
  lookup) — no new field needed on `[[router.backends]]`.
- **`remote`** backends (DeepSeek etc.): bypassed ⇒ `base_url` becomes the linked provider's real
  upstream, read via `auth.get_provider_target_url(credential)` — the `router_providers` entry's
  `target_url` field, which is kept synced to the real upstream by
  `api_headroom_provider_key_put()` whenever a `headroom_proxy` is linked.

**Do not repurpose `get_provider_credential()`'s `base_url_override` for this.** It reads a
`base_url` key that no writer ever sets (a real, currently-dormant naming mismatch against
`set_router_provider()`, which only writes `target_url`) — "fixing" that key read would make every
Headroom-linked remote backend bypass compression unconditionally, since `target_url` already
holds the real upstream for any linked provider. Passthrough resolution is deliberately separate
and gated by the config flag; see `get_provider_credential()`'s docstring in `auth.py`.

`auth.get_headroom_proxy_targets()` is a **cheap** variant of `get_headroom_proxies()` — static
config only (defaults + `secrets.toml`), no `systemctl`/unit-file reads — the only one safe to
call from this per-request hot path or `monitor.py`'s poll cycle.

Managed via `PUT /api/v1/headroom/passthrough` (operator role) and surfaced on
`GET /api/v1/headroom/config` (`passthrough_all` + a computed `passthrough` bool per proxy).

### Savings data

Each Headroom proxy exposes durable, restart-surviving Prometheus counters at its own `/metrics`
(distinct from the raw slot's `/metrics` — see the `forge_slot_proxied` section above):
`headroom_persistent_savings_input_tokens_total`, `headroom_persistent_savings_tokens_saved_total`,
plus `_requests_total`/`_input_cost_usd_total`/`_compression_savings_usd_total`. `monitor.py`'s
`_record_headroom_savings()` scrapes these once per existing 4s poll cycle (mirroring
`_record_token_samples()`'s exact delta/reset-detection pattern) and persists deltas via
`usage.record_headroom_savings()`, surfaced as a per-proxy `"headroom"` breakdown in
`GET /api/v1/usage` alongside the existing `models`/`external` breakdowns.

These are Headroom's own self-reported figures, not an independently-verified measurement —
`vault/Infra/headroom-evaluation.md` documents a known inaccuracy in Headroom's *per-request*
`EstimatingTokenCounter` for CJK-heavy content (it disagrees materially with the real tokenizer).
The *durable* counters used here are a coarser, proxy-lifetime accounting rather than a per-request
estimate, and are the right building block for a "tokens saved" stat regardless — but the
dashboard labels this data "Headroom-reported," not implying independent verification.

## Secrets Schema (`/etc/forge/secrets.toml`)

```toml
[[router_providers]]        # outbound — raw retrievable value, like hf_token
name     = "deepseek"
api_key  = "sk-..."
base_url = ""                # "" = use the backend's own base_url in config.toml

[[router_keys]]             # inbound — argon2-hashed, mirrors [[api_keys]]
keyid = "2962d8558e91"      # routes verify_router_key() to this entry — 1 Argon2 verify/request
hash = "$argon2id$..."
name = "hermes"
role = "api"
```

Mint an inbound token with `auth.mint_router_key(name)` — returns
`sk-router-<keyid>-<secret>` once; only the Argon2 hash + keyid persist. **2026-07: token
format changed to embed a keyid** so `verify_router_key()` routes straight to one entry instead
of Argon2-scanning every stored key on every request (also now rate-limited per client IP,
10 failed attempts/60s). No back-compat for pre-keyid tokens — the `hermes` key above had to be
re-minted when this shipped (see `multiuser-qa-remediation-plan.md` and `SECRETS.local.md`,
gitignored, for the current plaintext token).

Add/replace an outbound credential with `auth.add_router_provider(name, api_key, base_url="")`.
Never hand-edit `secrets.toml` directly. `auth._write_secrets_doc()` previously rebuilt the
whole file from a fixed section whitelist on every write, silently dropping any section added
outside that whitelist on the next unrelated write (this happened once during development —
see `progress.md` 2026-07-02). **Fixed 2026-07:** it now preserves any top-level key/section it
doesn't otherwise recognise, serialising list-of-dict values as arrays-of-tables and everything
else as scalars — hand-added sections survive subsequent writes.

## Request Flow — `POST /v1/chat/completions`

1. `Bearer sk-router-<keyid>-<secret>` → `auth.verify_router_key()` (keyid-routed, single Argon2
   verify, rate-limited per client IP). Fail → 401.
2. Validate via `ChatCompletionRequest` (`extra="allow"` — passthrough, not translation).
3. Match `model` against `[[router.routes]]` → build chain `[primary, *fallback]` (where
   `fallback` is optional and currently unused in the live config — each route has exactly one
   backend). **No static route matches?** (v0.5 only, "a0 local-config visibility" fix — see
   `catalogChain` in `go/internal/router/routing.go`): look up `model` as a catalog `Config`
   name via `store.Catalog.ConfigByName`. A hidden Config, or no match, falls through to the
   existing `model_not_found` 404. A visible match calls
   `scheduler.EnsureLoaded(Model: model, TargetSlot: "")` — deliberately *unpinned*, so the
   scheduler places the load on whichever configured slot is free, including `a3`/`a4`, which
   have no `[[router.backends]]` entry at all. The slot the ticket lands on is resolved to its
   raw port via `Deps.Slots`, and a synthetic single-backend `forge_slot` chain is built for
   it — same gate/resolve/proxy path as a static route from here on. A load failure surfaces as
   502 `catalog_load_failed`, not 404 (the model is known, just unavailable right now). This
   path always targets the raw slot port, never a Headroom proxy — Headroom fronting stays a
   `router.toml`-only concern (`forge_slot_proxied`, currently only configured for a1/a2).
4. Per backend in the chain: gate (see below) → skip if gated → else resolve the atomic
   `(base_url, api_key, wire_model)` triple → overwrite exactly one field in the request body
   (`model = wire_model`) → bounded retry on connection error/timeout/5xx → on 2xx or a
   definitive 4xx, return that response and stop (streaming failover isn't possible once bytes
   are committed to the client).
5. Chain exhausted → 502 `all_backends_unavailable`.
6. Audit every terminal outcome — backend names and coarse status labels only, never bodies,
   prompts, or credentials.

**Update 2026-07-29:** every 502 in this flow (`all_backends_unavailable`,
`catalog_load_failed`, `offering_resolve_failed`) now also carries a human-readable `message`
field alongside the existing `error` code — added after a live incident where a consumer (LibreChat)
had nothing to show a user but the bare code string. `error` is unchanged (existing
consumers/tests keyed on it still work); `message` is purely additive. See
`internal/router/proxy.go`'s `unavailableMessage()` and `docs/v5-headroom-topology.md`'s
"Update 2026-07-29 (later same day)" section for the incident this came out of — a full host
reboot took down every Headroom proxy (deliberately not systemd-enabled), and nothing detected or
self-healed it, so *every* chat-completion request through a0 failed this way for about 1.5 hours.
A boot-time reconcile now prevents recurrence, and `GET /api/v1/infra-services`'s "LLM Proxy (A0)"
row no longer reports healthy while a proxy genuinely on the routing path is down.

### Busy-Slot Behavior

`forge_slot` backends run with `--parallel 1` (one generation slot). `/health` is a liveness
signal, not a readiness signal — it reports `ok` even while a1 is mid-generation on another
request. Two configurable behaviors, toggled from Settings → Services in the dashboard
(`GET`/`PUT /api/v1/router/settings`, `config_writer.set_scalar("router", "busy_mode", ...)`):

- **`wait`** (default) — a busy-but-healthy slot is attempted anyway; the request queues at
  llama-server's own slot and the router just waits. Unbounded by default since 2026-07-30
  (`request_timeout_s` no longer defaults to a flat 300s — see `requestTimeout()`'s doc comment
  in `internal/router/config.go`): that ceiling was a V4-era number applied uniformly regardless
  of a model's actual speed, and laguna-s-21's slow decode routinely blew through it, severing
  the connection mid-stream. Still configurable per-deployment if a hard ceiling is ever wanted
  again. Right for long-horizon work you can let run.
- **`fail_fast`** — a busy slot (`llamacpp:requests_processing > 0` via `/metrics`, the same
  signal `monitor.py`'s hang detector already uses) is treated as unavailable. Without a
  fallback chain, the router returns an error immediately; with fallbacks configured, it skips
  to the next backend. Right for when you're in a hurry.

`router_catalog.is_slot_busy()` reuses `monitor.py`'s exact `/metrics` Prometheus-scalar
parsing (duplicated rather than imported, since `monitor.py` pulls in dashboard-only
background-thread state on import).

### Health/Busy Gate vs. Mode-Switch Dead Window

An **unhealthy** slot (mid slot-load switch, crashed, connection refused) is *always* skipped
immediately regardless of `busy_mode` — this is the fix for the original mode-switch dead
window. Only the **busy**-but-alive case is gated by `busy_mode`.

## `/v1/models`

Three sources, merged and deduplicated by model name (earlier source wins on collision):

1. **File-based `[[router.routes]]`** — one entry per configured route, regardless of the
   backend's current load/health state (F1: a consumer must be able to *see* a
   configured-but-unloaded model to request it and trigger on-demand load). `forge_slot`/
   `forge_slot_proxied` routes report a live `context_length` read from `/props`
   (TTL-cached) when the slot happens to be up; omitted otherwise.
2. **Store-backed `Offering`s** (MODEL CATALOG Phase 2) — remote-provider models from the
   `offerings` table. `owned_by` is the provider name. **One entry per catalog model** — when
   several providers offer the same model, only the PRIMARY is listed (see below).
3. **Store-backed `Config`s** (a0 local-config visibility fix) — every catalog Config with
   `visibility = "visible"` is listed by name, regardless of whether it's loaded anywhere
   right now. `owned_by = "forge-local"`, `context_length` is the Config's configured
   `n_ctx` (not a live probe — the config may not be loaded). This is what makes locally
   catalog-managed models (including ones destined for `a3`/`a4`) discoverable at all; see
   "Request Flow" above for how a request against one of these gets routed.

OpenAI-shaped response (`BuildModelsResponse` in `go/internal/router/catalog.go`).

## Multi-Provider Selection (2026-08-06)

The same model can be offered by several providers (glm-5.2 exists on AI& *and* Qwen Cloud).
Those are two distinct `offerings` of one catalog model — never interchangeable (cost, data
residency, reliability), so the operator's preference between them is explicit:

- **`offerings.priority`** — lowest value wins; default 100 = "no preference"; ties break by
  provider name, then offering id (deterministic). Managed in Settings → Catalog → Offerings.
- **`router_providers.enabled`** — disable a provider without deleting it. A disabled
  provider's offerings drop out of selection, `/v1/models`, and request routing; credentials,
  offerings, and any linked Headroom proxy are preserved, so re-enabling restores it exactly.
  Toggle in Settings → Provider Keys.
- **PRIMARY** — a model group's enabled offering of an enabled provider with the lowest
  priority value. `BuildModelsResponse` lists exactly the primary (its `wire_model` is the
  listed id, its provider is `owned_by`), and `offeringChain` routes to it.
- **Aliases** — providers name the same model differently (aiand `zai-org/glm-5.2` vs qwen
  `glm-5.2`). A request matching ANY offering's wire_model resolves the whole group to the
  primary and rewrites the wire name to the primary's — what `/v1/models` presents and what
  gets served never disagree. Disabling the primary's provider flips the group to the next
  offering in line.
- **`router.provider_failover`** (Settings → Routing, default **off**) — with it on, a failed
  primary (transport error/5xx) falls over to the next offering of the same model in priority
  order via the chain machinery above; with it off, the error surfaces instead of silently
  spending on the next provider. 4xx responses never fail over (a provider-side rejection is
  definitive). Local-slot chains are unaffected either way.
- A model whose offerings are all disabled (or whose providers all are) is listed nowhere and
  answers 502 `offering_resolve_failed` with a reason, not a silent 404.

The providers service (health/credits polling) also respects `enabled`: a disabled provider is
served its last cached state and never probed.

## `/v1/embeddings` (v0.5, static passthrough)

v0.5 `forge` adds `POST /v1/embeddings` as a **static passthrough** to the always-on
embedding service (`forge-embedding`, port 8083) so a consumer pointed at a0 gets chat AND
embeddings from one OpenAI base URL. It is deliberately *not* a routed backend: no failover,
no health-gating, no on-demand load — the embedding service is CPU-only and permanent, so
there is a single fixed upstream from `[router].embedding_url` (e.g.
`http://127.0.0.1:8083/v1`). The request body is forwarded byte-for-byte (no `model` rewrite),
auth is the same tailnet-conditional check as the chat path, unconfigured → 503, upstream
down → 502. STT/TTS audio endpoints are intentionally excluded (single-backend always-on
services best hit directly — e.g. Open Notebook → STT on 8084). This is a v0.5-only addition;
the V4 Python router (`router_app.py`) serves only chat + models + healthz.

## Deployment

- Tailnet: `tailscale.create_service("a0", 8085)` → `https://a0.example.ts.net`
  (needed one-time Tailscale admin approval, same as any new `svc:*` name).
- Systemd: `forge-router.service`, `User=testuser`, `gunicorn -c gunicorn-router.conf.py
  router_app:app`, `Restart=on-failure`, `ExecStartPost` curl against `/healthz`.
- `/var/log/forge/` did not exist on ForgeHost prior to this work (audit logging had been
  silently failing for the dashboard too, for at least a week — unrelated pre-existing gap,
  fixed in passing: `sudo mkdir -p /var/log/forge && sudo chown testuser:testuser /var/log/forge`).

### Two access paths (both work from the tailnet with no auth)

A0 is reachable via two paths, and consumers should use whichever resolves from their host:

| URL | Path | Notes |
|---|---|---|
| `https://a0.example.ts.net/v1` | HTTPS via `tailscale serve` | Clean URL, TLS. Default. Depends on the `svc:a0` service name resolving (a newer Tailscale feature). |
| `http://forge.example.ts.net:8085/v1` | Direct HTTP via node name | Fallback if the service name doesn't resolve. No `tailscale serve` dependency. |
| `http://203.0.113.7:8085/v1` | Direct HTTP via tailnet IP | Last-resort fallback if DNS is completely unavailable. |

Both paths terminate at the same `router_app:app` on port 8085. The router's tailnet-conditional
auth (see `docs/scheduler.md`) correctly handles both: it reads `X-Forwarded-For` when behind
`tailscale serve` (loopback peer, XFF set trustworthily by the Tailscale proxy) and uses
`request.remote_addr` directly for the HTTP path. XFF is **only** trusted when `remote_addr` is
loopback — a direct non-tailnet connection with a spoofed XFF header cannot bypass the check.

## Verified (standalone, curl only — no Hermes involved)

- `/healthz` unauthenticated; `/v1/models` 401 unauthenticated, lists routes authenticated.
- Non-streaming and streaming completions against a1, both correct.
- `busy_mode=fail_fast` correctly skips a busy a1 — confirmed against a genuine long-running
  production request on a1 (not synthetic). (This was a one-off verification test; the current
  live config has no fallback chains — a busy/unhealthy slot returns a clean error rather than
  failing over to a different provider.)
- Exhaustion → 502, zero secret leakage across audit log and journal (checked with a
  throwaway dead-backend route, not by touching real credentials).
- Tailnet path (`https://a0.example.ts.net`) confirmed working after Tailscale
  approval.
- DeepSeek completion routed through `headroom-deepseek` verified end-to-end (2026-07-10).

**Not yet live-tested:** the original mode-switch dead-window scenario itself (deliberately
triggering `forge load` mid-request) — a1 had genuine production work in flight throughout
testing and it wasn't worth interrupting. Same code path (`router_catalog.is_slot_healthy` →
`_slot_gate`) as the busy-skip test that *was* run live; worth a live confirmation opportunistically.

## Hermes Cutover — Done 2026-07-10

Hermes is now routing through a0. Config changes applied:
`model.default` → `deepseek-v4-flash`, `model.provider` → `custom`,
`model.base_url` → `https://a0.example.ts.net/v1`,
`model.api_key` → a minted `sk-router-` token, `fallback_providers` removed
(now unnecessary — a0 handles all failover internally). A fresh router key was
minted because the original Hermes key (keyid `2962d8558e91`) was never
re-minted after the 2026-07 keyid-format change and no longer existed in
`secrets.toml`.

For step-by-step instructions to configure another consumer (new agent or
application), see `../vault/Forge/a0-connection-guide.md`.

**Update (2026-07-21):** Hermes is being phased out project-wide in favor of OpenCode. OpenCode
is now the primary consumer configured against a0 — same three values (base URL, `sk-router-`
token, model name), no router-side change required for the swap. The Hermes cutover record above
is kept as-is for historical accuracy; see `../vault/Forge/a0-connection-guide.md` for current
consumer setup guidance.
