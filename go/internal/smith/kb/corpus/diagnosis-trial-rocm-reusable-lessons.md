ref: diagnosis-trial:rocm-reusable-lessons
doc: diagnosis-trial
slug: rocm-reusable-lessons
title: Reusable lessons
category: build
source: docs/diagnosis-trial.md

## Reusable lessons

1. **A "latest available" package index can itself be behind** — cross-check the exact version
   string against the project's own issue tracker before trusting a mirror is current.
2. **`ldd`/health-check success with `LD_LIBRARY_PATH` set proves nothing about how the binary
   will actually run under systemd** (or any launcher that doesn't set it). Always verify
   resolution in a truly empty environment (`env -i`) if that's how production will invoke it.
3. **CMake's automatic build-RPATH only covers a target's direct link dependencies** — a shared
   library your executable depends on transitively can silently lose its own RPATH, especially
   when the missing dependency is treated as a "system" package (common for vendor SDKs like
   TheRock's ROCm libraries via `find_package`). Don't assume "the main binary resolves
   correctly" implies "every `.so` in the dependency chain does too."
4. **`patchelf` on an already-linked ELF can corrupt it** if the new RPATH string doesn't fit in
   existing padding — prefer fixing the RPATH at CMake/link time and forcing a real relink over
   post-hoc binary patching.
5. **`CMAKE_INSTALL_RPATH` alone does nothing during a normal build** — it requires
   `CMAKE_BUILD_WITH_INSTALL_RPATH=ON` as a companion, and that flag makes the explicit value
   *replace* (not extend) the automatic per-target RPATH, so `$ORIGIN` has to be included
   explicitly to keep sibling-library resolution working.
6. **RUNPATH/RPATH baked as an absolute path breaks if the containing directory is renamed** —
   using `$ORIGIN`-relative paths (or a symlink at the canonical location, if the absolute form
   is unavoidable) survives directory reorganization.
