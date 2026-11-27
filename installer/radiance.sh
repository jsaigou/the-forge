#!/usr/bin/env bash
# installer/radiance.sh — the Radiance engine (unofficial fork) step of install.sh.
#
# Sourced by install.sh (and by installer/test_radiance.sh, which drives it with stub tools).
# Design and decisions: docs/radiance-installer-and-catalog-plan.md. What it does, in order:
#
#   rad_step            decide (auto: only on hardware the KGC matcher recognises), check prerequisites,
#                       fetch the PINNED tag, verify the commit hash, build, run the fast tests
#                       (informational: compared with the failures recorded for the tag), fix
#                       permissions and the SELinux label.
#   rad_register_build  after the daemon is up: register the build in the catalog through
#                       `forge radiance register-build` (the guarded API; a short-lived key it revokes).
#   rad_offer_models    print the optional container downloads (never downloads anything itself).
#
# It never fails the install: every problem is a warning and the llama.cpp/Vulkan stack is complete
# without it. It never adds a unit, a service, a sudo helper or a venv. It does not edit the
# bootloader: when cwsr_enable is off it prints the exact kernel-parameter line and the reboot note.
#
# Expects install.sh's helpers (ok/warn/info/step) and variables (TARGET, DRY_RUN, SCRIPT_DIR,
# ROCM_ROOT) to exist. Every knob is an environment variable so a release moves one string.

# ── The pin: move these three together, in the same commit (and record the fast-test baseline). ──
RADIANCE_REPO="${FORGE_RADIANCE_REPO:-https://github.com/jsaigou/radiance-strix.git}"
RADIANCE_REF="${FORGE_RADIANCE_REF:-strix-halo-r2}"
RADIANCE_COMMIT="${FORGE_RADIANCE_COMMIT:-a16657e8d61f9ae7a8aabfebe2a9d0dfbf942cf9}"
# The six fast tests that fail identically on stock upstream at this tag (the fork's fork-notes/07):
# the libhalo/libavx schema mismatch, the video-decode test and an avx_check int4 case.
RADIANCE_BASELINE_FAILS="${FORGE_RADIANCE_BASELINE_FAILS:-avx_check avx_check_general convert_test mm_test plugin_test spark25_convert_test}"
RADIANCE_GFX="${FORGE_RADIANCE_GFX:-gfx1151}"
RADIANCE_MIN_FREE_GIB="${FORGE_RADIANCE_MIN_FREE_GIB:-15}"
# on = build even where the matcher does not recognise the hardware; off = skip; auto = matcher decides.
RADIANCE_MODE="${RADIANCE_MODE:-auto}"
RADIANCE_BUILT=0          # set to 1 once a usable build directory exists (registration needs it)
RADIANCE_BUILD_DIR=""

rad_default_dir() {
    if [[ "${TARGET:-dev}" == "dev" ]]; then echo "${FORGE_RADIANCE_DIR:-/opt/radiance/${RADIANCE_REF}}"
    else echo "${FORGE_RADIANCE_DIR:-/opt/forge/radiance/${RADIANCE_REF}}"; fi
}

# Run a command, or print it under --dry-run. Output of a real run goes to the log when given.
rad_run() {
    if [[ "${DRY_RUN:-0}" -eq 1 ]]; then info "[dry-run] would run: $*"; return 0; fi
    "$@"
}

# 0 = this host should get the engine. auto asks the KGC matcher (the same one that picks the hardware
# profile): it recognises gfx1151 today.
rad_wanted() {
    case "${RADIANCE_MODE}" in
        off) info "Radiance engine skipped (--no-radiance)"; return 1 ;;
        on)  return 0 ;;
    esac
    local out rc matcher="${RAD_KGC_MATCH_CMD:-python3 ${SCRIPT_DIR:-.}/installer/kgc-match.py}"
    out="$(${matcher} 2>/dev/null)" && rc=0 || rc=$?
    if [[ $rc -eq 0 && -n "$out" ]]; then
        info "Hardware profile ${out}: the Radiance engine is built by default here"
        return 0
    fi
    info "Radiance engine skipped: this hardware is not one the fork supports (${RADIANCE_GFX}); pass --radiance to build anyway"
    return 1
}

# The ROCm root to build against: ROCM_ROOT if set, else what pf_find_rocm found ("path version").
rad_rocm() {
    if [[ -n "${FORGE_RADIANCE_ROCM:-}" && -d "${FORGE_RADIANCE_ROCM}" ]]; then echo "${FORGE_RADIANCE_ROCM}"; return 0; fi
    if [[ -n "${ROCM_ROOT:-}" && -d "${ROCM_ROOT}" ]]; then echo "${ROCM_ROOT}"; return 0; fi
    if declare -F pf_find_rocm >/dev/null; then
        local loc; loc="$(pf_find_rocm 2>/dev/null)" || return 1
        [[ -n "$loc" ]] && { echo "${loc%% *}"; return 0; }
    fi
    return 1
}

rad_hip_compiler() {
    local r="$1"
    for c in "${r}/lib/llvm/bin/clang++" "${r}/llvm/bin/clang++" "${r}/bin/hipcc"; do
        [[ -x "$c" ]] && { echo "$c"; return 0; }
    done
    return 1
}

# Free GiB on the filesystem that will hold `path` (its nearest existing parent).
rad_free_gib() {
    local p="$1"
    while [[ ! -d "$p" && "$p" != "/" ]]; do p="$(dirname "$p")"; done
    df -Pk "$p" 2>/dev/null | awk 'NR==2 { printf "%d\n", $4/1048576 }'
}

# Names of the fast tests that failed, from `ctest` output ("  51 - mm_test (Failed)").
rad_failed_tests() {
    sed -n -E 's/^[[:space:]]*[0-9]+ - ([A-Za-z0-9_.-]+) \((Failed|Timeout|SEGFAULT|Exception).*/\1/p' | sort -u
}

# Failures that are not in the recorded baseline (one per line).
rad_new_failures() {
    local baseline=" ${RADIANCE_BASELINE_FAILS} " t
    while read -r t; do
        [[ -z "$t" ]] && continue
        [[ "$baseline" == *" $t "* ]] || echo "$t"
    done
}

rad_cwsr_note() {
    local f=/sys/module/amdgpu/parameters/cwsr_enable v
    if [[ -r "$f" ]]; then v="$(cat "$f")"; else v="unreadable"; fi
    if [[ "$v" == "1" ]]; then ok "amdgpu cwsr_enable is 1 (the engine's host check passes)"; return; fi
    warn "amdgpu cwsr_enable is '${v}', and the engine refuses to serve without it. It cannot be changed at runtime: add"
    warn "  amdgpu.cwsr_enable=1"
    warn "to the kernel command line (your bootloader's config), then reboot once. This installer does not edit the bootloader."
}

# Everything before the daemon exists: decide, check, fetch, verify, build, test, permissions.
rad_step() {
    step "Radiance engine (unofficial fork, pinned ${RADIANCE_REF})"
    rad_wanted || return 0

    local missing=() t
    for t in git cmake ninja c++; do command -v "$t" >/dev/null 2>&1 || missing+=("$t"); done
    local rocm hipcc=""
    rocm="$(rad_rocm)" || rocm=""
    if [[ -z "$rocm" ]]; then missing+=("a ROCm install (set ROCM_ROOT)")
    else hipcc="$(rad_hip_compiler "$rocm")" || missing+=("a HIP compiler under ${rocm}"); fi
    if (( ${#missing[@]} > 0 )); then
        warn "Radiance engine not built: missing ${missing[*]}. Install them and re-run, or pass --no-radiance to silence this."
        return 0
    fi

    local dir free
    dir="$(rad_default_dir)"
    free="$(rad_free_gib "$dir")"
    if [[ -n "$free" && "$free" -lt "$RADIANCE_MIN_FREE_GIB" ]]; then
        warn "Radiance engine not built: ${free} GiB free for ${dir}, ${RADIANCE_MIN_FREE_GIB} needed for the tree and build"
        return 0
    fi
    rad_cwsr_note

    # Already built at the pinned commit? Then there is nothing to do but register it.
    if [[ -d "${dir}/.git" && -x "${dir}/build/bin/radiance" ]] &&
       [[ "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" == "$RADIANCE_COMMIT" ]]; then
        ok "Radiance ${RADIANCE_REF} already built at ${dir}/build (commit ${RADIANCE_COMMIT:0:7})"
        RADIANCE_BUILT=1; RADIANCE_BUILD_DIR="${dir}/build"
        return 0
    fi

    info "Fetching ${RADIANCE_REPO} @ ${RADIANCE_REF} into ${dir}"
    if [[ "${DRY_RUN:-0}" -eq 1 ]]; then
        info "[dry-run] would run: git clone --branch ${RADIANCE_REF} --depth 1 ${RADIANCE_REPO} ${dir}"
        info "[dry-run] would verify HEAD is ${RADIANCE_COMMIT}"
        info "[dry-run] would run: cmake -S ${dir} -B ${dir}/build -G Ninja -DCMAKE_BUILD_TYPE=Release -DRAD_GPU_TARGETS=${RADIANCE_GFX} -DCMAKE_PREFIX_PATH=${rocm} -DCMAKE_HIP_COMPILER=${hipcc} -DCMAKE_HIP_ARCHITECTURES=${RADIANCE_GFX}"
        info "[dry-run] would run: cmake --build ${dir}/build  (about 13 minutes at 12 jobs), then ctest -L fast (informational)"
        info "[dry-run] would set the plugin directory non-writable by group/other, label bin/ and radiance_home/ bin_t when SELinux is on, and register the build after the daemon starts"
        RADIANCE_BUILD_DIR="${dir}/build"
        return 0
    fi

    mkdir -p "$(dirname "$dir")" || { warn "Radiance engine not built: cannot create $(dirname "$dir")"; return 0; }
    if [[ -e "$dir" ]]; then
        warn "Radiance engine not built: ${dir} exists but is not a build of ${RADIANCE_COMMIT:0:7}; move it aside and re-run"
        return 0
    fi
    if ! git -c advice.detachedHead=false clone --quiet --branch "$RADIANCE_REF" --depth 1 "$RADIANCE_REPO" "$dir"; then
        warn "Radiance engine not built: could not fetch ${RADIANCE_REPO} @ ${RADIANCE_REF} (offline? tag missing?)"
        return 0
    fi
    local got; got="$(git -C "$dir" rev-parse HEAD)"
    if [[ "$got" != "$RADIANCE_COMMIT" ]]; then
        warn "Radiance engine not built: ${RADIANCE_REF} resolved to ${got:0:12}, not the pinned ${RADIANCE_COMMIT:0:12}. A retagged ref is refused; ${dir} is left for inspection."
        return 0
    fi
    ok "Fetched ${RADIANCE_REF} (commit ${got:0:7}, matches the pin)"

    info "Configuring and building (this takes a while) ..."
    local log="${dir}/install-build.log"
    if ! { cmake -S "$dir" -B "${dir}/build" -G Ninja -DCMAKE_BUILD_TYPE=Release \
              -DRAD_GPU_TARGETS="${RADIANCE_GFX}" -DCMAKE_PREFIX_PATH="${rocm}" \
              -DCMAKE_HIP_COMPILER="${hipcc}" -DCMAKE_HIP_ARCHITECTURES="${RADIANCE_GFX}" \
          && cmake --build "${dir}/build" -j"$(nproc)"; } >"$log" 2>&1; then
        warn "Radiance engine build failed; see ${log} (the rest of the install is unaffected)"
        return 0
    fi
    ok "Radiance engine built: ${dir}/build/bin/radiance"

    # The fast suite is informational. Compare the failing set with the recorded baseline.
    local tlog="${dir}/install-tests.log" newf
    ( cd "${dir}/build" && ctest -L fast -j"$(nproc)" ) >"$tlog" 2>&1 || true
    newf="$(rad_failed_tests <"$tlog" | rad_new_failures | tr '\n' ' ')"
    if [[ -z "${newf// /}" ]]; then
        ok "Fast tests: no failures beyond the recorded stock-upstream baseline (${RADIANCE_BASELINE_FAILS// /, })"
    else
        warn "Fast tests: failures not in the recorded baseline: ${newf}(see ${tlog}); the build is kept, but treat it as unverified"
    fi

    # The engine dlopens whatever it finds under radiance_home: nothing untrusted may write there.
    chown -R "${RADIANCE_OWNER:-testuser:testuser}" "$dir" 2>/dev/null || true
    chmod -R go-w "${dir}/build/radiance_home" "${dir}/build/bin" 2>/dev/null || true
    if command -v chcon >/dev/null 2>&1 && [[ "$(getenforce 2>/dev/null)" != "Disabled" ]]; then
        chcon -R -t bin_t "${dir}/build/bin" "${dir}/build/radiance_home" 2>/dev/null \
            && ok "SELinux: engine binaries and plugins labelled bin_t" \
            || warn "Could not label ${dir}/build for SELinux; run: chcon -R -t bin_t ${dir}/build/bin ${dir}/build/radiance_home"
    fi
    RADIANCE_BUILT=1; RADIANCE_BUILD_DIR="${dir}/build"
}

# After the daemon is healthy: register the build. Idempotent (the command reports an existing identical row).
rad_register_build() {
    [[ -n "$RADIANCE_BUILD_DIR" ]] || return 0
    local rocm_ver name forge="${FORGE_BIN:-/opt/forge/forge}"
    rocm_ver="$(basename "$(rad_rocm 2>/dev/null || echo rocm)" | sed -E 's/^rocm-?(therock-)?//')"
    name="radiance-unofficial/${RADIANCE_REF}-rocm${rocm_ver:+-${rocm_ver}}"
    if [[ "${DRY_RUN:-0}" -eq 1 ]]; then
        info "[dry-run] would run: ${forge} radiance register-build -path ${RADIANCE_BUILD_DIR} -name ${name}"
        return 0
    fi
    [[ "$RADIANCE_BUILT" -eq 1 ]] || return 0
    if "$forge" radiance register-build -path "$RADIANCE_BUILD_DIR" -name "$name" -db "${FORGE_DB:-/var/lib/forge/forge.db}"; then
        ok "Radiance build registered in the catalog as '${name}'"
    else
        warn "Could not register the Radiance build; once the daemon is up run: ${forge} radiance register-build -path ${RADIANCE_BUILD_DIR} -name ${name}"
    fi
}

# Suggest (never download) the published containers; the user picks what fits their memory.
rad_offer_models() {
    [[ "$RADIANCE_BUILT" -eq 1 || "${DRY_RUN:-0}" -eq 1 ]] || return 0
    [[ -n "$RADIANCE_BUILD_DIR" ]] || return 0
    local root; root="$(dirname "${MODELS_DIR:-/var/lib/forge/models}")"
    echo -e "\n    ${BOLD:-}Radiance containers (optional, published on Hugging Face by jsaigou)${NC:-}"
    info "Qwen3.6-35B-A3B W4/W5 (RTN)  21 GiB:  python3 installer/provision.py --only qwen36-35b-a3b-w4w5-rtn-rad  --root ${root}"
    info "Qwen3.6-35B-A3B W4/W5 (GPTQ) 21 GiB:  python3 installer/provision.py --only qwen36-35b-a3b-w4w5-gptq-rad --root ${root}"
    info "Qwen3.8-Flash-Next (UD size) 84 GiB:  python3 installer/provision.py --only qwen38-flash-next-ud-size-rad --root ${root}"
    info "Then add a config for the file in Settings → Catalog (Add Model); the engine is already registered."
}
