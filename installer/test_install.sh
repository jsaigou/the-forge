#!/usr/bin/env bash
# installer/test_install.sh — tests for installer/host.sh, speech.sh and voice-studio.sh with stub tools (no root, no network, no systemd).
#   bash installer/test_install.sh
# Stub go/ffmpeg/systemctl/curl/forge on PATH stand in for the real ones; the real git does the Voice Studio clone and the pin check.

set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$HERE/.." && pwd)"
PASS=0; FAILN=0
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT

# install.sh's helpers, recording instead of printing
OUT=""
ok()   { OUT+="ok: $*"$'\n'; }
warn() { OUT+="warn: $*"$'\n'; }
fail() { OUT+="fail: $*"$'\n'; }
info() { OUT+="info: $*"$'\n'; }
BOLD=""; NC=""

check() { # name, condition-exit-status
    if [[ "$2" -eq 0 ]]; then PASS=$((PASS+1)); else FAILN=$((FAILN+1)); echo "FAIL: $1"; echo "--- output ---"; echo "$OUT"; [[ -f "$T/calls" ]] && cat "$T/calls"; echo "--------------"; fi
}
has()  { [[ "$OUT" == *"$1"* ]]; }
called() { grep -qF -- "$1" "$T/calls"; }
reset() { OUT=""; SKIPPED=(); : >"$T/calls"; }

ME="$(id -un)"
TARGET=public; DRY_RUN=0; YES=1; SERVICE_USER="$ME"; SCRIPT_DIR="$REPO"; ENGINE_ROOT="$T/root"; SPEECH_CHOICE=""; VOICE_STUDIO_MODE=auto; TS_OK=0
export SUDO_USER=""
# shellcheck source=host.sh
source "$HERE/host.sh"
# shellcheck source=speech.sh
source "$HERE/speech.sh"
# shellcheck source=voice-studio.sh
source "$HERE/voice-studio.sh"

BIN="$T/bin"; mkdir -p "$BIN"
cat >"$BIN/systemctl" <<EOF
#!/bin/sh
echo "systemctl \$*" >>"$T/calls"; exit "\${STUB_SYSTEMCTL_RC:-0}"
EOF
cat >"$BIN/curl" <<'EOF'
#!/bin/sh
exit "${STUB_CURL_RC:-0}"
EOF
cat >"$BIN/forge" <<EOF
#!/bin/sh
echo "forge \$*" >>"$T/calls"
case "\$*" in
  *"speech install \${STUB_FAIL_ENGINE:-none}"*) exit 1 ;;
  *"speech key rotate"*) exit "\${STUB_ROTATE_RC:-0}" ;;
esac
exit 0
EOF
cat >"$BIN/go" <<EOF
#!/bin/sh
echo "go \$*" >>"$T/calls"
out=""; while [ \$# -gt 0 ]; do [ "\$1" = "-o" ] && out="\$2"; shift; done
[ -n "\$out" ] && printf '#!/bin/sh\n' >"\$out"
exit "\${STUB_GO_RC:-0}"
EOF
printf '#!/bin/sh\nexit 0\n' >"$BIN/ffmpeg"
printf '#!/bin/sh\necho 22\n' >"$BIN/node"
printf '#!/bin/sh\nexit 0\n' >"$BIN/npm"
chmod 755 "$BIN"/*
export PATH="$BIN:$PATH" FORGE_BIN="$BIN/forge"
: >"$T/calls"

# ── the platform ──────────────────────────────────────────────────────────────
printf 'ID=ubuntu\nID_LIKE=debian\n' >"$T/ubuntu"; printf 'ID=fedora\n' >"$T/fedora"; printf 'ID=arch\n' >"$T/arch"
[[ "$(FORGE_OS_RELEASE="$T/ubuntu" host_pkg_tool)" == apt && "$(FORGE_OS_RELEASE="$T/fedora" host_pkg_tool)" == dnf && -z "$(FORGE_OS_RELEASE="$T/arch" host_pkg_tool)" ]]
check "package tool per distribution" $?
[[ "$(FORGE_OS_RELEASE="$T/ubuntu" pkg_hint gcc-c++ g++)" == "sudo apt install g++" && "$(FORGE_OS_RELEASE="$T/arch" pkg_hint a b)" == *"package manager"* ]]
check "package hints" $?
echo systemd >"$T/pid1"; echo init >"$T/pid1-other"
FORGE_PID1_FILE="$T/pid1" host_has_systemd; check "systemd detected" $?
! FORGE_PID1_FILE="$T/pid1-other" host_has_systemd && ! FORGE_PID1_FILE="$T/none" host_has_systemd; check "no systemd detected" $?
reset; FORGE_DBUS_SOCKET="$T/none" FORGE_POLKITD="$T/none" FORGE_OS_RELEASE="$T/fedora" host_check_service_bus
has "dnf install dbus-broker" && has "dnf install polkit"; check "missing D-Bus and polkit are said, with the command" $?

reset; host_start_polkit; called "systemctl start polkit.service" && ! called "reload dbus" && [[ -z "$OUT" ]]; check "polkit started" $?
reset; STUB_SYSTEMCTL_RC=1 host_start_polkit; called "systemctl reload dbus.service" && has "polkit does not start"; check "a polkit that cannot start is said" $?

# ── build tools ───────────────────────────────────────────────────────────────
mkdir -p "$T/src/go" "$T/src/web"
reset; SCRIPT_DIR="$T/src" host_check_build_tools; check "tools present" $?
reset; ( PATH="$T/empty"; SCRIPT_DIR="$T/src" FORGE_OS_RELEASE="$T/fedora" host_check_build_tools; rc=$?; printf '%s' "$OUT" >"$T/out"; exit $rc ); rc=$?; OUT="$(cat "$T/out")"
[[ $rc -ne 0 ]] && has "Go is not installed" && has "Node.js and npm are not installed"; check "missing build tools stop, with the commands" $?
mkdir -p "$T/src/go/internal/httpapi/web_dist"; : >"$T/src/go/internal/httpapi/web_dist/index.html"
mkdir -p "$T/onlygo"; cp "$BIN/go" "$T/onlygo/"
reset; ( PATH="$T/onlygo"; SCRIPT_DIR="$T/src" host_check_build_tools ); check "a built web app needs no Node" $?

# ── units and polkit ──────────────────────────────────────────────────────────
SERVICE_USER=svc
render_unit "$REPO/systemd/forge-pool@.service" "$T/pool.unit"; grep -qx "User=svc" "$T/pool.unit" && grep -qx "Group=svc" "$T/pool.unit"; check "public units run as the service user" $?
[[ -z "$(for u in "$REPO"/systemd/forge-{daemon,tts,pool@,voice-studio,a1}.service; do render_unit "$u" "$T/u"; grep -E '^(User|Group)=' "$T/u" | grep -v '=svc$'; done)" ]]; check "no unit keeps another user" $?
render_polkit "$REPO/polkit/50-forge.rules" "$T/rules"; grep -qF 'if (subject.user === "svc")' "$T/rules" && ! grep -E 'subject\.user === "' "$T/rules" | grep -qv '"svc"'; check "polkit names the service user only" $?
TARGET=dev; render_unit "$REPO/systemd/forge-daemon.service" "$T/dev.unit"; cmp -s "$REPO/systemd/forge-daemon.service" "$T/dev.unit"; check "dev units are unchanged" $?; TARGET=public
SERVICE_USER="$ME"
printf 'ExecStart=%s --flag \\\n' "$BIN/ffmpeg" >"$T/p.unit"; printf 'ExecStart=/nowhere/server\n' >"$T/q.unit"
unit_program_present "$T/p.unit" && ! unit_program_present "$T/q.unit"; check "units whose program is missing are told apart" $?

# ── the model slot launchers ──────────────────────────────────────────────────
LS="$REPO/go/internal/smith/launchers"; LD="$T/launchers"; mkdir -p "$LD"
reset; install_slot_launchers "$LS" "$LD"
[[ ${#SKIPPED[@]} -eq 0 && -x "$LD/start-a1.sh" && -x "$LD/start-a4.sh" ]] && cmp -s "$LS/start-a3.sh" "$LD/start-a3.sh"; check "launchers installed" $?
# Every model slot unit runs a script that was installed.
missing=""; for u in "$REPO"/systemd/forge-a[1-4].service; do p="$(sed -n 's/^ExecStart=//p' "$u")"; [[ -x "$LD/$(basename "$p")" && "$p" == /usr/local/lib/forge/* ]] || missing+="$p "; done
OUT="$missing"; [[ -z "$missing" ]]; check "each slot unit has its launcher" $?
reset; install_slot_launchers "$LS" "$LD"; [[ -z "$(ls "$LD"/*.prev-* 2>/dev/null)" ]] && ! has "Replaced"; check "a second install changes nothing" $?
echo '# mine' >"$LD/start-a2.sh"; reset; install_slot_launchers "$LS" "$LD"
has "Replaced a different" && grep -qx '# mine' "$LD"/start-a2.sh.prev-* && cmp -s "$LS/start-a2.sh" "$LD/start-a2.sh"; check "a different launcher is kept beside the new one" $?
reset; install_slot_launchers "$T/nowhere" "$LD"; [[ ${#SKIPPED[@]} -eq 4 ]]; check "no sources, said per slot" $?

# ── llama.cpp builds ──────────────────────────────────────────────────────────
reset; llama_register_builds "$BIN/ffmpeg=vulkan" "$T/none=rocm"
called "forge catalog register-build -binary $BIN/ffmpeg -backend vulkan" && ! called "$T/none" && [[ ${#SKIPPED[@]} -eq 0 ]]; check "the builds that are there are registered" $?
reset; llama_register_builds "$T/none=vulkan"; ! called "register-build" && has "No llama.cpp server was found"; check "no build, said" $?
reset; cp "$BIN/forge" "$T/forge-fail"; printf '#!/bin/sh
exit 1
' >"$T/forge-fail"; FORGE_BIN="$T/forge-fail" llama_register_builds "$BIN/ffmpeg=vulkan"; [[ ${#SKIPPED[@]} -eq 1 ]] && has "Builds & files"; check "a refused build is said, with where to add it" $?

# ── speech selection and setup ────────────────────────────────────────────────
speech_parse none && ! speech_wanted; check "speech none" $?
speech_parse default && [[ "${SPEECH_IDS[*]}" == "kokoro zonos qwen-voicedesign" ]]; check "speech default" $?
speech_parse kokoro,qwen-base && [[ "${SPEECH_IDS[*]}" == "kokoro qwen-base" ]]; check "speech list" $?
reset; ! speech_parse kokoro,nope && has "Unknown speech engine 'nope'"; check "speech unknown id" $?

mkdir -p "$ENGINE_ROOT"; printf '#!/bin/sh\n' >"$ENGINE_ROOT/forge-tts"; chmod 755 "$ENGINE_ROOT/forge-tts"
reset; speech_parse kokoro,zonos; VS_READY=0; SPEECH_SERVICE_UP=0
STUB_FAIL_ENGINE=zonos speech_setup
called "forge speech key rotate -if-needed -env $ENGINE_ROOT/tts_voices/.internal_token_env" && ! called "voice-studio-env" && called "systemctl enable forge-tts.service" \
  && called "forge speech install kokoro" && called "forge speech install zonos" && [[ ${#SKIPPED[@]} -eq 1 ]] && has "Speech engine kokoro is set up" && has "speech engine zonos: the Forge cannot install it yet"
check "speech setup: key, service, engines; a failed engine is skipped by name" $?
reset; speech_parse kokoro; STUB_FAIL_ENGINE=kokoro speech_setup; has "later:" && has "speech install kokoro"; check "an installable engine that failed says how to retry" $?
reset; speech_parse kokoro; STUB_ROTATE_RC=1 speech_setup; ! called "speech install" && ! called "systemctl" && [[ ${#SKIPPED[@]} -eq 1 ]]; check "no key, no speech, and it says so" $?
reset; STUB_CURL_RC=1; export STUB_CURL_RC; ( sleep() { :; }; speech_setup; printf '%s' "$OUT" >"$T/out" ); unset STUB_CURL_RC; OUT="$(cat "$T/out")"; has "forge-tts did not answer"; check "a silent speech service is reported" $?
reset; DRY_RUN=1; speech_setup; DRY_RUN=0; [[ ! -s "$T/calls" ]] && has "dry-run"; check "dry run changes nothing" $?

# ── Voice Studio ──────────────────────────────────────────────────────────────
REMOTE="$T/vs-remote"; mkdir -p "$REMOTE/cmd/voice-studio"
( cd "$REMOTE" && git init -q && git config user.email fixture && git config user.name t && echo m > cmd/voice-studio/main.go && git add . && git commit -q -m init && git tag v1 )
GOOD="$(git -C "$REMOTE" rev-parse HEAD)"
VS_DATA_DIR="$T/vs-data"; export VS_DATA_DIR
speech_parse none; reset; vs_prepare; [[ ! -s "$T/calls" && ${#SKIPPED[@]} -eq 0 ]]; check "no speech, no Voice Studio" $?
speech_parse kokoro; reset; FORGE_VOICE_STUDIO_REF="" vs_prepare; has "no release is pinned"; check "unpinned is skipped" $?
FORGE_VOICE_STUDIO_REPO="$REMOTE"; FORGE_VOICE_STUDIO_REF=v1
reset; FORGE_VOICE_STUDIO_COMMIT=0000000000000000000000000000000000000000; vs_prepare; has "not the pinned" && ! called "go build" && [[ $VS_READY -eq 0 ]]; check "a moved tag is refused" $?
reset; FORGE_VOICE_STUDIO_COMMIT="$GOOD"; VOICE_STUDIO_MODE=off; vs_prepare; [[ ! -s "$T/calls" ]]; check "--no-voice-studio" $?
VOICE_STUDIO_MODE=auto; reset; vs_prepare
ENVF="$ENGINE_ROOT/voice-studio/voice-studio.env"
[[ $VS_READY -eq 1 && -x "$ENGINE_ROOT/voice-studio/voice-studio" ]] && grep -qx 'VOICE_STUDIO_NODE_URL=http://127.0.0.1:8082' "$ENVF" && [[ "$(ls -l "$ENVF")" == -rw-------* ]]
check "built, installed, settings private" $?
sed -i.bak 's#^VOICE_STUDIO_FORGE_URL=.*#VOICE_STUDIO_FORGE_URL=https://mine#' "$ENVF"; reset; vs_prepare
grep -qx 'VOICE_STUDIO_FORGE_URL=https://mine' "$ENVF" && [[ "$(grep -c '^VOICE_STUDIO_FORGE_URL=' "$ENVF")" -eq 1 ]]; check "an operator's setting stays" $?
reset; speech_parse kokoro; SPEECH_SERVICE_UP=0; speech_setup; called "-voice-studio-env $ENVF"; check "the token goes to Voice Studio's settings" $?
mkdir -p "$T/units"; reset; FORGE_UNIT_DIR="$T/units" FORGE_PID1_FILE="$T/pid1" vs_start
[[ -f "$T/units/forge-voice-studio.service" ]] && called "systemctl enable forge-voice-studio.service" && called "forge speech studio-link http://127.0.0.1:8090"; check "unit installed, started and linked" $?
reset; FORGE_UNIT_DIR="$T/units" FORGE_PID1_FILE="$T/pid1-other" vs_start; ! called "systemctl" && has "systemd is not running"; check "without systemd the unit is installed only" $?

# ── llama.cpp ─────────────────────────────────────────────────────────────────
# shellcheck source=llama.sh
source "$HERE/llama.sh"
step() { OUT+="step: $*"$'\n'; }
LR="$T/llama-remote"; mkdir -p "$LR"
( cd "$LR" && git init -q && git config user.email fixture && git config user.name t && echo c >CMakeLists.txt && git add . && git commit -q -m init && git config uploadpack.allowReachableSHA1InWant true )
LC="$(git -C "$LR" rev-parse HEAD)"
# The stub build writes a server whose --help lists the options (or STUB_LLAMA_HELP instead).
cat >"$BIN/cmake" <<STUB
#!/bin/sh
echo "cmake \$*" >>"$T/calls"
echo "env ROCM_PATH=\${ROCM_PATH-unset}" >>"$T/calls"
echo "env HIP_PATH=\${HIP_PATH-unset}" >>"$T/calls"
if [ "\$1" = "--build" ]; then
  mkdir -p "\$2/bin"
  printf '#!/bin/sh\necho "\${STUB_LLAMA_HELP:---load-mode --flash-attn}"\n' >"\$2/bin/llama-server"; chmod 755 "\$2/bin/llama-server"
fi
exit "\${STUB_CMAKE_RC:-0}"
STUB
for tool in c++ make glslc; do printf '#!/bin/sh\nexit 0\n' >"$BIN/$tool"; done; chmod 755 "$BIN"/*
: >"$T/vulkan.h"; : >"$T/spirv.hpp"; printf 'MemAvailable: 8388608 kB\n' >"$T/meminfo"
export FORGE_VULKAN_HEADER="$T/vulkan.h" FORGE_SPIRV_HEADER="$T/spirv.hpp" FORGE_MEMINFO="$T/meminfo"
mkdir -p "$T/kfd" "$T/norocm"; export FORGE_KFD_NODES="$T/kfd" FORGE_ROCM_PLACES="$T/norocm"; unset FORGE_ROCM_TARGET
LLAMA_ROCM_BIN="$T/lroot/llama.cpp/build-rocm/bin/llama-server"
FORGE_LLAMA_REPO="$LR"; FORGE_LLAMA_COMMIT="$LC"; LLAMA_MODE=auto; ENGINE_ROOT="$T/lroot"; mkdir -p "$ENGINE_ROOT"
LLAMA_VULKAN_BIN="$ENGINE_ROOT/llama.cpp/build-vulkan/bin/llama-server"
mkdir -p "$T/cg"; export FORGE_CGROUP="$T/cg"
printf '#!/bin/sh\necho 48\n' >"$BIN/nproc"; chmod 755 "$BIN/nproc"
[[ "$(llama_jobs)" == 4 ]]; check "compile jobs fit the memory" $?
echo 4294967296 >"$T/cg/memory.max"; [[ "$(llama_jobs)" == 2 ]]; check "a container's memory limit counts" $?
echo max >"$T/cg/memory.max"; echo "300000 100000" >"$T/cg/cpu.max"; [[ "$(llama_jobs)" == 3 ]]; check "a container's processor limit counts" $?
echo "max 100000" >"$T/cg/cpu.max"; echo 1048576 >"$T/cg/memory.max"; [[ "$(llama_jobs)" == 1 ]]; check "never fewer than one job" $?
rm -f "$T/cg/memory.max" "$T/cg/cpu.max" "$BIN/nproc"
reset; LLAMA_MODE=off llama_step; has "--no-llama" && [[ ! -s "$T/calls" ]]; check "--no-llama builds nothing" $?
reset; FORGE_VULKAN_HEADER="$T/none" FORGE_OS_RELEASE="$T/fedora" llama_step; has "missing Vulkan headers" && has "dnf install git cmake" && [[ ! -s "$T/calls" ]]; check "missing tools are named with the command" $?
reset; DRY_RUN=1 llama_step; DRY_RUN=0; has "dry-run" && [[ ! -e "$ENGINE_ROOT/llama.cpp" ]]; check "llama dry run" $?
reset; FORGE_LLAMA_COMMIT=0000000000000000000000000000000000000000 llama_step; has "could not fetch" && [[ ! -e "$LLAMA_VULKAN_BIN" ]]; check "a commit that is not there builds nothing" $?
reset; STUB_CMAKE_RC=1 llama_step 2>/dev/null; has "the build failed" && [[ ! -e "$LLAMA_VULKAN_BIN" ]]; check "a failed build leaves nothing in place" $?
reset; STUB_LLAMA_HELP="--no-mmap" llama_step; has "lacks an option" && [[ ! -e "$LLAMA_VULKAN_BIN" ]]; check "a server without the needed options is not installed" $?
reset; llama_step; [[ -x "$LLAMA_VULKAN_BIN" && -d "$ENGINE_ROOT/llama.cpp/src" && ! -e "$ENGINE_ROOT/.staging/llama.cpp" ]] && called "-DGGML_VULKAN=ON" && has "llama-server built"; check "built, moved into place, staging removed" $?
reset; llama_step; has "left as it is" && [[ ! -s "$T/calls" ]]; check "a server that is there is not rebuilt" $?
reset; LLAMA_VULKAN_BIN="$T/mine/llama-server" llama_step; has "only builds into" && [[ ! -s "$T/calls" ]]; check "another configured path is left to its owner" $?
reset; TARGET=dev llama_step; [[ -z "$OUT" ]]; check "the reference host is not touched" $?; TARGET=public

# ── llama.cpp with ROCm ───────────────────────────────────────────────────────
kfd_node() { mkdir -p "$T/kfd/$1"; printf 'simd_count 80\ngfx_target_version %s\nvendor_id %s\n' "$2" "$3" >"$T/kfd/$1/properties"; }
[[ -z "$(llama_gpu_targets)" ]]; check "no GPU, no targets" $?
kfd_node 0 0 0; kfd_node 1 110501 4098; [[ "$(llama_gpu_targets)" == gfx1151 ]]; check "the processor is passed over and the GPU named" $?
kfd_node 2 90010 4098; kfd_node 3 110501 4098; [[ "$(llama_gpu_targets)" == "gfx1151;gfx90a" ]]; check "each GPU type once, hex stepping" $?
rm -rf "$T/kfd/2" "$T/kfd/3"
kfd_node 4 120100 4318; [[ "$(llama_gpu_targets)" == gfx1151 ]]; check "another vendor is not a ROCm target" $?
[[ "$(FORGE_ROCM_TARGET=gfx1100 llama_gpu_targets)" == gfx1100 ]]; check "the target can be named" $?
RR="$T/rocmroot"; export FORGE_ROCM_PLACES="$T/norocm $RR"
for p in hip hipblas; do mkdir -p "$RR/lib64/cmake/$p"; : >"$RR/lib64/cmake/$p/$p-config.cmake"; done
! llama_rocm_root >/dev/null; check "ROCm without rocBLAS is not enough" $?
reset; FORGE_OS_RELEASE="$T/fedora" llama_step; has "ROCm development files were not found" && has "rocblas-devel" && ! called "GGML_HIP"; check "missing ROCm files are named with the command" $?
mkdir -p "$RR/lib/x86_64-linux-gnu/cmake/rocblas" "$RR/bin"; : >"$RR/lib/x86_64-linux-gnu/cmake/rocblas/rocblas-config.cmake" # Ubuntu's layout
[[ "$(llama_rocm_root)" == "$RR" ]]; check "a complete ROCm is found" $?
[[ "$(FORGE_ROCM_PLACES="$RR//" llama_rocm_root)" == "$RR" ]]; check "a trailing slash does not change the root" $?
reset; LLAMA_ROCM_BIN="$T/mine/rocm/llama-server" llama_step; has "FORGE_LLAMA_ROCM_BIN names" && ! called "GGML_HIP"; check "another configured ROCm path is left to its owner" $?
reset; DRY_RUN=1 llama_step; DRY_RUN=0; has "with ROCm ($RR, gfx1151)" && [[ ! -s "$T/calls" ]]; check "ROCm dry run" $?
reset; STUB_CMAKE_RC=1 llama_step 2>/dev/null; has "the build failed" && [[ ! -e "$LLAMA_ROCM_BIN" && -x "$LLAMA_VULKAN_BIN" ]]; check "a failed ROCm build leaves the Vulkan server alone" $?
reset; llama_step; [[ -x "$LLAMA_ROCM_BIN" && ! -e "$ENGINE_ROOT/.staging/llama.cpp" ]] && called "-DGGML_HIP=ON" && called "-DAMDGPU_TARGETS=gfx1151" && called "-DCMAKE_PREFIX_PATH=$RR" && ! called "-DGGML_VULKAN=ON" && ! called "CMAKE_HIP_COMPILER" && called "env ROCM_PATH=$RR"; check "ROCm build added beside the Vulkan one" $?
reset; llama_step; has "ROCm llama-server is there already" && [[ ! -s "$T/calls" ]]; check "a ROCm server that is there is not rebuilt" $?
rm -rf "$ENGINE_ROOT/llama.cpp"; printf '#!/bin/sh\n' >"$RR/bin/amdclang++"; chmod 755 "$RR/bin/amdclang++"
reset; llama_step; called "-DCMAKE_HIP_COMPILER=$RR/bin/amdclang++" && [[ -x "$LLAMA_ROCM_BIN" && -x "$LLAMA_VULKAN_BIN" && -d "$ENGINE_ROOT/llama.cpp/src" ]] && [[ "$(grep -c 'cmake --build' "$T/calls")" == 2 ]]; check "both builds from one source, AMD compiler used" $?
rm -rf "$ENGINE_ROOT/llama.cpp"; reset; LLAMA_MODE=off llama_step; [[ ! -s "$T/calls" ]]; check "--no-llama leaves out ROCm too" $?
rm -rf "$ENGINE_ROOT/llama.cpp"; reset; ROCM_PATH=/stale HIP_PATH=/stale FORGE_ROCM_SYSTEM="$RR" FORGE_ROCM_PLACES="$RR/" llama_step
[[ -x "$LLAMA_ROCM_BIN" ]] && called "env ROCM_PATH=unset" && called "env HIP_PATH=unset" && ! called "=/stale" && ! called "env ROCM_PATH=$RR"; check "the distribution's ROCm is not named through ROCM_PATH" $?
rm -rf "$ENGINE_ROOT/llama.cpp" "$T/kfd/1" "$T/kfd/4"; reset; llama_step; has "no AMD GPU was found" && [[ ! -e "$LLAMA_ROCM_BIN" ]]; check "no AMD GPU, no ROCm build" $?
has "FORGE_ROCM_TARGET"; check "an undetected GPU can be named" $?

# Which server a public install treats as the ROCm one.
OLD="$T/old/build/bin/llama-server"; NEW="$T/new/llama-server"; mkdir -p "$(dirname "$OLD")"
rd() { llama_rocm_default "$NEW" "$OLD" 2>"$T/rd.err"; }
OUT=""; [[ "$(rd)" == "$NEW" ]]; check "nothing at the old place: the installer's path" $?
printf 'ELF libvulkan.so.1\0' >"$OLD"; chmod 755 "$OLD"
r="$(llama_rocm_default "$NEW" "$OLD")"; [[ "$r" == "$NEW" ]]; check "a Vulkan server at the old place is not taken for ROCm" $?
printf 'ELF\0libamdhip64.so.7\0' >"$OLD"; [[ "$(rd)" == "$OLD" ]]; check "a ROCm server at the old place is kept" $?
printf 'ELF libggml.so.0\0' >"$OLD"; : >"$(dirname "$OLD")/libggml-hip.so"; [[ "$(rd)" == "$OLD" ]]; check "a shared ROCm build is recognised" $?

# ── install.sh arguments ──────────────────────────────────────────────────────
OUT="$(bash "$REPO/install.sh" --target=public --user=root 2>&1)"; [[ $? -ne 0 ]] && has "Invalid --user"; check "root is no service user" $?
OUT="$(bash "$REPO/install.sh" --target=dev --user=x 2>&1)"; [[ $? -ne 0 ]] && has "public target"; check "--user is public only" $?
OUT="$(bash "$REPO/install.sh" --help 2>&1)"; has "--speech=default" && has "--no-voice-studio" && has "--no-llama"; check "help lists the speech and llama flags" $?

echo "install.sh helpers: ${PASS} passed, ${FAILN} failed"
exit $(( FAILN > 0 ))
