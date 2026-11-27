# The launch is a database row; the slot launcher is one generic shim; Radiance is an engine kind

Status: **proposed (2026-11-11), Phase 1b of `docs/radiance-first-class-engine-plan.md`. Design only: nothing in this document is built, staged or
deployed.** (Update 2026-11-11: plan steps (a) `slot_launches` and (b) launch builders are built and merged, commits `beb9cb4` and `8a60741`; nothing is deployed.) (Update 2026-11-11, step e5 in the repo: the four slot shims are installed on ForgeHost and proven under systemd; the embedded launchers in `go/internal/smith/launchers/` are now the shim, the legacy scripts are frozen test oracles in `go/internal/launch/testdata/legacy/`, and the daemon wiring sets `SlotLaunchEnforce=true`. Takes effect on ForgeHost when the next binary is deployed; step (d2) is still open.) (Update 2026-11-11, step d2 in the repo: the daemon no longer writes the per-slot env/args files and no code reads them; slot occupancy, eviction footprint and weight accounting read the `slot_launches` row. Pending deploy; v0.6.83 remains a valid rollback for launching, with the stale-file caveat in docs/deployment.md.) The implementation plan is `docs/radiance-phase-1b-plan.md`; the measurement studies behind the first config are `docs/radiance-seqs-context-study-2026-11-11.md` and `docs/radiance-throughput-study-2026-11-11.md`. Builds on ADR-0025 (engine kind is a catalog fact).

Operator decisions this ADR implements (already made, not reopened here): (1) engine-kind dispatch first (Phase 1a, done); (2) no env files and no
args files, the launch comes from the database; the daemon writes the resolved launch for a slot into a table before it starts the unit, and the
static launcher becomes a generic shim running `forge slot-exec <slot>`, a read-only client subcommand; all engine-specific knowledge moves into Go,
driven by `engines.kind` and catalog rows; (3) Radiance needs no API key and binds 127.0.0.1; (4) the script paths under `/usr/local/lib/forge/` are
kept, only their contents change (one-time operator sudo and `chcon`); (5) Radiance lives in `/opt/radiance`; (6) the first config is a Radiance build of a model the operator chose; it is recorded in the Appendix as a worked example and constrains no other config.

## Context: how a slot is launched today

Everything below was read in the repository; `file:line` references are to the tree at commit `c1cabfc`. What is on ForgeHost itself (the contents of
`/etc/sysconfig/forge-a*-env`, unit enablement) could not be read from here and was marked **unverified**; plan step (b0) has since read it (2026-11-11): the env files hold exactly the six keys the daemon writes, and the units are static (not enabled at boot).

1. A load goes through `engine.Manager.Load` (`go/internal/engine/lifecycle.go:144`) or `SwitchMode` (`:22`), both under `m.opMu`
   (`:23-24`, `:145-146`). `Load` stops and awaits the old unit (`:230-241`), then calls `writeServiceFiles` (`:243`), then `Sys.Start(unit)` (`:247`), then
   `waitServiceRunning` (`:251`: unit `running`, then poll `/health` through the llama.cpp client, `:450`), then `verifiedContext` (`:258`, `:466-480`).
   `SwitchMode` does the same for a whole mode (`:52-103`). The scheduler reaches this through `POST /api/v1/load`, which runs the memory fit check
   before `Engine.Load` (`go/internal/httpapi/scheduler_handlers.go:435-470`, route `httpapi.go:815`).
2. `writeServiceFiles` (`go/internal/engine/sysconfig.go:40-97`) writes `/etc/sysconfig/forge-<slot>-env` (`:45`, mode 0600, `:89`) with
   `FORGE_MODEL_PATH`, `FORGE_MODEL_ALIAS`, `FORGE_CONTEXT`, `FORGE_MMPROJ`, `FORGE_BACKEND` and, if the build has one, `FORGE_LLAMA_BIN` (`:73-86`), and
   `forge-<slot>-args` (`:46`, `:92-95`) with the config's `extra_args`, one per line. It **preserves** every line of the existing env file that is not
   `FORGE_*` or `FOUNDRY_*` (`:50-63`); those lines (documented as `LD_LIBRARY_PATH`, `HSA_*`, ROCm variables) are host state, not catalog state. What
   they contain on ForgeHost today: **none host-derived** (b0, 2026-11-11: only the six daemon-written keys; earlier unverified) (`docs/pitfalls.md` records `GGML_CUDA_ENABLE_UNIFIED_MEMORY` was removed from all four on 2026-09-03).
3. The unit (`systemd/forge-a1.service:13-20`) loads the env file with `EnvironmentFile=` (not optional), runs `/usr/local/lib/forge/start-a1.sh`, and has
   `Restart=on-failure`, `RestartSec=10`, `TimeoutStopSec=300`. The four units differ only in name, port and a comment (diffed a1 vs a2/a3/a4).
4. The launcher (`go/internal/smith/launchers/start-a1.sh:9-46`, identical for a2-a4 except the args path and port 8081/8087/8088) reads the args file
   with `mapfile` (`:13-14`), adds `--mmproj` when `FORGE_MMPROJ` is set (`:16-17`), then branches on `FORGE_BACKEND`: `vllm` execs
   `/opt/forge/venv-vllm/bin/vllm serve` with `ROCM_PATH=/opt/rocm-therock-7.13` (`:19-28`, the operator reports that ROCm tree no longer exists on ForgeHost, so the branch cannot run as written; see Deferred), otherwise `FORGE_LLAMA_BIN` or the plain-upstream ROCm/Vulkan `llama-server` (`:29-35`), and execs
   it with `--model --alias --host 127.0.0.1 --port N --ctx-size -ngl 99 --metrics [mmproj] [extra args]` (`:37-46`).
5. The launcher text is embedded in the forge binary (`go/internal/smith/launchers/launchers.go:22`), and Smith may install one **only if it is missing**,
   never overwriting (`go/internal/smith/execute.go:857-891`). The only drift test pins the two fallback `llama-server` paths
   (`launchers/launchers_test.go:13-35`).
6. The env file is also the daemon's record of what runs in a slot. It is read back for (a) restart reconciliation, `inferSlotMode` over alias then model
   path (`go/internal/engine/manager.go:380`, `sysconfig.go:120-145`), (b) the footprint fallback of an unloaded slot (`engine/memory.go:173-179`),
   (c) weight accounting (`collector/sysconfig.go:16-35`, `:111-`, called at `collector/run.go:767`).
7. A second, separate launcher family exists for the specialist pool (`systemd/start-pool.sh`, `forge-pool@.service`; ADR-0022). It already follows the
   rule this ADR generalises: the launcher takes a kind and builds the command itself, with a second wall of path checks (`start-pool.sh:5-9`,
   `check_bin`). It is out of scope for 1b.
8. Engine-specific code that Phase 1a did not move: the process name `llama-server` is hard-coded in `engine/memory.go:135` and
   `collector/run.go:889`, `:936`; the llama.cpp metrics reader is used for every engine (`collector/llama.go:213-243`); the context ledger parses only
   `timings.cache_n` (`ctxledger/timings.go:10-49`, called at `router/usage.go:367`); the router probes `/props` (`router/catalog.go:154-182`).

Why this has to change for Radiance: Radiance needs its own executable, an environment (`RADIANCE_HOME`, a kernel-library selection and a ROCm library path: host-dependent values, held in a host launch-environment record) that has no home in the env-file scheme except as hand-edited "preserved" lines, and flags that differ from llama.cpp. The first-class
engine plan (section 3, "Launch") proposed a third branch in each shell script. Four copies of a growing shell script, an env file that mixes
catalog-derived and host-derived lines, and a launcher whose behaviour cannot be unit-tested are the reasons the operator chose to move it into Go.

## Decision

### 1. The launch is a row in `slot_launches`

Migration `0104_slot_launches.sql` (SQLite, same file as the catalog, `forge.db`):

```sql
CREATE TABLE slot_launches (
    slot            TEXT    PRIMARY KEY,               -- slot name; ^[a-z0-9][a-z0-9-]{0,31}$ (the existing slotNameRe)
    generation      INTEGER NOT NULL,                  -- +1 on every write, set inside the write transaction
    format_version  INTEGER NOT NULL DEFAULT 1,        -- slot-exec refuses a version it does not know
    armed           INTEGER NOT NULL DEFAULT 1 CHECK (armed IN (0, 1)),
    config_id       INTEGER,                           -- catalog configs.id at write time; NO foreign key (a deleted config must not delete history)
    config_name     TEXT    NOT NULL,                  -- the alias; what inferSlotMode derives today
    engine_kind     TEXT    NOT NULL,                  -- enginekind name at write time (llamacpp | vllm | radiance)
    backend         TEXT    NOT NULL DEFAULT '',       -- vulkan | rocm (or legacy vllm)
    exe             TEXT    NOT NULL,                  -- absolute path of the engine executable
    argv_json       TEXT    NOT NULL,                  -- JSON array of strings: argv[1:], one flag or value per element
    env_json        TEXT    NOT NULL DEFAULT '{}',     -- JSON object of strings: variables to ADD to the inherited environment
    cwd             TEXT    NOT NULL DEFAULT '',       -- '' = leave as systemd set it
    port            INTEGER NOT NULL,
    model_path      TEXT    NOT NULL DEFAULT '',       -- resolved weight path (what FORGE_MODEL_PATH holds today)
    mmproj_path     TEXT    NOT NULL DEFAULT '',
    n_ctx           INTEGER NOT NULL DEFAULT 0,        -- configured context (what FORGE_CONTEXT holds today)
    spec_hash       TEXT    NOT NULL,                  -- sha256 of canonical(exe, argv, env, cwd); drift/audit evidence, not a secret
    written_at      INTEGER NOT NULL,                  -- unix seconds
    written_by      TEXT    NOT NULL DEFAULT ''        -- daemon version string
);
```

One row per slot (the primary key is the slot). The columns after `port` duplicate facts that are derivable from `argv_json`, on purpose: the
three existing readers of the env file (section 6 of the context) need them as plain columns, and a reader must not parse another engine's argv to learn
a model path.

What is deliberately **not** in the row: the API key or any other secret. A secret never goes in `argv_json` or `env_json`. The builders enforce this
(see "Secrets" below); if a future engine ever needs one, the row carries a *reference* that `slot-exec` resolves, and that is a new ADR.

### 2. Who writes, who reads, when

- **Single writer: the engine manager**, through a new `store` API (`PutSlotLaunch`, `GetSlotLaunch`, `ListSlotLaunches`, `DisarmSlotLaunch`).
  `Put` runs `INSERT ... ON CONFLICT(slot) DO UPDATE SET generation = generation + 1, ...` in one transaction and returns the new generation. An audit
  entry (slot, config, generation, spec_hash; no argv or env content) goes to the audit log in the same path.
- **The write happens at exactly the place `writeServiceFiles` is called today** (`lifecycle.go:62`, `:243`): inside `m.opMu`, after the old unit has
  been confirmed gone and before `Sys.Start`. Both paths already return early when the old unit did not stop (`:52-54`, `:233-240`), so the invariant
  is **a row is only ever replaced while the slot's unit is inactive**. There is no second lock; the scheduler already reaches the slot only through
  the engine, which serialises on `opMu`. SQLite makes each write atomic, so a reader sees the old row or the new row, never a mixture.
- **Readers:** `forge slot-exec` (read-only connection, below); the engine's reconcile (`SlotStates`), footprint fallback and weight accounting
  (replacing the three env-file readers); Smith checks.
- **`armed`:** `Unload`/`stopAll` set `armed = 0` with `UPDATE ... WHERE slot = ? AND generation = ?` after the unit is gone (compare-and-set, so a
  late unload can never disarm a newer row). `slot-exec` refuses to launch a disarmed row. This is new behaviour: today a raw `systemctl start forge-a1`
  after an unload relaunches the previous config from the env file. With `armed` it fails closed and says so. Crash restarts never touch the row.

### 3. Lifecycle cases

| Situation | What happens |
|---|---|
| Normal load | engine stops old unit, waits until gone, builds the launch (`launch.Build(svc, cfg)`), `Put` (generation g+1, armed 1), `Sys.Start`. systemd runs the shim, the shim runs `forge slot-exec a1`, which reads the row and `exec()`s the engine. |
| Crash of the engine | `Restart=on-failure` (`forge-a1.service`) reruns the shim 10 s later; `slot-exec` reads the **same row** (generation unchanged) and relaunches the same config, as the env file does today. While the daemon is waiting for readiness it sees the unit crash (`lifecycle.go:445-448`) and stops the unit, as today. |
| Daemon restart | Rows persist. Running slots are untouched. Reconcile infers a slot's mode from `config_name` of an armed row whose unit is active (exact, replacing the alias/path heuristic; the heuristic stays as the fallback while env files still exist). Inactive units are ignored regardless of their row (same rule as `manager.go:372-386`). |
| Host reboot | Rows persist (WAL database). Nothing is relaunched by the daemon. The units are static, not enabled at boot (plan step b0, 2026-11-11), so nothing relaunches at boot and no armed row is acted on until a daemon load starts the unit. |
| Row missing / disarmed / unknown `format_version` | `slot-exec` exits non-zero with a stable message; the unit retries every 10 s; the engine, if it started the unit, sees the failure and stops it. |
| Config edited or deleted in the catalog after load | The row is the truth about what was launched until the next load. A Smith check compares a freshly built launch hash with `spec_hash` and reports drift as informational. |
| Database restored from a backup (rollback) | Rows revert with it. For an active unit whose row generation or hash no longer matches the running process (`/proc/<pid>/cmdline` vs `exe` + `argv_json`), the Smith check below reports it; the operator reloads the slot. |
| Stale row (unit inactive, row armed) | Harmless and kept (it is the last launch, like today's env file) until the next `Put` or `Disarm`. Smith reports rows older than the unit's last stop only if the unit is active with no matching row. |
| Two daemons | Not supported, as today. The last writer wins; the generation makes the interleaving visible in the audit log. |

### 4. `forge slot-exec <slot>`

A subcommand of the existing `forge` binary, dispatched in `cmd/forge/main.go` **before `flag.Parse`**, next to `mint-key`/`config`/`catalog`
(`main.go:97-126`), because a bare `forge` boots a second daemon (`:127-149` documents the incident). It never starts any server, never writes the
database, never calls the daemon's HTTP API.

- **Finding the database:** `-db` flag, else `$FORGE_DB`, else `/var/lib/forge/forge.db` (the default every CLI verb uses). Opened with a *separate*
  read-only code path, **not** `store.Open`: `store.Open` runs the migrations and sets the journal mode (`store/db.go:49-62`), neither of which a
  launcher may do. DSN: `file:<path>?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)`. Reading a WAL database read-only needs the `-shm`
  file to be readable or creatable; the unit runs as `testuser`, the owner of the directory (**unverified**, tested in plan step c).
- **Behaviour:** (1) validate the slot name against `slotNameRe`; (2) read the row; (3) refuse unless `armed = 1` and `format_version` is known;
  (4) second wall: `exe` must be absolute, a regular executable file, and (after resolving symlinks) under one of the roots of the catalog build guard
  (`internal/buildguard`, setting `catalog.build_roots`, read-only from the same database; no install location is hard-coded: the operator lists `/opt/radiance` in that setting, which the build guard requires anyway before a build under it can be registered); `cwd`, if set, must be an
  existing directory; `env_json` keys must match `^[A-Z][A-Z0-9_]*$`; (5) build the environment: the inherited environment (what systemd gives a
  `User=testuser` unit) minus any variable starting `FORGE_` or `FOUNDRY_` (the old env-file namespace, so a stale `EnvironmentFile` can never leak),
  plus `env_json`; (6) `chdir` if `cwd` is set; (7) `syscall.Exec(exe, [exe, argv...], env)`. It is an **exec**, not a fork: the engine becomes the
  unit's MainPID, so `TimeoutStopSec=300`, `SIGTERM`, the polkit-permitted restarts, `PortArg` (`collector/proc.go:176`, which needs `--port` and its value as
  two argv elements) and GPU footprint accounting by MainPID (`engine/memory.go:185-197`) all work unchanged.
- **Exit codes** (sysexits-style, stable, documented in `--help`): 64 usage; 66 no row for the slot; 69 database unavailable or unreadable; 73
  `exe` fails the guard; 75 row disarmed; 78 unknown `format_version` or malformed row. 126 and 127 are the exec failures a shell would report. A row
  that is permanently bad (66, 73, 75, 78) should not be retried every 10 s forever; the unit edit in the rollout adds `RestartPreventExitStatus=66 73 75 78`
  (see Decisions needed).
- **Logging:** to stderr, which the unit sends to the journal. It logs the slot, config name, generation, `spec_hash`, `exe`, the number of arguments
  and the **names** of the variables it adds. It never prints a variable's value. Argument values are logged, except that any argument that follows a
  flag whose name contains `key`, `token`, `secret` or `password` is replaced by `***` (belt and braces: the builders already refuse such flags). Messages
  go through `i18n.T` like the other CLI verbs (`internal/i18n/locales/{en,ja}.json`, keys under `forge.cli.slot_exec.*`).
- **`--dry-run`:** prints exe, argv and variable names (not values) and exits 0 without exec. The golden tests and the operator's pre-flight use it.
- **SELinux and paths:** the shim stays at `/usr/local/lib/forge/start-aN.sh`, `bin_t`, executable, unchanged path (decision 4); its content is
  `#!/bin/bash`, `exec /opt/forge/forge slot-exec aN`. `/opt/forge/forge` is already `bin_t` (deployment.md deploy steps). The new accesses are the
  unit domain reading `/var/lib/forge/forge.db`(-wal, -shm) and executing `/opt/radiance/...`; whether the policy permits the first is **unverified**
  and is the first thing plan step (c) tests, on the free slot. `/opt/radiance` needs `chcon -R -t bin_t` on the build tree and the plugin directory
  (operator, interactive sudo), as for llama.cpp builds (deployment.md:121).
- **Missing database or missing row** is never "fall back to the old env file". There is no fallback inside `slot-exec`; during the transition the
  *old script* is still in place on slots not yet converted, which is how the two worlds coexist (plan section "Rollout").

### 5. Launch builders (all engine knowledge in Go)

A new package `go/internal/launch` with one pure function per kind, `Build(kind, Input) (Launch, error)`, where `Input` is what the engine already holds:
the resolved `config.Service` (model, alias, context, backend, `LlamaBin`, mmproj, `ExtraArgs`), the slot's port, and the config paths. It returns exe,
argv, env, cwd. The dispatch is `enginekind.Spec.Launch` (a new Spec field naming the builder, like `Metrics` and `Ledger`), so no caller compares a kind.

- **llamacpp** reproduces `start-aN.sh` exactly: exe is `LlamaBin` if set, else the plain-upstream ROCm or Vulkan `llama-server` by backend; argv is
  `--model P --alias A --host 127.0.0.1 --port N --ctx-size C -ngl 99 --metrics [--mmproj M] <extra_args...>`; env is empty.
- **vllm**: the vLLM branch is deferred (see "Deferred: reinvestigate vLLM"). The golden-test step reproduces it verbatim or marks it known-dead, decided then;
  its ROCm tree no longer exists on ForgeHost.

- **radiance** (built in plan step f3; `internal/launch/radiance.go`): exe is the build's `binary_path`. Argv is `--model P --served-model-name A --host
  127.0.0.1 --port N --max-model-len C` followed by `extra_args` verbatim. The builder **owns** those flags and refuses a config whose `extra_args` repeat them
  or set `--api-key`, `--api-key-env`, `--radiance-home` or `--help`/`-h`. `--max-model-len` is always passed (0 would mean the model's training context, a value
  no tuple could name), so a config with `n_ctx` below 1 is refused. It **requires** `--max-num-seqs`, `--num-speculative-tokens` (the binary defaults it to
  `auto`, not 0) and `--tp` (its `--help` states no default) in `extra_args`, each with a valid value, and never supplies, defaults or infers any of them: each
  sets the footprint or the behaviour of the engine, so each is a per-config decision under the plan's Config derivation procedure. Radiance matches flag names
  exactly, so `--flag=value` is refused (give two elements); every element is one token without whitespace. No card-budget flag is supplied and none is refused
  (they are part of the memory tuple). The model path must be absolute (Radiance would fetch anything else as a hub repo id). **Env is the build row's
  `launch_env` and nothing else** (below).
- **The host launch environment lives on the build row (decided 2026-11-11, operator).** `builds.launch_env` (migration `0106`, nullable TEXT holding a JSON
  object of strings, additive) holds the variables a build's engine needs on this host (for Radiance `RADIANCE_HOME` and `RADIANCE_KERNELS`; `ROCM_PATH` and
  `LD_LIBRARY_PATH` too when a build needs them, which the pinned build does not because it carries `RUNPATH=/opt/forge/rocm-therock-10.1/lib`). It is written
  only through the guarded build-write API (ADR-0020 F8) and validated there: names `^[A-Z][A-Z0-9_]*$`; no secret-looking name or credential-prefixed value; no
  `FORGE_`/`FOUNDRY_` names (slot-exec strips them), no `LD_PRELOAD`/`LD_AUDIT`; bounded sizes; no NUL, newline or other control character; and any value that
  looks like a path must be a list of absolute, clean paths each under `catalog.build_roots` **or** `catalog.launch_env_roots`. *Why a second list:*
  `build_roots` is the trust boundary for what may be executed as an engine binary and is the second wall of `slot-exec`; the ROCm tree (`/opt/forge/rocm-therock-10.1`)
  is code the engine loads but is not a place a build binary should become registrable from, and widening `build_roots` to include it would make it one. The
  operator lists such trees in `catalog.launch_env_roots` (same shape and validation as `build_roots`, unset means none, so the default is fail closed). Only
  kinds with `Spec.UsesLaunchEnv` accept a `launch_env` (an environment that would be silently ignored is refused at write time); retiring a build clears it; a
  `PUT` replaces it. Nothing about Radiance's host layout is in Go.

- **Common rules enforced by the shared validator:** every argv element is a single token (no shell, nothing is ever interpreted by a shell again);
  env keys and values pass a secret screen (key names containing KEY, TOKEN, SECRET, PASSWORD or CREDENTIAL are refused); an unknown kind is
  `ErrUnknownKind` (ADR-0025, fail closed before anything is stopped).

The host-derived lines the env file preserves (item 2 of the context) were to be **inventoried on ForgeHost before conversion** (key names only). The inventory (plan step b0,
2026-11-11) found none: the env files hold exactly the six keys the daemon writes, so there is nothing to fold into a builder or declare dead. Step (b) also found that the
four launcher scripts differ only in slot and port, that `VulkanBin`/`RocmBin` are not used for launching, and that kind `vllm` on backend `rocm` now follows the kind.

### 6. Radiance as an engine kind (what the rest of 1b adds, in brief)

A `radiance` entry in `enginekind` (process name `radiance`; readiness `/health`; the llama.cpp `/props` reader for context and caps, subject to a live
check, see below; `Profilable` false; `BinaryRequired` true; metrics reader `radiance-prometheus`; ledger parser `openai-cached-tokens`; launch builder
`radiance`). Catalog data through the guarded write API where one exists, and one data migration where none does (engines, formats and quantizations are
GET-only, ADR-0020 F8): migration `0105` seeds engine `Radiance` (`kind = 'radiance'`) and the container format `rad`; quantization names are added with the model that needs them. Detail, series names and
the fit-gate input are in the plan.

Three Radiance facts the design depends on, from the fork clone (`/private/tmp/.../scratchpad/rad-src/repo`, at the commit the build row pins; the first one is in the Appendix):

- **Context is configured, not negotiated.** `/v1/models` and `/server_info` report `opt_.max_ctx`, the requested `--max-model-len`
  (`core/server/admin.cpp:160`, `:205`, `:434`; `core/engine_bringup.cpp:964`, `:1010`). Radiance does not silently lower it, but the KV pool may hold fewer
  tokens than the limit; it then only logs a warning (`engine_bringup.cpp:3745-3775`). The honest "actual n_ctx" check is therefore
  `radiance:kv_cache_max_tokens` (`core/server/metrics.cpp:286`) against the configured context, not `/props`.
- **`/props` exists in the fork source** with `default_generation_settings.n_ctx`, `model_alias`, `model_path` and `chat_template_caps`
  (`admin.cpp:254-345`), which the existing llama.cpp readers parse unchanged (`collector/llama.go` dual-location read; `router/catalog.go:154-182`).
  The evaluation record (finding 7) says Radiance does not serve `/props`. The two disagree; **unverified until probed on the built binary** (plan f1).
- **Health:** `/health` answers 200 `ok`, 503 `stopping`, or 503 `stuck` when the device has advanced nothing for 60 s (`admin.cpp:562-582`), and is
  exempt from the API key (`http.cpp:719-720`). `stuck` is a hang signal llama.cpp lacks.

### 7. Values are per config, never design constants

Nothing in the launch table, the builders or the fit gate encodes a value observed on one model or one machine state. Context, `--max-num-seqs`,
speculation depth, KV dtype and the memory figure are per-config inputs derived by the plan's **Config derivation procedure**. The fit-gate figure is
recorded with the tuple it was measured at (model variant, build, seqs, len, KV dtype, speculation depth) and a different tuple is refused, not
approximated. Footprint is established per model and per tuple, never by a cross-model formula (operator decision 2026-11-11); the figure is the GTT delta over baseline **plus the engine's pinned host memory** (operator decision 2026-11-11: the study's MemAvailable drop was 4 GiB larger than the GTT delta); card-budget flags (`--gpu-memory-mib`) are a per-model, per-config consideration and part of the tuple. Slot choice is the scheduler's (any free slot), and shim
conversion order is by idleness at the time, never evicting; no document names a slot order. The operator chose `--max-num-seqs 4` for the first model; `n_ctx`, speculation depth, KV dtype and
card-budget flags are per-model, per-config choices made by the plan's derivation procedure, and the Qwen3.6 values are a worked example in Appendix A, not defaults.

## Consequences

- The four launcher scripts become identical apart from the slot name (one line). The env/args files, the "preserved lines" mechanism and the
  `FOUNDRY_`/`FORGE_` divergence class of incidents (`sysconfig.go:24-39`) disappear for converted slots.
- The launch is unit-testable. The golden tests (plan step b) compare the Go builders to what the current scripts and env files produce, which is the
  strongest guard against the largest risk (a bad shim breaks every slot at once).
- Every reader of the env file moves to the table: one source of truth, and a Smith check can compare the row to the live process.
- New failure mode: **the database is on the start path of an inference engine.** If the database is unavailable when systemd restarts a crashed
  engine, the engine stays down until it is back (exit 69, retried every 10 s). The database is local, WAL, and already a hard dependency of the daemon
  that started the unit; the engine process, once running, never touches it.
- Behaviour change: a disarmed row stops a raw `systemctl start` from relaunching an unloaded config. Intended, consistent with "load through the scheduler".
- The unit files must change once: `EnvironmentFile=` is non-optional (`forge-a1.service:13`) and the env file will no longer exist. This and the
  scripts are one-time operator actions with sudo; nothing here asks for them now.
- A schema bump (`format_version`) is the only coordination between daemon and `slot-exec`, which are the same binary but may be different builds across
  a deploy; an old shim target binary that does not know the new version fails closed (78) instead of guessing.
- The `forge` binary path (`/opt/forge/forge`) is now on every slot's start path. Deploys replace it with an atomic `mv` (deployment.md), so a restart that
  coincides with a deploy sees the old or new binary, never a partial one.
- Out of scope and unchanged: the specialist pool launcher (`start-pool.sh`), `start-comfyui.sh`, the compressor units.

## Alternatives considered

1. **A per-kind launcher script** (add a `radiance` branch to the four scripts, as the first-class-engine plan first proposed). Smallest diff, and no
   database on the start path. Rejected by the operator's decision: the shell scripts are untestable, hold engine knowledge outside Go, and the env
   file keeps mixing catalog state with hand-edited host state. It also needs four edits per engine on the host with sudo each time.
2. **Transient units** (`systemd-run` with the command line, or the daemon authoring a unit per launch). The command line would live in systemd, not
   the catalog, which gives the cleanest crash-restart semantics. Rejected: it needs polkit/DBus rights beyond the current "restart `forge-*.service`"
   grant, makes unit files authored at runtime (which `forge-pool@.service` states the repo avoids), and conflicts with the standing rule that new
   units and off-path services need explicit human approval.
3. **Keep env and args files, add a Radiance section.** Least change to the daemon. Rejected by the operator ("no env files, no args files"); it would
   also keep the preserved-lines problem and a world-readable-by-mistake secrets surface on disk.
4. **A generic shim that reads a JSON file** under `/var/lib/forge` instead of a table. Same shape, simpler read path (no SQLite in the shim).
   Rejected: it is another file with its own atomic-write and backup story, outside the database that already has backups, audit and migrations;
   the operator asked for the database.
5. **`forge slot-exec` calling the daemon's HTTP API** to fetch the launch. Rejected: it makes the engine's start depend on the daemon being up (a
   daemon restart would break every engine restart in that window) and on API auth.
6. **Resolving the launch inside `slot-exec` from the catalog** (no row; the shim rebuilds argv from config tables). Rejected: it would launch
   whatever the catalog says *now* rather than what the daemon verified and recorded, so a catalog edit during a crash-restart would silently change a
   running slot's configuration.

## Operator decisions

Accepted (2026-11-11): the unit edit shape (remove `EnvironmentFile=`, add `RestartPreventExitStatus=66 73 75 78`); disarm on unload; the UI word
"runtime" (ランタイム); specialist pool conversion deferred to a later **Phase 1c with its own ADR** (a committed follow-up, not open-ended; see the plan); vLLM
deferred (below). No longer decisions about values: the first slot (the scheduler picks any free slot; shim conversion goes by idleness, never evicting)
and the first config's context, `--max-num-seqs`, depth and memory figure (measured inputs, re-derived by the plan's Config derivation procedure; the
seqs value and memory figure for the first config are now recorded in the plan from the two studies, 2026-11-11). Details: `docs/radiance-phase-1b-plan.md`.

## Deferred: reinvestigate vLLM

Is vLLM used for anything today? The launcher branch names `/opt/rocm-therock-7.13`, which no longer exists on ForgeHost (only ROCm 10.1 remains), so it cannot
run as written. If it is in use it needs a rebuild against ROCm 10.1. The operator deferred this. The golden-test step reproduces the branch verbatim or
marks it known-dead, decided when this item is taken up.

## Appendix. First-config worked example (Qwen3.6-35B-A3B FP8 on ForgeHost; values are measured inputs, not design; to be re-derived by the procedure above)

The first config is `qwen3.6-35b-a3b-fp8.rad` (operator decision 6), built from the fork `jsaigou/radiance` branch `unofficial-fixes` at commit `ac51376`
(the B7 corruption fix), run with `RADIANCE_KERNELS=libhalo`, ROCm 10.1 at `/opt/forge/rocm-therock-10.1`, builds under `/opt/radiance`. Radiance
`/props` facts above were read from that commit. The remaining first-config values (context, seqs, depth, KV dtype, memory figure, ports, slot history,
canary parameters) are tabulated, with their evidence and the step that re-derives each, in Appendix A of `docs/radiance-phase-1b-plan.md`. None of
them is a design input of this ADR.

## Update 2026-11-12: plan step f2-code, f3-core, f5 built (branch `phase-1b-f-core`, not deployed)

Migrations `0106` (engine `Radiance`/kind `radiance`, format `rad`, `builds.launch_env`; the plan's name `0105_radiance_engine` was taken by usage-concurrency) and `0107`
(`benchmarks.tuple_json`). `enginekind.Spec` gained `Launch`, `UsesLaunchEnv`, `ContextFromModels`, `ContextCapacitySeries`, `ChatTemplateCapsFromModels`,
`NeedsCuratedMemory` and `BusySeries`; the `radiance` entry names the metrics reader `radiance-prometheus`, the ledger parser `openai-cached-tokens` and busy series
`vllm:num_requests_running` (the readers themselves are a separate step). **Fit gate:** for a kind with `NeedsCuratedMemory` the fit check consults only a
config-scoped `safe_memory_bytes` benchmark whose recorded tuple (`launch.MemoryTuple`: build, variant, weight artifact, `n_ctx` and the whole canonical flag set, so
seqs, KV dtype, speculation depth, `--tp` and any card-budget flag are all in it without Go knowing their names) equals the config's; no figure, or a figure at another
tuple, is a refusal that names the tuple needed; there is no fallback to file size, profile or a model/variant-scoped figure. Admission then is the existing
`need + used <= budget - reserve` over live occupants, where used = GTT + the RSS of every engine process (the process-name sweeps now follow `enginekind.ProcessNames()`,
so a Radiance process's pinned host memory reaches the budget through the existing `gtt_used + RSS` rule with no collector change). **Load verification:** readiness is `/health`
(the existing client treats 503 `stopping`/`stuck` as not ready); configured context is `/v1/models` `max_model_len`, capacity is `radiance:kv_cache_max_tokens`, the actual
context is the smaller, under 95% of configured warns and is recorded `ctx_reduced` in `mode_history`; the chat-template probe tries `/props` and falls back to `/v1/models`
`capabilities`.

## Update 2026-11-12: metrics reader, ledger parser, router and hang detection built (branch `phase-1b-observability`, not deployed)

The names `radiance-prometheus` and `openai-cached-tokens` are now selected through the Spec: `collector.scrapeMetrics` (by `Spec.Metrics`), `ctxledger.ParserFor` (by `Spec.Ledger`),
the router's probe and busy series (by `Spec.ContextFromModels`/`ContextFromProps` and `Spec.BusySeries`). The router learns a port's kind from the armed `slot_launches` row, the same record the
engine manager writes at launch (this ADR's section 2), through `router.Deps.SlotKind`. For a Radiance slot the hang rule is keyed on `radiance:engine_steps_total` and ignores the rate gauges, which keep
their last value at idle; `/health` 503 `stuck` raises `INFERENCE_HANG` at once. The 90 s sustain and 120 s cooldown remain inherited from llama.cpp pending a mid-prefill scrape (plan, "Status of the f3
observability items"). No `enginekind` field was added.

