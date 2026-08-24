ref: runbook:build-refresh
doc: runbook
slug: build-refresh
title: Refresh an inference build (rebase fork, dual-build, test, promote, deploy)
category: build
source: hand-authored runbook (2026-08-17 session — qwen38-27b device-lost investigation, kintsugi rebase, ROCm 10.1 upgrade)

This is the full, repeatable procedure for taking a llama.cpp fork (or a
tracked binary) from "behind upstream / stale" to "rebuilt, tested, promoted,
deployed." It encodes everything the 2026-08-17 session did to qwen38-27b's
kintsugi build and the ROCm upgrade. Follow the steps in order; each has a
Why (so a less-capable reasoning model understands the intent, not just the
command) and a Verify (so you know the step worked before moving on).

## 0. Before starting — confirm scope

- `smith` check `binary_versions` reports the fork's source tree ahead of (or
  diverged from) its installed build, OR you're chasing a specific model bug.
- Identify the fork's upstream remote (`git remote -v` in the fork root) and
  the branch you run (`git branch --show-current`, or `git log -1 --oneline`).
- Record the current installed version before touching anything:
  `<fork>/<builddir>/bin/llama-server --version` (save the `commit <sha>`).

## 1. Fetch upstream and rebase the fork

Why: llama.cpp moves fast; a fork is only valuable for its *own* patches on
top of current upstream. Rebasing keeps those patches fresh and pulls in bug
fixes (the 2026-08-16 qwen38-27b device-lost was compounded by running a
1-month-old base).

```bash
cd <fork-root>                       # e.g. /opt/<org>/llama.cpp-<fork>
git fetch upstream                  # or `git fetch origin`
git checkout <your-branch>          # e.g. rebase/reasoning-fix
git rebase upstream/master          # expect conflicts in patched files
```

- Why: a fork patch written against old mainline often conflicts with newer
  code (e.g. the kintsugi checkpoint patch hit `server-context.cpp` when
  upstream moved `metrics.on_prediction`).
- Verify: `git log --oneline -1` shows your fork commit on top of a recent
  upstream sha; `git status` clean after resolving conflicts.
- If the fork's *entire* purpose has been upstreamed, retire it instead of
  rebasing: search upstream for the feature (`git show origin/master:<file>`
  or `git log --oneline -S '<unique-string>' origin/master`) — if present, the
  fork may be deletable and the model moved to a stock build.

## 2. Build BOTH backends in fresh dirs (never overwrite the live build)

Why: the 2026-08-17 decision was "build vulkan AND rocm, test both, promote the
winner" — never guess which backend is better. Fresh dirs (`build-vulkan-new`,
`build-rocm-new`) so a broken build can't take down the running slot.

### Vulkan
```bash
cmake -S . -B build-vulkan-new \
  -DCMAKE_BUILD_TYPE=Release -DGGML_VULKAN=ON \
  -DGGML_NATIVE=ON -DGGML_OPENMP=ON
cmake --build build-vulkan-new -j32
```

### ROCm (against a specific ROCm-TheRock install)

`<ROCM_ROOT>` below is the ROCm-TheRock install root for the tree being built
(deployment data — the fork registry's `lib_dir_substring` carries it).
```bash
cmake -S . -B build-rocm-new \
  -DCMAKE_BUILD_TYPE=Release \
  -DGGML_HIP=ON -DAMDGPU_TARGETS=gfx1151 \
  -DGGML_HIP_GRAPHS=ON -DGGML_HIP_MMQ_MFMA=ON -DGGML_HIP_NO_VMM=ON \
  -DGGML_HIP_RCCL=OFF -DGGML_HIP_UMA=ON \
  -DROCM_PATH=<ROCM_ROOT> \
  -DCMAKE_HIP_COMPILER=<ROCM_ROOT>/bin/amdclang++ \
  -DCMAKE_PREFIX_PATH=<ROCM_ROOT> \
  -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON \
  '-DCMAKE_INSTALL_RPATH=$ORIGIN;<ROCM_ROOT>/lib' \
  -DGGML_NATIVE=ON -DGGML_OPENMP=ON
cmake --build build-rocm-new -j32
```

- **RPATH gotcha (the 2026-08-17 deploy outage):** the `-DCMAKE_INSTALL_RPATH`
  value MUST be quoted with the literal `$ORIGIN` — the shell expands
  `$ORIGIN` to empty if you don't single-quote it. Without `$ORIGIN`, the
  binary can't find its sibling `libllama-server-impl.so` and dies at exec.
  Verify the cache actually kept it:
  `grep CMAKE_INSTALL_RPATH: build-rocm-new/CMakeCache.txt` must show
  `$ORIGIN;<ROCM_ROOT>/lib` (the `$ORIGIN` literal, not a
  leading `;`).
- **The same trap hits Vulkan builds, via a different mechanism (2026-08-17
  `forge-a2` exit-127):** the default Vulkan build config (`build-vulkan`,
  no RPATH flags) bakes an ABSOLUTE RUNPATH to the configure-time tree path
  (e.g. `<tree>/build-vulkan/bin:`). A tree rename then makes
  the loader fail on `libllama-server-impl.so` even though the file is right
  next to the binary. ALWAYS build Vulkan with `$ORIGIN` too:
  `cmake -S . -B build-vulkan-new -DGGML_VULKAN=ON ... -DCMAKE_BUILD_WITH_INSTALL_RPATH=ON '-DCMAKE_INSTALL_RPATH=$ORIGIN'`.
  Verify with `readelf -d build-vulkan-new/bin/llama-server | grep RUNPATH`
  (must show `[$ORIGIN]`) and a clean-env `ldd` (no stale absolute paths).
- **Clean-env ldd (the 7.13→7.15 silent-nothing trap):** a ROCm binary that
  "works" under `LD_LIBRARY_PATH` may still be resolving the WRONG ROCm. Check
  in a truly clean environment:
  `env -i HOME=$HOME PATH=/usr/bin:/bin ldd build-rocm-new/bin/llama-server`
  and confirm `libamdhip64`/`hipblas`/`rocblas`/`hsa-runtime` all resolve to
  the intended `<ROCM_ROOT>/lib`, not a stale
  `rocm-therock-7.15` or ComfyUI's Python ROCm SDK.
- **WMMA note:** as of llama.cpp PR #26046 (2026-07-24) the rocWMMA
  FlashAttention kernel and `GGML_HIP_ROCWMMA_FATTN` option are gone upstream —
  do NOT pass that flag. (The Poolside/laguna fork is the exception and still
  requires `-DGGML_HIP_ROCWMMA_FATTN=ON`.)

## 3. Smoke test each build before wiring it up

```bash
<builddir>/bin/llama-server --version          # must print the new commit
env -i HOME=$HOME PATH=/usr/bin:/bin ldd <builddir>/bin/llama-server 2>/dev/null | grep -iE 'hsa-runtime|amdhip'
# launch on a scratch port, load a small model, confirm /health:
<builddir>/bin/llama-server -m <small-model.gguf> -ngl 999 --port 8099 -c 512 &
curl -sf localhost:8099/health   # {"status":"ok"}
```

## 4. Reliability test — the exact scenario that caused the incident

Why: the 2026-08-16 qwen38-27b hangs happened on a **giant cold prefill**
(~120K tokens submitted immediately after load), which is the documented
KFD/GPU-hang risk. A build is only trusted after it survives that.

### Sizing the giant prefill — and the RAM ceiling behind it

Size the prefill to the **full ~120K-token scenario** (~480K chars at 4
chars/token) whenever the canary's configured context can hold it, and
scale down to **~75% of what the canary can actually hold** for smaller
contexts — a prompt the canary's own context cannot hold returns HTTP 400
before the test exercises anything. Budget characters conservatively: real
technical text lands near ~5–5.5 chars/token, not the oft-quoted 4 — a
"480K-char" prompt is ~88K real tokens, which at ~35–55 t/s prompt
processing runs **30–40 minutes** on the 1M-context canary class; the
automated procedure gives the prefill call its own 60-min budget
(`buildRefreshGiantPrefillHTTP`) for exactly this. If the canary's context
is unknown, prefill the FULL scenario and let it fail loudly — never shrink
silently.

**The capacity constraint (Sprint 6, reproduced live):** prefill cost is
weights + KV for the submitted tokens, so a ~90 GB model at 1M context can
exceed a 128 GB unified-memory host's RAM **whenever any other model is
co-resident**. The observed failure shape is a memory-thrash hang, not a
build defect: prefill throughput collapses stepwise (83 → 36 → 9.5 t/s,
then zero progress), RSS climbs past ~100 GB into swap, GPU use falls to 0%
while the process spins. When you see that signature: it is host capacity —
SIGKILL the hung slot unit, let the revert run, and retry with a smaller
canary context or with co-resident models unloaded first. Don't condemn
the build for it.

1. Load the model onto a free slot via the catalog config (register a test
   config, or repoint the real one — see step 6) and wait for `/health`.
   Before the giant prefill, confirm the host has room for weights + prefill
   KV — unload co-resident models where the maintenance window allows it.
2. **Giant cold prefill:** submit ONE chat completion sized per the guidance
   above (the full ~120K-token scenario where the canary's context holds
   it, scaled to ~75% of context for smaller canaries; e.g. a repeated
   technical paragraph) with `max_tokens: 8`. It must return 200 and
   eventually complete — for 1M-context canaries "eventually" means tens
   of minutes, so use a matching client timeout.
3. **Multi-turn cache reuse:** for kintsugi/hybrid models, a second turn in
   the same slot must show a small `prompt eval time` (tens of tokens, NOT a
   full reprocess of the conversation) and log `Kintsugi: saved generation
   checkpoint`.
4. **Unload/reload cycling:** unload and reload the slot 3×, confirming
   `/health` + a tiny completion each time.
5. Check for the failure signatures:
   `journalctl -k | grep -iE 'ring.*timeout|device wedged'`
   `journalctl -u forge-a<N> | grep -iE 'ErrorDeviceLost|vk::Queue::submit'`
   Zero hits = pass.

## 5. Performance test — pick the winner

Why: "prefer ROCm when tied" is a standing fleet tiebreaker (ROCm fails OOM
cleanly in-process; Vulkan can OOM-kill the adjacent unit). Run the same
generation on both builds and compare.

```bash
# decode t/s via /metrics deltas (prompt ~200 tokens, max_tokens ~300):
B=$(curl -sf localhost:8088/metrics | awk '/^llamacpp:tokens_predicted_total /{print $2}')
S=$(curl -sf localhost:8088/metrics | awk '/^llamacpp:tokens_predicted_seconds_total /{print $2}')
curl -sf -X POST localhost:8088/v1/completions -d '{"prompt":"<long prompt>","max_tokens":300}'
A=$(curl -sf localhost:8088/metrics | awk '/^llamacpp:tokens_predicted_total /{print $2}')
E=$(curl -sf localhost:8088/metrics | awk '/^llamacpp:tokens_predicted_seconds_total /{print $2}')
python3 -c "print(f\"{(($A-$B)/(($E-$S))):.1f} t/s\")"
# giant-prefill t/s comes from the reliability test's journal:
journalctl -u forge-a<N> | grep 'prompt processing' | tail -1   # ".../ N tokens per second"
```

## 6. Promote the winner + repoint configs

1. Register the winning build in the catalog: insert a `builds` row
   (`engine_id=1`, name, `binary_path` → the new dir's `bin/llama-server`,
   backend, reason) and repoint every config that should use it
   (`UPDATE configs SET build_id=<new> WHERE name IN (...)`). SIGHUP
   forge (`systemctl kill -s HUP forge-daemon`) to reload.
2. Re-verify EVERY repointed mode loads and serves (each model has its own
   quirks — e.g. nemotron-puzzle needs the puzzle fork's `get_key_or_arr`
   tolerance for `expert_used_count`; current mainline rejects that GGUF's
   array form with "wrong type arr but expected type u32").
3. Keep test configs (`*-vk`, `*-rocm`) around as switchable alternatives;
   delete them only after the winner is proven in production.

## 7. Clean up old builds

- Confirm nothing references the old build first:
  `SELECT DISTINCT build_id FROM configs;` + `pgrep -af '<old-builddir>'`
- Delete the catalog `builds` row AND the on-disk dir (both — a dangling row
  or a dead dir are both traps). `du -sh` before/after to confirm the win.

## 8. Deploy forge (SELinux trap — the 2026-08-17 outage)

Why: replacing `/opt/forge/forge` via scp/cp drops the file's SELinux
context to the default (`usr_t`), and systemd then refuses to exec it with
`status=203/EXEC` — the daemon won't come back. The original binary carries
`system_u:object_r:bin_t:s0`.

```bash
# build: (from go/ module root)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/forge ./cmd/forge
scp /tmp/forge <deploy-host>:/tmp/forge
ssh <deploy-host> '
  cp /opt/forge/forge /opt/forge/forge.prev-$(date +%Y%m%d)
  install -m 0755 /tmp/forge /opt/forge/forge
  chcon -u system_u -r object_r -t bin_t /opt/forge/forge   # THE critical step
  ls -Z /opt/forge/forge        # must show bin_t
  systemctl restart forge-daemon'
# verify: all three listeners (dashboard :5000, a0 :8085, mcp :8095) + /v1/models count
```

Prefer the repo helper `scripts/deploy-forge.sh` which does install +
relabel + restart in one step.
