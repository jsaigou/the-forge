#!/usr/bin/env bash
# installer/host.sh — what install.sh needs to know and do about the machine it runs on: the service user, systemd, build tools, unit files
# (docs/speech-setup-plan.md section 5.1). Sourced by install.sh; uses its ok/warn/fail/info helpers and TARGET, SERVICE_USER, SCRIPT_DIR, DRY_RUN.
# Platform differences warn on the public target and never stop it. Tests: bash installer/test_install.sh

SKIPPED=()

# skip <what> <why and how to get it>: a step that cannot work is left out by name and listed again in the summary.
skip() { SKIPPED+=("$1: $2"); warn "Skipped $1: $2"; }

host_has_systemd() { [[ "$(cat "${FORGE_PID1_FILE:-/proc/1/comm}" 2>/dev/null)" == "systemd" ]]; }

# dnf, apt or empty, from os-release.
host_pkg_tool() {
    local ids
    ids="$( (. "${FORGE_OS_RELEASE:-/etc/os-release}" 2>/dev/null && echo "${ID:-} ${ID_LIKE:-}") || true )"
    case " ${ids} " in
        *" fedora "*|*" rhel "*|*" centos "*) echo dnf ;;
        *" debian "*|*" ubuntu "*) echo apt ;;
    esac
}

# pkg_hint <dnf packages> <apt packages>: the command that installs them here.
pkg_hint() {
    case "$(host_pkg_tool)" in
        dnf) echo "sudo dnf install $1" ;;
        apt) echo "sudo apt install $2" ;;
        *)   echo "install with your package manager: $1" ;;
    esac
}

# The web app is embedded in the binary: either it is built already or Node builds it here.
host_web_prebuilt() { [[ -f "${SCRIPT_DIR}/go/internal/httpapi/web_dist/index.html" ]]; }

# A missing build tool with no prebuilt result stops the install, before anything is changed. Returns 1 and says what to install.
host_check_build_tools() {
    local missing=0
    if [[ -d "${SCRIPT_DIR}/go" ]]; then
        if ! command -v go &>/dev/null; then
            fail "Go is not installed; it builds the Forge. $(pkg_hint golang golang-go)"; missing=1
        fi
        if ! host_web_prebuilt && [[ -d "${SCRIPT_DIR}/web" ]]; then
            if ! command -v node &>/dev/null || ! command -v npm &>/dev/null; then
                fail "Node.js and npm are not installed; they build the web app. $(pkg_hint 'nodejs npm' 'nodejs npm')"; missing=1
            else
                local major; major="$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null || echo 0)"
                (( major >= 22 )) || warn "Node.js ${major} is older than the version the web app is built with (22 or later); the build may fail"
            fi
        fi
    elif [[ ! -f "${SCRIPT_DIR}/forge" ]]; then
        fail "Neither the Go sources (${SCRIPT_DIR}/go) nor a prebuilt binary (${SCRIPT_DIR}/forge) is here"; missing=1
    fi
    return $missing
}

# The daemon starts and stops its services over the system D-Bus, and polkit is what lets its user do that.
host_check_service_bus() {
    if [[ ! -S "${FORGE_DBUS_SOCKET:-/run/dbus/system_bus_socket}" ]]; then
        warn "No system D-Bus socket: the daemon cannot start without it. $(pkg_hint dbus-broker dbus)"
    fi
    # /usr/share/polkit-1 exists on machines without polkit (other packages put files there): look for the service itself.
    if [[ ! -x "${FORGE_POLKITD:-/usr/lib/polkit-1/polkitd}" ]]; then
        warn "polkit is not installed: the daemon cannot start engines or models without it. $(pkg_hint polkit polkitd)"
    fi
}

# Without a running polkit the daemon cannot start a single engine, and says so only in its log.
host_start_polkit() {
    systemctl start polkit.service &>/dev/null && return 0
    # Just installed: D-Bus may not have read its policy yet.
    systemctl reload dbus.service &>/dev/null; systemctl reset-failed polkit.service &>/dev/null
    systemctl start polkit.service &>/dev/null && return 0
    warn "polkit does not start, so the daemon cannot start engines or models. Check: journalctl -u polkit -n 30"
    return 0
}

ensure_service_user() {
    if id -u "${SERVICE_USER}" &>/dev/null; then
        ok "Service user ${SERVICE_USER} exists"
    elif [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would create the service user ${SERVICE_USER}"
        return 0
    else
        local shell=/usr/sbin/nologin
        [[ -x $shell ]] || shell=/sbin/nologin
        useradd --system --user-group --home-dir /var/lib/forge --no-create-home --shell "$shell" "${SERVICE_USER}" \
            || { fail "Could not create the service user ${SERVICE_USER}"; return 1; }
        ok "Created the service user ${SERVICE_USER}"
    fi
    # An existing user chosen with --user need not have a group of its own name.
    SERVICE_GROUP="$(id -gn "${SERVICE_USER}")"
    local g
    for g in render video; do
        if getent group "$g" &>/dev/null && [[ $DRY_RUN -eq 0 ]]; then
            usermod -aG "$g" "${SERVICE_USER}" || warn "Could not add ${SERVICE_USER} to the ${g} group (GPU access needs it)"
        fi
    done
}

# run_as <user> <command...>: runs it as that user, with that user's home.
run_as() {
    local user="$1"; shift
    if [[ "$(id -un)" == "$user" ]]; then "$@"; return; fi
    local home; home="$(getent passwd "$user" 2>/dev/null | cut -d: -f6)"
    if command -v runuser &>/dev/null; then
        runuser -u "$user" -- env HOME="${home:-/tmp}" "$@"
    else
        su -s /bin/sh "$user" -c "HOME=$(printf '%q' "${home:-/tmp}") $(printf '%q ' "$@")"
    fi
}

# Builds run as the person who called sudo, so their checkout does not fill with files owned by root.
builder_user() {
    if [[ -n "${SUDO_USER:-}" && "${SUDO_USER}" != "root" ]]; then echo "${SUDO_USER}"; else id -un; fi
}

# render_unit <source> <destination>: on the public target the unit runs as the service user.
render_unit() {
    if [[ "$TARGET" == "public" ]]; then
        sed -E -e "s/^User=.*/User=${SERVICE_USER}/" -e "s/^Group=.*/Group=${SERVICE_GROUP:-${SERVICE_USER}}/" "$1" >"$2"
    else
        cp "$1" "$2"
    fi
}

render_polkit() {
    if [[ "$TARGET" == "public" ]]; then
        sed -E "s/subject\.user === \"[^\"]*\"( \|\| subject\.user === \"[^\"]*\")*/subject.user === \"${SERVICE_USER}\"/" "$1" >"$2"
    else
        cp "$1" "$2"
    fi
}

# The program a unit starts is there.
unit_program_present() {
    local program
    program="$(sed -n 's/^ExecStart=[-@+!]*\([^ \\]*\).*/\1/p' "$1" | head -1)"
    [[ -n "$program" && -x "$program" ]]
}

# SELinux refuses to start a program from /opt unless it carries bin_t.
label_bin() {
    command -v chcon &>/dev/null || return 0
    chcon -u system_u -r object_r -t bin_t "$1" 2>/dev/null && return 0
    if command -v getenforce &>/dev/null && [[ "$(getenforce)" == "Enforcing" ]]; then
        warn "Could not label $1 for SELinux; systemd may refuse to start it (chcon -t bin_t $1)"
    fi
    return 0
}

# install_program <source> <destination> <owner>: atomic, executable, labelled.
install_program() {
    install -m 0755 "$1" "$2.new" || return 1
    label_bin "$2.new"
    chown "$3:" "$2.new" 2>/dev/null || warn "Could not give $2 to $3"
    mv -f "$2.new" "$2"
}

# install_slot_launchers <source directory> <destination>: the scripts the model slot units (forge-a1..a4) run. Their one source is the directory the
# daemon embeds them from, so what is installed is what Smith checks them against. A different script already there is kept beside the new one.
install_slot_launchers() {
    local slot src dst failed=0
    for slot in a1 a2 a3 a4; do
        src="$1/start-${slot}.sh"; dst="$2/start-${slot}.sh"
        if [[ ! -f "$src" ]]; then
            skip "model slot ${slot}" "its launch script is not in this checkout (${src})"; failed=1; continue
        fi
        if [[ -f "$dst" ]] && ! cmp -s "$src" "$dst"; then
            local kept; kept="${dst}.prev-$(date +%Y%m%d-%H%M%S)"
            cp -p "$dst" "$kept" || { skip "model slot ${slot}" "the existing ${dst} could not be saved, so it was left as it is"; failed=1; continue; }
            warn "Replaced a different ${dst}; the previous one is ${kept}"
        fi
        if install -m 0755 "$src" "$dst"; then
            label_bin "$dst"
        else
            skip "model slot ${slot}" "${dst} could not be written"; failed=1
        fi
    done
    (( failed )) || ok "Installed: start-a1.sh to start-a4.sh (the model slots' launchers)"
}

# llama_register_builds <binary>=<backend>...: a model added while no build is registered cannot be loaded, so the servers found on this machine are
# recorded in the catalog (through the daemon, which checks them). One that is registered already stays as it is.
llama_register_builds() {
    local forge="${FORGE_BIN:-/opt/forge/forge}" pair bin backend found=0
    for pair in "$@"; do
        bin="${pair%=*}"; backend="${pair##*=}"
        [[ -x "$bin" ]] || continue
        found=1
        if [[ $DRY_RUN -eq 1 ]]; then
            info "[dry-run] Would register the llama.cpp build ${bin} (${backend})"
        elif ! run_as "${SERVICE_USER}" "$forge" catalog register-build -binary "$bin" -backend "$backend"; then
            skip "the llama.cpp build ${bin}" "it was not registered (see above); add it under Settings, Catalog, Builds & files"
        fi
    done
    (( found )) || warn "No llama.cpp server was found, so no language model can be loaded yet: build one, then run the installer again or add it under Settings, Catalog, Builds & files"
}

# wait_for <url> <seconds>
wait_for() {
    local i
    for (( i = 0; i < $2; i++ )); do
        curl -sf --max-time 2 "$1" &>/dev/null && return 0
        sleep 1
    done
    return 1
}
