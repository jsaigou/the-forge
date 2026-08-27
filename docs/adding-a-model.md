# Adding a Model to The Forge

**Example:** Qwen2.5-Coder-7B-Instruct-Q4_K_M

There are three ways to get a model into the catalog, depending on what you're adding:

1. **The Add Model tab** (recommended for any GGUF model on HuggingFace) - search, rank,
   pre-flight, download, and auto-register in one flow. No file editing, no restart.
2. **Ask smith** - the same engine, driven from chat instead of the search UI.
3. **Manual catalog entry** (Settings → Catalog) - for a non-GGUF model (vLLM/safetensors), a
   file you already have locally, or any case where you want full control over every row before
   it exists.

There is no config file to edit for any of these - the catalog (models, variants, artifacts,
configs) lives entirely in the store-backed database. Nothing in this repo reads a config file
for catalog data.

---

## Path 1 - Add Model tab

Go to **Models → Add Model**.

1. **Search.** Type a model name or `org/repo` and hit enter - this searches HuggingFace
   directly (GGUF-tagged repos, sorted by downloads).
2. **Pick a repo.** Selecting a result fetches its real (recursive) file tree and ranks every
   GGUF file against this host's live memory budget - a 1.2× VRAM headroom rule, preferring
   `_L` over `_M` and `IQ`/`UD` variants at equal size. The recommended file is starred.
3. **Select a file.** This runs pre-flight: disk headroom (with a 10% margin), whether the file
   needs the ROCm+unified-memory backend (over the ~63 GB Vulkan ceiling) or Vulkan is fine, and
   whether the destination path already exists. A blocked check (not enough disk, an existing
   file) disables the Download button; a warning (won't fit in VRAM right now) doesn't - you can
   still download a model you can't load yet.
4. **Download.** Real, resumable - pause and resume continue from the on-disk byte offset rather
   than restarting from zero. A model split across shards (`…-00001-of-00003.gguf` and so on)
   downloads as a single job; you select one file and every shard comes with it. Progress, speed,
   and ETA show live in the Downloads list at the top of the tab.
5. **Done - automatically.** Once every shard verifies (checksum when known, and a mandatory GGUF
   header read for every `.gguf` file), the model registers itself: a new Model, Variant,
   Artifact, and Config row, with defaults derived from the file itself:
   - `n_ctx` = the model's own trained context length (never guessed, never inflated)
   - `--parallel 1` - a hard rule, because `--parallel N` *splits* the configured context
     across N slots rather than multiplying it, so a mode meant to serve one long conversation
     at full context must stay at `1`
   - `--no-mmap --jinja --flash-attn on --threads 16` (the universal llama.cpp flags every other
     mode already uses)
   - the Build (Vulkan or ROCm) that actually matches the file's size

   The new Config lands **`status=unverified`, `visibility=hidden`** - it exists for review, not
   for immediate use. It will not appear in the dashboard's mode switcher until you promote it.

6. **Promote it.** Open the new Config in Settings → Catalog, review the generated fields (context,
   flags, backend), and flip `visibility` to `visible` once you're satisfied. This is the same
   review step every other catalog entry goes through - nothing here bypasses it.

### Gated repos

License click-through models (Gemma, Llama, and similar) need a HuggingFace access token.
Set one in **Settings → Security → HuggingFace access token** before searching or downloading a
gated repo - without one, a gated repo's tree/download calls fail closed with a clear "needs a
token" error rather than a bare transport failure.

### Repointing an existing config instead of registering a new one

If you already have a Config for a model and just want to swap in a different quant or a fresher
build of the same weights, pick it from the **"Repoint an existing config"** dropdown that appears
once you select a file in the Add Model tab, instead of leaving it on "register as a new model."
This repoints that config's existing weight artifact at the newly downloaded file rather than
creating a new Model/Variant/Config. A typo'd/stale name is rejected immediately, before any
download work starts - but nothing checks that the new file is actually the same model family, so
only repoint to a genuinely compatible build, and note that the config's quantization label,
context size, and any mmproj/sharded companion files are left untouched by a repoint.

The download API/smith's `download_start` tool accept the same `config_name` field directly:

```bash
curl -sf -X POST http://localhost:5000/api/v1/hf/downloads \
  -H "Authorization: Bearer sk-forge-..." -H "Content-Type: application/json" \
  -d '{"repo":"org/repo","files":[{"filename":"model-Q5_K_M.gguf","size_bytes":1234567890}],"config_name":"qwen25coder-7b"}'
```

---

## Path 2 - Ask smith

The same engine is exposed as tools smith can call from a chat turn: `hf_search`, `hf_preflight`,
and `download_status` are read-only - smith can look things up and report back freely.
`download_start` is the one exception, and deliberately narrow: it only ever creates a job in
`pending_approval`. Nothing downloads until you see it and approve it (Approve button in the
Downloads list, or `POST /api/v1/hf/downloads/{id}/approve`) - smith cannot cause a real download
on its own, the same propose→approve pattern every other action smith can take already follows.

---

## Path 3 - Manual catalog entry (non-GGUF, or a file you already have)

The acquisition engine only understands GGUF files fetched from HuggingFace. For a vLLM/safetensors
model, or a GGUF file you already have on disk from somewhere else:

1. Place the file under the models directory (check the live value with
   `forge config get infra.paths` - e.g. `/opt/forge/models` on a default install).
2. Settings → Catalog → **New Model** → **New Variant** → **New Artifact** (point `file_path` at
   the file, relative to the models directory) → **New Config** (pick the Engine/Build, set
   `n_ctx`/`--parallel`/extra args yourself - nothing derives these for you on this path).
3. Verify the config parses and loads correctly (see Verifying below) before setting
   `visibility=visible`.

---

## Verifying a new config

After promoting a config (any path above), load it onto a slot and confirm the context actually
took:

```bash
forge load <config-name> [slot]
curl -sf http://localhost:8080/props | jq .default_generation_settings.n_ctx
```

(`8080` is `a1`'s configured port on a default install - check `forge status` or
`GET /api/v1/catalog/slots` if you're not sure which port a given slot uses.) If `n_ctx` comes
back lower than configured, the kernel silently failed a contiguous GTT allocation - see
`docs/pitfalls.md`'s GTT section.

---

## FIM / autocomplete models

Fill-in-the-middle (FIM) autocomplete for editor ghost text (e.g. Continue in VS Code) is meant
to run as a separate, always-on service outside the mode-switching engine entirely, so it stays
available regardless of which config is loaded on A1–A4. Check the service's own unit file for
its current port and status before relying on the specifics here.

FIM requires a model specifically trained with FIM tokens - not every instruction model qualifies
(Qwen2.5-Coder-7B **base**, not instruct, is a solid choice; CodeGemma 2B base is a lighter
alternative).

---

## Removing a model

Settings → Catalog → open the Model or Config → **Delete**. Deleting a Model cascades to its
Variants and Artifacts; a Config still referencing an Artifact blocks the Artifact/Variant/Model
delete until the Config itself is removed first (409, "has dependent … - delete those first").
The model file on disk is never touched by a catalog delete - remove it from the models directory
yourself if you want to reclaim the space.

Do not remove a config that's currently loaded on a slot. Unload it first.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `n_ctx` lower than configured | GTT contiguous block too small | Reduce `n_ctx`, reboot to defragment GTT, or switch to a smaller quant |
| `cudaMalloc failed: out of memory` (ROCm backend) | Missing `GGML_CUDA_ENABLE_UNIFIED_MEMORY=ON` | Verify `/etc/sysconfig/forge-a{1,2,3,4}-env` all contain this var - see `docs/pitfalls.md` |
| Add Model preflight blocks on disk | Not enough free space for the file + 10% margin | Free up space, or pick a smaller quant |
| Add Model preflight warns "requires rocm" | File is over the ~63 GB Vulkan ceiling | Expected for large models - the generated Config will need a ROCm Build to actually load |
| Download fails immediately, "gated or private" | No HuggingFace token configured, or the account hasn't accepted the repo's license | Settings → Security → set a token; accept the license on huggingface.co if you haven't |
| New config doesn't appear in the dashboard switcher | It's still `visibility=hidden` (the default for a freshly auto-registered config) | Settings → Catalog → open it → set visibility to visible |
| Model loads but the client's `model` field doesn't match | The Config's name/alias doesn't match what the client sends | Check the Config's name in Settings → Catalog |
