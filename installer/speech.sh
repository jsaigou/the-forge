#!/usr/bin/env bash
# installer/speech.sh — `install.sh --speech=...` (docs/speech-setup-plan.md section 5.3). Sourced by install.sh, after host.sh. Speech never fails
# the base install: whatever cannot be done is skipped by name, with the command that finishes it. The engines themselves are made by
# `forge speech install`, which runs as the service user; keys and tokens are written by the forge binary and never printed.

# Kept in step with the recipes by a test (go/internal/speechsetup).
SPEECH_KNOWN="kokoro zonos qwen-voicedesign qwen-base qwen-customvoice"
SPEECH_INSTALLABLE="kokoro" # the others can only be registered from files already on the server
SPEECH_DEFAULT="kokoro zonos qwen-voicedesign"
SPEECH_IDS=()

# speech_parse <none|default|id[,id...]> fills SPEECH_IDS; returns 1 for an unknown id.
speech_parse() {
    SPEECH_IDS=()
    local value="$1" id
    case "$value" in
        ""|none) return 0 ;;
        default) value="${SPEECH_DEFAULT// /,}" ;;
    esac
    for id in ${value//,/ }; do
        if [[ " ${SPEECH_KNOWN} " != *" ${id} "* ]]; then
            fail "Unknown speech engine '${id}' (known: ${SPEECH_KNOWN}; or: default, none)"
            return 1
        fi
        SPEECH_IDS+=("$id")
    done
}

# Without --speech an interactive public install asks; --yes never downloads anything it was not told to.
speech_ask() {
    [[ -z "${SPEECH_CHOICE}" && "$TARGET" == "public" && $YES -ne 1 && -t 0 && $DRY_RUN -eq 0 ]] || return 0
    echo -e "\n    ${BOLD}Speech${NC}"
    info "Kokoro gives preset voices: about 0.35 GB of weights and about 1 GB of packages, runs on the processor."
    echo -en "\n  Install it? [y/N]: "
    local answer; read -r answer || answer="n"
    [[ "${answer,,}" == "y" ]] && SPEECH_CHOICE="kokoro"
    return 0
}

speech_wanted() { (( ${#SPEECH_IDS[@]} > 0 )); }

# speech_setup runs once the daemon answers: the speech service's key, the service, then each engine.
speech_setup() {
    speech_wanted || return 0
    local forge="${FORGE_BIN:-/opt/forge/forge}" id
    if [[ $DRY_RUN -eq 1 ]]; then
        info "[dry-run] Would mint the speech service's key, start forge-tts and run: forge speech install ${SPEECH_IDS[*]}"
        return 0
    fi
    if [[ ! -x "${ENGINE_ROOT}/forge-tts" ]]; then
        skip "speech" "forge-tts was not built (${ENGINE_ROOT}/forge-tts is missing)"
        return 0
    fi
    local rotate=("$forge" speech key rotate -if-needed -env "${ENGINE_ROOT}/tts_voices/.internal_token_env")
    [[ ${VS_READY:-0} -eq 1 ]] && rotate+=(-voice-studio-env "${VS_ENV}")
    if ! run_as "${SERVICE_USER}" "${rotate[@]}"; then
        skip "speech" "the speech service's key could not be written; later: sudo -u ${SERVICE_USER} ${forge} speech key rotate"
        return 0
    fi
    if systemctl enable forge-tts.service &>/dev/null && systemctl restart forge-tts.service && wait_for http://127.0.0.1:8082/health 30; then
        ok "forge-tts is running"
        SPEECH_SERVICE_UP=1
    else
        skip "the speech service" "forge-tts did not answer; check: journalctl -u forge-tts -n 50"
    fi
    for id in "${SPEECH_IDS[@]}"; do
        info "Speech engine: ${id}"
        # `speech install` also adopts an engine whose files are already here, whether or not it can build one.
        if run_as "${SERVICE_USER}" "$forge" speech install "$id"; then
            ok "Speech engine ${id} is set up"
        elif [[ " ${SPEECH_INSTALLABLE} " == *" ${id} "* ]]; then
            skip "speech engine ${id}" "see the message above; later: sudo -u ${SERVICE_USER} ${forge} speech install ${id}"
        else
            skip "speech engine ${id}" "the Forge cannot install it yet"
        fi
    done
}

# Printed in the summary: what speech can do now.
speech_report() {
    speech_wanted || return 0
    [[ $DRY_RUN -eq 0 && ${SPEECH_SERVICE_UP:-0} -eq 1 ]] || return 0
    run_as "${SERVICE_USER}" "${FORGE_BIN:-/opt/forge/forge}" speech status || warn "Could not read the speech status"
}
