#!/usr/bin/env bash
# preflight.sh — The Forge hardware/software pre-flight check library
#
# Sourced by install.sh (provides preflight_run / preflight_json), or run
# standalone:
#
#   installer/preflight.sh                 # table output, public-target rules
#   installer/preflight.sh --json          # machine-readable output
#   installer/preflight.sh --target=dev    # dev rules (FAILs tolerated by caller)
#
# Exit codes (standalone):
#   0  all checks passed (warnings allowed)
#   1  at least one FAIL
#   2  usage error
#
# Checks: kernel >= 6.5 (gfx1151 support), amdgpu module, Vulkan loader + ICD,
# ROCm >= 6.0, GTT pool size, system RAM, free disk, core tools.

PREFLIGHT_TARGET="${PREFLIGHT_TARGET:-public}"

# ── Result collection ─────────────────────────────────────────────────────────

PF_RESULTS=()   # "STATUS|CHECK|DETAIL"

pf_pass() { PF_RESULTS+=("PASS|$1|$2"); }
pf_warn() { PF_RESULTS+=("WARN|$1|$2"); }
pf_fail() { PF_RESULTS+=("FAIL|$1|$2"); }

pf_summary() {
    local p=0 w=0 f=0 r s
    for r in "${PF_RESULTS[@]}"; do
        s="${r%%|*}"
        case "$s" in
            PASS) ((p++)) || true ;;
            WARN) ((w++)) || true ;;
            FAIL) ((f++)) || true ;;
        esac
    done
    echo "$p $w $f"
}

pf_print_table() {
    printf '\n'
    printf '  %-6s %-18s %s\n' "STATUS" "CHECK" "DETAIL"
    printf '  %-6s %-18s %s\n' "------" "------------------" "--------------------"
    local r s c d
    for r in "${PF_RESULTS[@]}"; do
        s="${r%%|*}"; r="${r#*|}"; c="${r%%|*}"; d="${r#*|}"
        printf '  %-6s %-18s %s\n' "$s" "$c" "$d"
    done
    local counts
    counts="$(pf_summary)"
    printf '\n  Summary: %s pass, %s warn, %s fail\n' \
        "$(echo "$counts" | cut -d' ' -f1)" \
        "$(echo "$counts" | cut -d' ' -f2)" \
        "$(echo "$counts" | cut -d' ' -f3)"
}

pf_print_json() {
    local r s c d first=1
    printf '{"target":"%s","results":[' "$PREFLIGHT_TARGET"
    for r in "${PF_RESULTS[@]}"; do
        s="${r%%|*}"; r="${r#*|}"; c="${r%%|*}"; d="${r#*|}"
        d="${d//\\/\\\\}"; d="${d//\"/\\\"}"
        [[ $first -eq 1 ]] || printf ','
        first=0
        printf '{"check":"%s","status":"%s","detail":"%s"}' "$c" "$s" "$d"
    done
    local counts
    counts="$(pf_summary)"
    printf '],"summary":{"pass":%s,"warn":%s,"fail":%s}}\n' \
        "$(echo "$counts" | cut -d' ' -f1)" \
        "$(echo "$counts" | cut -d' ' -f2)" \
        "$(echo "$counts" | cut -d' ' -f3)"
}

# ── Individual checks ─────────────────────────────────────────────────────────

pf_version_ge() {  # pf_version_ge <have> <min> — dotted-decimal compare
    local h m
    h="$(printf '%s\n%s\n' "$1" "$2" | sort -t. -k1,1n -k2,2n -k3,3n | head -1)"
    m="$2"
    [[ "$h" == "$m" ]]
}

pf_check_kernel() {
    if [[ "$(uname -s)" != "Linux" ]]; then
        pf_fail "kernel" "not Linux ($(uname -s)) — Forge inference requires a Linux host with gfx1151 support"
        return
    fi
    local kver
    kver="$(uname -r)"
    # Strip local suffix (e.g. 6.12.15-200.fc41.x86_64 -> 6.12.15)
    kver="${kver%%-*}"
    if pf_version_ge "$kver" "6.5"; then
        pf_pass "kernel" "${kver} (>= 6.5 required for gfx1151)"
    else
        pf_fail "kernel" "${kver} too old — >= 6.5 required for gfx1151 (RDNA3.5) support"
    fi
}

pf_check_amdgpu() {
    if grep -q '^amdgpu ' /proc/modules 2>/dev/null; then
        pf_pass "amdgpu" "module loaded"
    else
        pf_fail "amdgpu" "module not loaded — check dmesg; Secure Boot may block unsigned modules"
    fi
}

pf_check_vulkan() {
    local found_loader=0 icds=()
    if ldconfig -p 2>/dev/null | grep -q libvulkan; then
        found_loader=1
    fi
    if (( found_loader )); then
        pf_pass "vulkan-loader" "libvulkan present"
    else
        pf_fail "vulkan-loader" "libvulkan not found — install mesa-vulkan-drivers / vulkan-loader"
    fi
    # shellcheck disable=SC2207
    icds=(/usr/share/vulkan/icd.d/*.json)
    if [[ -f "${icds[0]}" ]]; then
        pf_pass "vulkan-icd" "${#icds[@]} ICD(s) in /usr/share/vulkan/icd.d/"
    else
        pf_fail "vulkan-icd" "no ICD json in /usr/share/vulkan/icd.d/ — install a Vulkan driver (RADV for AMD)"
    fi
}

pf_find_rocm() {
    # Echo "<path> <version>" for the first usable ROCm install, or nothing.
    local p v
    if command -v rocm-smi &>/dev/null; then
        p="$(dirname "$(dirname "$(command -v rocm-smi)")")"
    fi
    for p in "${ROCM_ROOT:-}" /opt/rocm /opt/rocm-*; do
        [[ -n "$p" && -d "$p" ]] || continue
        if [[ -f "$p/.info/version" ]]; then
            v="$(cat "$p/.info/version")"
        elif [[ -x "$p/bin/rocm-smi" ]]; then
            v="$("$p/bin/rocm-smi" --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' | head -1)"
        else
            v=""
        fi
        if [[ -n "$v" ]]; then
            echo "$p $v"
            return 0
        fi
    done
    return 1
}

pf_check_rocm() {
    local loc
    loc="$(pf_find_rocm)" || loc=""
    if [[ -z "$loc" ]]; then
        # ROCm is optional (Vulkan is the default backend; ROCm only needed for
        # very large models) — warn, don't fail.
        pf_warn "rocm" "not found (checked \$ROCM_ROOT, /opt/rocm*, PATH) — large (>63 GB) models unavailable; Vulkan backend unaffected"
        return
    fi
    local path="${loc%% *}" ver="${loc##* }"
    if pf_version_ge "$ver" "6.0"; then
        pf_pass "rocm" "${path} v${ver} (>= 6.0)"
    else
        pf_fail "rocm" "${path} v${ver} too old — >= 6.0 required for gfx1151"
    fi
}

pf_gtt_bytes() {
    # Try mem_info_gtt_total sysfs nodes first, then rocm-smi.
    local f total=0 b
    for f in /sys/class/drm/card*/device/mem_info_gtt_total; do
        [[ -f "$f" ]] || continue
        b="$(cat "$f" 2>/dev/null || echo 0)"
        (( b > total )) && total=$b
    done
    if (( total > 0 )); then
        echo "$total"
        return 0
    fi
    if command -v rocm-smi &>/dev/null; then
        b="$(rocm-smi --showmeminfo gtt 2>/dev/null | grep -i 'total' | grep -oE '[0-9]+' | head -1)"
        if [[ -n "$b" && "$b" -gt 0 ]]; then
            echo "$((b * 1024))"   # rocm-smi reports KB
            return 0
        fi
    fi
    return 1
}

pf_check_gtt() {
    local gtt gb
    if ! gtt="$(pf_gtt_bytes)"; then
        pf_warn "gtt" "cannot determine GTT pool size (no sysfs node, no rocm-smi) — verify manually: rocm-smi --showmeminfo gtt"
        return
    fi
    gb=$(( gtt / 1024 / 1024 / 1024 ))
    if (( gb < 48 )); then
        pf_fail "gtt" "${gb} GB — below 48 GB minimum; large-context loads will fail. Check amdgpu.gttsize kernel param and BIOS GART aperture"
    elif (( gb < 96 )); then
        pf_warn "gtt" "${gb} GB — below 96 GB recommended ceiling (~120 GB expected on Strix Halo)"
    else
        pf_pass "gtt" "${gb} GB"
    fi
}

pf_check_ram() {
    local kb gb required=96
    kb="$(awk '/MemTotal/ {print $2}' /proc/meminfo 2>/dev/null || echo 0)"
    gb=$(( kb / 1024 / 1024 ))
    if (( gb <= 0 )); then
        pf_fail "ram" "cannot read MemTotal from /proc/meminfo"
    elif (( gb < 96 )); then
        pf_fail "ram" "${gb} GB — below 96 GB minimum; 128 GB expected (unified-memory APU)"
    elif (( gb < 128 )); then
        pf_warn "ram" "${gb} GB — below 128 GB expected; largest models may not fit"
    else
        pf_pass "ram" "${gb} GB"
    fi
}

pf_check_disk() {
    local need_mb=256000 free_mb path="${FORGE_DATA_DIR:-/var/lib}"
    free_mb="$(df -Pm "$path" 2>/dev/null | awk 'NR==2 {print $4}')" || free_mb=0
    if [[ -z "$free_mb" || "$free_mb" -le 0 ]]; then
        pf_fail "disk" "cannot determine free space on ${path}"
    elif (( free_mb * 1024 * 1024 < 100 * 1024 * 1024 * 1024 )); then
        pf_fail "disk" "${free_mb} MB free on ${path} — critically low (mandatory assets need >= 250 GB)"
    elif (( free_mb < need_mb )); then
        pf_warn "disk" "${free_mb} MB free on ${path} — below 250 GB recommended for mandatory assets"
    else
        pf_pass "disk" "$(( free_mb / 1024 )) GB free on ${path}"
    fi
}

pf_check_tools() {
    local t
    for t in curl python3 systemctl; do
        if command -v "$t" &>/dev/null; then
            pf_pass "tool:$t" "$(command -v "$t")"
        else
            pf_fail "tool:$t" "not found in PATH — required"
        fi
    done
}

# ── Entry points ──────────────────────────────────────────────────────────────

preflight_run() {   # populates PF_RESULTS
    PF_RESULTS=()
    pf_check_kernel
    pf_check_amdgpu
    pf_check_vulkan
    pf_check_rocm
    pf_check_gtt
    pf_check_ram
    pf_check_disk
    pf_check_tools
}

preflight_main() {
    local out_mode="table"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --json) out_mode="json" ;;
            --target=*) PREFLIGHT_TARGET="${1#*=}" ;;
            *) echo "usage: installer/preflight.sh [--json] [--target=dev|public]" >&2; return 2 ;;
        esac
        shift
    done
    preflight_run
    if [[ "$out_mode" == "json" ]]; then
        pf_print_json
    else
        pf_print_table
    fi
    local counts
    counts="$(pf_summary)"
    [[ "${counts##* }" -eq 0 ]]
}

# Sourced vs standalone
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    preflight_main "$@"
    exit $?
fi
