#!/usr/bin/env bash
# installer/test_radiance.sh — tests for installer/radiance.sh with stub tools (no root, no ROCm, no network).
#   bash installer/test_radiance.sh
# A fake git remote, and stub cmake/ninja/ctest/c++/nproc/forge on PATH, stand in for the real toolchain;
# the real git does the clone and the pin check.

set -u
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PASS=0; FAILN=0
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT

# install.sh's helpers, recording instead of printing
OUT=""
ok()   { OUT+="ok: $*"$'\n'; }
warn() { OUT+="warn: $*"$'\n'; }
info() { OUT+="info: $*"$'\n'; }
step() { OUT+="step: $*"$'\n'; }
BOLD=""; NC=""

check() { # name, condition-exit-status
    if [[ "$2" -eq 0 ]]; then PASS=$((PASS+1)); else FAILN=$((FAILN+1)); echo "FAIL: $1"; echo "--- output ---"; echo "$OUT"; echo "--------------"; fi
}
has()  { [[ "$OUT" == *"$1"* ]]; }

# ── a fake remote with the pinned tag, and stub tools ─────────────────────────────────────────────
REMOTE="$T/remote"; mkdir -p "$REMOTE"
( cd "$REMOTE" && git init -q && git config user.email fixture && git config user.name t \
  && echo hello > README && git add README && git commit -q -m init && git tag -a strix-halo-r2 -m r2 )
GOOD_COMMIT="$(git -C "$REMOTE" rev-parse HEAD)"

BIN="$T/bin"; mkdir -p "$BIN"
for tool in c++ ninja; do printf '#!/bin/sh\nexit 0\n' >"$BIN/$tool"; done
printf '#!/bin/sh\necho 4\n' >"$BIN/nproc"
cat >"$BIN/cmake" <<'EOF'
#!/bin/sh
# cmake -S DIR -B BUILD ...   |   cmake --build BUILD ...
if [ "$1" = "--build" ]; then
  mkdir -p "$2/bin" "$2/radiance_home/kernels"; printf '#!/bin/sh\n' >"$2/bin/radiance"; chmod 755 "$2/bin/radiance"
fi
exit "${STUB_CMAKE_RC:-0}"
EOF
cat >"$BIN/ctest" <<'EOF'
#!/bin/sh
echo "The following tests FAILED:"
n=1; for t in $STUB_CTEST_FAILS; do echo "	 $n - $t (Failed)"; n=$((n+1)); done
exit 1
EOF
cat >"$BIN/forge" <<'EOF'
#!/bin/sh
echo "forge $*" >>"$STUB_FORGE_LOG"; exit "${STUB_FORGE_RC:-0}"
EOF
chmod 755 "$BIN"/*
ROCM="$T/rocm"; mkdir -p "$ROCM/lib/llvm/bin"; printf '#!/bin/sh\n' >"$ROCM/lib/llvm/bin/clang++"; chmod 755 "$ROCM/lib/llvm/bin/clang++"

export PATH="$BIN:$PATH"
export FORGE_RADIANCE_REPO="$REMOTE" FORGE_RADIANCE_COMMIT="$GOOD_COMMIT" ROCM_ROOT="$ROCM"
export STUB_FORGE_LOG="$T/forge.log" RADIANCE_OWNER="$(id -un):$(id -gn)"
TARGET=public; DRY_RUN=0; SCRIPT_DIR="$HERE/.."; MODELS_DIR="$T/data/models"
export FORGE_BIN="$BIN/forge"
# shellcheck source=radiance.sh
source "$HERE/radiance.sh"
reset() { OUT=""; RADIANCE_BUILT=0; RADIANCE_BUILD_DIR=""; DRY_RUN=0; RADIANCE_MODE=auto; RAD_KGC_MATCH_CMD="echo strix-halo"; rm -rf "$T/dest"; export FORGE_RADIANCE_DIR="$T/dest"; : >"$STUB_FORGE_LOG"; STUB_CTEST_FAILS="mm_test avx_check"; export STUB_CTEST_FAILS; }

# ── pure helpers ──────────────────────────────────────────────────────────────────────────────────
reset
got="$(printf '  51 - mm_test (Failed)\n 111 - avx_check (Failed)\n  9 - foo_test (Timeout)\n 10 - ok_test (Passed)\n' | rad_failed_tests | tr '\n' ' ')"
[[ "$got" == "avx_check foo_test mm_test " ]]; check "failed tests are parsed from ctest output" $?
got="$(printf 'mm_test\nnew_test\nconvert_test\n' | rad_new_failures | tr '\n' ' ')"
[[ "$got" == "new_test " ]]; check "only failures outside the recorded baseline are new" $?

# ── the decision ──────────────────────────────────────────────────────────────────────────────────
reset; RADIANCE_MODE=off; rad_wanted; r=$?
[[ $r -ne 0 ]] && has "--no-radiance"; check "--no-radiance skips" $?
reset; RADIANCE_MODE=on; RAD_KGC_MATCH_CMD="false"; rad_wanted; check "--radiance builds on any hardware" $?
reset; RAD_KGC_MATCH_CMD="echo strix-halo"; rad_wanted; check "auto builds where the matcher recognises the hardware" $?
reset; RAD_KGC_MATCH_CMD="false"; rad_wanted; r=$?
[[ $r -ne 0 ]] && has "not one the fork supports"; check "auto skips unrecognised hardware, saying why" $?

# ── missing prerequisites never fail the install ──────────────────────────────────────────────────
reset; ( PATH="/usr/bin:/bin"; ROCM_ROOT="$T/nowhere"; rad_step ) >/dev/null 2>&1
reset; ( PATH="/usr/bin:/bin:$T/emptybin"; ROCM_ROOT="$T/nowhere"; OUT=""; rad_step; echo "$OUT" >"$T/out.missing" ) >/dev/null 2>&1
grep -q "not built: missing" "$T/out.missing"; check "missing tools or ROCm is a warning, not a failure" $?

# ── dry run changes nothing ───────────────────────────────────────────────────────────────────────
reset; DRY_RUN=1; rad_step
[[ ! -e "$T/dest" ]] && has "[dry-run] would run: git clone" && has "would verify HEAD is ${GOOD_COMMIT}" && has "ctest -L fast"; check "dry run prints the plan and creates nothing" $?
OUT=""; rad_register_build
has "register-build -path ${T}/dest/build"; check "dry run prints the registration command" $?
[[ ! -s "$STUB_FORGE_LOG" ]]; check "dry run does not call forge" $?

# ── the real run, against stubs ───────────────────────────────────────────────────────────────────
reset; rad_step
[[ -x "$T/dest/build/bin/radiance" && $RADIANCE_BUILT -eq 1 && "$RADIANCE_BUILD_DIR" == "$T/dest/build" ]]; check "a pinned fetch and build leaves a registered-able build" $?
has "matches the pin" && has "no failures beyond the recorded stock-upstream baseline"; check "baseline-only test failures are not a warning" $?
[[ "$(stat -c %a "$T/dest/build/radiance_home" 2>/dev/null || stat -f %Lp "$T/dest/build/radiance_home")" =~ ^7[0-5][0-5]$ ]]; check "the plugin directory is not group/other writable" $?

reset; STUB_CTEST_FAILS="mm_test brand_new_test"; export STUB_CTEST_FAILS; rad_step
has "brand_new_test" && [[ $RADIANCE_BUILT -eq 1 ]]; check "a new failing test warns but keeps the build" $?

reset; export FORGE_RADIANCE_COMMIT="0000000000000000000000000000000000000000"; RADIANCE_COMMIT="$FORGE_RADIANCE_COMMIT"; rad_step
has "not the pinned" && [[ $RADIANCE_BUILT -eq 0 && ! -x "$T/dest/build/bin/radiance" ]]; check "a ref that resolves to another commit is refused" $?
RADIANCE_COMMIT="$GOOD_COMMIT"; export FORGE_RADIANCE_COMMIT="$GOOD_COMMIT"

reset; export STUB_CMAKE_RC=1; rad_step; unset STUB_CMAKE_RC
has "build failed" && [[ $RADIANCE_BUILT -eq 0 ]]; check "a failed build is a warning with the log named" $?

reset; rad_step; OUT=""; RADIANCE_BUILT=0; rad_step
has "already built" && [[ $RADIANCE_BUILT -eq 1 ]]; check "a second run finds the existing pinned build and does not rebuild" $?

reset; mkdir -p "$T/dest"; rad_step
has "exists but is not a build" && [[ $RADIANCE_BUILT -eq 0 ]]; check "an unrelated existing directory is never overwritten" $?

# ── registration and the model offer ──────────────────────────────────────────────────────────────
reset; rad_step; OUT=""; rad_register_build
grep -q "forge radiance register-build -path $T/dest/build -name radiance-unofficial/strix-halo-r2-rocm" "$STUB_FORGE_LOG"; check "registration calls forge radiance register-build with the pinned name" $?
reset; rad_step; OUT=""; STUB_FORGE_RC=1 rad_register_build
has "Could not register the Radiance build" && has "register-build -path"; check "a failed registration warns and prints the command to retry" $?
reset; rad_step; OUT=""; rad_offer_models
has "--only qwen38-flash-next-ud-size-rad" && has "qwen36-35b-a3b-w4w5-rtn-rad" && [[ ! -e "$T/data" ]]; check "the offer lists the containers and downloads nothing" $?

# ── install.sh runs with set -euo pipefail: no path of the step may abort it ──────────────────────
strict() { # scenario setup commands (run after reset), under the installer's own shell options
    ( set -euo pipefail; reset; eval "$1"; rad_step >/dev/null 2>&1; rad_register_build >/dev/null 2>&1; rad_offer_models >/dev/null 2>&1 ) ; }
strict 'RADIANCE_MODE=off';                       check "strict mode: --no-radiance" $?
strict 'DRY_RUN=1';                               check "strict mode: dry run" $?
strict ':';                                       check "strict mode: a full run" $?
strict 'RAD_KGC_MATCH_CMD=false';                 check "strict mode: unsupported hardware" $?
strict 'export STUB_CMAKE_RC=1';                  check "strict mode: a failed build" $?
unset STUB_CMAKE_RC
strict 'STUB_FORGE_RC=1';                         check "strict mode: a failed registration" $?

echo "radiance installer tests: ${PASS} passed, ${FAILN} failed"
[[ $FAILN -eq 0 ]]
