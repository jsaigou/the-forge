# Radiance 2.x is its own engine kind: the row keeps the flags, `forge slot-exec` writes the YAML

Status: **accepted 2026-11-25 (operator: "A: teach Forge's launcher the 2.x form"); built on branch `feat/radiance-2x-launch`; not deployed until the deploy steps below are done and recorded in `progress.md`.**

Builds on [ADR-0025](0025-engine-kind-dispatch.md) (engine kinds) and [ADR-0026](0026-radiance-engine-and-generic-slot-launch.md) (the launch is a database row; one generic shim).

## Context

Forge launches Radiance 1.17 as `radiance --model P --served-model-name A --host 127.0.0.1 --port N --max-model-len C <flags>`. Upstream Radiance 2.0.0 removed the command-line flags: the binary takes `radiance --config FILE` and nothing else (`usage: radiance --config FILE [--check]`); given the old flag form it stops with "radiance takes its settings from a YAML file" and prints the YAML it would have wanted. Verified on ForgeHost against `f1113ee9` (v2.2.0 plus 102 commits) on 2026-11-25.

Why Forge needs 2.x: upstream stopped serving block-FP8 containers (2.0.0), so the production Qwen3.6 container cannot move to 2.x and a rebuilt container (`qwen3.6-35b-a3b-iq8r-w4-rtn.rad`, published as v2) is the 2.x form. Measured like for like against v1 (`docs/radiance-poc/speed-plan.md`, result section): 17% faster decode at one stream, 9 to 13% at four, quality at parity (`quality-plan.md`). Nothing else about the engine differs for Forge: on 2.x the readiness (`/health`), the context (`/v1/models` `max_model_len`) and the metrics (`radiance:kv_cache_max_tokens`, `vllm:num_requests_running`) are the ones the Radiance kind already reads (checked against a running 2.x engine).

## Decision

1. **A new engine kind `radiance2`** in the `enginekind` table, with the same Spec as `radiance` except its name and its launch builder (both come from one constructor, `radianceSpec`, and a test pins that they differ in nothing else). `enginekind.IsRadiance(kind)` answers "any Radiance engine" for the code that does not care about the launch form (the "recommended" chip, the Radiance builds smith checks, `forge radiance register-build`). Migration `0111_radiance2_engine.sql` adds the engine row `Radiance 2.x`; migration `0112_iq8r_w4_quantization.sql` adds the quantization name `IQ8R_W4` (rotated-int8 trunk, 4-bit experts), because quantization names arrive by migration and none of the existing ones (FP8 for the 1.17 containers, the GGUF set) describes the 2.x container honestly.
2. **The stored row keeps the flag form.** The `radiance2` builder is the 1.17 builder plus one check: the flag list must be convertible to a settings file, so a config that cannot be launched is refused when its row is built. The spec hash, `forge launch-check`, the fit gate's tuple (the whole flag set of the config) and drift detection are defined over the same list as before and mean the same thing.
3. **`forge slot-exec` writes the file.** For a row whose `engine_kind` is `radiance2`, the shim converts the stored flags with `internal/radianceyaml`, writes `/var/lib/forge/slot-config/<slot>.yaml` (directory 0700, file 0600, temporary file plus rename, so the engine never reads half of one) and execs `radiance --config <that file>`. `--dry-run` prints the file and writes nothing. A row that cannot be converted exits 78 (bad row, the unit does not restart); a file that cannot be written exits 69 (the unit may retry). The environment (`RADIANCE_HOME`, `RADIANCE_KERNELS`, library path) is the build row's `launch_env`, exactly as for 1.17. Every other kind is launched unchanged and never touches the directory.
4. **The translation is a small pure function** (`radianceyaml.FromArgv`): owned flags first (`--host`/`--port` under `server:`, the served name as the model key, `--model` and `--max-model-len` first), then the config's flags in order, a bare flag as `key: true`. Strict: a duplicate flag, a `--flag=value` spelling, a name outside `^--[a-z][a-z0-9-]*$`, a missing owned flag, or a value with a newline is an error. A scalar is written plain only when YAML would read it back as the same string; everything else is double-quoted. A value can never add structure to the file.
5. **No change to 1.17 launches, to the fit gate, to the catalog schema (beyond the engine row) or to the store.** Config 69 keeps serving v1 on build 25 until a separate, operator-approved switch.

## Consequences

- One more place knows a flag-to-YAML mapping (the translation); the engine's own `--config FILE --check` is the authority, and the deploy steps run it against the generated file before any config is switched.
- `forge slot-exec` is no longer strictly read-only: for `radiance2` it writes one file under `/var/lib/forge/slot-config` (a directory the slot units may already write).
- The file is replaced at every launch, so a stale file never outlives its row.
- Deploy: a new `forge` binary (the sudo-free context-preserving copy), the migration runs at daemon start (online database backup first). Then, each with the operator's yes: register the 2.x build (`forge radiance register-build -kind radiance2 …`), measure the memory figure on the first scheduler load of the exact tuple, create the v2 config next to config 69 (hidden), verify with a real load and request, and only then switch.

## Not decided here

Whether a 2.x config should be able to set server-level engine keys (log level, kernels list): not needed, refused by omission (the translation only writes `host` and `port` under `server:`).
