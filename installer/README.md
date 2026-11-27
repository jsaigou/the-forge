# The Forge Installer

This directory contains the host-provisioning track for The Forge v0.8:

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

1. **Pre-flight** (`installer/preflight.sh`) — reports what differs from the reference platform; it warns and never stops the install.
   Checks: Linux kernel ≥ 6.5 (gfx1151 support), `amdgpu` loaded, Vulkan
   loader + at least one ICD json, ROCm ≥ 6.0 (warn-only if absent),
   GTT pool (fail < 32 GB, warn < 48 GB), RAM (fail below the 64 GB minimum),
   ≥ 250 GB free disk, curl/python3/systemctl present.
2. Shared steps: OS/CPU/GPU/ROCm/llama.cpp/prereqs/model inventory.
   Reference-host specifics (Fedora-only gates, BIOS checklist, uv venv)
   are dev-target only.
3. Systemd unit installation (same units on both targets).
4. **KGC match** (`installer/kgc-match.py`) — on a match writes
   `FORGE_KGC_PROFILE=<id>` to `/etc/sysconfig/forge-kgc`. The scheduler's
   runtime auto-tuning is expected to honour this marker and use the
   profile's verified baselines instead of probing (integration stub — no
   consumer wired yet).
5. **Provisioning prompt** — runs `installer/provision.py --mandatory`
   (downloads can be tens of GB; declined under `--yes`, never surprise-downloaded).
6. **Radiance engine (unofficial fork)** — `installer/radiance.sh`, a step after the llama.cpp
   check, **on by default where the KGC matcher recognises the hardware** (gfx1151 today; `--no-radiance`
   skips it, `--radiance` builds on any host). It fetches the **pinned** tag of
   `github.com/jsaigou/radiance-strix`, refuses a ref that resolves to another commit, builds against
   ROCm 10.x (about 13 minutes), runs the fast tests (informational: failures are compared with the six
   recorded for stock upstream) and fixes permissions and the SELinux label. After the daemon is up it
   registers the build with `forge radiance register-build` (the guarded catalog API, with a 10-minute key
   it revokes). It never fails the install, adds no unit or service, and does not edit the bootloader:
   when `amdgpu.cwsr_enable` is off it prints the exact kernel parameter and the reboot note. It then
   *suggests* the optional `.rad` containers (the manifest's `*-rad` entries) and downloads nothing.
   The pin (`FORGE_RADIANCE_REF`, `FORGE_RADIANCE_COMMIT`, `FORGE_RADIANCE_BASELINE_FAILS`) lives at the top
   of `installer/radiance.sh`; a release moves those together. Tests: `bash installer/test_radiance.sh`.
7. Health check + next-steps summary (mint key, open dashboard, `forge tui`).

## Language models

On the public target the installer creates the four model slots (`a1` to `a4`) with their launch scripts, sets the models folder, **builds
llama.cpp** when no `llama-server` is there, and registers it. A model added on the Models page appears at once; load it from the Console.

- The build is a pinned upstream commit with Vulkan (it also runs on the processor), in `/opt/forge/llama.cpp`; it takes several minutes. It needs
  `git cmake gcc-c++ make glslc vulkan-headers vulkan-loader-devel spirv-headers-devel` (Debian/Ubuntu: `git cmake g++ make glslc libvulkan-dev
  spirv-headers`); the installer names what is missing, finishes, and builds on the next run. `--no-llama` leaves it out. A server that is there is
  never replaced (`FORGE_LLAMA_VULKAN_BIN` for another place).
- Models above about 63 GB need a **ROCm** build (Vulkan cannot load them). The installer adds one in `build-rocm` where it finds an AMD GPU and
  the ROCm development files (`rocm-hip-devel hipblas-devel rocblas-devel`; Debian/Ubuntu: `hipcc libamdhip64-dev libhipblas-dev librocblas-dev`;
  or AMD's own packages in `/opt/rocm*`). It takes much longer than the Vulkan build. Without them the install finishes and says what to add.
  `FORGE_ROCM_TARGET=gfx1151` names the GPU type when it cannot be read.
- Downloads appear at once because a new install sets `catalog.show_new_downloads`; set it to `false` to review them first.
- The smallest supported machine has 64 GB of memory. The default reserves (`infra.memory`: OS floor 2, growth margin 4, host floor 8 GB) leave
  about 49 GB for models there, provided the GPU's memory pool (`amdgpu.gttsize`) is at least that large.

## Speech and Voice Studio

```bash
sudo ./install.sh --target=public --speech=default            # kokoro, zonos, qwen-voicedesign (only kokoro can be built so far)
sudo ./install.sh --target=public --speech=kokoro             # preset voices: about 0.35 GB of weights and 1 GB of packages
sudo -u forge /opt/forge/forge speech install kokoro          # the same engine step on its own (--dry-run, --stage, --replace)
```

- Needed for Kokoro: `uv`, `cmake`, a C++ compiler and `make` (the Japanese front end is compiled). The installer never runs a package manager: it
  says which packages to install, skips the step, and finishes. Run it again afterwards; a re-run changes nothing that is already in place.
- The speech service's key is written to a mode-0600 file and never printed. `forge speech key rotate` replaces it; restart `forge-tts` after.
- Voice Studio is installed with speech (`--no-voice-studio` leaves it out) once a release is pinned in `installer/voice-studio.sh`. It runs on
  `127.0.0.1:8090` and **has no sign-in**.
- Platform differences (another GPU, another distribution, little memory) are warnings. Without systemd (WSL with it off) files are installed and no
  service is started.

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

The Radiance containers (`qwen36-35b-a3b-w4w5-rtn-rad`, `qwen36-35b-a3b-w4w5-gptq-rad`,
`qwen38-flash-next-ud-size-rad`) are `optional` entries with pinned hashes, fetched with
`--only <id>`; after the download add a config for the file in Settings → Catalog.

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
relabels, uv strict-mode package verification.
