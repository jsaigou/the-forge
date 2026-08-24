# The Forge Installer

This directory contains the host-provisioning track for The Forge v0.5:

```
install.sh                    dual-target installer (dev | public)
installer/preflight.sh        hardware/software pre-flight checks (library + CLI)
installer/assets.manifest.json  model asset manifest (mandatory + optional)
installer/provision.py        asset download/verify/installer
installer/kgc/kgc.json        KGC hardware-profile registry index
installer/kgc/profiles/*.json verified per-hardware profiles + baselines
installer/kgc-match.py        match local hardware to a KGC profile
```

## Whole flow (public target)

```bash
sudo ./install.sh --target=public
```

1. **Pre-flight** (`installer/preflight.sh`) — MUST pass unless `--force`.
   Checks: Linux kernel ≥ 6.5 (gfx1151 support), `amdgpu` loaded, Vulkan
   loader + at least one ICD json, ROCm ≥ 6.0 (warn-only if absent),
   GTT pool (fail < 48 GB, warn < 96 GB), RAM (fail < 96 GB, warn < 128 GB),
   ≥ 250 GB free disk, curl/python3/systemctl present.
2. Shared steps: OS/CPU/GPU/ROCm/llama.cpp/prereqs/model inventory.
   Reference-host specifics (Fedora-only gates, BIOS checklist, PairNode
   probe, uv venv) are dev-target only.
3. Systemd unit installation (same units on both targets).
4. **KGC match** (`installer/kgc-match.py`) — on a match writes
   `FORGE_KGC_PROFILE=<id>` to `/etc/sysconfig/forge-kgc`. The scheduler's
   runtime auto-tuning is expected to honour this marker and use the
   profile's verified baselines instead of probing (integration stub — no
   consumer wired yet).
5. **Provisioning prompt** — runs `installer/provision.py --mandatory`
   (downloads can be tens of GB; declined under `--yes`, never surprise-downloaded).
6. Health check + next-steps summary (mint key, open dashboard, `forge tui`).

## Pre-flight

```bash
installer/preflight.sh              # human-readable PASS/WARN/FAIL table
installer/preflight.sh --json       # machine-readable
```

Exit codes: 0 = pass (warnings allowed), 1 = any FAIL, 2 = usage error.
Sourced by `install.sh` (`preflight_run`, `pf_print_table`, `pf_summary`).

## Provisioning models

Manifest schema (`assets.manifest.json`): entries carry `id`, `role`
(`mandatory`|`optional`), optional `type: "service"` (no files — deployed as a
unit instead), `hf_repo` (null = TBD upstream), `gguf` flag, `dest_rel_path`
(resolved against `--root`, mirroring the reference host's
`models/<family>/<model>/<file>` layout) and a `files` array of
`{filename, sha256|null, size_bytes|null}`.

```bash
sudo installer/provision.py                       # mandatory entries only
sudo installer/provision.py --all                 # everything
sudo installer/provision.py --only gemma4-26b-a4b
sudo installer/provision.py --root /var/lib/forge # where models/<...> lands
installer/provision.py --dry-run                  # show plan, touch nothing
sudo installer/provision.py --force               # re-download verified files
```

Behaviour: downloads from
`https://huggingface.co/<repo>/resolve/main/<file>` with HTTP Range resume +
exponential-backoff retry; verifies sha256 (streamed, when non-null) and the
`GGUF` magic (first 4 bytes) for gguf entries; installs atomically
(`*.part` then rename); idempotent — complete+verified files are SKIPped
unless `--force`. Entries with null `hf_repo`/`filename` report PENDING and
are skipped. Exit codes: 0 OK/skipped, 1 failures, 2 usage/manifest error.

> **TBD:** most `sha256` / `size_bytes` / some `hf_repo` values are `null`
> pending first verified downloads — pin them in the manifest as they land.

## KGC profiles

`kgc-match.py` detects CPU (`/proc/cpuinfo`, fallback `lscpu`) and GPU
(KFD topology gfx id, fallback `lspci` AMD vendor scan) and matches them
against the regexes in `kgc/kgc.json`. Exit codes: **0** match (profile id on
stdout), **1** no match, **2** environment/error. Set `KGC_DEBUG=1` to print
the detected signals on stderr.

`profiles/strix-halo-gfx1151.json` is the live-verified Strix Halo profile:
recommended kernel params (`amdgpu.mcbp=0 amdgpu.vm_fragment_size=9`), GTT
ceiling, and per-model performance baselines. Baselines marked
`"source": "pending-benchmark"` have not been measured yet.

## Dev target

`sudo ./install.sh` (default) preserves the historical reference-host flow:
strict Fedora/kernel gates, BIOS checklist, `/opt/forge` paths, SELinux
relabels, uv strict-mode package verification, PairNode ComfyUI probe.
