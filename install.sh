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

print_hermes_guide() {
    cat <<'HERMES'

════════════════════════════════════════════════════════════════
  AGENT CONNECTION GUIDE
  Connecting to Forge's inference API from agent frameworks
════════════════════════════════════════════════════════════════

Recommended: the a0 router (OpenAI-compatible, port 8085)

  https://a0.example.ts.net/v1/chat/completions

  a0 picks a live slot for you, tracks usage/cost, applies the compressor,
  and audit-logs the request — direct slot access below does none of that.
  Mint a bearer key first:
    /opt/forge/forge mint-key -db /var/lib/forge/forge.db -kind mcp -name <agent>
  (token prints once — store it immediately). Requests from a tailnet
  address skip the bearer check; anything else needs
  "Authorization: Bearer sk-router-...".

Direct slot access (bypasses a0 entirely — no usage tracking, no
compression, no audit log; tailnet membership is the only gate):

  https://a1.example.ts.net  (slot a1, port 8080 internally)
  https://a2.example.ts.net  (slot a2, port 8081 internally)
  — a3/a4 exist too; see `tailscale serve status` on the host for the
    full current list, it changes as slots/services are added.

Quick test (from any tailnet node):
  curl -s https://a0.example.ts.net/v1/models \
    -H "Authorization: Bearer sk-router-..." | python3 -m json.tool

The ops dashboard (mode switching, metrics) is at:
  https://ops.example.ts.net
  — requires Forge login credentials.

Full router design + auth model: docs/llm-router.md
HERMES
    exit 0
}

usage() {
    sed -n '2,22p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
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

# ── dev-target host guard ──────────────────────────────────────────────────
# --target=dev hardcodes /opt/forge paths and testuser:testuser ownership — correct by
# design (it's this operator's reference host), but a landmine if run
# elsewhere without reading the header comment above. Require --force to
# proceed with dev on a host that isn't recognizably ForgeHost.
if [[ "$TARGET" == "dev" ]]; then
    CURRENT_HOST="$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo unknown)"
    if [[ "${CURRENT_HOST,,}" != "forgehost"* ]]; then
        if [[ $FORCE -eq 1 ]]; then
            warn "Host '${CURRENT_HOST}' doesn't look like ForgeHost — continuing with --target=dev anyway (--force)"
        else
            fail "Host '${CURRENT_HOST}' doesn't look like ForgeHost, but --target=dev hardcodes"
            fail "/opt/forge paths and testuser:testuser ownership for the reference host."
            fail "Use --target=public on a generic host, or pass --force to proceed with dev anyway."
            exit 1
        fi
    fi
fi

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

    # Required models come from installer/assets.manifest.json's mandatory,
    # non-service entries — the same manifest offer_provisioning already
    # treats as authoritative. A hardcoded model list drifts silently (the
    # catalog DB, not this script, owns which models a live deployment
    # actually uses); the manifest is the one place this repo already
    # maintains that list.
    MANIFEST="${SCRIPT_DIR}/installer/assets.manifest.json"
    ROOT_DIR="$(dirname "${MODELS_DIR}")"
    if [[ ! -f "${MANIFEST}" ]]; then
        warn "installer/assets.manifest.json not found — skipping mandatory-model check"
    else
        while IFS=$'\t' read -r MODEL_ID MODEL_PATH; do
            [[ -z "${MODEL_ID}" ]] && continue
            if [[ -f "${ROOT_DIR}/${MODEL_PATH}" ]]; then
                ok "Found: ${MODEL_ID}"
            else
                warn "Missing: ${MODEL_ID} (expected ${ROOT_DIR}/${MODEL_PATH})"
                if [[ $DRY_RUN -eq 0 ]]; then
                    echo -en "  Download now? [y/N]: "
                    read -r -t 30 ANSWER || ANSWER="n"
                    if [[ "${ANSWER,,}" == "y" ]]; then
                        info "  Use: installer/provision.py --only ${MODEL_ID} --root ${ROOT_DIR}"
                    fi
                fi
            fi
        done < <(python3 - "${MANIFEST}" <<'PYEOF'
import json, sys
m = json.load(open(sys.argv[1]))
for item in m.get("models", []):
    if item.get("role") != "mandatory" or item.get("type") == "service":
        continue
    for f in item.get("files", []):
        fn = f.get("filename")
        if fn:
            print(f"{item['id']}\t{item['dest_rel_path']}/{fn}")
PYEOF
)
    fi

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
    # This used to check uv.lock (PyPI age + OSV) for V4's Python supply
    # chain. There is no uv.lock, pyproject.toml, or Python package supply
    # chain left in this repo since the 2026-07-28 TOML-decommission cutover
    # — the v0.5 daemon is a single self-contained Go binary — so this step
    # is a permanent no-op on every target now, not just "public".
    step "Package verification (none required — v0.5 is a single Go binary)"
}

# ── Step 10: Install Python environment ───────────────────────────────────────

setup_python_env() {
    # V4's Flask/gunicorn+gevent app (the only thing that ever needed this
    # venv) was deleted from the repo in the 2026-07-28 TOML-decommission
    # cutover — there is no rollback path and no pyproject.toml/uv.lock left
    # in this repo to sync from. The v0.5 daemon is a single Go binary; this
    # step is a no-op on every target now, not just "public". (It used to
    # fall back to `uv pip install`-ing V4's Python stack — flask, gunicorn,
    # gevent, tomlkit, etc — whenever uv.lock was absent, which is always
    # true today; that branch is deleted, not patched, since nothing in this
    # repo can use its output anymore.)
    step "Python environment (none required — v0.5 is a single Go binary)"
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

# ── Step 12: Build & deploy the forge binary ──────────────────────────────────
# v0.5 is a single Go binary serving the dashboard, a0 router, and MCP — the
# React PWA is embedded into it via go:embed (go/internal/httpapi/pwa.go),
# not read from disk. This step used to sync forge/templates + forge/static
# instead of ever building/installing the actual binary: those directories
# are leftover pre-Go-rewrite assets nothing in go/ reads at runtime (the
# live dashboard is embedded web_dist), so a fresh install could complete
# with no forge binary at /opt/forge/forge at all.

deploy_app_files() {
    step "Build & deploy forge binary"

    GO_SRC="${SCRIPT_DIR}/go"
    PREBUILT="${SCRIPT_DIR}/forge"
    DEST="/opt/forge/forge"

    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would build ${GO_SRC}/cmd/forge (or use ${PREBUILT} if go/ is absent) and atomically deploy to ${DEST}"
        return
    fi

    VERSION="$(git -C "${SCRIPT_DIR}" describe --tags --always --dirty 2>/dev/null || echo dev)"

    if [[ -d "${GO_SRC}" ]]; then
        if ! command -v go &>/dev/null; then
            fail "go toolchain not found on PATH — required to build ${GO_SRC}/cmd/forge"
            fail "Install Go, or remove ${GO_SRC} and place a pre-built binary at ${PREBUILT} instead."
            exit 1
        fi
        info "Building forge binary (version ${VERSION}) ..."
        ( cd "${GO_SRC}" && go build -ldflags "-X main.version=${VERSION}" -o "${SCRIPT_DIR}/forge" ./cmd/forge )
        ok "Build complete"
    elif [[ -x "${PREBUILT}" ]]; then
        info "go/ module not present — using pre-built binary at ${PREBUILT}"
    else
        fail "Neither ${GO_SRC} (Go module) nor a pre-built ${PREBUILT} binary found — nothing to deploy."
        exit 1
    fi

    # Atomic same-fs copy → chmod 755 → chcon bin_t → mv. A fresh `install`/
    # `cp`-created file lands 0644 with the default (non-bin_t) SELinux
    # context; systemd then refuses to exec it (status=203/EXEC), and
    # overwriting a running binary in place risks ETXTBSY. Both were hit
    # live against this exact deploy path (2026-07-29/07-30) — see
    # CLAUDE.md's "Deploy" section.
    TMP_BIN="/opt/forge/forge.new"
    install -m 0755 "${SCRIPT_DIR}/forge" "${TMP_BIN}"
    chcon -u system_u -r object_r -t bin_t "${TMP_BIN}" 2>/dev/null || restorecon -F "${TMP_BIN}" 2>/dev/null || true
    mv -f "${TMP_BIN}" "${DEST}"
    chown testuser:testuser "${DEST}"
    ok "forge binary deployed to ${DEST} (${VERSION})"
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
    # This list is ground-truthed against `systemctl list-unit-files
    # 'forge-*'` on the live reference host (ForgeHost), not guessed from
    # systemd/*.service's file listing — that directory also still carries
    # dead units from before the primary/secondary→a1/a2 rename and the
    # dashboard/a0/MCP single-binary merge (forge-dashboard.service,
    # forge-primary.service, forge-secondary.service, forge-mcp.service,
    # forge-router.service). forge-dashboard.service in particular execs a
    # gunicorn+V4 Flask app deleted from the repo in the 2026-07-28
    # TOML-decommission cutover — installing and enabling it, as this
    # script used to, would crash-loop on first boot. forge-daemon.service
    # is the real v0.5 binary (dashboard + a0 router + MCP, one process).
    UNITS=(
        forge-daemon.service
        forge-a1.service
        forge-a2.service
        forge-a3.service
        forge-a4.service
        forge-aligner.service
        forge-embedding.service
        forge-stt.service
        forge-tts.service
        forge-tts-base.service
        forge-tts-custom.service
        forge-tts-backup.service
        forge-tts-backup.timer
        forge-compress@.service
    )
    # Enabled (WantedBy=multi-user.target, starts on boot): the always-on
    # core + infra services. a1-a4 are scheduler-managed (the a0 on-demand
    # loader starts/stops them, not systemd at boot) and forge-compress@ is
    # a template — instances are provisioned per-provider at runtime by
    # internal/headroom.Provisioner, never enabled directly. Neither is
    # started here, matching live ForgeHost (`static`/`disabled` respectively).
    ENABLE_UNITS=(
        forge-daemon.service
        forge-aligner.service
        forge-embedding.service
        forge-stt.service
        forge-tts.service
        forge-tts-base.service
        forge-tts-custom.service
    )

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

    POLKIT_SRC="${SCRIPT_DIR}/polkit/50-forge.rules"
    if [[ -f "${POLKIT_SRC}" ]]; then
        if [[ $DRY_RUN -eq 0 ]]; then
            cp "${POLKIT_SRC}" /etc/polkit-1/rules.d/50-forge.rules
            ok "Installed: polkit/50-forge.rules (passwordless forge-* restart for testuser/forge)"
        else
            info "[dry-run] Would install: /etc/polkit-1/rules.d/50-forge.rules"
        fi
    else
        warn "Polkit rule not found: ${POLKIT_SRC} — restart/reconfigure actions will prompt for a password"
    fi

    if [[ $DRY_RUN -eq 0 ]]; then
        systemctl daemon-reload
        for UNIT in "${ENABLE_UNITS[@]}"; do
            systemctl enable "${UNIT}" 2>/dev/null || true
        done
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

# ── Tailscale service exposure ─────────────────────────────────────────────────
# Only offered when check_prereqs already confirmed the host is joined to a
# tailnet (TS_OK=1). Runs as a sub-action of the final step, not its own
# numbered step, matching run_kgc_match/offer_provisioning's pattern.

configure_tailscale_serve() {
    [[ ${TS_OK:-0} -eq 1 ]] || return 0

    # Only the ports this script can state with certainty at install time:
    # ops/a0/a1-a4 are config.go's compiled-in defaults, tts is
    # forge-tts.service's own hardcoded FORGE_TTS_LISTEN (systemd/forge-
    # tts.service). embedding/stt/aligner bind to whatever port the operator
    # points infra.ports at post-install (Settings → Infrastructure) — this
    # script cannot know that value yet, so they're deliberately left out.
    # See docs/infrastructure.md for adding those once configured, and for
    # the tailnet-policy step some tailnets require before --service=svc:x
    # will actually bind.
    # Parallel indexed arrays (not associative) — matches this script's
    # existing array style everywhere else and stays portable to bash 3.2
    # (macOS's shipped /bin/bash, used to develop/test this script, has no
    # declare -A at all).
    local NAMES=(ops a0 a1 a2 a3 a4 tts)
    local PORTS=(5000 8085 8080 8081 8087 8088 8082)

    echo -e "\n    ${BOLD}Tailscale service exposure${NC}"
    info "Host is on tailnet (${TAILNET:-unknown}). This can expose Forge's core"
    info "services as named tailnet services (HTTPS, tailnet-only):"
    local i
    for i in "${!NAMES[@]}"; do
        info "  sudo tailscale serve --bg --service=svc:${NAMES[$i]} --https=443 localhost:${PORTS[$i]}"
    done
    info "embedding/stt/aligner aren't included — their real ports depend on your own"
    info "STT/embedding/aligner setup. See docs/infrastructure.md for adding those once"
    info "infra.ports is configured (Settings → Infrastructure)."

    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would offer to configure the ${#NAMES[@]} services above"
        return 0
    fi

    echo -en "\n  Configure these ${#NAMES[@]} Tailscale services now? [y/N]: "
    if [[ $YES -ne 1 ]]; then
        read -r ANSWER || ANSWER="n"
    else
        ANSWER="n"   # --yes must never silently expose new public hostnames
    fi
    if [[ "${ANSWER,,}" != "y" ]]; then
        info "Skipping. Run the commands above manually whenever you're ready."
        return 0
    fi

    # install.sh already runs as root (its own usage header requires sudo),
    # so tailscale is invoked directly here — the printed commands above
    # carry their own sudo for anyone copying them into a non-root shell.
    local ANY_FAILED=0
    for i in "${!NAMES[@]}"; do
        if tailscale serve --bg --service="svc:${NAMES[$i]}" --https=443 "localhost:${PORTS[$i]}" 2>/dev/null; then
            ok "svc:${NAMES[$i]} → localhost:${PORTS[$i]}"
        else
            warn "svc:${NAMES[$i]} failed — some tailnets require declaring the service in the admin console's policy file first (docs/infrastructure.md)"
            ANY_FAILED=1
        fi
    done
    if [[ $ANY_FAILED -eq 0 ]]; then
        ok "All Tailscale services configured"
    fi
}

# ── Step 15/16: SELinux + health check + summary ──────────────────────────────

finish_selinux_health_summary() {
    step "SELinux labels + service start + summary"

    if command -v chcon &>/dev/null && [[ $DRY_RUN -eq 0 ]]; then
        # /opt/forge/forge itself is already labeled bin_t by
        # deploy_app_files' atomic install — this is only the llama.cpp
        # inference binaries, which live outside that path.
        for BIN in "${LLAMA_VULKAN_BIN}" "${LLAMA_ROCM_BIN}"; do
            [[ -f "${BIN}" ]] && chcon -t bin_t "${BIN}" 2>/dev/null || true
        done
        ok "SELinux labels applied"
    fi

    if [[ $DRY_RUN -eq 0 ]]; then
        info "Starting forge-daemon.service ..."
        systemctl restart forge-daemon.service 2>/dev/null || true

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
            ok "forge-daemon.service running — /api/v1/health responded"
        else
            warn "forge-daemon.service did not respond on /api/v1/health within 30s"
            info "Check: journalctl -u forge-daemon.service -n 50"
        fi
    fi

    configure_tailscale_serve

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
            echo -e "Agent API (a0 router, tracked/compressed): https://a0.${TAILNET}"
            echo -e "Direct slot access (untracked, tailnet-gated only): https://a1.${TAILNET}, a2, a3, a4"
            echo -e "${DIM}  --help-hermes for the full agent connection guide${NC}"
        fi
        echo -e "\n${DIM}BIOS checklist and kernel parameters: docs/bios-setup.md${NC}"
        echo -e "${DIM}Adding models: docs/adding-a-model.md${NC}"
    else
        # Public next-steps
        echo -e "\n${BOLD}Next steps:${NC}"
        echo -e "  1. Provision mandatory models (if not done above):"
        echo -e "     ${DIM}sudo python3 ${SCRIPT_DIR}/installer/provision.py --mandatory --root $(dirname "${MODELS_DIR}")${NC}"
        if [[ -n "${TAILNET:-}" ]]; then
            echo -e "  2. Open the dashboard: ${DIM}https://ops.${TAILNET}${NC} (first visit creates the admin account)"
        else
            echo -e "  2. Open the dashboard: ${DIM}http://localhost:5000${NC} (first visit creates the admin account)"
        fi
        echo -e "  3. Mint keys for agents/MCP consumers:"
        echo -e "     ${DIM}/opt/forge/forge mint-key -db /var/lib/forge/forge.db -kind mcp -name <agent>${NC}"
        echo -e "     (token prints once — store it immediately)"
        echo -e "  4. Ops console: ${DIM}forge tui${NC}  ·  fleet status: ${DIM}forge status${NC}"
        if [[ -f /etc/sysconfig/forge-kgc ]]; then
            echo -e "  5. Hardware profile pinned: ${DIM}$(cat /etc/sysconfig/forge-kgc)${NC}"
        fi
        if [[ -n "${TAILNET:-}" ]]; then
            echo -e "\n${DIM}Agent API (a0 router, tracked/compressed): https://a0.${TAILNET}${NC}"
            echo -e "${DIM}  --help-hermes for the full agent connection guide${NC}"
        fi
        echo -e "\n${DIM}Full flow: installer/README.md${NC}"
    fi

    echo -e "${DIM}Tailscale networking/ports reference: docs/infrastructure.md${NC}"
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
