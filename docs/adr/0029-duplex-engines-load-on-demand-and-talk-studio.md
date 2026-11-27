# Duplex engines are On-demand pool occupants; talk-studio on Core is the first consumer

Status: **proposed (2026-11-20), for the operator's approval. Design only: nothing here is built, staged or deployed.** Implements decision D4 of
`docs/duplex/forgehost-plan.md` section 7 ("load only as needed") and answers the operator's 2026-11-20 direction: *"let's do proper on demand"*; *a new
consumer as a Docker container on Core, similar to voice-studio*; *name it talk-studio*; *flexible enough to be used with future speech-to-speech
models*. Builds on ADR-0022 (the specialist slot pool), ADR-0025/0026 (engine kind, generic slot launch) and ADR-0028 (the duplex platform).

## Context

- Haru 4B runs on ForgeHost's GPU through Radiance and meets the real-time budget (`docs/duplex/forgehost-plan.md` section 4c): one lane p95 72.5 ms over 13,000
  steps, two lanes p95 122.5 ms at real-time pace, about 12 GB of host memory resident. The model's own quality limits (garbled starts, vowel slips) are
  the model's, and newer checkpoints are expected.
- It must be loadable on demand, not resident: it takes about 12 GB and the GPU, and is used only while someone is talking to it.
- There is no way to talk to it. LibreChat has only a chained speech-to-text, model, text-to-speech loop. The repo already has a browser client
  (`go/cmd/forge-duplex/static`) and the transport (`forge-duplex`), but the client sends no voice or card and keeps no recording.
- What exists that this reuses (read 2026-11-20): the pool (`go/internal/specialist`) with Behavior (Always on / On demand / Disabled), occupants of kind
  `catalog-config`, functions routed by `Manager.Resolve`, and a **lease** mechanism (`grantLease`, `RenewLease`, release) already written for every function; the
  Radiance engine kind (`enginekind.Radiance`, ADR-0025); `forge-duplex` with a remote-engine driver; the voice-studio deployment pattern on Core
  (compose + docktail labels, `docs`: `~/Documents/vault/Infra/deploying-to-core.md`).

## Verified before deciding

A scratch `radiance --model haru-bf16.rad --kernels libhalo --max-num-seqs 2 --duplex-max-lanes 2 --duplex-max-ctx 14500` on ForgeHost loopback (2026-11-20), a duplex-only engine:
`/health` answers 200 (`{"status":"ok","model":"haru","in_flight":0}`), `/props` reports `n_ctx` 14500, `/v1/models` lists `haru` with `max_model_len`, `/metrics`
carries `radiance:kv_cache_max_tokens` and the vLLM-named series, `/duplex/v1/info` lists the tenant and the frame geometry. So the pool's checks for the Radiance kind
work unchanged; this was the plan's open question "health and context verification for a duplex-only engine". Cold start to `/health` 200 took under 60 s. **Not verified:** that
`in_flight` or the busy series counts duplex sessions (the design below does not rely on it: leases carry the busy signal).

## Decision

1. **Haru is a catalog config on a Radiance build, hosted as a `catalog-config` occupant in a dedicated slot (pool `specialist`), Behavior On demand, with one new
   function, `duplex`.** The config carries the engine flags (`--kernels libhalo`, `--max-num-seqs`, `--duplex-max-lanes`, `--duplex-max-ctx`) in the usual typed fields and
   `extra_args`; the launch comes from the database through the generic shim (ADR-0026), no env or args file. A new Radiance build row is needed because the
   deployed `strix-halo-r2` build does not contain the Haru plugin (it lives on fork branch `haru/main`); registering it is the guarded catalog write, plus
   `chcon -R -t bin_t` on its directory (operator sudo).
2. **The daemon grows a `duplex` function, not a new mechanism.** `Manager.Resolve(function="duplex", model=<config or tenant>)` loads the occupant if needed (On demand) and returns
   its address and a lease, exactly as for speech and embeddings. Three authenticated internal endpoints beside the speech ones: resolve, renew, release. A lease is held for the
   whole session, renewed while it lives, released at close; an engine with a live lease is not evicted, and a daemon-side loss of the engine ends the lease so the session sees a
   clear error. The idle unload rule is unchanged (stopped only if its slot or memory is needed).
3. **`forge-duplex` gets an engine locator instead of a fixed `FORGE_DUPLEX_ENGINE_URL`.** At session start it asks the daemon to resolve and lease the engine for the requested tenant,
   waits for it to load (a cold start is about a minute, so the session start reports "starting the engine" progress and has a generous timeout), renews the lease while the
   session runs and releases it at close. The fixed URL stays as the development mode. `forge-duplex` runs on ForgeHost as `forge-duplex.service` (unit already in `systemd/`,
   one-time operator install, port 8086, `svc:duplex`).
4. **talk-studio is a consumer on Core, shaped like voice-studio.** A Go backend plus static PWA in one container, `/home/testuser/docker/talk-studio/`, docktail labels
   (`svc:talk-studio`, `https://talk-studio.example.ts.net`), a data volume, deployed with `git archive`. Source in this repo (operator decision 2026-11-20) under
   `go/cmd/talk-studio` and `talk-studio/web`, so it can share the wire constants with `forge-duplex`. It talks to `svc:duplex` over the tailnet:
   the backend holds the long-lived duplex token and mints the short-lived ticket the browser uses (the existing `/v1/audio/duplex/ticket`), so no secret reaches the page.
   HTTPS from docktail gives the browser the secure context a microphone needs.
5. **talk-studio is model-agnostic by construction.** Everything model-specific comes from the engine's info document, not from the UI:
   frame geometry (sample rate, samples per frame; v1 fixes 24 kHz and 3,200, but the client reads them), the tenant list, and an additive `capabilities` object per tenant
   (reference voice accepted, card accepted, languages, text events, maximum lanes, optional sampling controls). The page builds its controls from that, so a future speech-to-speech
   model is a new tenant and a capabilities entry, not a new client. A different frame size or wire encoding needs a protocol version bump (`docs/duplex/protocol.md`); the
   client keeps the encode/decode behind one module for that.
6. **What talk-studio does** (the testing interface the operator asked for): pick a tenant; pick or upload a reference voice and write a card; talk over headphones or speakers
   (browser echo cancellation on); see the live transcript, state, per-frame latency percentiles and engine status; **record every session** (the user's audio, the model's audio, the
   text with times, the latency series) to the data volume and list, replay, export and delete them; keep voice references and card presets. Recording turns a conversation into the
   material the repo's checks use (sequence alignment of text against audio, ASR, parity). Reference voices stay on Core's volume and are never sent anywhere but the engine.

## Operator-only steps (nothing is attempted without approval)

1. Register the Haru Radiance build row (guarded write API) and `chcon -R -t bin_t` on its binary directory: **sudo**.
2. Install `forge-duplex.service` and the route: **sudo**, `tailscale serve --service=svc:duplex`, plus **tailnet ACL grants** for `svc:duplex` and `svc:talk-studio` (shared ACL; the vault
   warns a grant is necessary but not sufficient and names must be cross-checked first).
3. Deploy the Forge daemon (the `duplex` function and lease endpoints): the normal deploy including one `chcon`.
4. `docker compose up` on Core for talk-studio (existing access via `tailscale ssh core`).

## Order of work, with a gate at each step

(a) Daemon: `duplex` function, resolve/renew/release endpoints, tests with the fake pool (no deploy). (b) `forge-duplex`: engine locator and the wait-for-load behaviour, tested against the mock
engine and the pool fake. (c) talk-studio backend and PWA (voice/card, recorder, capabilities-driven UI), tested in dev mode against the mock engine in a real browser. (d) Deploy daemon and
forge-duplex (steps 1-3 above), verify cold start through the pool end to end with a lease, eviction refusal while leased, and release. (e) Deploy talk-studio to Core, verify with a real
microphone session. (f) The coexistence test (below) before anyone relies on it. Each step ends with real-path verification, not unit tests alone.

## Correction (2026-11-20, found while staging the Haru engine): the pool cannot launch a Radiance engine

Decision 1 says Haru is hosted as a `catalog-config` occupant of the specialist pool. **That was not verified against the launcher, and it does not work today.** What I checked before writing it was only that a
duplex-only engine answers `/health`, `/props`, `/v1/models` and `/metrics` as the Radiance engine kind expects. The pool's `catalog-config` occupant launches a *llama-server*: `specialist.ValidateLaunchSpec` requires
`path.Base(bin) == "llama-server"` under `/opt/forge/llama.cpp-upstream-plain` and a vulkan or rocm backend, and `start-pool.sh` knows only `llama-server`, `catalog-llama`, `qwen-tts-server`, `qwen3-asr-server`, `aligner`
and `kokoro`. LLM slots (`a1`-`a4`) do launch Radiance, through the generic `forge slot-exec` shim and `slot_launches` (ADR-0026), which is how `a1` runs Qwen3.6 today.

Measured the same day, with the registered Haru build (id 26, `/opt/radiance/strix-halo-haru`, fork `haru/main` `920813e`) and exactly the flags the Radiance launch builder produces (`--max-model-len 14500 --max-num-seqs 2 --num-speculative-tokens 0
--tp 1 --duplex-max-lanes 2 --duplex-max-ctx 14500`, `RADIANCE_KERNELS=libhalo`): the engine starts, `/props` n_ctx and `/v1/models` max_model_len both read 14500, and its footprint is **12.4 GiB** (GTT +12.38 GiB, RSS 0.03 GiB; the figure
the fit gate's `safe_memory_bytes` benchmark needs, to be recorded with a margin, for example 13 GiB). **The engine does not report an open duplex session as busy:** during a live conversation `/health` `in_flight` and
`vllm:num_requests_running` stayed 0 and `radiance:engine_steps_total` stayed 0. So any host that decides eviction from the engine's own metrics sees a loaded Haru as idle, and an explicit lease is required (this confirms why decision 2
makes leases carry the busy signal).

Options, not decided: **(A)** teach the pool to launch a Radiance config: a Radiance `LaunchSpec` built by the launch package, a `catalog-radiance` kind in `start-pool.sh` (a root-owned file, one operator install), Go and launcher tests kept in step;
decisions 2 to 6 stand unchanged, and the daemon endpoints, `forge-duplex` locator and talk-studio already built need no change. **(B)** host Haru in an LLM slot through the scheduler: no launcher change, but the scheduler has no external
busy or lease API, so one must be added to scheduler core, and Haru would compete with LLM models for the four slots. **Recommendation: (A)**: it keeps what is built, keeps the LLM slots free, and its only privileged step is one launcher install.

## Option A as built (2026-11-20, operator decision: "A")

The pool now hosts an engine whose launch is a `slot_launches` row, which is how Radiance launches everywhere else (ADR-0026), instead of growing a Radiance command line inside a bash launcher:
`launch.FromServicePort` and `launch.BuildServiceOn` build the launch for a slot whose port the caller supplies (pool slots are not in `cfg.Slots`); `engine.BuildSlotLaunchRowOn` makes the row; the daemon's `catalogResolver`
returns a `LaunchSpec` with `Row: true` for a config whose engine kind launches as Radiance (llama.cpp configs keep the llama-server path, untouched) and builds the row with `LaunchRow`; `specialist.Manager.start`
builds the row first (a refusal writes nothing), writes it only after the unit is confirmed down and before it starts, never starts a unit with no row to launch from, and `stopUnit` disarms the row by compare-and-set once the unit
is down (a failed disarm is logged, not swallowed). The env file carries only the kind, slot, occupant and port; **`start-pool.sh` gained one branch, `catalog-launch`, which runs exactly `/opt/forge/forge slot-exec <slot>`**
(`forge slot-exec` reads the row read-only and re-checks the executable against `catalog.build_roots`). The pool's own validation of a row-launched spec is deliberately small (model under the model roots, plain alias, positive
context, no control characters); the flag rules (the pool owns `--model`, `--port`, `--host`, `--max-model-len`, the API-key flags, and a config must set `--max-num-seqs`, `--num-speculative-tokens` and `--tp`) are the launch
builder's, so the LLM slots and the pool cannot disagree. Decisions 2 to 6 stand unchanged. The one privileged step is installing the new launcher (`/usr/local/lib/forge/start-pool.sh`, root-owned). Tests: the manager's sequencing
(row written while the unit is down, disarmed only after it is down, nothing started on a failed row or with no row store, no row for a llama member), the real `start-pool.sh` dry run for the new kind and the unknown-kind message,
the resolver on a pool slot's port with a Radiance config, and a duplex lease resolving through a row-launched member.

**Verified live on ForgeHost (2026-11-20, forge `v0.6.94-pool-radiance-0723b1c`).** Build 26 (`/opt/radiance/strix-halo-haru/build`, labelled `bin_t`), catalog model 53, variant 58, artifact 69, config 82 (`haru-4b`, n_ctx 14500, hidden),
a `safe_memory_bytes` benchmark 129 at 13 GiB (the pool's engine row picked it up as its memory figure: `memory_bytes` 13958643712), and the pool engine `haru-4b` (kind `catalog-config`, function `duplex`, On demand).
`POST /api/v1/duplex/resolve` for `haru-4b` loaded it into slot s7 in 4.9 s (weights in the page cache): the daemon wrote an armed row (`written_by` `specialist`), `forge-pool@s7` ran `start-pool.sh`, which ran `forge slot-exec s7`, and
`radiance` came up with exactly the flags the launch builder produces. A real 130-frame session against the leased engine ran at p50 70.7 / p95 80.5 / max 88.7 ms with none over budget; the lease renewed (204), released (204) and was
refused after release (410). Unloading stopped the unit, closed the port and disarmed the row (armed 0, generation 1); a second cold start took 4.4 s and re-armed it (generation 2). Haru is left unloaded. The production Qwen slot and all six
existing pool engines were untouched throughout. **Not yet exercised live:** that a lease actually blocks eviction under memory pressure (covered by the pool's lease tests, not by a live eviction), and the path through `forge-duplex` and talk-studio,
which waits for the `forge-duplex` unit, the tailnet routes and the ACL.

**The launcher is not root-owned.** ADR-0022 describes `/usr/local/lib/forge/start-pool.sh` as a one-time root install. On this host the script and its directory belong to `testuser` (label `bin_t`), and the installed file was byte-identical to the repository's
previous version, so the new launcher was installed without sudo by copying the old file with its SELinux context preserved and overwriting the content (`start-pool.sh.prev-deploy` is the rollback). A pool member only runs the launcher when it starts.

## Step (a) as built (2026-11-20, not deployed)

`store.FnDuplex` (the `duplex` function); `specialist.Manager.ResolveDuplex(ctx, name)` (strict: a mistyped name is `ErrNotFound`, never the default; an empty name is the function's
default; `Target` now carries `LeaseID` and `Expires`); `POST /api/v1/duplex/{resolve,renew,release}` with stable codes `duplex_no_engine`, `duplex_engine_off`, `duplex_lease_limit`,
`duplex_lease_gone` (410 on renew when the engine under a session is no longer protected); a fourth key scope, `duplex` (loopback-bound, at most a year, only those three calls), which
needed **migration 0110** to widen the `api_keys.scope` CHECK; locale strings in English and Japanese and a glossary entry. Tests: pool fakes, the handlers and their error mapping, the
scope table, key minting rules. Consequence found while building it: a default can only be an **Always on** engine (ADR-0022), so an On demand Haru is never the `duplex` default and
every caller must name it; the empty-name path exists for a future always-on model.

## Step (b) as built (2026-11-20, not deployed)

`duplexremote.Locator` (`Locate`, `Renew`, `Release`) and `DaemonLocator`, which speaks the three daemon endpoints with a duplex-scoped key sent only as a header. With a locator, `Tenant.Start`
locates (the call returns when the engine is up), checks the engine's info at the address it was given, leases for the session, renews at a third of the lease (retrying a daemon that does not answer
while the lease is still good), ends the session on `ErrLeaseGone` or once an unrenewable lease has lapsed, and releases at close and after a failed start. `Load` answers from the configuration
because the engine is not running when the service starts. `forge-duplex` gained `Options.NewDriverFor` (the tenant a client asks for picks the driver, so one service can front several models)
and the environment in `docs/duplex/service.md`: `FORGE_DUPLEX_DAEMON_KEY` turns the pool on, `FORGE_DUPLEX_TENANTS` lists `tenant[=pool-engine]`. Tested with a fake locator against the mock engine
(lifecycle, loss, transient failure, failed start), a fake daemon (code mapping, the key never in an error), and a whole-path server test. **Not yet exercised:** the real daemon and a real engine
(step d). The browser sees only "starting" while a cold engine loads, which the talk-studio page must explain.

## Step (c) as built (2026-11-20, not deployed)

`go/cmd/talk-studio` (Go backend, the web app embedded) and `deploy/core/talk-studio/` (Dockerfile, compose with docktail labels, README with the operator's steps). The backend keeps the model registry,
reference voices, card presets and chunked, resumable recordings (the page uploads audio every 5 s, so a closed tab loses at most that), and brokers the ticket so the duplex token never reaches the
browser. The page builds its controls from the registry. **Differences from the decision text:** (1) capabilities come from talk-studio's own model registry (`data/models.json`, editable through `PUT /api/models`),
not yet from an additive field in the engine's info, which is still the intended end state; (2) the UI is English only (the repo's English and Japanese rule is for the Forge web app, not this one).

Verified in a real browser (Orca's built-in browser, a fresh tab) against a local `forge-duplex`: the page loads with no console errors, voice upload (decoded and resampled in the browser, validated by the
server), card presets, voice and card selection remembered across reloads, a live conversation, the recordings list. Then against the **real Haru** on a scratch engine on ForgeHost (reached through an SSH
port forward, a silent fake microphone, playback muted): the session went live in under 6 s with the voice and card, ran 40 s with no gaps, the model spoke its own name and words from the persona, and the
recording held 40.3 s of audio from each side, 62 timed text events, the card and the latency stats (reply latency 223 ms typical, 294 ms slow through the tunnel). Three real defects were found only by this
run and are fixed and tested: the `hidden` attribute overridden by CSS; the 128 KiB per-message limit in the WebSocket layer, which cut off a `session.start` carrying a voice even after the application limit was raised;
and Chrome splitting such a message into frames, which the WebSocket library does not reassemble. A fourth, the page sending bare samples instead of a WAV file, was caught by the real engine and not by the mock.
**Not verified:** a human speaking into a real microphone and listening, the Docker image (no Docker on the development laptop), and anything on Core.

## Risks and consequences

- **B7, silent corruption under contention** (2026-11-11, 27% of answers while two long sequences ran in Radiance and llama.cpp prefilled): not yet tested for Haru. Test before relying on
  it; interim rule, one lane and no heavy production use during a conversation, or an exclusive GPU window.
- **Memory**: about 12 GB of host memory and 10.5 GB of weights plus a KV pool in GTT while loaded. The catalog's measured figure feeds the fit gate (`NeedsCuratedMemory`); the host partition
  (ADR-0027) must count it before a load.
- **Cold start**: about a minute plus the 1.2 s prompt open and a 1.3 s warm-up of a second lane. Fine for a studio, poor for a product; "Always on" is a single setting if that changes.
- **Two lanes** run inside the 133 ms frame with 8-17 ms of headroom; the default stays one lane per config, two as a deliberate setting.
- A pooled config cannot load into an LLM slot (ADR-0022 item 3); Haru gets no chat route, and the API will answer as it does for other pooled configs.

## Alternatives rejected

- **Haru's own `haru-server`**: runs the Python model itself; ForgeHost has only CPU PyTorch (about 196 ms per frame, slower than real time), and it would not test our engine.
- **LibreChat voice mode**: a chained speech-to-text, model, text-to-speech loop, not speech-to-speech.
- **A hand-run engine plus a laptop client**: fine for development (and used for the measurements), not a shared tool and not on demand.
- **An always-on engine**: holds about 12 GB and a GPU share for a test tool; the operator chose On demand.
- **A separate repo like voice-studio**: could not import the-forge's `internal/` packages and would copy the wire code (operator chose this repo).

## Not decided

- Default lane count and `--duplex-max-ctx` per config (30 minutes needs about 14,500 rows; shorter sessions need less).
- Whether talk-studio should list voice-studio's saved voices (a read over the tailnet) or keep its own references; the first version keeps its own.
- Authentication for people other than the operator: the first version relies on the tailnet and the duplex token.
- Whether the `capabilities` object belongs in protocol v1 as an additive field (proposed) or waits for v2.
