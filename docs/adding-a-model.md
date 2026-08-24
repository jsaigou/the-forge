# Adding a Model to The Forge

**Example:** Qwen2.5-Coder-7B-Instruct-Q8_0

This guide walks through every step to download a GGUF model and wire it into The Forge as a new switchable mode.

---

## Step 1 — Download the model file

Models live in `models_dir` from `config.toml` (`/opt/forge/models` on ForgeHost). Download from Hugging Face using `hf` (the current CLI — `huggingface-cli` is deprecated) or `wget`/`curl`.

SSH to ForgeHost first:

```bash
tailscale ssh forgehost
```

Then download:

```bash
cd /opt/forge/models

# Using hf (preferred — handles auth and resumable downloads)
hf download \
  Qwen/Qwen2.5-Coder-7B-Instruct-GGUF \
  qwen2.5-coder-7b-instruct-q8_0.gguf \
  --local-dir .
```

Or with `wget` if you have the direct URL:

```bash
wget -c "https://huggingface.co/Qwen/Qwen2.5-Coder-7B-Instruct-GGUF/resolve/main/qwen2.5-coder-7b-instruct-q8_0.gguf"
```

Verify the file landed correctly:

```bash
ls -lh /opt/forge/models/qwen2.5-coder-7b-instruct-q8_0.gguf
```

---

## Step 2 — Add a mode block to config.toml

Edit `/etc/forge/config.toml` on ForgeHost. Add the new `[modes.<key>]` block **above** `[modes.unloaded]` (keep `unloaded` last; `creative` is a service mode and not relevant to placement):

```toml
[modes.qwen25coder-7b]
label       = "Qwen 2.5 Coder 7B"
description = "Qwen2.5-Coder-7B-Instruct Q8_0 — fast local coder, 32K context"
icon        = "qwen.svg"
color       = "#00A884"
tags        = ["Coding", "Fast", "Small"]

  [[modes.qwen25coder-7b.services]]
  unit       = "forge-primary"
  port_role  = "primary"
  model      = "qwen2.5-coder-7b-instruct-q8_0.gguf"
  alias      = "qwen25coder-7b"
  context    = 32768
  mmproj     = ""
  extra_args = [
    "--no-mmap", "--jinja",
    "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
    "--flash-attn", "on", "--threads", "16",
    "--parallel", "4",
  ]
```

### Key fields

| Field | What it controls |
|---|---|
| `model` | Path relative to `models_dir` — filename only for flat files, subdirectory path for sharded models |
| `context` | KV cache is allocated for this many tokens. Check the model card. Never set higher than tested. |
| `alias` | The `model_alias` llama-server advertises on `/v1/models` |
| `extra_args` | Appended verbatim to the llama-server command line |
| `mmproj` | Vision projector — leave `""` if the model has no multimodal support |
| `port_role` | `"primary"` → port 8080. Use `"secondary"` only for dual-model modes. |
| `llama_bin` | (optional) Override the llama-server binary path. Use when a model needs a custom build (e.g. `puzzle-port` branch). Defaults to the backend-appropriate binary. |
| `backend` | (mode-level) `"vulkan"`, `"rocm"`, or `"vllm"`. Propagated to services automatically. |

### `--parallel` and context budget

`--parallel N` means N simultaneous request slots, each consuming a full KV cache. For a 7B Q8_0 model at 32K context this is cheap — `--parallel 4` is safe. For 70B+ models at 256K context, use `--parallel 1` or `2`.

---

## Step 3 — Verify the config parses

On ForgeHost:

```bash
python3 -c "
import tomllib
with open('/etc/forge/config.toml', 'rb') as f:
    cfg = tomllib.load(f)
print(list(cfg['modes'].keys()))
"
```

The new key (`qwen25coder-7b`) should appear in the list. If you get a `TOMLDecodeError`, fix the syntax before continuing.

---

## Step 4 — Restart the dashboard

The engine reads config.toml at startup. Restarting the dashboard picks up the new mode without touching inference:

```bash
sudo systemctl restart forge-dashboard.service
```

Verify the new mode appears in the ops UI at `ops.example.ts.net`.

---

## Step 5 — Switch to the new mode and verify

```bash
# Load onto a slot via the forge CLI, or use the dashboard's load action
forge load qwen25coder-7b [slot]
```

Or via the dashboard API:

```bash
curl -sf -X POST http://localhost:5000/api/switch \
  -H "Content-Type: application/json" \
  -d '{"mode": "qwen25coder-7b"}'
```

Then verify context loaded correctly:

```bash
curl -sf http://localhost:8080/props | python3 -c "
import sys, json
d = json.load(sys.stdin)
n = d.get('n_ctx') or d.get('default_generation_settings', {}).get('n_ctx', 0)
print(f'n_ctx={n}')
"
```

Expected output: `n_ctx=32768`

If `n_ctx` is lower than configured, GTT contiguous allocation silently fell back. Run `journalctl -u forge-primary.service -n 50` and look for KV cache allocation warnings.

---

## Step 6 — Smoke-test inference

```bash
curl -sf http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen25coder-7b",
    "messages": [{"role": "user", "content": "Write a Python hello world."}],
    "max_tokens": 128
  }' | python3 -m json.tool
```

---

## Sharded models

If the GGUF is split across multiple files (e.g. three shards), point `model` at the first shard only — llama-server discovers the rest automatically by suffix pattern:

```toml
model = "qwen25coder-7b/Q8_0/Qwen2.5-Coder-7B-Instruct-Q8_0-00001-of-00003.gguf"
```

Download all shards into the same directory before switching.

---

## Dual-model modes

To pair the new model as a secondary worker alongside an existing primary (e.g. in a planner/worker setup), add a second `[[modes.<key>.services]]` block:

```toml
[modes.coding-7b]
label       = "Coding (Dual 7B)"
description = "Gemma 4 31B (planner) + Qwen2.5-Coder-7B (worker)"
icon        = "code.svg"
color       = "#1DA462"
tags        = ["Dual Model", "Coding", "Fast Worker"]

  [[modes.coding-7b.services]]
  unit       = "forge-primary"
  port_role  = "primary"
  model      = "gemma4-31b-abliterated-Q6_K.gguf"
  alias      = "gemma4-31b"
  context    = 65536
  mmproj     = "gemma4-mmproj/mmproj-31b-F32.gguf"
  extra_args = [
    "--no-mmap", "--jinja",
    "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
    "--flash-attn", "on", "--threads", "16",
    "--parallel", "1", "--ctx-checkpoints", "0",
  ]

  [[modes.coding-7b.services]]
  unit       = "forge-secondary"
  port_role  = "secondary"
  model      = "qwen2.5-coder-7b-instruct-q8_0.gguf"
  alias      = "qwen25coder-7b"
  context    = 32768
  mmproj     = ""
  extra_args = [
    "--no-mmap", "--jinja",
    "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
    "--flash-attn", "on", "--threads", "16",
    "--parallel", "4",
  ]
```

The secondary service runs on port 8081 (`a2.example.ts.net`).

---

## FIM / autocomplete models (always-on, outside mode switching)

Some models serve a different purpose than main inference: **fill-in-the-middle (FIM) autocomplete** for VS Code Continue ghost text. These run on a dedicated port (8082) via `forge-autocomplete.service` and are never managed by the mode-switching engine.

### Which models support FIM

FIM requires a model specifically trained with FIM tokens. Not all instruction models qualify:

| Model | FIM support | Notes |
|---|---|---|
| **Qwen2.5-Coder-7B (base)** | ✅ Yes | `<\|fim_prefix\|>`, `<\|fim_suffix\|>`, `<\|fim_middle\|>` — recommended. Use base, not instruct. |
| CodeGemma 2B (base) | ✅ Yes | Lightweight, ~2 GB Q4. Use `codegemma-2b`, not `codegemma-2b-it`. |
| Gemma 4-2B | ❌ No | Instruction model only — use as a chat mode, not autocomplete |
| Qwen3-Coder-Next | ✅ Yes | Already in config; too large for always-on autocomplete role |

### llama-server args for autocomplete

Autocomplete requests are short. Use a small context and more parallel slots:

```toml
context    = 4096     # FIM doesn't need large context
extra_args = [
  "--no-mmap", "--flash-attn", "on", "--threads", "8",
  "--cache-type-k", "q8_0", "--cache-type-v", "q8_0",
  "--parallel", "4",
  "--metrics",
]
```

### Why not a mode in config.toml?

Autocomplete must always be available regardless of which primary mode is active. Wiring it into the mode-switching engine would make it disappear when switching modes. The autocomplete service is a separate systemd unit on port 8082, outside the engine entirely.

See `docs/autocomplete-plan.md` for the full deployment plan and Continue config.

---

## Removing a mode

1. Delete the `[modes.<key>]` block and its `[[modes.<key>.services]]` entries from config.toml.
2. Restart `forge-dashboard.service`.
3. The model file in `/opt/forge/models/` is not touched — delete it manually if you want to reclaim disk.

Do not remove a mode that is currently active. Switch to another mode first.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `n_ctx` lower than configured | GTT contiguous block too small | Reduce `context`, reboot to defragment GTT, or switch to a smaller quant |
| `cudaMalloc failed: out of memory` | Missing `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` | Verify `/etc/sysconfig/forge-primary-env` contains this var |
| Mode not visible in dashboard after restart | TOML parse error | Run the Step 3 parse check |
| `forge-primary` restarts in a loop after stop | `TimeoutStopSec=60` too short | Apply `TimeoutStopSec=300` (see CLAUDE.md pending task) |
| Model loads but alias is wrong | `alias` field mismatch | Check `alias` in config.toml matches what your client sends as `model` |
