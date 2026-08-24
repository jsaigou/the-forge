ref: modes:critical-runtime-flags
doc: modes
slug: critical-runtime-flags
title: Critical Runtime Flags
category: config
source: docs/modes.md

## Critical Runtime Flags
- **`--parallel`**: `1` for every mode except `qwen25coder` (`4`, intentional — see incident write-up above). Do not set to anything other than 1 for a mode meant to serve one long conversation at full context; see the incident section above for why.
- **`--ctx-checkpoints`**: only `swallow-32b`/`swallow-8b` set `0`. Everything else that sets it explicitly uses `16`; several modes (`gpt-oss-120b`, `nemotron`, `nemotron-nano`, `nemotron-puzzle`, `qwen25coder`, `qwen3coder-q6`) don't set it at all (llama.cpp default, currently 32 per `docs/investigations.md`). **Older doc claims of a blanket `--ctx-checkpoints 0` policy across all modes were never true of the live config** — verified against both the pre-migration V4 `config.toml` and the current v0.5 `forge.toml` on 2026-07-23; treat any doc that asserts otherwise as stale. Also the mechanism hybrid/recurrent models would need for cross-turn cache reuse — see `docs/pitfalls.md` before changing it; open upstream hang bugs make this unsafe to flip as of 2026-07 (`docs/investigations.md` item 9).
- **mmproj**: gemma4-mtp uses `gemma4-26b-a4b-qat-hauhau-balanced-mtp/mmproj-Gemma4-26B-A4B-QAT-Uncensored-HauhauCS-Balanced-BF16.gguf`; qwen36 and qwen36-mtp both use `mmproj-qwen36-35b-BF16.gguf` (vision confirmed working with MTP enabled on mainline). **Vision only** — mmproj is an image/video-frame projector with no audio encoder; video input is sampled as frames and the audio track is never read. None of these modes can be used for STT.
- **MTP flags** (`qwen36-mtp`): `--spec-type draft-mtp --spec-draft-n-max 3 --parallel 1`. Requires a GGUF with embedded MTP head tensors. Merged into mainline llama.cpp (PR #22673, 2026-05-16). **Confirmed ~79 t/s (1.7× production qwen36) on this host 2026-05-19.** The HauhauCS abliterated Q6_K_P has no MTP tensors; this mode uses the stock GGUF. Download: `hf download havenoammo/Qwen3.6-35B-A3B-MTP-GGUF Qwen3.6-35B-A3B-MTP-UD-Q4_K_XL.gguf --local-dir <models dir>`
