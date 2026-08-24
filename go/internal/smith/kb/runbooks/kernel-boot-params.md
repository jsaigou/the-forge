ref: runbook:kernel-boot-params
doc: runbook
slug: kernel-boot-params
title: Missing amdgpu kernel boot-parameter mitigations
category: gpu
source: docs/bios-setup.md (hand-authored runbook, not an extraction)

`smith`'s `kernel_params` check (`internal/smith/checks.go`'s `runKernelParams`) reads
`/proc/cmdline` and warns when either of two amdgpu mitigations pinned in AGENTS.md
("Kernel mitigations are applied") is missing:

- `amdgpu.mcbp=0` — disables mid-command-buffer preemption. Without it, GPU queue eviction
  errors can occur during long prefills (80K+ token prompts), which can crash the inference
  service mid-request.
- `amdgpu.vm_fragment_size=9` — sets the VM fragment size to 2 MB pages, improving the odds of
  a contiguous GTT allocation. Without it, large-context loads are more likely to hit
  `docs/pitfalls.md`'s "Contiguous Allocation" failure mode (kernel silently downscales `n_ctx`
  rather than erroring — see the `silent-context-reduction` KB entry).

A third parameter, `amdgpu.gttsize=122880`, sets the GTT pool ceiling itself (~120 GB) — it is
not part of this check today (a missing/small GTT pool shows up as a `gtt_ceiling` finding
instead, from actual measured usage against `gtt_total_bytes`), but it lives in the same GRUB
line and is worth confirming while here.

**This is guidance only — smith never edits the bootloader.** A GRUB kernel-parameter change
needs a reboot to take effect, which is well outside any action kind smith is allowed to
propose (`docs/v5-smith.md` §4.6's action kinds are all live-daemon operations: load/unload a
slot, restart a forge unit, a settings change — none of them touch the kernel command line).

**Fix (real GRUB path — confirm before editing on a different host):**

1. Check what's actually applied: `cat /proc/cmdline | tr ' ' '\n' | grep amdgpu`
2. Edit `/etc/default/grub`'s `GRUB_CMDLINE_LINUX` to include all three parameters:
   ```
   amdgpu.gttsize=122880 amdgpu.mcbp=0 amdgpu.vm_fragment_size=9
   ```
3. Regenerate the GRUB config (`grub2-mkconfig -o /boot/grub2/grub.cfg` on Fedora-family, or the
   distro-appropriate equivalent) and reboot.
4. After reboot, re-run smith's `kernel_params` check (or `POST /api/v1/smith/checks/run` with
   `check_ids: ["kernel_params"]`) to confirm it now reports `ok`, and separately confirm the
   GTT pool actually grew: `rocm-smi --showmeminfo gtt` should read ~119–122 GB.

**If the GTT pool is still small after the reboot**, the kernel parameter alone isn't the whole
story — check the BIOS GART aperture setting too (`docs/bios-setup.md`'s "GART Aperture / UMA
Frame Buffer Size" section): a large GART reservation (e.g. 4 GB instead of the recommended
512 MB minimum) steals headroom from the GTT pool regardless of what `amdgpu.gttsize` says.
