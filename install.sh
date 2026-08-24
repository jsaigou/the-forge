#!/usr/bin/env bash
# install.sh — The Forge v0.5 install / upgrade script
# Run as root: sudo ./install.sh [options]
#
# Usage:
#   sudo ./install.sh --target=dev           # reference-host install (default; ForgeHost-oriented)
#   sudo ./install.sh --target=public        # generic-host install — pre-flight MANDATORY
#   sudo ./install.sh --force                # public target: proceed despite pre-flight FAILs
#   sudo ./install.sh --upgrade              # in-place legacy upgrade
#   sudo ./install.sh --dry-run              # show checks without making changes
#   sudo ./install.sh --skip-models          # skip model download prompts
#   sudo ./install.sh --min-age-hours 48     # stricter package age window
#   sudo ./install.sh --skip-strict --reason "reason"  # disable age check
#   sudo ./install.sh --help-hermes          # print Hermes agent connection guide
#
# Targets:
#   dev    — current behaviour, oriented at the reference host (ForgeHost):
#            fixed /opt/forge paths, SELinux relabel notes, Fedora-only gates,
#            PairNode/ComfyUI reachability probe.
#   public — generic-host path: identical unit installation, but
#            reference-host assumptions are skipped/guarded, the hardware
#            pre-flight (installer/preflight.sh) MUST pass unless --force,
#            the KGC profile matcher runs after units are installed, and the
#            operator is prompted to provision models (installer/provision.py).

set -euo pipefail

# ── Colour codes ──────────────────────────────────────────────────────────────

R='\033[0;31m'; G='\033[0;32m'; Y='\033[0;33m'; B='\033[0;34m'
C='\033[0;36m'; W='\033[1;37m'; M='\033[0;35m'; DIM='\033[2m'; NC='\033[0m'
BOLD='\033[1m'

ok()   { echo -e "${G}  ✓${NC}  $*"; }
warn() { echo -e "${Y}  ⚠${NC}  $*" >&2; WARNINGS+=("$*"); }
fail() { echo -e "${R}  ✗${NC}  $*" >&2; }
step() { echo -e "\n${BOLD}${C}[${STEP}/${TOTAL_STEPS}]${NC} ${BOLD}$*${NC}"; (( STEP++ )) || true; }
info() { echo -e "    ${DIM}$*${NC}"; }

STEP=1
TOTAL_STEPS=15
WARNINGS=()
ERRORS=()

# ── Argument parsing ──────────────────────────────────────────────────────────

TARGET="dev"
FORCE=0
UPGRADE=0
DRY_RUN=0
SKIP_MODELS=0
SKIP_STRICT=0
SKIP_STRICT_REASON=""
MIN_AGE_HOURS=24
CVE_EXCEPTION=""
OVERRIDE_PKG=""
OVERRIDE_REASON=""

print_hermes_guide() {
    cat <<'HERMES'

════════════════════════════════════════════════════════════════
  HERMES AGENT CONNECTION GUIDE
  Connecting to the Forge inference API from agent frameworks
════════════════════════════════════════════════════════════════

The Forge exposes two persistent inference endpoints via Tailscale:

  Primary slot:    https://a1.example.ts.net  (port 8080 internally)
  Secondary slot:  https://a2.example.ts.net  (port 8081 internally)

Both endpoints speak the OpenAI-compatible chat completions API:

  POST https://a1.example.ts.net/v1/chat/completions

Authentication: these inference endpoints do NOT require a Forge API key.
They are gated by Tailscale network membership — any node on the
example-tailnet.ts.net tailnet can reach them directly.

Quick test (from any tailnet node):
  curl -s https://a1.example.ts.net/v1/models | python3 -m json.tool

The ops dashboard (mode switching, metrics) is at:
  https://ops.example.ts.net
  — requires Forge login credentials.

For full agent framework configuration examples, see:
  docs/hermes-connection.md
HERMES
    exit 0
}

usage() {
    sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 2
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --target=*)        TARGET="${1#*=}" ;;
        --target)          TARGET="${2:-}"; [[ -n "${2:-}" ]] && shift ;;
        --force)           FORCE=1 ;;
        --upgrade)         UPGRADE=1 ;;
        --dry-run)         DRY_RUN=1 ;;
        --skip-models)     SKIP_MODELS=1 ;;
        --skip-strict)     SKIP_STRICT=1; SKIP_STRICT_REASON="${2:-}"; [[ -n "${2:-}" ]] && shift ;;
        --min-age-hours)   MIN_AGE_HOURS="$2"; shift ;;
        --cve)             CVE_EXCEPTION="$2"; shift ;;
        --override-package) OVERRIDE_PKG="$2"; shift ;;
        --reason)          OVERRIDE_REASON="$2"; shift ;;
        --help|-h)         usage ;;
        --help-hermes)     print_hermes_guide ;;
        --yes)             YES=1 ;;
        *)                 fail "Unknown argument: $1"; exit 1 ;;
    esac
    shift
done

YES="${YES:-0}"

case "$TARGET" in
    dev|public) ;;
    *) fail "Invalid --target '${TARGET}' (expected dev|public)"; exit 1 ;;
esac

[[ "$TARGET" == "public" ]] && TOTAL_STEPS=16   # extra pre-flight step

[[ $EUID -ne 0 ]] && { fail "Run as root: sudo ./install.sh"; exit 1; }

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=installer/preflight.sh
source "${SCRIPT_DIR}/installer/preflight.sh"

# Per-target defaults (overridable via environment).
if [[ "$TARGET" == "dev" ]]; then
    ROCM_ROOT="${ROCM_ROOT:-/opt/rocm-therock-7.13}"
    MODELS_DIR="${FORGE_MODELS_DIR:-/opt/forge/models}"
    LLAMA_VULKAN_BIN="${FORGE_LLAMA_VULKAN_BIN:-/opt/forge/llama.cpp/build-vulkan/bin/llama-server}"
    LLAMA_ROCM_BIN="${FORGE_LLAMA_ROCM_BIN:-/opt/forge/llama.cpp/build/bin/llama-server}"
else
    ROCM_ROOT="${ROCM_ROOT:-}"                       # scan /opt/rocm* generically
    MODELS_DIR="${FORGE_MODELS_DIR:-/var/lib/forge/models}"
    LLAMA_VULKAN_BIN="${FORGE_LLAMA_VULKAN_BIN:-/opt/forge/llama.cpp/build-vulkan/bin/llama-server}"
    LLAMA_ROCM_BIN="${FORGE_LLAMA_ROCM_BIN:-/opt/forge/llama.cpp/build/bin/llama-server}"
fi
export FORGE_DATA_DIR="${FORGE_DATA_DIR:-$(dirname "${MODELS_DIR}")}"

echo -e "\n${BOLD}${C}╔══════════════════════════════════════════════╗${NC}"
echo -e "${BOLD}${C}║  The Forge v0.5 — Installation Script        ║${NC}"
echo -e "${BOLD}${C}║  target: ${TARGET^^}${NC}"
echo -e "${BOLD}${C}╚══════════════════════════════════════════════╝${NC}\n"
[[ $DRY_RUN -eq 1 ]] && echo -e "${Y}DRY RUN — no changes will be made${NC}\n"

# ══════════════════════════════════════════════════════════════════════════════
# Shared step implementations. Dev and public targets call the same functions;
# they differ only at the guarded points below.
# ══════════════════════════════════════════════════════════════════════════════

# ── Pre-flight (public target only) ───────────────────────────────────────────

run_preflight_step() {
    step "Hardware/software pre-flight (installer/preflight.sh)"

    PREFLIGHT_TARGET="public" preflight_run
    pf_print_table

    local counts fails
    counts="$(pf_summary)"
    fails="${counts##* }"
    if (( fails > 0 )); then
        if [[ $FORCE -eq 1 ]]; then
            warn "Pre-flight reported ${fails} failure(s) — continuing (--force)"
            echo -en "\n  Proceed DESPITE failed pre-flight checks? [y/N]: "
            if [[ $YES -ne 1 ]]; then
                read -r ANSWER || ANSWER="n"
            else
                ANSWER="y"
            fi
            [[ "${ANSWER,,}" == "y" ]] || { fail "Aborted."; exit 1; }
        else
            fail "Pre-flight FAILED (${fails} check(s)). The public target requires a passing pre-flight."
            fail "Fix the failures above, or re-run with --force to override (not recommended)."
            exit 1
        fi
    fi
}

# ── Step 1: OS + kernel ───────────────────────────────────────────────────────

check_os_kernel() {
    step "OS + kernel verification"

    # OS
    if [[ -f /etc/os-release ]]; then
        . /etc/os-release
        if [[ "${ID:-}" != "fedora" ]]; then
            if [[ "$TARGET" == "dev" ]]; then
                fail "Unsupported OS: ${ID:-unknown}. Forge requires Fedora."
                exit 1
            else
                warn "Untested OS: ${ID:-unknown} — the reference host runs Fedora. Continuing (public target)."
            fi
        else
            VER_ID="${VERSION_ID:-0}"
            if (( VER_ID < 44 )); then
                if [[ "$TARGET" == "dev" ]]; then
                    fail "Fedora ${VER_ID} detected. The Forge requires Fedora 44+."
                    exit 1
                else
                    warn "Fedora ${VER_ID} detected — reference host runs 44+. Continuing (public target)."
                fi
            else
                ok "Fedora ${VER_ID}"
            fi
        fi
    else
        fail "/etc/os-release not found"; exit 1
    fi

    # Kernel — dev gate is the stricter reference-host rule (≥ 7.0); public
    # relies on the pre-flight ≥ 6.5 gfx1151 floor checked above.
    KERNEL="$(uname -r)"
    KVER_MAJOR="${KERNEL%%.*}"
    KVER_MINOR="${KERNEL#*.}"; KVER_MINOR="${KVER_MINOR%%.*}"
    if (( KVER_MAJOR < 7 )) || { (( KVER_MAJOR == 7 )) && (( KVER_MINOR < 0 )); }; then
        if [[ "$TARGET" == "dev" ]]; then
            fail "Kernel ${KERNEL} too old. Require ≥ 7.0 for stable RDNA3.5 GTT support."
            exit 1
        fi
        # public: already gated at ≥ 6.5 by the pre-flight step — no-op here.
        :
    else
        ok "Kernel ${KERNEL}"
    fi
}

# ── Step 2: CPU ───────────────────────────────────────────────────────────────

check_cpu() {
    step "CPU identification"

    CPU_MODEL="$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2- | xargs)"
    if echo "${CPU_MODEL}" | grep -qi "Ryzen AI Max"; then
        ok "CPU: ${CPU_MODEL}"
    else
        warn "CPU '${CPU_MODEL}' is not a Ryzen AI Max (Strix Halo). GTT ceiling, ROCm behaviour, and env tuning parameters are designed specifically for Strix Halo APUs."
    fi
}

# ── Step 3: BIOS observable checks ────────────────────────────────────────────

check_bios() {
    if [[ "$TARGET" != "dev" ]]; then
        # Reference-host firmware specifics (EXPO/MT-s speed probe, GART
        # aperture checklist) are dev-target material. RAM/GTT floors were
        # already enforced by the pre-flight step.
        step "BIOS / firmware checks (skipped details on public target)"
        info "RAM and GTT floors were verified by the pre-flight step."
        info "Strix Halo firmware guidance lives in docs/bios-setup.md."
        return
    fi

    step "BIOS / firmware observable checks"

    # BIOS version + date
    BIOS_VENDOR="$(cat /sys/class/dmi/id/bios_vendor 2>/dev/null || echo unknown)"
    BIOS_VERSION="$(cat /sys/class/dmi/id/bios_version 2>/dev/null || echo unknown)"
    BIOS_DATE="$(cat /sys/class/dmi/id/bios_date 2>/dev/null || echo unknown)"
    BOARD_VENDOR="$(cat /sys/class/dmi/id/board_vendor 2>/dev/null || echo unknown)"
    BOARD_NAME="$(cat /sys/class/dmi/id/board_name 2>/dev/null || echo unknown)"
    info "BIOS: ${BIOS_VENDOR} ${BIOS_VERSION} (${BIOS_DATE})"
    info "Board: ${BOARD_VENDOR} ${BOARD_NAME}"

    # Check BIOS age
    if [[ "${BIOS_DATE}" != "unknown" ]]; then
        BIOS_EPOCH=$(date -d "${BIOS_DATE}" +%s 2>/dev/null || echo 0)
        NOW_EPOCH=$(date +%s)
        BIOS_AGE_DAYS=$(( (NOW_EPOCH - BIOS_EPOCH) / 86400 ))
        if (( BIOS_AGE_DAYS > 365 )); then
            warn "BIOS is ${BIOS_AGE_DAYS} days old — check ${BOARD_VENDOR} support page for ${BOARD_NAME} updates"
        else
            ok "BIOS date: ${BIOS_DATE} (${BIOS_AGE_DAYS} days old)"
        fi
    fi

    # Total memory
    MEM_KB="$(grep MemTotal /proc/meminfo | awk '{print $2}')"
    MEM_GB=$(( MEM_KB / 1024 / 1024 ))
    REQUIRED_MEM_GB=126
    if (( MEM_GB < REQUIRED_MEM_GB )); then
        warn "System reports ${MEM_GB} GB RAM — expected ≥ ${REQUIRED_MEM_GB} GB (128 GB minus OS overhead). Check BIOS memory training. See docs/bios-setup.md §Memory."
    else
        ok "Memory: ${MEM_GB} GB"
    fi

    # Memory speed (requires dmidecode)
    if command -v dmidecode &>/dev/null; then
        CONFIGURED_MT=$(dmidecode -t 17 2>/dev/null | grep -i "Configured Memory Speed" | head -1 | grep -oP '\d+')
        RATED_MT=$(dmidecode -t 17 2>/dev/null | grep -w "Speed:" | head -1 | grep -oP '\d+')
        if [[ -n "${CONFIGURED_MT}" && -n "${RATED_MT}" ]]; then
            # Normalise: some firmware reports in MHz (4000 MHz = 8000 MT/s DDR)
            (( CONFIGURED_MT < 5000 )) && CONFIGURED_MT=$(( CONFIGURED_MT * 2 ))
            (( RATED_MT < 5000 )) && RATED_MT=$(( RATED_MT * 2 ))
            if (( CONFIGURED_MT < RATED_MT )); then
                warn "Memory running at ${CONFIGURED_MT} MT/s, rated for ${RATED_MT} MT/s. EXPO profile may not be active. See docs/bios-setup.md §Memory Speed."
            else
                ok "Memory speed: ${CONFIGURED_MT} MT/s (EXPO active)"
            fi
        fi
    fi

    # GTT pool
    GTT_FILE="/sys/class/drm/card0/device/mem_info_gtt_total"
    if [[ -f "${GTT_FILE}" ]]; then
        GTT_BYTES="$(cat "${GTT_FILE}")"
        GTT_GB=$(( GTT_BYTES / 1024 / 1024 / 1024 ))
        if (( GTT_GB < 100 )); then
            warn "GTT pool is ${GTT_GB} GB — expected ~120 GB. Check amdgpu.gttsize kernel param and BIOS GART aperture. See docs/bios-setup.md §GART."
        else
            ok "GTT pool: ${GTT_GB} GB"
        fi
    else
        warn "Cannot read GTT pool size — /sys/class/drm/card0/device/mem_info_gtt_total not found"
    fi

    # Secure Boot
    if command -v mokutil &>/dev/null; then
        SB_STATE="$(mokutil --sb-state 2>/dev/null || echo unknown)"
        if echo "${SB_STATE}" | grep -qi "enabled"; then
            warn "Secure Boot is ON. Custom ROCm builds and unsigned kernel modules may fail. If ROCm fails, check: dmesg | grep 'module verification failed'. See docs/bios-setup.md §Secure Boot."
        else
            ok "Secure Boot: disabled"
        fi
    fi

    echo -e "\n    ${Y}BIOS checklist — verify manually (cannot be read from Linux):${NC}"
    echo -e "    ${DIM}□ EXPO/XMP memory profile enabled (for 8000 MT/s)${NC}"
    echo -e "    ${DIM}□ GART aperture: set to minimum (512 MB or Auto)${NC}"
    echo -e "    ${DIM}□ iGPU UMA frame buffer: Auto or minimum${NC}"
    echo -e "    ${DIM}□ BIOS is current — check: ${BOARD_VENDOR} ${BOARD_NAME} support page${NC}"
    echo -e "    ${DIM}Full guide: docs/bios-setup.md${NC}"
}

# ── Step 4: GPU — Vulkan (BLOCKING) ──────────────────────────────────────────

check_gpu_vulkan() {
    step "GPU — Vulkan (required)"

    GFX_FOUND=0
    for node in /sys/class/kfd/kfd/topology/nodes/*/name; do
        [[ -f "${node}" ]] || continue
        if grep -qi "gfx1151" "${node}"; then
            GFX_FOUND=1
            break
        fi
    done

    if [[ $GFX_FOUND -eq 0 ]]; then
        fail "No gfx1151 GPU found in KFD topology. Forge requires Radeon 8060S (RDNA3.5 / Strix Halo)."
        if [[ "$(grep -E '^ID=' /etc/os-release 2>/dev/null | cut -d= -f2)" == "fedora" ]]; then
            fail "To install Vulkan drivers: sudo dnf install mesa-vulkan-drivers"
        else
            fail "Install the mesa/RADV Vulkan driver for your distribution."
        fi
        exit 1
    fi
    ok "KFD: gfx1151 detected"

    if command -v vulkaninfo &>/dev/null; then
        if vulkaninfo --summary 2>/dev/null | grep -qi "RADV STRIX_HALO"; then
            ok "Vulkan: RADV STRIX_HALO driver present"
        else
            fail "Vulkan driver does not report RADV STRIX_HALO. Install mesa-vulkan-drivers (RADV)."
            exit 1
        fi
    else
        warn "vulkaninfo not found — cannot confirm RADV STRIX_HALO driver. Install vulkan-tools."
    fi
}

# ── Step 5: ROCm (non-blocking) ───────────────────────────────────────────────

check_rocm() {
    step "ROCm (optional — required for models > 63 GB)"

    local loc path ver
    loc="$(ROCM_ROOT="${ROCM_ROOT}" pf_find_rocm)" || loc=""
    if [[ -z "$loc" ]]; then
        warn "ROCm not found. Models > 63 GB cannot be loaded (Vulkan ceiling ~63 GB GTT). All Vulkan-backed modes are unaffected."
        return
    fi
    path="${loc%% *}"; ver="${loc##* }"
    ok "ROCm: ${path} v${ver}"
    if command -v rocm-smi &>/dev/null && rocm-smi 2>/dev/null | grep -qi "gfx1151"; then
        ok "ROCm: gfx1151 device visible to rocm-smi"
    elif [[ -x "${path}/bin/rocm-smi" ]] && "${path}/bin/rocm-smi" 2>/dev/null | grep -qi "gfx1151"; then
        ok "ROCm: gfx1151 device visible to rocm-smi"
    else
        warn "ROCm present at ${path} but rocm-smi couldn't confirm the gfx1151 device. Models > 63 GB may fail to load."
    fi
}

# ── Step 6: llama.cpp binaries ────────────────────────────────────────────────

check_llama_bins() {
    step "llama.cpp build verification"

    for BIN in "${LLAMA_VULKAN_BIN}" "${LLAMA_ROCM_BIN}"; do
        BACKEND="$( [[ "${BIN}" == *vulkan* ]] && echo "Vulkan" || echo "ROCm" )"
        if [[ ! -f "${BIN}" ]]; then
            if [[ "$TARGET" == "dev" ]]; then
                warn "llama.cpp ${BACKEND} binary not found: ${BIN}"
            else
                warn "llama.cpp ${BACKEND} binary not found: ${BIN} (set FORGE_LLAMA_${BACKEND^^}_BIN to override)"
            fi
            continue
        fi
        if [[ ! -x "${BIN}" ]]; then
            warn "llama.cpp ${BACKEND} binary not executable: ${BIN}"
            continue
        fi
        # Smoke test
        if timeout 5 "${BIN}" --version &>/dev/null; then
            VER=$(timeout 5 "${BIN}" --version 2>&1 | head -1)
            ok "llama-server (${BACKEND}): ${VER}"
        else
            warn "llama-server (${BACKEND}) --version failed or timed out: ${BIN}"
        fi
    done
}

# ── Step 7: Prerequisites ─────────────────────────────────────────────────────

check_prereqs() {
    step "Prerequisites"

    # Python ≥ 3.12
    if ! command -v python3 &>/dev/null; then
        fail "python3 not found"; exit 1
    fi
    PY_VER="$(python3 -c 'import sys; print(f"{sys.version_info.major}.{sys.version_info.minor}")')"
    PY_MAJOR="${PY_VER%.*}"; PY_MINOR="${PY_VER#*.}"
    if (( PY_MAJOR < 3 )) || { (( PY_MAJOR == 3 )) && (( PY_MINOR < 12 )); }; then
        fail "Python ${PY_VER} too old. Require ≥ 3.12."; exit 1
    fi
    ok "Python ${PY_VER}"

    # systemd
    if [[ "$(cat /proc/1/comm 2>/dev/null)" != "systemd" ]]; then
        fail "systemd must be PID 1"; exit 1
    fi
    ok "systemd (PID 1)"

    if [[ "$TARGET" == "dev" ]]; then
        # uv — legacy V4 Python venv management (reference host only)
        if ! command -v uv &>/dev/null; then
            warn "uv not found — install from https://docs.astral.sh/uv/"
            # Could auto-install here; skipping for now since it requires network + hash verify
            fail "Please install uv and re-run install.sh"; exit 1
        fi
        UV_VER="$(uv --version 2>&1 | head -1)"
        ok "uv: ${UV_VER}"
    fi

    # tailscale
    if command -v tailscale &>/dev/null && tailscale status &>/dev/null; then
        TAILNET="$(tailscale status --json 2>/dev/null | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("MagicDNSSuffix",""))' 2>/dev/null || echo "")"
        ok "Tailscale: connected (${TAILNET})"
        TS_OK=1
    else
        if [[ "$TARGET" == "dev" ]]; then
            warn "Tailscale not active — Tailscale service discovery will be unavailable"
        else
            warn "Tailscale not active — optional; remote service discovery unavailable (local access still works)"
        fi
        TS_OK=0
    fi
}

# ── Step 8: Model inventory ───────────────────────────────────────────────────

check_model_inventory() {
    step "Model inventory"

    if [[ ! -d "${MODELS_DIR}" ]]; then
        warn "Models directory ${MODELS_DIR} does not exist — no models available yet"
        info "Public target: run installer/provision.py to populate it (offered after unit installation)."
        return
    elif [[ $SKIP_MODELS -eq 1 ]]; then
        info "Skipping model inventory (--skip-models)"
        return
    fi

    GGUF_COUNT=$(find "${MODELS_DIR}" -name "*.gguf" 2>/dev/null | wc -l)
    ok "Models directory: ${MODELS_DIR} (${GGUF_COUNT} GGUF files found)"

    # Required models for default modes
    declare -A REQUIRED_MODELS=(
        ["Gemma 4 E2B (gemma4-e2b mode)"]="gemma4-e2b"
        ["Qwen3.6-35B (qwen36, coding modes)"]="Qwen3.6-35B"
    )
    for MODEL_DESC in "${!REQUIRED_MODELS[@]}"; do
        PATTERN="${REQUIRED_MODELS[$MODEL_DESC]}"
        if find "${MODELS_DIR}" -name "*${PATTERN}*.gguf" -maxdepth 2 2>/dev/null | grep -q .; then
            ok "Found: ${MODEL_DESC}"
        else
            warn "Missing: ${MODEL_DESC}"
            info "  Required for modes referencing this model."
            if [[ $DRY_RUN -eq 0 ]]; then
                echo -en "  Download now? [y/N]: "
                read -r -t 30 ANSWER || ANSWER="n"
                if [[ "${ANSWER,,}" == "y" ]]; then
                    info "  Use: installer/provision.py --only <model-id> --root $(dirname "${MODELS_DIR}")"
                    info "  (Manifest: installer/assets.manifest.json)"
                fi
            fi
        fi
    done

    # ComfyUI reachability (PairNode) — reference-host topology only
    if [[ "$TARGET" == "dev" && ${TS_OK:-0} -eq 1 && -n "${TAILNET:-}" ]]; then
        COMFY_URL="https://comfy-pairnode.${TAILNET}"
        if curl -sf --max-time 5 "${COMFY_URL}/system_stats" &>/dev/null; then
            ok "PairNode ComfyUI reachable at ${COMFY_URL}"
        else
            warn "PairNode ComfyUI unreachable (${COMFY_URL}) — creative mode and Notes gallery will not function (may just be offline)"
        fi
    fi
}

# ── Step 9: Package age + OSV check (strict mode) ────────────────────────────

verify_packages() {
    if [[ "$TARGET" != "dev" ]]; then
        step "Package verification (dev-target strict mode — skipped on public)"
        info "The v0.5 daemon is a self-contained Go binary — no Python package supply chain on the host."
        return
    fi

    step "Package verification (strict mode — PyPI age + OSV)"

    LOCKFILE="${SCRIPT_DIR}/uv.lock"
    if [[ ! -f "${LOCKFILE}" ]]; then
        warn "uv.lock not found — skipping package verification"
    elif [[ $SKIP_STRICT -eq 1 ]]; then
        warn "Strict mode DISABLED. Reason: ${SKIP_STRICT_REASON}"
    else
        info "Checking package ages against PyPI API (min age: ${MIN_AGE_HOURS}h) ..."

        # Extract (name, version) pairs from uv.lock
        PACKAGES=$(python3 - <<'PYEOF'
import re, sys
with open(sys.argv[1]) as f:
    content = f.read()
pkgs = re.findall(r'name\s*=\s*"([^"]+)"\nversion\s*=\s*"([^"]+)"', content)
for name, ver in pkgs:
    print(f"{name} {ver}")
PYEOF
"${LOCKFILE}" 2>/dev/null || true)

        if [[ -n "${PACKAGES}" ]]; then
            AGE_FAILS=()
            VULN_PKGS=()

            # OSV batch check
            if command -v curl &>/dev/null; then
                OSV_PAYLOAD=$(python3 -c "
import json, sys
lines = sys.stdin.read().strip().split('\n')
queries = []
for line in lines:
    parts = line.split(' ', 1)
    if len(parts) == 2:
        queries.append({'package': {'name': parts[0], 'ecosystem': 'PyPI'}, 'version': parts[1]})
print(json.dumps({'queries': queries}))
" <<< "${PACKAGES}" 2>/dev/null || echo '{"queries":[]}')

                OSV_RESP=$(curl -sf --max-time 30 \
                    -H "Content-Type: application/json" \
                    -d "${OSV_PAYLOAD}" \
                    "https://api.osv.dev/v1/querybatch" 2>/dev/null || echo "{}")

                VULN_JSON=$(python3 -c "
import json, sys
resp = json.loads(sys.stdin.read())
pkgs_input = '''${PACKAGES}'''.strip().split('\n')
results = resp.get('results', [])
for i, (line, result) in enumerate(zip(pkgs_input, results)):
    vulns = result.get('vulns', [])
    if vulns:
        ids = ', '.join(v['id'] for v in vulns[:3])
        print(f'{line}  VULNS:{ids}')
" <<< "${OSV_RESP}" 2>/dev/null || true)

                while IFS= read -r line; do
                    [[ -n "${line}" ]] && VULN_PKGS+=("${line}")
                done <<< "${VULN_JSON}"

                if (( ${#VULN_PKGS[@]} > 0 )); then
                    fail "OSV scan found vulnerabilities:"
                    for v in "${VULN_PKGS[@]}"; do
                        fail "  ${v}"
                    done
                    fail "Resolve vulnerabilities before installing. Use 'uv add <pkg>==<fixed_version>' and re-lock."
                    exit 1
                fi
                ok "OSV scan: no known vulnerabilities in locked packages"
            else
                warn "curl not found — skipping OSV scan"
            fi

            # PyPI age check (sample first 5 packages to avoid rate limiting; full check on --dry-run)
            CHECKED=0
            while IFS=" " read -r PKG VER; do
                [[ -z "${PKG}" ]] && continue
                AGE_RESULT=$(curl -sf --max-time 10 \
                    "https://pypi.org/pypi/${PKG}/${VER}/json" 2>/dev/null | \
                    python3 -c "
import json, sys
from datetime import datetime, timezone
data = json.load(sys.stdin)
ver = '${VER}'
files = data.get('releases', {}).get(ver, [])
if not files:
    print('NOT_FOUND')
    sys.exit(0)
earliest = min(f['upload_time_iso_8601'] for f in files)
dt = datetime.fromisoformat(earliest.replace('Z', '+00:00'))
age_h = (datetime.now(timezone.utc) - dt).total_seconds() / 3600
print(f'TOO_NEW:{age_h:.1f}h' if age_h < ${MIN_AGE_HOURS} else f'OK:{age_h:.1f}h')
" 2>/dev/null || echo "SKIP")
                if [[ "${AGE_RESULT}" == TOO_NEW* ]]; then
                    AGE_FAILS+=("${PKG}==${VER} (${AGE_RESULT})")
                fi
                (( CHECKED++ )) || true
                [[ $DRY_RUN -eq 0 && $CHECKED -ge 10 ]] && break  # sample check in non-dry-run
            done <<< "${PACKAGES}"

            if (( ${#AGE_FAILS[@]} > 0 )); then
                fail "Package age check failed (min age: ${MIN_AGE_HOURS}h):"
                for f in "${AGE_FAILS[@]}"; do fail "  ${f}"; done
                fail "Wait until packages age through the hold window, or use --cve / --skip-strict."
                exit 1
            fi
            ok "Package age check passed (${CHECKED} packages checked)"
        fi
    fi
}

# ── Step 10: Install Python environment ───────────────────────────────────────

setup_python_env() {
    if [[ "$TARGET" != "dev" ]]; then
        step "Python environment (dev-target only — skipped on public)"
        info "The v0.5 daemon is a single Go binary; no host venv required."
        return
    fi

    step "Python environment"

    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would run: uv sync --frozen --require-hashes"
    elif [[ -f "${SCRIPT_DIR}/uv.lock" ]]; then
        cd "${SCRIPT_DIR}"
        uv sync --frozen --require-hashes 2>&1 | tail -5
        ok "uv sync complete"
    else
        warn "uv.lock not found — running uv pip install from venv instead"
        uv pip install --python /opt/forge/venv/bin/python \
            flask flask-login argon2-cffi tomlkit filelock \
            gunicorn gevent cachetools pydantic \
            mistune bleach pillow defusedxml gguf huggingface_hub \
            2>&1 | tail -5
        ok "pip install complete"
    fi
}

# ── Step 11: System directories + user ────────────────────────────────────────

create_dirs() {
    step "System directories"

    DIRS=(
        /etc/forge
        /opt/forge
        /var/lib/forge
        /var/log/forge
        /var/cache/forge/thumbs
    )
    if [[ "$TARGET" == "dev" ]]; then
        DIRS+=(/mnt/forge)
    fi

    for DIR in "${DIRS[@]}"; do
        if [[ $DRY_RUN -eq 1 ]]; then
            info "[dry-run] Would create: ${DIR}"
        else
            mkdir -p "${DIR}"
        fi
    done

    if [[ $DRY_RUN -eq 0 ]]; then
        # Ensure testuser owns the app directory
        chown -R testuser:testuser /opt/forge /var/lib/forge /var/cache/forge
        chmod 755 /var/log/forge
        ok "Directories created and permissions set"
    else
        info "[dry-run] Would set ownership/permissions on app directories"
    fi
}

# ── Step 12: Deploy application files ────────────────────────────────────────

deploy_app_files() {
    step "Deploy application files"

    FORGE_SRC="${SCRIPT_DIR}/forge"
    FORGE_DEST="/opt/forge"

    if [[ ! -d "${FORGE_SRC}" ]]; then
        fail "forge/ source directory not found at ${FORGE_SRC}"; exit 1
    fi

    # v0.5 ships the dashboard/a0/MCP as the single Go binary (`forge`, deployed
    # via scripts/deploy-forge.sh). This dir now only carries legacy web assets.
    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would sync ${FORGE_SRC}/templates → ${FORGE_DEST}/templates"
        info "[dry-run] Would sync ${FORGE_SRC}/static → ${FORGE_DEST}/static"
    else
        rsync -a --delete "${FORGE_SRC}/templates/" "${FORGE_DEST}/templates/" 2>/dev/null || \
            cp -r "${FORGE_SRC}/templates" "${FORGE_DEST}/"
        rsync -a "${FORGE_SRC}/static/" "${FORGE_DEST}/static/" 2>/dev/null || \
            cp -r "${FORGE_SRC}/static" "${FORGE_DEST}/"
        chown -R testuser:testuser "${FORGE_DEST}"
        ok "Web assets synced to ${FORGE_DEST}"
    fi
}

# ── Step 13: Config ────────────────────────────────────────────────────────────
# v0.5 (forge binary): configuration is store-backed (catalog DB + infra.* settings +
# auth store). No /etc/forge/config.toml / secrets.toml are written or read.

apply_config() {
    step "Configuration"

    info "v0.5 store-backed config — nothing to bootstrap"
}

# ── Step 14: Systemd units (+ public-target KGC marker & provisioning prompt) ─

install_systemd_units() {
    step "Systemd units"

    SYSTEMD_SRC="${SCRIPT_DIR}/systemd"
    UNITS=(
        forge-dashboard.service
    )
    # NOTE: boot-to-default-mode was retired several major versions ago — no
    # forge-boot.service exists anymore; models load on demand via the a0 scheduler.

    for UNIT in "${UNITS[@]}"; do
        SRC="${SYSTEMD_SRC}/${UNIT}"
        if [[ -f "${SRC}" ]]; then
            if [[ $DRY_RUN -eq 0 ]]; then
                cp "${SRC}" "/etc/systemd/system/${UNIT}"
                ok "Installed: ${UNIT}"
            else
                info "[dry-run] Would install: /etc/systemd/system/${UNIT}"
            fi
        else
            warn "Unit file not found: ${SRC}"
        fi
    done

    if [[ $DRY_RUN -eq 0 ]]; then
        systemctl daemon-reload
        systemctl enable forge-dashboard.service 2>/dev/null || true
        ok "systemd units reloaded and enabled"
    fi

    if [[ "$TARGET" == "public" ]]; then
        run_kgc_match
        offer_provisioning
    fi
}

run_kgc_match() {
    info "Matching hardware against KGC profile registry ..."
    local kgc_out rc
    kgc_out="$(python3 "${SCRIPT_DIR}/installer/kgc-match.py" 2>/dev/null)" && rc=0 || rc=$?
    if [[ $rc -eq 0 && -n "${kgc_out}" ]]; then
        if [[ $DRY_RUN -eq 0 ]]; then
            printf 'FORGE_KGC_PROFILE=%s\n' "${kgc_out}" > /etc/sysconfig/forge-kgc
            chmod 644 /etc/sysconfig/forge-kgc
        fi
        ok "KGC profile matched: ${kgc_out} (written to /etc/sysconfig/forge-kgc)"
        # NOTE(scheduler-integration): runtime auto-tuning is SKIPPED when this
        # marker exists — the scheduler's auto-tune path is expected to read
        # /etc/sysconfig/forge-kgc and adopt the profile's verified baselines
        # (see installer/kgc/profiles/<id>.json) instead of probing at runtime.
        # INTEGRATION STUB: no consumer is wired in go/ yet (this track does not
        # touch go/); flagged in the track report. Until wired, the marker is
        # informational only.
        info "Runtime auto-tuning will be skipped in favour of this profile's baselines."
    elif [[ $rc -eq 1 ]]; then
        warn "No KGC profile matched this host — runtime auto-tuning stays enabled"
    else
        warn "KGC match could not run on this host (see installer/kgc-match.py) — runtime auto-tuning stays enabled"
    fi
}

offer_provisioning() {
    echo -e "\n    ${BOLD}Model provisioning${NC}"
    info "Mandatory assets (embedding, STT, TTS, aligner, default brain GGUF) are listed in"
    info "installer/assets.manifest.json and fetched by installer/provision.py."
    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would offer: python3 installer/provision.py --mandatory --root $(dirname "${MODELS_DIR}")"
        return
    fi
    echo -en "\n  Provision mandatory assets now? Downloads can be large. [y/N]: "
    if [[ $YES -ne 1 ]]; then
        read -r ANSWER || ANSWER="n"
    else
        ANSWER="n"   # --yes must never surprise-download tens of GB
    fi
    if [[ "${ANSWER,,}" == "y" ]]; then
        python3 "${SCRIPT_DIR}/installer/provision.py" --mandatory --root "$(dirname "${MODELS_DIR}")" || \
            warn "provision.py reported failures — re-run it later; the stack works once assets land"
    else
        info "Skipping for now. Run later:"
        info "  sudo python3 ${SCRIPT_DIR}/installer/provision.py --mandatory --root $(dirname "${MODELS_DIR}")"
    fi
}

# ── Step 15/16: SELinux + health check + summary ──────────────────────────────

finish_selinux_health_summary() {
    step "SELinux labels + service start + summary"

    if command -v chcon &>/dev/null && [[ $DRY_RUN -eq 0 ]]; then
        chcon -R -t bin_t /opt/forge/venv/bin/ 2>/dev/null || true
        for BIN in "${LLAMA_VULKAN_BIN}" "${LLAMA_ROCM_BIN}"; do
            [[ -f "${BIN}" ]] && chcon -t bin_t "${BIN}" 2>/dev/null || true
        done
        ok "SELinux labels applied"
    fi

    if [[ $DRY_RUN -eq 0 ]]; then
        info "Starting forge-dashboard.service ..."
        systemctl restart forge-dashboard.service 2>/dev/null || true

        # Wait for health endpoint
        HEALTH_OK=0
        for i in $(seq 1 30); do
            if curl -sf --max-time 2 http://localhost:5000/api/v1/health &>/dev/null; then
                HEALTH_OK=1
                break
            fi
            sleep 1
        done

        if [[ $HEALTH_OK -eq 1 ]]; then
            ok "forge-dashboard.service running — /api/v1/health responded"
        else
            warn "forge-dashboard.service did not respond on /api/v1/health within 30s"
            info "Check: journalctl -u forge-dashboard.service -n 50"
        fi
    fi

    # ── Final summary ──
    echo -e "\n${BOLD}${C}══════════════════════════════════════════════${NC}"
    echo -e "${BOLD}${C}  Install Summary (target: ${TARGET})${NC}"
    echo -e "${BOLD}${C}══════════════════════════════════════════════${NC}\n"

    if (( ${#WARNINGS[@]} > 0 )); then
        echo -e "${Y}Warnings (${#WARNINGS[@]}):${NC}"
        for w in "${WARNINGS[@]}"; do
            echo -e "  ${Y}⚠${NC}  ${w}"
        done
        echo
    fi

    if [[ $DRY_RUN -eq 1 ]]; then
        echo -e "${Y}DRY RUN complete — no changes made.${NC}"
        return
    fi

    echo -e "${G}${BOLD}Installation complete.${NC}"

    if [[ "$TARGET" == "dev" ]]; then
        TAILNET_MSG=""
        if [[ -n "${TAILNET:-}" ]]; then
            TAILNET_MSG=" at https://ops.${TAILNET}"
        fi
        echo -e "Dashboard${TAILNET_MSG}"
        if [[ -n "${TAILNET:-}" ]]; then
            echo -e "Inference: https://a1.${TAILNET} (primary slot)"
        fi
        echo -e "\n${DIM}BIOS checklist and kernel parameters: docs/bios-setup.md${NC}"
        echo -e "${DIM}Adding models: docs/adding-a-model.md${NC}"
    else
        # Public next-steps
        echo -e "\n${BOLD}Next steps:${NC}"
        echo -e "  1. Provision mandatory models (if not done above):"
        echo -e "     ${DIM}sudo python3 ${SCRIPT_DIR}/installer/provision.py --mandatory --root $(dirname "${MODELS_DIR}")${NC}"
        echo -e "  2. Open the dashboard: ${DIM}http://localhost:5000${NC} (first visit creates the admin account)"
        echo -e "  3. Mint keys for agents/MCP consumers:"
        echo -e "     ${DIM}/opt/forge/forge mint-key -db /var/lib/forge/forge.db -kind mcp -name <agent>${NC}"
        echo -e "     (token prints once — store it immediately)"
        echo -e "  4. Ops console: ${DIM}forge tui${NC}  ·  fleet status: ${DIM}forge status${NC}"
        if [[ -f /etc/sysconfig/forge-kgc ]]; then
            echo -e "  5. Hardware profile pinned: ${DIM}$(cat /etc/sysconfig/forge-kgc)${NC}"
        fi
        echo -e "\n${DIM}Full flow: installer/README.md${NC}"
    fi
}

# ══════════════════════════════════════════════════════════════════════════════
# Main
# ══════════════════════════════════════════════════════════════════════════════

main() {
    if [[ "$TARGET" == "public" ]]; then
        run_preflight_step          # step 1 of ${TOTAL_STEPS} on public
    fi
    check_os_kernel                 # OS/kernel gates (stricter on dev)
    check_cpu
    check_bios
    check_gpu_vulkan
    check_rocm
    check_llama_bins
    check_prereqs
    check_model_inventory
    verify_packages
    setup_python_env
    create_dirs
    deploy_app_files
    apply_config
    install_systemd_units           # + KGC marker & provisioning prompt (public)
    finish_selinux_health_summary   # final step on both targets
}

main
