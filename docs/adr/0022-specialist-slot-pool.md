# Small engines live in dedicated slots, apart from the LLM slots

Status: **accepted (2026-11-03).** Decided in a grilling session 2026-11-02; mockups approved 2026-11-02 and 2026-11-03; built, deployed and running on ForgeHost
(six members since 2026-11-02; forge v0.6.69). The history, incidents and live verification are in `progress.md` (entries from 2026-11-02 to 2026-11-03);
the step-by-step plan that used to be in this file is there too. Evidence: `docs/lfm3-capability-plan.md` (Phase 5), `docs/lfm3-evaluation-results.md`.
Mockups: `docs/mockups/specialist-pool.html`, `docs/mockups/residency-ui.html`.

## Vocabulary

User-facing words (UI in English and Japanese, docs): an **engine** is what runs in a dedicated slot (Kokoro, Qwen3-ASR, the embedding model, d1-2b); a
**dedicated slot** is where it runs (S1, S2, apart from the LLM slots A1-A4); **Behavior** is its one setting (Always on, On demand, Disabled). The earlier word
"specialist" read as a person or as something separate from the engine, so it is gone from everything a person reads. It survives only in code and wire
names, which were not renamed (the `specialist` package, `/api/v1/specialists`, the `specialist_*` tables, error codes and the `SPECIALIST_FAILED` anomaly,
this file's name). `web/src/locales/GLOSSARY.md` records the mapping.

## Context

The operator's direction (2026-11-02): *"We want to have one or more always resident. We need to rethink our existing stt tts setup to have a gui with
slot behavior for things like stt tts that are separate from the regular slots we use for llms. We also need to have on demand similar to what we do for
llms now."* Refined in the grilling: these are **slots for small, specialised models with persistent services: embedding, TTS, STT, timing (forced
alignment), decisions/classifiers. Much smaller than LLMs, specialised, and often left loaded because latency matters.**

What existed before:

- **Four LLM slots** (`a1`-`a4`) share about 120 GB of GTT. Placement iterates one flat list: a free slot, otherwise evict one idle at least
  `idle_unload_s` (180 s), never proactively.
- **Bespoke always-on services outside the scheduler**, each its own unit: embedding (8083), STT (8084), aligner (8086), and `forge-tts`, a Go front door
  on 8082 fronting Kokoro and up to three `qwentts.cpp` engines. About 8 GB of GTT held permanently. Only TTS had a policy (`resident`, `available`,
  `disabled`); a0's embeddings and speech routes were static passthroughs.
- **Small catalog models** (d1 decision models, LFM3-Audio/Omni) were ordinary configs that occupied LLM slots and were evicted like chat models.

Measured (2026-11-02, small models, through a0): free slot 3.2 s cold, warm 0.05 s (text) and 0.8 s (short speech); evicting an idle slot 5.2 s; every
slot recently active up to about 196 s. Interactive voice cannot live with the last two, and an LLM request could evict one of these engines right before it was
needed.

## Decision

1. **A second pool of generic slots, `s1`-`s12` at most**, separate from `a1`-`a4`. Same `slots` table with a `pool` column (`llm` default,
   `specialist`). LLM placement only sees `llm` slots and never evicts an engine in a dedicated slot; placement for an engine only sees the `specialist`-pool slots. Ports 8100-8111
   (verified free; the kernel's ephemeral range starts at 32768). Admins add and remove slots in the UI; the config reloads live, with no restart. A slot
   can be removed only when empty and not an always-on member's home.
2. **Slots are generic; occupants are registered launch templates, never free-form command lines.** A slot hosts one occupant, started by one fixed
   launcher that maps the occupant's launch kind to a binary under an allow-listed root. Kinds: `llama-server`, `qwen-tts-server`, `qwen3-asr-server`,
   `aligner`, `kokoro` (fixed port 8880, so at most one), and `catalog-config`. Parameters are typed and validated in Go and again by the launcher; the
   env file the daemon writes never carries a command line.
3. **One catalog model, one owner.** A `catalog-config` occupant runs a catalog config exactly as an LLM slot would (merged args, args file, `n_ctx`
   verified after start). A model is either an engine in a dedicated slot or an LLM-slot model, never both: the engine and the scheduler refuse to load a pooled config
   into an LLM slot (the API answers 409 `specialist_owned_config` up front), and a0 routes requests for it to the pool member.
4. **Every engine in a dedicated slot has one setting, Behavior: Always on, On demand or Disabled**, in the same three words on the Engines tab, the Models page
   and Voice & Speech (a voice engine's old "Resident / Available / Disabled" mean the same and now share the control).
   - *Always on:* its slot is reserved. Started at daemon start (background, smallest first), restarted if it dies with backoff (10 s, 30 s, 2 min, then
     every 5 min), shown `failed` with the last error, and a Smith anomaly (`SPECIALIST_FAILED`) after 3 failures in a row. Never evicted by traffic.
   - *On demand:* started by the first request, stopped after `idle_unload_s` **only if its slot or memory is needed** (least recently used first), and
     never while a request is in flight.
   - *Disabled:* registered, not loaded, no slot held. For a catalog model it simply goes back to loading into the LLM slots. There is no Stop and no
     Remove on a card; removing an engine from the system is a different, deliberate act (not built).
5. **Residency is a promise.** An operator's stop of an always-on member is persisted and survives a daemon restart (written before the stop). Memory
   owed to always-on members that should be running is subtracted from what an LLM load may take. A Smith check, `specialist_pool`, reports always-on
   members that are not running. After a cold boot the members come back in about 15 s, smallest first. Memory is not otherwise walled off: engines in dedicated slots
   share the GTT budget and the fit gate with the LLM slots, and a pool ceiling is not built.
6. **a0 routes by function.** Functions: `embeddings`, `speech`, `transcription`, `alignment`, `decisions`. Each has a default occupant set in the UI. A
   request whose `model` names a pool occupant or a catalog config goes to it; otherwise the function default answers, so existing URLs and shapes stay
   as they were. Embeddings, transcription, alignment (`/v1/align`), decisions (`/v1/systemone`) and chat/audio for a pooled catalog config resolve
   through the pool. Speech goes through the `forge-tts` front door, which asks the daemon for an engine per request (see the addendum below: each TTS engine is an occupant with its
   own behavior). Disabling a speech engine makes the front door refuse that engine with a clear message; it never falls back to loading
   a slower copy per request.
7. **A full, failed or stopping pool fails fast.** When no slot is free and nothing on-demand is evictable, or a member is starting past its health
   timeout, a0 answers 503 with a stable `specialist_pool_full` / `specialist_unavailable` code and a `Retry-After` where known. Never a minutes-long wait.
   Unloading or disabling a member that is mid-generation can take about a minute, so those calls answer 202 `stopping` after at most 10 s and the stop
   finishes in the background; the slot reads `stopping` until it ends.
8. **Boot order: the dedicated-slot engines first, in the background, then traffic.** The daemon does not block LLM serving on them.
9. **UI.** An `Engines` tab (capacity, plain states, one Behavior control, Retry / Start / Show log / Unload now only where they apply, Waiting and
   Disabled lists, per-function defaults, add a slot), a strip on the Console, a three-step Add-engine dialog (what, how, confirm; the slot is chosen
   automatically; a plain warning for a big model; only a model larger than the whole machine is refused), a "Dedicated slot" section on a model's detail view
   (the square card has no room for another row, so the card face shows only a badge instead of Load), and Voice & Speech with a "Runs as" choice (unit and
   URL only for a built-in process). Slot cards are deliberately smaller and teal-tinted so they never read like LLM slots. Admin role to change anything;
   viewers see state. Every string is English and Japanese; terms are in `web/src/locales/GLOSSARY.md`.

## Unit design (installed by the operator)

One template unit `forge-pool@.service` (instances `forge-pool@s1`..), one static launcher `/usr/local/lib/forge/start-pool.sh`, per-slot env files in
`/var/lib/forge/pool/` (testuser-owned, 0600, written by the daemon), ports on 127.0.0.1. This follows the `forge-compress@.service` precedent: a one-time
manual root install, no unit authored at runtime, no new polkit rule (`^forge-.*\.service$` already matches). The launcher allow-list maps each kind to a
fixed binary and refuses one outside that kind's root; a kind not in the table exits 2; it refuses the flags the pool owns; a test keeps the launcher's table
and the Go registry in step.

- **`Restart=no`: the daemon supervises.** This deliberately differs from the LLM slots (`Restart=on-failure`): two restart mechanisms would double-restart
  and hide the `failed` state the UI shows.
- **`TimeoutStopSec=60`**, and **`TimeoutStopFailureMode=kill`** as a drop-in, `systemd/forge-pool@.service.d/90-kill-on-timeout.conf`. A busy
  llama-server does not exit on SIGTERM, and Fedora's global `10-timeout-abort.conf` makes the host default `abort`, which dumps core (about 5 GB of cores
  piled up). It has to be a drop-in, because a drop-in beats a line in the unit file, and it has to be named `90-...`, because systemd applies drop-ins
  sorted by file name across directories and the last wins. After installing it, `systemctl show forge-pool@sN -p TimeoutStopFailureMode` must print `kill`.
- **Migration** of the old services was done one at a time, each verified live; the old units are stopped and disabled so old and new never share a port.

## Consequences

- Embedding, STT, TTS, alignment and decisions share one lifecycle, one screen and one vocabulary; interactive speech never waits behind an LLM eviction;
  always-on members are warm; the LLM slots are untouched. Adding a small model to the pool is data (an occupant row), not a new hand-written unit.
- Standing GTT use is now an operator-visible policy, not a side effect of whichever units are enabled; resident engines reduce what the LLM slots can
  fit. The memory the dialog shows for a catalog model is the catalog's measured figure at the config's full context (a 2B model can read 12-14 GB), or file
  size plus nothing for KV when no measurement exists.
- The launcher is an execution surface for registered binaries; the allow-listed roots, typed parameters, no free-form commands and audit rows are the
  mitigation. Catalog args run on the LLM slots' trust model.
- **Known limits:** stopping a busy member takes about a minute (a quicker stop would need the daemon to send a second SIGTERM; not done). The speech path
  cannot refresh an engine's idle clock, because forge-tts is handed the engine's address once, when voice settings are applied, and then talks to it
  directly. So **an engine that voice requests go to must be Always on**: Voice & Speech refuses to save an On demand one, and the behavior, policy and update
  endpoints refuse to make one On demand (409 `engine_backs_voice`) while an enabled, Always on row runs in it. Several rows may share one engine; nothing
  limits that. Other engines that are not catalog models are not probed for in-flight requests, so a request longer than `idle_unload_s` can see
  one evicted. The busy probe fails closed: a member whose
  `/metrics` fails or times out counts as busy (404/501, metrics off, counts as idle), so a member with a broken metrics endpoint is never evicted until fixed.
  A full machine reboot has been simulated, not run.

## Addendum, 2026-11-04: speech engines are plain pool engines

Speech used to be configured as four fixed rows (`tts.engines`: Kokoro, customvoice, voicedesign, base), each with a systemd unit, a URL or a `pool_member`, and a rule
that a row's engine had to stay Always on; the daemon rendered forge-tts's environment from them and restarted forge-tts on every change. That is gone
(docs/speech-routing-plan.md). Now:

- A speech engine is a pool occupant of function `speech` whose kind (base, customvoice, voicedesign, kokoro) says what it makes. Any of them may be Always on,
  On demand or Disabled. Nothing renders an environment file or restarts forge-tts.
- forge-tts is daemon-only: for each request it asks `POST /api/v1/speech/resolve` (a loopback-bound `speech`-scoped key) for an address and takes a **lease**
  that the engine cannot be evicted under; it releases the lease when the response ends. A voice names its engine (recorded by voice-studio, carried by
  deploy and rename); a voice that names none uses the default for its kind (`tts.defaults`, the one speech setting). An engine that is off or missing is a clear
  503, never another engine and never a command-line copy; the command-line path and the fixed addresses were removed.
- Leases cover every function: a0's `viaPool` takes one for embeddings, transcription and alignment, and a pooled catalog config takes one through the router
  backend, so no engine is evicted under an in-flight request (a lease expires on its own after ten minutes; `viaPool` renews it while a response is open). This closes
  the gap noted in Consequences ("non-catalog engines are not probed for in-flight requests").
- `tts.engines` was migrated once into `tts.defaults` when the daemon started (each enabled row's engine became the default for its kind) and then removed.

## Alternatives considered

- **Pin a model inside an LLM slot** (reservation-like). Rejected: reservations are time-boxed, and a permanent pin leaves audio sharing the LLM pool's
  eviction and count.
- **Fail fast only, no residency.** Rejected by the operator: interactive use needs warm models.
- **Fall back to forge-tts/forge-stt when a slot model cannot be placed.** Rejected: the answer would come from a different model than requested, which
  ADR-0014 refuses for the same reason. The same rule is why a Disabled voice engine refuses rather than loading a slower copy.
- **A registry over the existing units, no new slots.** Declined by the operator in favour of generic slots; it would not make small catalog models
  first-class members.
- **Free-form launch commands in the catalog.** Rejected: arbitrary command execution from a stored value.
- **A pool memory ceiling now.** Deferred; the shared fit gate plus a visible cost is enough for now.
- **Two overlapping vocabularies (policy plus Stop/Remove).** Rejected by the operator (2026-11-03): "Always on, On demand, Disabled make more sense for the
  behavior. If we're actually removing something we would be removing it from the system as a whole."

## Changed from the original draft

The draft of 2026-11-02 said: six slots on 8100-8105 (now up to twelve, 8100-8111, added live); "disabled becomes not in the pool" and an operator Stop
(now Disabled is a Behavior value that keeps the registration and frees the slot, and cards have no Stop or Remove); speech not yet routed (routed
through the front door, first by a `pool_member` on a fixed row, now per request; see the addendum); the unit "not installed" (installed, with a kill-on-timeout drop-in added later); and an occupant library with
load/unload/retry (replaced by the Behavior control and the Add dialog). Decision 3 (one owner), the residency promise, the `stopping` answer and the
fail-closed busy probe are new.

## Still open

- A pool memory ceiling, if the shared fit gate proves too loose.
- Whether an engine in a dedicated slot should run a catalog config at a reduced context, so the memory the dialog shows is not the full-context figure.
- Whether the realtime proxy holds its slot for the life of a session (probably yes: it makes the member busy, so never evictable).
- Per-kind health timeouts and backoff numbers beyond the current defaults.
- The deliberate "delete an engine from the system" flow (Settings → Engines, advanced), if it is ever needed.
