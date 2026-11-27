# The duplex audio platform: Forge owns the clock, engines are passive step servers

Status: **accepted for the platform and the Radiance engine seam (2026-11-18); the Haru model plugin is planned, not built; nothing is
deployed.** Details and live status: `docs/duplex/` (plan, protocol, decisions D1-D10, status, `engine-contract.md`,
`radiance-plan.md`, `haru-plugin-plan.md`).

## Context

Forge's voice path is half-duplex (speech to text, an LLM, text to speech, one turn at a time): no continuous audio session, no
barge-in or overlap, and no model that hears the user while it speaks. Full-duplex speech-to-speech models (the first is Haru 4B, a
7.5 frames-per-second model: one 133 ms audio frame in, one out) need a continuous session with a steady clock, bounded queues and
explicit backpressure. We wanted that once, as a platform feature, so that a failed or replaced model does not take it with it.

## Decisions

1. **A dedicated service, `forge-duplex` (port 8086), not `forge-tts` and not a0.** OpenAI chat cannot carry duplex audio and the TTS
   contract is text in. Tailnet and loopback only; no public exposure; a single-use ticket for browser connections.
2. **One frame in, one frame out; Forge owns the frame clock.** 24 kHz mono, 3200 samples (133.33 ms). The platform owns time, the input
   and output rings, backpressure, per-step deadlines, the kill switch and the session time limit; it never invents a frame and never
   drops one silently. The model decides when to speak, overlap or stay silent; the platform adds no VAD or turn buffer.
3. **Engines are passive step servers behind an engine-neutral wire contract** (`docs/duplex/engine-contract.md`; the engine-side copy,
   with Forge terms removed, is `docs/DUPLEX-ENGINE.md` in the public Radiance fork). A lane is one WebSocket; the engine answers each
   binary step with exactly one reply, in order; a dropped socket releases the lane. This keeps the platform usable by any engine and
   keeps Forge policy (cards, voices, safety, deployment) out of the public fork. Forge's side is the `remote` tenant adapter and a
   mock engine (`go/internal/duplexremote`); a **conformance suite** that runs against any engine URL is the acceptance test for an
   engine.
4. **Reference audio travels in chunks of at most 1 MiB.** The Radiance server caps a WebSocket message at 2 MiB on purpose
   (`CPPHTTPLIB_WEBSOCKET_MAX_PAYLOAD_LENGTH=2097152` in the fork's `vendor/CMakeLists.txt`: a realtime client sends a few hundred
   milliseconds of audio per message, and a larger one closes the connection; the existing `/v1/realtime` route has the same cap), and a
   3-minute reference is several MB.
5. **Radiance gets the duplex tenant as a plugin-ABI feature (ABI 38).** A plugin declares a duplex tenant; a separate pass driver runs
   its lanes; the engine bridges it to `/duplex/v1/session`; two model-free echo tenants exist behind an off-by-default flag as test
   tenants. The Haru model itself is a later plugin that implements this.
6. **Haru specifics decided (plan, not built):** text sampling on the **host** (the plugin returns logits and takes the sampled token as the
   next step's input; ABI 39 additive), keeping the reference's DRY repetition penalty; model frame 0 is **dropped** (the reference
   replies to user frame t with model frame t+1), measured later under `speak_first`; the system-prompt wrapper lives in the plugin's
   template, with the card text tokenised with special tokens off; the engine is **loaded on demand** (design to be written and approved
   first; nothing is built for it).
7. **Safety is a platform hook, not a model feature:** a loop and policy detector on the text stream and a kill switch, off by default.

## Consequences

- Two repositories carry the work: the private Forge repo (platform, policy, conformance) and the public Radiance fork `jsaigou/radiance-strix`
  (engine seam, plugin ABI, the Haru plugin). The seam between them is the contract document and the conformance suite.
- **The fork's ABI number is ours.** The duplex tenant bumped the fork's plugin ABI from 37 to 38. Upstream Radiance may claim 38 for
  something else; whoever rebases the fork on upstream must reconcile it (the layout table and the version check both catch a mismatch).
- The Radiance tree builds only on Linux x86-64 with clang, so changes to it are verified on a Linux machine, not on a developer laptop.
- Agents that write C++ for the fork cannot build it on a laptop; every package is built and run on Linux before acceptance (see the plan's
  verification protocol). Their own reports are claims, not evidence.
- Latency budget: a frame is the only structural latency; the plan's end-to-end target is 300 to 400 ms. Any design that holds a frame back
  (rejected: replying to user frame t with model frame t) adds about 133 ms to every reply.

## Alternatives rejected

- **Radiance owns the whole session** (driver and endpoint in the engine): no per-frame IPC, but it would put the clock and protocol in a
  public fork, make every duplex model a Radiance port, and discard most of the Go platform.
- **Extending `RadAudioGenDecl`** for duplex: it requires window and vocoder geometry a duplex model does not have; a separate
  declaration (`RadDuplexDecl`) keeps the audio generator untouched.
- **A sidecar for the model's non-LM parts:** needs a bidirectional hidden-state API Radiance does not have.

## Not decided

How the duplex engine is started and supervised on ForgeHost (scheduler-managed on demand is the direction; no unit, route or service row
exists, and building one needs the operator's approval); the `forge-duplex` deployment itself (a systemd unit, `svc:duplex`, a dashboard
row); the behaviour gate for Haru (loops and aggression); fine-tuning.
