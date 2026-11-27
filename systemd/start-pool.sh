#!/bin/bash
# /usr/local/lib/forge/start-pool.sh <slot>
#
# Static launcher for the specialist slot pool (ADR-0022, docs/adr/0022-specialist-slot-pool.md). One copy serves every slot: forge-pool@.service
# runs it as `start-pool.sh %i`, and the daemon (internal/specialist) has already written /var/lib/forge/pool/<slot>.env, which systemd loads.
#
# THE SECURITY RULE: this script never runs a command line taken from the environment. It takes a launch KIND, maps it to ONE fixed binary under ONE
# allow-listed root, and builds the arguments itself from validated KEY=VALUE parameters. An unknown kind, a binary outside its root, or a value with a
# surprise in it is exit status 2 and nothing is started. internal/specialist/kinds.go validates the same things before a row is stored; this is the
# second wall, so a bad database row cannot become arbitrary execution. internal/specialist/launcher_test.go runs this script's dry run for every
# kind the Go registry can render, so the two tables cannot drift apart.
#
# Environment (from the env file):
#   FORGE_POOL_KIND       llama-server | catalog-llama | catalog-launch | qwen-tts-server | zonos3-server | qwen3-asr-server | aligner | kokoro
#   FORGE_POOL_PORT       listen port on 127.0.0.1 (kokoro's server.py hard-codes 8880 and the daemon passes that value)
#   FORGE_POOL_P_<NAME>   the kind's parameters (see each branch)
# Test hooks: FORGE_POOL_DRYRUN=1 prints the command instead of running it (and skips the on-disk checks); FORGE_POOL_SOURCE_ONLY=1 makes the script
# sourceable so the on-disk checks themselves can be tested against fixtures (launcher_test.go).
#
# SELinux: chcon -t bin_t /usr/local/lib/forge/start-pool.sh
set -euo pipefail

die() { echo "start-pool: $*" >&2; exit 2; }

slot="${1:-}"
[[ "$slot" =~ ^s[0-9]{1,2}$ ]] || die "usage: start-pool.sh <slot> (s1..s99)"
kind="${FORGE_POOL_KIND:-}"
port="${FORGE_POOL_PORT:-}"
[[ "$port" =~ ^[0-9]{4,5}$ ]] || die "FORGE_POOL_PORT must be 4-5 digits"
dry="${FORGE_POOL_DRYRUN:-}"

# --- value checks -------------------------------------------------------------------------------------------------------------------
need() { local v="${!1:-}"; [[ -n "$v" ]] || die "$1 is required for kind $kind"; }
is_int()  { [[ "${1}" =~ ^[0-9]{1,10}$ ]]; }
is_word() { [[ "${1}" =~ ^[A-Za-z0-9._:-]{1,64}$ ]]; }
is_bool() { [[ "${1}" == true || "${1}" == false ]]; }
# A path parameter: absolute, no "..", no control characters, under the install tree. (The daemon validates the exact model roots from its own config.)
is_path() { [[ "${1}" == /opt/forge/* && "${1}" != *..* && "${1}" != *[[:cntrl:]]* ]]; }
int_opt()  { local n="FORGE_POOL_P_$1" f="$2"; local v="${!n:-}"; [[ -z "$v" ]] && return 0; is_int "$v" || die "$n must be a whole number"; ARGS+=("$f" "$v"); }
word_opt() { local n="FORGE_POOL_P_$1" f="$2"; local v="${!n:-}"; [[ -z "$v" ]] && return 0; is_word "$v" || die "$n must be a plain word"; ARGS+=("$f" "$v"); }
path_opt() { local n="FORGE_POOL_P_$1" f="$2"; local v="${!n:-}"; [[ -z "$v" ]] && return 0; is_path "$v" || die "$n must be a path under /opt/forge"; ARGS+=("$f" "$v"); }
path_req() { need "FORGE_POOL_P_$1"; path_opt "$1" "$2"; }
real() { readlink -f "$1" 2>/dev/null || realpath "$1"; }
# The binary must exist, be executable, and (after symlinks) still lie under its root.
check_bin() {
  local bin="$1" root="$2"
  [[ "$bin" == "$root"/* && "$bin" != *..* ]] || die "$bin is outside $root"
  [[ -n "$dry" ]] && return 0
  [[ -x "$bin" && -f "$bin" ]] || die "$bin is not an executable file"
  local r; r="$(real "$bin")"
  [[ "$r" == "$root"/* ]] || die "$bin resolves outside $root"
}
# A Python virtualenv's interpreter is a symlink to the base interpreter BY DESIGN, so check_bin would refuse it. For a venv interpreter the
# symlink may resolve outside the root only into the directory the venv's OWN pyvenv.cfg declares as its `home`: <venv>/bin/python must resolve under
# the real path of that home. This survives an interpreter patch upgrade (the home is usually a version alias) and accepts nothing else: the path as
# written must still lie under the root, and a pyvenv.cfg that is missing, relative or climbing (..) is a refusal.
check_venv_bin() {
  local bin="$1" root="$2"
  [[ "$bin" == "$root"/* && "$bin" != *..* ]] || die "$bin is outside $root"
  [[ -n "$dry" ]] && return 0
  [[ -x "$bin" && -f "$bin" ]] || die "$bin is not an executable file"
  local venv cfg home rb rh
  venv="$(dirname "$(dirname "$bin")")"; cfg="$venv/pyvenv.cfg"
  [[ -f "$cfg" ]] || die "$bin: no pyvenv.cfg in $venv"
  home="$(sed -n 's/^home[[:space:]]*=[[:space:]]*//p' "$cfg" | head -1)"
  [[ "$home" == /* && "$home" != *..* && "$home" != *[[:cntrl:]]* ]] || die "$cfg: home must be an absolute path without .."
  rb="$(real "$bin")"; rh="$(real "$home")"
  [[ -n "$rb" && -n "$rh" && "$rb" == "$rh"/* ]] || die "$bin resolves outside its venv's declared interpreter home ($home)"
}

# Test hook: `FORGE_POOL_SOURCE_ONLY=1 source start-pool.sh <slot>` stops here, leaving the checks above callable.
[[ -n "${FORGE_POOL_SOURCE_ONLY:-}" ]] && return 0

ARGS=()
CWD=/opt/forge
case "$kind" in
  llama-server)
    root=/opt/forge/llama.cpp-upstream-plain
    need FORGE_POOL_P_BUILD
    [[ "$FORGE_POOL_P_BUILD" =~ ^build-[a-z0-9-]{1,48}$ ]] || die "FORGE_POOL_P_BUILD must be a build directory name"
    BIN="$root/$FORGE_POOL_P_BUILD/bin/llama-server"; check_bin "$BIN" "$root"
    path_req MODEL -m
    path_opt MMPROJ --mmproj
    word_opt ALIAS --alias
    int_opt CTX -c; int_opt BATCH -b; int_opt UBATCH -ub; int_opt THREADS -t; int_opt NGL -ngl; int_opt PARALLEL --parallel
    if [[ -n "${FORGE_POOL_P_EMBEDDING:-}" ]]; then
      is_bool "$FORGE_POOL_P_EMBEDDING" || die "FORGE_POOL_P_EMBEDDING must be true or false"
      [[ "$FORGE_POOL_P_EMBEDDING" == true ]] && ARGS+=(--embedding)
    fi
    ARGS+=(--host 127.0.0.1 --port "$port")
    ;;
  catalog-llama)
    # A catalog config hosted in the pool (ADR-0022): the daemon resolves the config exactly as it does for an LLM slot and writes its env file
    # plus <slot>.args (one argument per line, as start-aN.sh reads them). The args are the catalog's own, admin-written and step-up gated, and run
    # on the same trust model as the LLM slots. This launcher only runs a llama-server under the install root and refuses the flags the pool owns.
    root=/opt/forge/llama.cpp-upstream-plain
    need FORGE_POOL_P_BIN
    BIN="$FORGE_POOL_P_BIN"
    [[ "${BIN##*/}" == llama-server ]] || die "FORGE_POOL_P_BIN must be a llama-server"
    check_bin "$BIN" "$root"
    need FORGE_POOL_P_MODEL; is_path "$FORGE_POOL_P_MODEL" || die "FORGE_POOL_P_MODEL must be a path under /opt/forge"
    need FORGE_POOL_P_ALIAS; is_word "$FORGE_POOL_P_ALIAS" || die "FORGE_POOL_P_ALIAS must be a plain word"
    need FORGE_POOL_P_CTX;   is_int  "$FORGE_POOL_P_CTX"   || die "FORGE_POOL_P_CTX must be a whole number"
    ARGS+=(--model "$FORGE_POOL_P_MODEL" --alias "$FORGE_POOL_P_ALIAS" --host 127.0.0.1 --port "$port" --ctx-size "$FORGE_POOL_P_CTX" -ngl 99 --metrics)
    path_opt MMPROJ --mmproj
    ARGS_FILE="${FORGE_POOL_ARGS_FILE:-/var/lib/forge/pool/$slot.args}"
    if [[ -f "$ARGS_FILE" ]]; then
      while IFS= read -r line || [[ -n "$line" ]]; do
        [[ -z "${line//[[:space:]]/}" ]] && continue
        case "$line" in
          # Flags the pool sets itself, and the ones that would expose the process beyond loopback or move it off its slot.
          --model|-m|--alias|--host|--port|--ctx-size|-c|--n-gpu-layers|-ngl|--mmproj|--metrics|--api-key|--api-key-file|--ssl-key-file|--ssl-cert-file|--path|--model=*|-m=*|--alias=*|--host=*|--port=*|--ctx-size=*|-c=*|--n-gpu-layers=*|-ngl=*|--mmproj=*|--metrics=*|--api-key=*|--api-key-file=*|--ssl-key-file=*|--ssl-cert-file=*|--path=*)
            die "the catalog args contain a flag the specialist pool owns: $line" ;;
        esac
        ARGS+=("$line")
      done < "$ARGS_FILE"
    fi
    ;;
  catalog-launch)
    # A catalog config whose launch is a slot_launches row (ADR-0026, ADR-0029): the engine kinds that are not llama.cpp, today Radiance. Nothing about the
    # engine is known here: the daemon wrote the row (executable, arguments, environment) before starting this unit, and `forge slot-exec` reads it through
    # a read-only connection, re-checks the executable against catalog.build_roots and execs it. This branch runs exactly one fixed command.
    BIN=/opt/forge/forge; check_bin "$BIN" /opt/forge
    ARGS=(slot-exec "$slot")
    ;;
  qwen-tts-server)
    root=/opt/forge/qwentts.cpp/build
    BIN="$root/tts-server"; check_bin "$BIN" "$root"
    path_req MODEL --model; path_req CODEC --codec; word_opt ALIAS --alias
    ARGS+=(--host 127.0.0.1 --port "$port")
    ;;
  zonos3-server)
    # ZONOS3-Mini. The backbone GGUF is the binary's first positional argument; the decoder and encoders are flags.
    # Every value is a validated path under /opt/forge, so nothing here is a command line.
    root=/opt/forge/zonos3.cpp/build
    BIN="$root/zonos3-server"; check_bin "$BIN" "$root"
    need FORGE_POOL_P_MODEL; is_path "$FORGE_POOL_P_MODEL" || die "FORGE_POOL_P_MODEL must be a path under /opt/forge"
    ARGS+=("$FORGE_POOL_P_MODEL")
    path_req ZAC --zac; path_req SPK_ENCODER --spk-encoder; path_opt ZAC_ENCODER --zac-encoder; path_opt SSL_TEACHER --ssl-teacher
    # Alignment heads, one file per language (the steering spans follow the model's reading position through them). English serves en_us and en_gb.
    if [[ -n "${FORGE_POOL_P_LACI_EN:-}" ]]; then
      is_path "$FORGE_POOL_P_LACI_EN" || die "FORGE_POOL_P_LACI_EN must be a path under /opt/forge"
      ARGS+=(--laci-heads "en_us=$FORGE_POOL_P_LACI_EN" --laci-heads "en_gb=$FORGE_POOL_P_LACI_EN")
    fi
    if [[ -n "${FORGE_POOL_P_LACI_JA:-}" ]]; then
      is_path "$FORGE_POOL_P_LACI_JA" || die "FORGE_POOL_P_LACI_JA must be a path under /opt/forge"
      ARGS+=(--laci-heads "ja=$FORGE_POOL_P_LACI_JA")
    fi
    ARGS+=(--host 127.0.0.1 --port "$port" --gpu)
    ;;
  qwen3-asr-server)
    root=/opt/forge/qwen3-asr-src/build
    BIN="$root/qwen3-asr-server"; check_bin "$BIN" "$root"
    path_req MODEL -m
    int_opt MAX_UPLOAD_MB --max-upload-mb; int_opt THREADS -t
    ARGS+=(--host 127.0.0.1 --port "$port" --convert)
    ;;
  aligner)
    root=/opt/forge/venv-qwen3-aligner/bin
    BIN="$root/gunicorn"; check_bin "$BIN" "$root"
    int_opt WORKERS -w; int_opt TIMEOUT --timeout
    ARGS+=(-k sync -b "127.0.0.1:$port" aligner_server:app)
    ;;
  kokoro)
    root=/opt/forge/kokoro
    BIN="$root/venv312/bin/python"; check_venv_bin "$BIN" "$root"
    # Kokoro's server.py hard-codes 8880; refuse a different port rather than start something the daemon cannot reach.
    [[ "$port" == 8880 ]] || die "kokoro listens on 8880 only (FORGE_POOL_PORT=$port)"
    ARGS=("$root/server.py")
    CWD="$root"
    ;;
  *) die "unknown kind '$kind' (llama-server, catalog-llama, catalog-launch, qwen-tts-server, zonos3-server, qwen3-asr-server, aligner, kokoro)" ;;
esac

if [[ -n "$dry" ]]; then
  echo "cwd=$CWD"
  printf '%s' "$BIN"; printf ' %s' "${ARGS[@]}"; echo
  exit 0
fi
cd "$CWD"
exec "$BIN" "${ARGS[@]}"
