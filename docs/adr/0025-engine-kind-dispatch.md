# Engine kind is a catalog fact, separate from the GPU backend

Status: **accepted (2026-11-11), Phase 1a of `docs/radiance-first-class-engine-plan.md`** (operator decision 2026-11-10: "engine-kind dispatch first").
Implemented on branch `engine-kind-dispatch-1a`; not deployed. Behaviour-preserving by construction.

## Context

`builds.backend` held two different facts. For `vulkan` and `rocm` it is the GPU path a llama.cpp binary needs. For `vllm` it was the *engine*: the
value switched the launcher script, skipped the `/props` context check and the chat-template probe, refused PROFILE, and let a build omit its binary.
Those rules were scattered string compares (`backend == "vllm"`) in `engine/lifecycle.go`, `profile/profile.go`, `store/build_writes.go` and
`config/config.go`, plus the launcher shell scripts. A third engine (Radiance, which also runs on `rocm`) cannot be expressed as a `backend` value
without making the field mean even more things.

## Decision

1. **`backend` means the GPU path only.** New rows use `vulkan` or `rocm`. The value `vllm` is still accepted and read (existing catalogs hold it), but
   nothing new writes it.
2. **The engine is `engines.kind`** (migration 0103, `TEXT NOT NULL DEFAULT 'llamacpp'`). Backfill: the seeded `vLLM` engine becomes `vllm`, every other
   row stays `llamacpp`. No CHECK constraint: the set of kinds is owned by Go, which refuses an unknown one.
3. **One dispatch point: `go/internal/enginekind`.** A table of `Spec` values (process names, readiness path, whether context and chat-template caps
   come from `/props`, whether PROFILE supports it, whether a build needs a binary, the metrics reader and ledger parser names). Callers ask
   `config.Service.Engine()`; none compares a kind or backend string. Adding a kind is a table entry plus its adapters.
4. **Resolution:** `enginekind.Resolve(kind, backend)`: the engines row decides; the one legacy rule lives there, a build with `backend = vllm` on a
   llama.cpp or unset engine row resolves to `vllm`, so no existing build or config is rewritten.
5. **Fail closed.** An unknown kind is `ErrUnknownKind`. `forge` omits the mode from the merged view with a logged error (like a missing backend);
   `SwitchMode` and `Load` refuse before stopping or starting any unit; `writeServiceFiles` refuses to write; PROFILE refuses; a build cannot be created
   on an engine whose kind is unknown. No HTTP surface was added, so no new error codes.
6. **No new env key.** The kind lives in the catalog (`engines.kind`) only; the slot env files are unchanged. How a launcher learns the kind in Phase 1b is open (see below): the aim is to read it from the DB, not from another env line.

Former special-case sites: `lifecycle.go` (n_ctx source, chat-template probe, orphan process names), `profile.go` (PROFILE refusal),
`build_writes.go` (binary optional), all now read the `Spec`. `config.validate` still accepts `vllm` as a backend (legacy) and additionally checks the
kind. The launcher scripts (`scripts/start-*.sh`, `smith/launchers/start-a*.sh`) still branch on `FORGE_BACKEND=vllm`; they move in Phase 1b.

## Consequences

- Behaviour for every existing config is unchanged; per-kind tests pin launch files, context source, probe, PROFILE, process names, binary rule and the
  metrics/ledger names. Slot env files are byte-identical.
- A new `vllm`-kind build on a `rocm` backend now needs no binary (previously only backend `vllm` did). A `vllm`-kind engine with backend `rocm` is the
  new, preferred spelling; the legacy spelling keeps working.
- The metrics reader and ledger parser names in the table are declarative: vLLM is still read by the llama.cpp reader (finding none of its series, as
  before). Nothing selects a reader through them yet.
- `docs/deployment.md` needs no change until a binary with migration 0103 is deployed; migration runs automatically at daemon start (take the usual
  online DB backup first). Rollback to the previous binary is safe: the extra column is ignored by the old queries.

## What Phase 1b adds

A `radiance` kind: catalog engine and build rows (data through the guarded write API), the launcher branch (keyed on the catalog kind, not a new env key: to be decided in 1b), readiness via
`/health` and `n_ctx` from `/v1/models` `max_model_len`, a second metrics reader (`vllm:` and `radiance:` series, selected through `Spec.Metrics`), the
ledger parser for `usage.prompt_tokens_details.cached_tokens` (`Spec.Ledger`), the router probe branch, scheduler admission from the engine's reported
footprint, Smith checks, and en/ja strings. Also: moving the vLLM launcher branch onto the catalog kind and retiring the `backend = vllm` spelling.
