#!/usr/bin/env bash
# installer/llama.sh — builds llama.cpp's llama-server, which runs the language models, on a public install (docs/speech-setup-plan.md section 14).
# Distribution packages can be too old for the options the Forge passes, so a pinned upstream commit is built: the one the reference host runs. A
# Vulkan build is always made (it also runs on the processor where there is no GPU); a ROCm build is added where an AMD GPU and the ROCm development
# files are present, because Vulkan cannot load models above about 63 GB. Sourced by install.sh, after host.sh. It never fails the install and never
# overwrites a server that is there.

# The pin. A release moves it together with the reference host.
FORGE_LLAMA_REPO="${FORGE_LLAMA_REPO:-https://github.com/ggml-org/llama.cpp}"
FORGE_LLAMA_COMMIT="${FORGE_LLAMA_COMMIT:-a91eab67d8389d979d29fdf6633b2edd0718eb4b}" # b12030

LLAMA_DNF="git cmake gcc-c++ make glslc vulkan-headers vulkan-loader-devel spirv-headers-devel"
LLAMA_APT="git cmake g++ make glslc libvulkan-dev spirv-headers"
LLAMA_ROCM_DNF="rocm-hip-devel hipblas-devel rocblas-devel"
LLAMA_ROCM_APT="hipcc libamdhip64-dev libhipblas-dev librocblas-dev"
LLAMA_DISK_MB=4096

llama_missing_tools() {
    local tool missing=()
    for tool in git cmake c++ make glslc; do command -v "$tool" &>/dev/null || missing+=("$tool"); done
    [[ -f "${FORGE_VULKAN_HEADER:-/usr/include/vulkan/vulkan.h}" ]] || missing+=("Vulkan headers")
    [[ -f "${FORGE_SPIRV_HEADER:-/usr/include/spirv/unified1/spirv.hpp}" ]] || missing+=("SPIR-V headers")
    (( ${#missing[@]} == 0 )) && return 0
    echo "${missing[*]}"
}

# The GPU types to compile ROCm code for ("gfx1151", several joined by ";"), read from the kernel without starting anything on the GPU.
# Empty when there is no AMD GPU the compute driver knows. FORGE_ROCM_TARGET overrides it.
llama_gpu_targets() {
    if [[ -n "${FORGE_ROCM_TARGET:-}" ]]; then echo "${FORGE_ROCM_TARGET}"; return 0; fi
    local f v vendor targets=""
    for f in "${FORGE_KFD_NODES:-/sys/class/kfd/kfd/topology/nodes}"/*/properties; do
        [[ -r "$f" ]] || continue
        vendor="$(awk '$1 == "vendor_id" {print $2}' "$f" 2>/dev/null)"
        v="$(awk '$1 == "gfx_target_version" {print $2}' "$f" 2>/dev/null)"
        [[ "$vendor" == 4098 && "$v" =~ ^[1-9][0-9]*$ ]] || continue # processors report 0
        # 110501 is major 11, minor 5, stepping 1: gfx1151. Minor and stepping are written in hex (90010 is gfx90a).
        v="$(printf 'gfx%d%x%x' $(( v / 10000 )) $(( v / 100 % 100 )) $(( v % 100 )))"
        [[ ";$targets;" == *";$v;"* ]] || targets="${targets:+$targets;}$v"
    done
    echo "$targets"
}

# Whether a llama-server was built with ROCm: it names the HIP libraries itself, or they sit beside it (a shared build). The file is only read.
llama_is_rocm_server() {
    local bin="$1" lib
    for lib in "$(dirname "$bin")"/libggml-hip*.so*; do [[ -e "$lib" ]] && return 0; done
    grep -aqE 'libamdhip64|libggml-hip' "$bin" 2>/dev/null
}

# llama_rocm_default <the installer's path> <where earlier installers looked>: the ROCm server of a public install when none is named. A ROCm build
# made by hand at the old place keeps being used; any other server there is not one, and must not stand in the way of building it.
llama_rocm_default() {
    local ours="$1" old="$2"
    if [[ -x "$old" ]]; then
        if llama_is_rocm_server "$old"; then
            info "Using the ROCm llama-server already at ${old}" >&2
            echo "$old"
            return 0
        fi
        info "The server at ${old} is not a ROCm build, so it is not used as one (FORGE_LLAMA_ROCM_BIN names another)" >&2
    fi
    echo "$ours"
}

# The first ROCm installation that has what llama.cpp links against: AMD's packages in /opt, or the distribution's in /usr.
llama_rocm_root() {
    local r p lib have
    local -a places
    if [[ -n "${FORGE_ROCM_PLACES:-}" ]]; then
        read -r -a places <<<"${FORGE_ROCM_PLACES}"
    else
        places=("${ROCM_PATH:-}" "${ROCM_ROOT:-}" /opt/rocm /opt/rocm-* "${FORGE_ROCM_SYSTEM:-/usr}")
    fi
    for r in "${places[@]}"; do
        while [[ "$r" == */ ]]; do r="${r%/}"; done # "/usr/" is /usr
        [[ -n "$r" && -d "$r" ]] || continue
        have=0
        for p in hip hipblas rocblas; do
            for lib in lib lib64 lib/x86_64-linux-gnu; do
                if [[ -f "$r/$lib/cmake/$p/$p-config.cmake" ]]; then have=$(( have + 1 )); break; fi
            done
        done
        if (( have == 3 )); then echo "$r"; return 0; fi
    done
    return 1
}

# One compile job needs up to about 2 GB. A container may allow far less than the machine shows, so its limits count too.
llama_jobs() {
    local cpus mem quota="" period="" limit="" cg="${FORGE_CGROUP:-/sys/fs/cgroup}"
    cpus="$(nproc 2>/dev/null || echo 2)"
    mem="$(awk '/MemAvailable/ {print int($2/1048576/2)}' "${FORGE_MEMINFO:-/proc/meminfo}" 2>/dev/null)"
    mem="${mem:-1}"
    if [[ -r "$cg/cpu.max" ]]; then read -r quota period <"$cg/cpu.max" || true; fi
    if [[ "$quota" =~ ^[0-9]+$ && "$period" =~ ^[1-9][0-9]*$ ]] && (( quota / period < cpus )); then cpus=$(( quota / period )); fi
    if [[ -r "$cg/memory.max" ]]; then read -r limit <"$cg/memory.max" || true; fi
    if [[ "$limit" =~ ^[0-9]+$ ]] && (( limit / 2147483648 < mem )); then mem=$(( limit / 2147483648 )); fi
    (( cpus < mem )) && mem=$cpus
    (( mem < 1 )) && mem=1
    echo "$mem"
}

# Fetches the pinned source into the staging directory, once for both builds. Returns 1 after saying why it could not.
llama_fetch() {
    local label="$1" staging="$2" src="$2/src" got
    [[ "${LLAMA_FETCHED:-0}" == 1 ]] && return 0
    if ! { run_as "${SERVICE_USER}" rm -rf "$staging" && run_as "${SERVICE_USER}" mkdir -p "$src" \
            && run_as "${SERVICE_USER}" git -C "$src" init -q \
            && run_as "${SERVICE_USER}" git -C "$src" fetch -q --depth 1 "${FORGE_LLAMA_REPO}" "${FORGE_LLAMA_COMMIT}" \
            && run_as "${SERVICE_USER}" git -C "$src" -c advice.detachedHead=false checkout -q FETCH_HEAD; }; then
        skip "$label" "could not fetch ${FORGE_LLAMA_COMMIT:0:12} from ${FORGE_LLAMA_REPO}"
        return 1
    fi
    got="$(run_as "${SERVICE_USER}" git -C "$src" rev-parse HEAD)"
    if [[ "$got" != "${FORGE_LLAMA_COMMIT}" ]]; then
        skip "$label" "the fetched commit is ${got:0:12}, not the pinned ${FORGE_LLAMA_COMMIT:0:12}; nothing was built"
        return 1
    fi
    LLAMA_FETCHED=1
}

# llama_build <label> <build directory name> <ROCM_PATH or ""> <cmake options...>: compiles into staging, checks the server takes the options the
# Forge starts it with, and moves it into place.
llama_build() {
    local label="$1" name="$2" rocm="$3"; shift 3
    local home="${ENGINE_ROOT}/llama.cpp" staging="${ENGINE_ROOT}/.staging/llama.cpp"
    local src="$staging/src" build="$staging/$name" log="$staging/$name.log" bin="${ENGINE_ROOT}/llama.cpp/$name/bin/llama-server"
    local free jobs
    free="$(df -Pm "${ENGINE_ROOT}" 2>/dev/null | awk 'NR==2 {print $4}')"
    if [[ -n "$free" ]] && (( free < LLAMA_DISK_MB )); then
        skip "$label" "${free} MB free under ${ENGINE_ROOT}, ${LLAMA_DISK_MB} MB needed"
        return 0
    fi
    llama_fetch "$label" "$staging" || return 0
    jobs="$(llama_jobs)"
    info "Building ${label} ${FORGE_LLAMA_COMMIT:0:12} with ${jobs} jobs (several minutes; log: ${log}) ..."
    # The log is written by the service user, which owns the staging directory.
    if ! run_as "${SERVICE_USER}" sh -c '
            src="$1" build="$2" log="$3" jobs="$4" rocm="$5"; shift 5
            if [ -n "$rocm" ]; then export ROCM_PATH="$rocm" HIP_PATH="$rocm"; else unset ROCM_PATH HIP_PATH; fi
            cmake -S "$src" -B "$build" -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=ON \
                -DLLAMA_BUILD_UI=OFF -DLLAMA_USE_PREBUILT_UI=OFF -DLLAMA_OPENSSL=OFF -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF \
                "$@" >"$log" 2>&1 \
            && cmake --build "$build" --target llama-server -j "$jobs" >>"$log" 2>&1' llama-build "$src" "$build" "$log" "$jobs" "$rocm" "$@"; then
        tail -15 "$log" >&2 || true
        skip "$label" "the build failed (see ${log})"
        LLAMA_KEEP_STAGING=1
        return 0
    fi
    # The check reads the help text only, so the GPU is hidden from it.
    local help
    help="$(run_as "${SERVICE_USER}" env HIP_VISIBLE_DEVICES=-1 "$build/bin/llama-server" --help 2>&1)" || true
    if [[ "$help" != *"--load-mode"* || "$help" != *"--flash-attn"* ]]; then
        skip "$label" "the built server does not run or lacks an option the Forge needs; it was left in ${build}"
        LLAMA_KEEP_STAGING=1
        return 0
    fi
    if ! { run_as "${SERVICE_USER}" mkdir -p "$home" \
            && { [[ ! -e "$home/$name" ]] || run_as "${SERVICE_USER}" mv "$home/$name" "$home/$name.prev-$(date +%Y%m%d-%H%M%S)"; } \
            && run_as "${SERVICE_USER}" mv "$build" "$home/$name"; }; then
        skip "$label" "the build could not be moved to ${home}/${name}"
        LLAMA_KEEP_STAGING=1
        return 0
    fi
    label_bin "$bin"
    ok "llama-server built: ${bin}"
}

llama_vulkan() {
    local home="${ENGINE_ROOT}/llama.cpp" bin="${ENGINE_ROOT}/llama.cpp/build-vulkan/bin/llama-server"
    if [[ -x "${LLAMA_VULKAN_BIN}" ]]; then
        ok "llama-server is there already: ${LLAMA_VULKAN_BIN} (left as it is)"
        return 0
    fi
    if [[ "${LLAMA_VULKAN_BIN}" != "$bin" ]]; then
        skip "llama.cpp" "FORGE_LLAMA_VULKAN_BIN names ${LLAMA_VULKAN_BIN}, which is not there; the installer only builds into ${home}"
        return 0
    fi
    local missing; missing="$(llama_missing_tools)"
    if [[ -n "$missing" ]]; then
        skip "llama.cpp" "missing ${missing}; $(pkg_hint "$LLAMA_DNF" "$LLAMA_APT") and run the installer again"
        return 0
    fi
    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would build llama.cpp ${FORGE_LLAMA_COMMIT:0:12} with Vulkan into ${home}/build-vulkan (several minutes)"
        return 0
    fi
    llama_build "llama.cpp" build-vulkan "" -DGGML_VULKAN=ON
}

llama_rocm() {
    local home="${ENGINE_ROOT}/llama.cpp" bin="${ENGINE_ROOT}/llama.cpp/build-rocm/bin/llama-server"
    if [[ -x "${LLAMA_ROCM_BIN}" ]]; then
        ok "ROCm llama-server is there already: ${LLAMA_ROCM_BIN} (left as it is)"
        return 0
    fi
    local targets; targets="$(llama_gpu_targets)"
    if [[ -z "$targets" ]]; then
        info "No ROCm build: no AMD GPU was found. If there is one, name its type (for example FORGE_ROCM_TARGET=gfx1151) and run the installer again."
        return 0
    fi
    if [[ "${LLAMA_ROCM_BIN}" != "$bin" ]]; then
        skip "llama.cpp (ROCm)" "FORGE_LLAMA_ROCM_BIN names ${LLAMA_ROCM_BIN}, which is not there; the installer only builds into ${home}"
        return 0
    fi
    local root; root="$(llama_rocm_root)" || root=""
    if [[ -z "$root" ]]; then
        info "No ROCm build: the ROCm development files were not found. Models above about 63 GB need one: $(pkg_hint "$LLAMA_ROCM_DNF" "$LLAMA_ROCM_APT") (or install AMD's ROCm) and run the installer again."
        return 0
    fi
    local missing=() tool
    for tool in git cmake c++ make; do command -v "$tool" &>/dev/null || missing+=("$tool"); done
    if (( ${#missing[@]} )); then
        skip "llama.cpp (ROCm)" "missing ${missing[*]}; $(pkg_hint "git cmake gcc-c++ make" "git cmake g++ make") and run the installer again"
        return 0
    fi
    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would build llama.cpp ${FORGE_LLAMA_COMMIT:0:12} with ROCm (${root}, ${targets}) into ${home}/build-rocm (many minutes)"
        return 0
    fi
    local opts=(-DGGML_HIP=ON "-DAMDGPU_TARGETS=${targets}" "-DCMAKE_PREFIX_PATH=${root}")
    # AMD's own packages carry their compiler; the distribution's are found by CMake.
    [[ -x "$root/bin/amdclang++" ]] && opts+=("-DCMAKE_HIP_COMPILER=$root/bin/amdclang++")
    # An installation of its own is named to the compiler. The distribution's must not be: with ROCM_PATH=/usr, clang no longer finds the device
    # libraries Ubuntu keeps beside it.
    local rocm_path="$root"
    [[ "$root" == "${FORGE_ROCM_SYSTEM:-/usr}" ]] && rocm_path=""
    llama_build "llama.cpp (ROCm)" build-rocm "$rocm_path" "${opts[@]}"
}

llama_step() {
    [[ "$TARGET" == "public" ]] || return 0
    step "llama.cpp (the language model server)"
    if [[ "${LLAMA_MODE}" == "off" ]]; then
        if [[ -x "${LLAMA_VULKAN_BIN}" ]]; then
            ok "llama-server is there already: ${LLAMA_VULKAN_BIN} (left as it is)"
        else
            info "Not built (--no-llama). Language models need a llama-server at ${LLAMA_VULKAN_BIN}."
        fi
        return 0
    fi
    LLAMA_FETCHED=0 LLAMA_KEEP_STAGING=0
    llama_vulkan
    llama_rocm
    # The source is kept; build logs of a failed build stay in staging.
    if [[ "$LLAMA_FETCHED" == 1 ]]; then
        local home="${ENGINE_ROOT}/llama.cpp" staging="${ENGINE_ROOT}/.staging/llama.cpp"
        if [[ "$LLAMA_KEEP_STAGING" == 0 ]]; then
            if [[ -d "$home" && ! -e "$home/src" ]]; then
                run_as "${SERVICE_USER}" mv "$staging/src" "$home/src" || warn "The llama.cpp source was not kept"
            fi
            run_as "${SERVICE_USER}" rm -rf "$staging" || warn "Could not remove ${staging}"
        fi
    fi
}
