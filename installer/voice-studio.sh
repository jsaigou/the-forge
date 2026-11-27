#!/usr/bin/env bash
# installer/voice-studio.sh — installs Voice Studio, the web app for making and using voices (docs/speech-setup-plan.md section 5.4). Sourced by
# install.sh, after host.sh. It builds a pinned release, runs it as the service user on 127.0.0.1:8090 and connects it to the speech service. It has
# no sign-in of its own: reaching it from other machines is opt-in and said so. A Voice Studio already linked in the Console is left alone.

# The pin. A release moves these together; while they are empty there is nothing to install.
FORGE_VOICE_STUDIO_REPO="${FORGE_VOICE_STUDIO_REPO:-https://github.com/jsaigou/forge-voice-studio}"
FORGE_VOICE_STUDIO_REF="${FORGE_VOICE_STUDIO_REF-v0.8.0}"
FORGE_VOICE_STUDIO_COMMIT="${FORGE_VOICE_STUDIO_COMMIT-3372bf7761c809f9a074c63964662474c15b2e8d}"

VS_READY=0

vs_wanted() {
    case "${VOICE_STUDIO_MODE}" in
        on) return 0 ;;
        off) return 1 ;;
        *) speech_wanted ;;
    esac
}

# add_setting <file> <KEY> <value>: only when the file does not set the key, so an operator's choice stays.
add_setting() { grep -q "^$2=" "$1" 2>/dev/null || printf '%s=%s\n' "$2" "$3" >>"$1"; }

# vs_prepare builds and installs the app and writes its settings; the speech service's token is added later, with the key.
vs_prepare() {
    vs_wanted || return 0
    VS_DIR="${ENGINE_ROOT}/voice-studio"
    VS_ENV="${VS_DIR}/voice-studio.env"
    VS_DATA="${VS_DATA_DIR:-/var/lib/forge/voice-studio}"
    if [[ -z "${FORGE_VOICE_STUDIO_REF}" || -z "${FORGE_VOICE_STUDIO_COMMIT}" ]]; then
        skip "Voice Studio" "no release is pinned yet (set FORGE_VOICE_STUDIO_REPO, FORGE_VOICE_STUDIO_REF and FORGE_VOICE_STUDIO_COMMIT)"
        return 0
    fi
    local tool missing=()
    for tool in git go ffmpeg; do command -v "$tool" &>/dev/null || missing+=("$tool"); done
    if (( ${#missing[@]} > 0 )); then
        skip "Voice Studio" "missing ${missing[*]}; $(pkg_hint 'git golang ffmpeg-free' 'git golang-go ffmpeg') and run the installer again"
        return 0
    fi
    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would build Voice Studio ${FORGE_VOICE_STUDIO_REF} from ${FORGE_VOICE_STUDIO_REPO} into ${VS_DIR}"
        return 0
    fi
    local builder work got
    builder="$(builder_user)"
    work="$(mktemp -d)" && chown "$builder" "$work" || { skip "Voice Studio" "no temporary directory"; return 0; }
    if ! run_as "$builder" git -c advice.detachedHead=false clone -q --depth 1 --branch "${FORGE_VOICE_STUDIO_REF}" "${FORGE_VOICE_STUDIO_REPO}" "$work/src"; then
        skip "Voice Studio" "could not fetch ${FORGE_VOICE_STUDIO_REF} from ${FORGE_VOICE_STUDIO_REPO}"; rm -rf "$work"; return 0
    fi
    got="$(run_as "$builder" git -C "$work/src" rev-parse HEAD)"
    if [[ "$got" != "${FORGE_VOICE_STUDIO_COMMIT}" ]]; then
        skip "Voice Studio" "${FORGE_VOICE_STUDIO_REF} is commit ${got:0:12}, not the pinned ${FORGE_VOICE_STUDIO_COMMIT:0:12}; nothing was built"
        rm -rf "$work"; return 0
    fi
    info "Building Voice Studio ${FORGE_VOICE_STUDIO_REF} ..."
    if ! ( cd "$work/src" && run_as "$builder" env CGO_ENABLED=0 go build -trimpath -o "$work/voice-studio" ./cmd/voice-studio ); then
        skip "Voice Studio" "its build failed (see above)"; rm -rf "$work"; return 0
    fi
    mkdir -p "${VS_DIR}" "${VS_DATA}"
    if ! install_program "$work/voice-studio" "${VS_DIR}/voice-studio" "${SERVICE_USER}"; then
        skip "Voice Studio" "could not install the program into ${VS_DIR}"; rm -rf "$work"; return 0
    fi
    rm -rf "$work"
    ( umask 077; touch "${VS_ENV}" )
    chmod 600 "${VS_ENV}"
    add_setting "${VS_ENV}" VOICE_STUDIO_NODE_URL http://127.0.0.1:8082
    add_setting "${VS_ENV}" VOICE_STUDIO_FORGE_URL http://127.0.0.1:5000
    chown -R "${SERVICE_USER}:" "${VS_DIR}" "${VS_DATA}" || warn "Could not give ${VS_DIR} to ${SERVICE_USER}"
    VS_READY=1
    ok "Voice Studio ${FORGE_VOICE_STUDIO_REF} installed in ${VS_DIR}"
}

# vs_start installs the unit, starts the app, links it from the Console and offers remote access.
vs_start() {
    [[ ${VS_READY:-0} -eq 1 ]] || return 0
    local src="${SCRIPT_DIR}/systemd/forge-voice-studio.service" unit="${FORGE_UNIT_DIR:-/etc/systemd/system}/forge-voice-studio.service"
    render_unit "$src" "$unit" || { skip "Voice Studio" "its unit could not be installed"; return 0; }
    if ! host_has_systemd; then
        skip "starting Voice Studio" "systemd is not running"
        return 0
    fi
    if ! { systemctl daemon-reload && systemctl enable forge-voice-studio.service &>/dev/null && systemctl restart forge-voice-studio.service \
            && wait_for http://127.0.0.1:8090/api/config 30; }; then
        skip "Voice Studio" "it did not answer; check: journalctl -u forge-voice-studio -n 50"
        return 0
    fi
    ok "Voice Studio is running on http://127.0.0.1:8090"
    run_as "${SERVICE_USER}" "${FORGE_BIN:-/opt/forge/forge}" speech studio-link http://127.0.0.1:8090 || warn "Could not add the Voice Studio link to the Console"
    VS_UP=1
    vs_offer_remote
}

vs_offer_remote() {
    [[ ${TS_OK:-0} -eq 1 && -n "${TAILNET:-}" && $YES -ne 1 && -t 0 ]] || return 0
    echo -e "\n    ${BOLD}Reach Voice Studio from your other machines?${NC}"
    warn "Voice Studio has no sign-in: everyone on your tailnet who can open it can use and delete your voices."
    echo -en "\n  Publish it as voice-studio.${TAILNET} with Tailscale? [y/N]: "
    local answer; read -r answer || answer="n"
    [[ "${answer,,}" == "y" ]] || return 0
    if tailscale serve --bg --service=svc:voice-studio --https=443 localhost:8090; then
        add_setting "${VS_ENV}" VOICE_STUDIO_ALLOWED_HOSTS "voice-studio.${TAILNET}"
        systemctl restart forge-voice-studio.service || warn "Restart Voice Studio: systemctl restart forge-voice-studio"
        ok "Voice Studio: https://voice-studio.${TAILNET}"
    else
        warn "tailscale serve failed; some tailnets require declaring the service in the admin console first"
    fi
}
