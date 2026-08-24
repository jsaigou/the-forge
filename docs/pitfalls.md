# Critical Pitfalls

## ROCm & GTT Memory (Strix Halo APU)

The Strix Halo host is a unified memory APU. There is no discrete GPU or PCIe bus. CPU and GPU share LPDDR5x-8000 DRAM (~256 GB/s GPU bandwidth, ~128 GB/s CPU bandwidth). Model weights live in **GTT** (GPU-addressable system RAM), not in a separate VRAM pool.

- **Unified Memory flag:** `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` must be in **every** per-slot sysconfig env file (`/etc/sysconfig/forge-a{1,2,3,4}-env`), not just whichever slot a large mode usually lands on — the scheduler places any mode on whichever slot is free, never pinned to one physical slot. Without it, ROCm is capped at ~63 GB. (Stale singular-slot framing here — `forge-primary-env` — is pre-2026-07-29-rename; see the incident entry below for how this actually got dropped and re-confirmed.)
- **All layers to GPU:** Always pass `--gpu-layers 99` (or equivalent). CPU-resident layers run at half bandwidth (~128 GB/s vs 256 GB/s). This is the single biggest performance lever.
- **GART vs GTT:** GART is a fixed BIOS aperture — keep it at 512 MB. GTT is the dynamic GPU memory pool; models allocate from here. rocm-smi `GTT Total` (~120 GB) is the effective model size ceiling.
- **Contiguous Allocation:** The kernel may fail to find large contiguous GTT blocks. Always verify `n_ctx` via `/props` after startup — it silently downscales on allocation failure.
- **Queue Eviction:** 80K+ token prefills can trigger GPU queue eviction. Kernel mitigations: `amdgpu.mcbp=0`, `amdgpu.vm_fragment_size=9`.
- **GPU utilization is not a proxy for efficiency:** rocm-smi GPU% measures shader occupancy. Token generation is memory-bandwidth-limited — 60–80% utilization is expected and normal on large models.
- **rocWMMA — standard llama.cpp build:** Do NOT use `-DGGML_HIP_ROCWMMA_FATTN=ON` with ROCm 7.0.2+ on gfx1151. It is slower than the standard HIP path at longer context depths. **UPDATE 2026-08-17: the flag is gone upstream** — llama.cpp PR #26046 (merged 2026-07-24) deleted the rocWMMA FlashAttention kernel entirely ("all relevant AMD hardware can use the better kernel in `fattn-mma-f16.cuh`"), so neither the standard nor kintsugi builds have the option anymore. Issue #24437 (the −41% prefill regression) was resolved by deletion, not a driver fix. This bullet is retained for history; the Poolside-fork bullet below is still live and unchanged.
- **rocWMMA — Poolside fork (laguna):** The Poolside llama.cpp fork is the **exception** — it MUST be built with `-DGGML_HIP_ROCWMMA_FATTN=ON`. (A fork's tree root is deployment data — `smith.binaries.tracked` carries it; the lesson is the flag.) The fork's hand-rolled WMMA flash-attn kernel (`fattn-mma-f16.cuh`) has no device code for gfx1151 because `AMD_WMMA_AVAILABLE` is gated on `defined(RDNA3)` which is only set for gfx11xx (RDNA3.0), not gfx1151 (RDNA3.5). Without rocWMMA, `--flash-attn on` triggers `NO_DEVICE_CODE` → GPU hang → core dump. With rocWMMA, the `rocwmma` library handles gfx1151 correctly. The standard llama.cpp build (used by all other modes) does not have this issue because its standard HIP path already includes the RDNA support merged in [llama.cpp#22051](https://github.com/ggml-org/llama.cpp/pull/22051).
- **GTT counter blind spot with Unified Memory:** `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` backs allocations with HMM-mapped host RAM, not classic GTT BOs — `rocm-smi --showmeminfo gtt` does not see them. Confirmed live with `nemotron` at 1M context (2026-06-19): `gtt_used_mb` stayed at baseline (~5.5 GB) for the whole run while actual process RSS was 95.7 GB. Vulkan and non-unified-memory ROCm modes still report correctly via GTT — this blind spot is specific to the unified-memory path. `engine.get_metrics()` adds this path's RSS on top of `gtt_used_mb` (see Memory display note below) rather than comparing the two — an earlier `max(gtt_used, vmrss_used)` version silently dropped `gtt_used_mb`, and any non-Forge GPU consumer (ComfyUI) riding on it, whenever this path outweighed it (fixed 2026-07-21). **(v0.5 A1, 2026-07-24: v0.5 field names are `gtt_used_bytes`/`inference_rss_bytes` etc. — bytes not MiB — but the additive semantics are identical; the V4 names above are retained for historical context.)
- **`GGML_CUDA_ENABLE_UNIFIED_MEMORY` was silently dropped by the 2026-07-29 slot rename — found and RESTORED 2026-08-04 (Sprint C Phase 0 + follow-up).** First found absent from every per-slot sysconfig env file, every `start-a*.sh` launcher, and every unit file — `go/internal/engine/sysconfig.go`'s `writeServiceFiles` only preserves non-`FORGE_` lines already present in an env file, it never writes this one itself. **Root cause confirmed, not just suspected**, by reading `mode_history`: the real `nemotron` mode (91 GB, needs unified memory) last loaded successfully at full 1M context on **2026-07-28** — a normal API-triggered load, not a manual debug session, so the flag really was set in a real sysconfig file and working. **Nobody loaded full `nemotron` even once after that** — every load since is `nemotron-puzzle` (51.5 GB, fits under the ~63 GB ceiling without the flag), which is exactly why the gap went unnoticed. **2026-07-29, the very next day**, is the slot rename (`primary`→`a1`) — that session's own notes record the new `forge-a1-env`/`forge-a1-args` files needing a `sudo touch` to even exist, i.e. they were created **empty**, not copied from the old `forge-primary-env`'s content. Any hand-set non-`FORGE_` line — this one included — would have been silently lost right there. **Confirmed live 2026-08-04**: added `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` to `/etc/sysconfig/forge-a3-env`, loaded the real `nemotron` mode into `a3` — `/props` confirmed `n_ctx: 1048576` (no reduction), `VmRSS` measured **95.8 GB** (matches the original 2026-06-19 measurement of 95.7 GB almost exactly), `rocm-smi`/fdinfo both stayed near baseline (the blind spot, reproduced live and correctly this time) — then evicted cleanly. **Fixed on all four slots (`a1`-`a4`), not just `a3`** — the scheduler places any mode on whichever slot is free (`Load()`/`EnsureLoaded` never pin `nemotron` to one physical slot, same no-physical-address-pinning principle as Headroom's topology redesign), so a fix scoped to one slot would silently fail the moment `nemotron` landed anywhere else (the operator caught this, corrected mid-session). Verified inert on the three Vulkan-backend slots (`a1`/`a2`/`a4`) by reloading `qwen25coder` on `a4` after adding the line — started clean; the `ctx_reduced` result recorded in `mode_history` for that load is qwen25coder's own pre-existing, documented `--parallel 4` context-splitting exception (CLAUDE.md), confirmed present on loads from before this change too, not a regression from the env var.
- **Dashboard memory-bar total ≠ GTT/VRAM, fixed 2026-08-04 (Sprint C Phase 0):** operator reported the Overview memory bar reading like "all memory use" rather than GTT/VRAM specifically. Root cause, confirmed with a live two-slot measurement (`nemotron-puzzle` on `a3` + `qwen25coder` on `a4`): the bar's total came from `memory_budget.used_bytes` (`InferenceRSSBytes` = `gtt_used_bytes` + a per-ROCm-slot `VmRSS` addend, the same additive fix from the blind-spot bullet above) — a deliberately conservative figure built for the OOM-prevention fit check, not a GTT/VRAM-scoped display number. Live test measured it ~6 GB higher than the sum of the same two slots' own real fdinfo (`slot_memory_bytes`) figures. The old frontend code then silently *rescaled every per-model segment* to fill whatever `committedPct` that inflated total implied, so the visible split never matched each segment's own printed GB label. Fixed in `web/src/pages/Dashboard.tsx`: when every loaded slot's real `slot_memory_bytes` figure is known, the bar's committed/free split is now driven directly by that real sum instead of `used_bytes` (falls back to `used_bytes` only when a slot's real figure is genuinely unavailable, same as before). No backend change — `memory_budget.used_bytes` is left exactly as-is for `can_fit()`/OOM prevention, which should stay conservative; only the Dashboard's own display math changed.
- **Fit-check weight estimate double-counts a shared model directory (found + fixed 2026-08-20, `v5.0.115-weightsize-693f904`):** `collector.WeightSetSizeBytes` treated any subdirectory as a "shard directory" and summed **every** `*.gguf` in it — once for the model path and again for the mmproj path when both live in the same dir. `qwen3.8-27b/` holds two sibling quants + an mmproj (Q5_K_M 19.8 + UD-Q8_K_XL 31.5 + mmproj 0.93 = 52.2 GB), so the fit check reported **~97 GiB** for the 19.8 GiB Q5_K_M config and refused every load (`"Won't fit even after evicting every loaded slot"`) even when GTT had room — the exact blocker that made `qwen38-27b` unrunnable while `gemma4-26b` / ComfyUI shared the box. Fix: only a genuine shard (`-NNNN-of-NNNN.gguf`) globs its directory, and only same-prefix siblings; everything else is stat'd exactly; the model+mmproj passes de-dup so a shared dir is never summed twice. `ActiveWeightsBytes` (collector metrics) and the model card's `file_size_bytes`/`derived.memory_req_bytes` inherit the fix. **Live-verified:** `qwen38-27b` loaded at full 262K ctx via a0 with ComfyUI resident (~26 GiB GTT in use). See `progress.md` 2026-08-20 entry.
- **Whole-host GTT/unified-memory wedge, 2026-08-22 17:46 JST (power cycle required) — three defects compounding, all fixed same day:** (1) the fit gate counted llama-server RSS for **rocm slots only** (`engine/memory.go unifiedRSSBytes`, pre-rename), so the two **standard-vulkan** gemma4-26b loads (~35–48 GB real footprint each once materialized) were invisible to scheduling decisions while qwen38-27b's giant-prefill KV kept growing on a1; (2) nothing reserved an in-flight load's footprint between admission and materialization — two router requests for sibling configs of the SAME GGUF (`gemma4-26b-a4b` then `-nothink`, duplicate catalog artifact rows 6/7) started 35 s apart and both passed fit checks that could not physically be satisfied; (3) no layer below forge protected the host: 8 GB zram + swappiness 60 turned pressure into thrash instead of an OOM kill, systemd-oomd/earlyoom were inactive, `memwatch.sh` only logs, and amdgpu/KFD wedged hard (no panic logged; journal cut mid-write at 17:46:17 while sqlite writes limped to ~17:46:29). Fixes: all-backend RSS accounting + MemAvailable−8 GiB headroom cap on FreeBytes + in-flight-load reservation + per-decision fit logging (`engine/memory.go`), same-weights sibling guard gated on proven headroom (`lifecycle.go`; ADR-0006 amendment), timeout errors now carry the last retryable blocker (`sched/core.go`), earlyoom enabled host-side. Lesson: on Strix Halo every counter lies differently — GTT misses HMM pages, fdinfo misses unmapped pages, RSS double-counts GTT-resident mappings — so gate on the kernel's own MemAvailable floor AND per-slot estimates, never one probe alone. Attribution gap: `audit_log.remote_addr` was null, so which client sent the two fatal requests is unknown.


## Backend Selection: Vulkan vs ROCm

Vulkan (RADV STRIX_HALO) is the default backend and outperforms ROCm on token generation across all tested model sizes. Benchmarks on Strix Halo (gfx1151, May 2026):

| Model | Size | ROCm tg128 | Vulkan tg128 | Vulkan pp advantage |
|---|---|---|---|---|
| Qwen2.5-Coder 7B Q4_K_M | 4.4 GB | 42.8 t/s | 46.2 t/s (+8%) | ROCm +11% pp |
| Qwen3.6 35B MoE Q4_K_XL | 20.8 GB | 49.2 t/s | 55.0 t/s (+12%) | tied |
| GPT-OSS 120B Q6_K | 58.9 GB | 47.7 t/s | 48.3 t/s (tied) | Vulkan +12% pp |

**Hard ceiling:** Vulkan cannot load models > ~63 GB (`radv: Not enough memory for command submission`). ROCm with `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` reaches ~120 GB GTT. Nemotron (91 GB) requires ROCm.

**Backend is per-mode** in config.toml (`backend = "vulkan"` or `"rocm"`). The dashboard exposes a toggle on each mode button. The engine writes `FORGE_BACKEND` to the sysconfig env file; the launcher script selects the binary.

## Hybrid/Recurrent Models Force Full Prompt Reprocessing Every Turn (llama.cpp, not deployment-specific)

Affects any mode whose GGUF architecture has SSM/recurrent layers alongside attention —
confirmed via GGUF metadata on **`ornith-35b`** and all **`qwen36`** variants (`general.architecture
= qwen35moe`, with `qwen35moe.ssm.*` fields present). Every turn of a multi-turn session logs:

```
forcing full prompt re-processing due to lack of cache data (likely due to SWA or hybrid/recurrent memory, ...)
```

...and reprocesses the **entire accumulated conversation** from scratch, even when the new
request is a 99%+ prefix match of the previous one. Per-turn latency grows with conversation
length (confirmed: prompt-eval throughput drops from ~1050 tok/s to ~350–400 tok/s within a
single reprocess pass as it works through the growing prompt) until it exceeds a calling
agent's client-side timeout — this is what caused the Ornith→DeepSeek Hermes failovers on
2026-07-01 and 2026-07-02, not a hardware fault. GPU temp/power/GTT usage stay completely
normal throughout; the GPU is doing genuine (if wasteful) work, not hung.

**`--swa-full` does not fix this for hybrid/recurrent models.** That flag (from llama.cpp PR
#13194) only disables sliding-window-attention cache eviction — it has no effect on recurrent
(SSM/Mamba/Gated-DeltaNet) state, which is the actual cause here. Gemma4 (26B-A4B and 31B) is
plain `gemma4` architecture with no SSM layers, so its own occurrences of this warning (166+
seen on `forge-secondary`) are the genuine SWA case and `--swa-full` should apply there — not
yet verified live, since surfacing this took priority.

**`--ctx-checkpoints` (alias `--swa-checkpoints`) is the flag the warning itself implies would
fix this** — it's the context-checkpoint/restore mechanism SWA and hybrid caches need to avoid
reprocessing. That setting predates this investigation and was established for a narrower
reason: preventing a GTT leak, confirmed only on `qwen36` (see `forge-v2-reference/CLAUDE.md`:
*"Required on qwen36... Gemma4 does not require it"*).

**Correction (2026-07-23, live config audit — see `docs/modes.md`):** the "blanket-applied to
all modes" claim above was never actually true of the live config, in either V4's `config.toml`
or v0.5's `forge.toml`. Only `swallow-32b`/`swallow-8b` actually set `--ctx-checkpoints 0`;
`qwen36` and every Gemma4/Ornith mode that sets it explicitly uses `16`, not `0`, and several
modes (`gpt-oss-120b`, `nemotron`, `nemotron-nano`, `nemotron-puzzle`, `qwen25coder`,
`qwen3coder-q6`) don't set it at all (llama.cpp default, currently 32). Whatever this repo's
docs previously believed about a blanket-`0` rollout, it didn't happen — treat this file's
"Every Forge mode currently sets `--ctx-checkpoints 0`" claim (and any other doc repeating it)
as historical/aspirational, not current fact.

**Do not just flip `--ctx-checkpoints` to a positive value without re-reading
[investigations.md](investigations.md) item 9 first.** As of 2026-07-02, upstream llama.cpp has
*no maintainer-approved fix* for hybrid/recurrent checkpoint restore, and there are currently
**open, unresolved hang bugs** in that exact code path — including one reproduced on
`Qwen3.6-35B-A3B MoE` specifically (llama.cpp#22450). Enabling checkpoints today risks trading
"slow but working" for "actually hangs." Full research trail, upstream PR/issue timeline, and
the unblock condition are tracked there — check it before touching this flag again.

**Separate failure mode — Ornith's broken embedded chat template (FIXED 2026-08-03):** the
original `ornith-1.0-35b-Q8_0.gguf` shipped with a `tokenizer.chat_template` whose
assistant-turn branch rendered `<think>` **unconditionally** (no `last_query_index` guard,
md5 `fa5eb1eee497fc8f3bccc3fcf60d1a51`). Every prior turn's reasoning was re-injected into the
prompt, so on long agentic sessions the model re-read its own stale plan and re-issued the
same tool call — presenting as an **infinite tool-call loop**, plus context inflation per turn
(byte-identical to [HF discussion #31](https://huggingface.co/deepreinforce-ai/Ornith-1.0-35B/discussions/31)).
This is distinct from (and independent of) the full-reprocess issue above. Fixed by swapping to
the unsloth build (`Ornith-1.0-35B-Q8_0.gguf`, template md5 `b3f24e8a...`) **and** forcing
froggeric v21.3 via `--chat-template-file`; see `docs/modes.md`.

**Text-repetition loops → samplers + `preserve_thinking`, not the template (2026-08-03):** a
separate "repeats the same text output" symptom is a **sampling** issue, not the template bug.
Default llama-server samplers are `repeat_penalty 1.0` (off), `dry_multiplier 0.0` (off), temp
1.0 — the exact profile that degenerates into text-repetition loops on reasoning-heavy MoE
models. Ornith now sets `--repeat-penalty 1.08 --dry-multiplier 0.9 --temp 0.7`. **Also
required for Ornith specifically: `--chat-template-kwargs '{"preserve_thinking":false}'`** —
Ornith is Qwen 3.5 lineage, which must NOT preserve historical reasoning; froggeric v21.3's
default `preserve_thinking: true` made the model burn its whole token budget inside `<think>`
with empty `content` (verified live). This is the opposite of `qwen36`/`qwen36-mtp`, which
correctly use `{"preserve_thinking":true}` because Qwen 3.6 is the preserving lineage.

**Third, separate failure mode — reasoning-budget not enforced + reasoning leak (FIXED
2026-08-04):** even with the two fixes above, Ornith's `<think>` length is highly variable
(100–4300+ tokens) and could still blow past `--reasoning-budget` entirely, starving a
small-`max_tokens` client request of any room for `content` — a documented, industry-wide
reasoning-model failure mode (DeepSeek R1, Gemini hit the same class of bug), not specific to
this fleet. A previous agent's attempted fix (`--reasoning off`) was believed deployed but never
actually was — caught via the audit log and a direct `ps` check on the live process, not
assumed. Root-caused instead to two llama.cpp bugs already fixed upstream as of commit
`91c631b21` (2026-07-13): `99f3dc322` (server silently discarded per-request
`reasoning_budget_tokens` overrides) and `91c631b21` itself (reasoning leak specific to
force-opened bare `<think>` templates — directly applicable, froggeric v21.3 is exactly that).
See `docs/modes.md`'s Ornith entry for the full fix/test/deploy writeup. **The "Kintsugi" hybrid-cache
patch below was found to be living only as uncommitted working-tree edits on the deployment host** (no git
history at all, despite the name implying a maintained fork) — now committed
(`fcd4e598d`/rebased to `52bb51280`) and backed up off-host; see that same `docs/modes.md` entry.

## vLLM on Strix Halo APU — PyTorch `hipMalloc` Ceiling

PyTorch's standard allocator uses `hipMalloc`, which is bounded by `hipGetDeviceProperties().totalGlobalMem` (~66 GB on Strix Halo) even though the GTT pool is ~120 GB. `hipMallocManaged` can exceed this limit, but PyTorch does not use it — this is an upstream gap with no runtime workaround.

**Effect on vLLM:** vLLM's `MemorySnapshot` reads `total_memory = 66 GB` from HIP. At `--gpu-memory-utilization 0.98`, requested budget = 64.7 GB. Weights + MoE profiling overhead consume most of this, leaving a small residual for KV cache:

| Model | Weights | MoE overhead | KV available | KV needed (32k ctx) | Result |
|---|---|---|---|---|---|
| Carbon 8B | ~16 GB | none | ~48 GB | 0.5 GB | ✓ fits |
| Hy-MT2 30B-A3B | ~56 GB | ~7 GB | ~1.8 GB | 3.0 GB | ✗ blocked |

MoE profiling overhead scales with `--max-num-batched-tokens` and arises from expert routing buffer allocation during the vLLM profiling forward pass.

**Unblocked by:** PyTorch/ROCm managed-memory allocator for APU targets (no upstream ETA), or llama.cpp adding `hy_v3` architecture support (Hy-MT2 moves to `backend = "vulkan"`).

**Memory display note:** The dashboard's GTT status bar plots `inference_rss_mb` (not the raw `gtt_used_mb` hardware counter) against `gtt_total_mb`. As of the 2026-07-21 fix, `inference_rss_mb` is `gtt_used_mb + unified_mb`, where `gtt_used_mb` is the whole-GPU `rocm-smi` reading (correctly includes ComfyUI and any other classic-GTT-backed process, but does not see unified-memory/HMM allocations — see above) and `unified_mb` is `VmRSS` summed *only* for `llama-server` processes in slots whose mode uses `backend = "rocm"` (`engine._get_unified_memory_rss_mb()`) — the only consumers genuinely invisible to `gtt_used_mb`. Previously this was `max(gtt_used, vmrss_used)` with `vmrss_used` summing *all* `llama-server` processes regardless of backend — additive would have been correct, but `max()` meant that whenever a ROCm+unified slot outweighed `gtt_used_mb` (the common case), every other GTT-backed consumer running alongside it — ComfyUI included — was silently dropped from the total; see `docs/scheduler.md` "ComfyUI as a Reservable Resource" for the full writeup. The weight/KV breakdown (`weights_mb`, `kv_mb`) is still derived from forge env files and does not account for non-forge GPU consumers either way — KV estimate will still be inflated if ComfyUI is co-running (a cosmetic breakdown-attribution quirk, not a budget/OOM-prevention correctness issue).

## Performance Monitoring
- **Hang Detection:** `monitor.py` polls `/metrics` every 4s. 
- **Trigger:** Flagged if `requests > 0` AND `TPS < 0.1` for 90 seconds. 
- **Note:** Older GPU%/Temp heuristics are unreliable for ROCm dual-model contexts.
- **GPU device-lost is NOT a stall — the hang detector misses it entirely (2026-08-16 incident).** When the amdgpu driver loses the compute device (`vk::Queue::submit: ErrorDeviceLost` after a `ring comp_X.Y timeout` → `device wedged`), llama-server *survives*: `/health` stays green, the process keeps running, but **every request errors out instantly**. Because requests fail fast (not hang), `requests_processing` returns to 0 and the stall detector (active-request + ~0 TPS, sustained) never fires. The engine's health check (also `/health`) stays green too. The 2026-08-16 qwen38-27b incident left a slot "healthy" while completely unresponsive for 26 minutes. **Detection must come from the journals** (the kernel ring is `journalctl -k`, llama-server's error is `ErrorDeviceLost` in the `forge-a*` unit journal) or the **router's 5xx count** (a wedged slot 5xxes every request — smith's `SLOT_ERROR_STORM` alert + `gpu_device_lost` check). Recovery is a trivial unload→reload (smith auto-recover). A strong *pre*-hang signal is the engine's `waitGTTDrain` 20s-timeout warning ("GTT still … after 20s") — it fired before both hangs on 2026-08-16 and is now surfaced as `GTT_DRAIN_TIMEOUT`.

## Service Reliability
- **Shutdown Timeout:** `TimeoutStopSec` must be `300`. Large models (80GB+) take several minutes to unload. Default 60s leads to SIGABRT and restart loops.
- **Parallelism:** Never assume `llama-server` defaults; always define `--parallel` to fit within GTT limits.
- **`unload_slot()`'s own wait loop is capped at 60s, not `TimeoutStopSec`'s 300s:** after `systemctl stop`, it polls for `dead`/`failed`/`inactive` substate for at most 60s, then proceeds regardless — force-killing any lingering PID (`_find_lingering_gpu_pids()`) and waiting for GTT drain. In practice a real unload has left orphaned GPU PIDs after `systemctl stop` even for small models (confirmed live, 2026-07-21) — `systemctl`'s own view of the unit can go `inactive` while the actual `llama-server` process is still exiting. Any code reading slot occupancy (`engine._reconcile_slot_state()`, the scheduler, the dashboard) must treat a slot as still occupied until `unload_slot()` itself clears it, not the instant the unit stops being `active` — see `docs/scheduler.md`'s status/state-model fix.

## Debugging Gotchas
- **`curl localhost:5000` vs `curl 127.0.0.1:5000`:** gunicorn binds `0.0.0.0:5000` (IPv4-only — see `gunicorn.conf.py`). `curl`'s hostname resolution for `localhost` tries `::1` first; since nothing listens there, it waits out a connect timeout (~200ms) before falling back to `127.0.0.1`. This adds a flat ~200ms to every request that has nothing to do with the app — enough to fully mask (or fake) a real latency fix. Always benchmark against `127.0.0.1` directly, never `localhost`, when timing dashboard/router/MCP endpoints on this host.
- **One slow request-handler blocks the *entire* dashboard for *every* user — `gunicorn -k gevent -w 1` is a single worker.** Found 2026-07-21: `GET /api/v1/models/cards` (`registry.get_cards()`, called by the Phase 7 PWA's Console page on every mount) was taking 60+ seconds because `_derive_gguf()` → `engine.read_model_metadata()`'s `gguf.GGUFReader` parses the *entire tensor-info table* for every registered model (16 on this host) on every single call, uncached — 2.7-8.1s per model measured live, scaling with tensor count (worst on MoE models: Gemma MoE, GPT-OSS-120B). Because there's only one worker, that one slow request held up literally every other concurrent request — including a completely unrelated page (a user switching to the Headroom tab while Console's request was still in flight saw the *Headroom* page hang, not Console, since the whole server was stuck). Symptom looked page-specific; root cause was global. **Fix:** `registry.py` now caches `_derive_gguf()`'s result per model path, invalidated on file mtime change (so a real re-download/replace still picks up fresh metadata) — see its docstring. First call after a dashboard restart is still slow (cold cache, ~60s) since nothing warms it proactively; every call after that is sub-100ms. **The general lesson, not just this one function:** any handler doing real disk I/O, `subprocess.run`, or CPU-bound parsing without a cache is a full-dashboard outage waiting to happen on this single-worker setup — this is the same class of bug the earlier status/state-model fix (`docs/scheduler.md`) addressed for `/api/v1/status`'s `systemctl` calls; check new endpoints against this before assuming "it's just slow for me."