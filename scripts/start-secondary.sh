#!/bin/bash
# /usr/local/lib/forge/start-secondary.sh
# Static launcher for the secondary inference slot (port 8081).
# All model-specific values injected by the engine — same pattern as start-primary.sh.
#   FORGE_LLAMA_BIN (optional) — override llama-server path
# SELinux: chcon -t bin_t /usr/local/lib/forge/start-secondary.sh
set -euo pipefail

ARGS_FILE=/etc/sysconfig/forge-secondary-args

mapfile -t _ALL_LINES < <(grep -v '^[[:space:]]*$' "$ARGS_FILE" 2>/dev/null || true)
EXTRA_ARGS=("${_ALL_LINES[@]+"${_ALL_LINES[@]}"}")

MMPROJ_ARGS=()
[[ -n "${FORGE_MMPROJ:-}" ]] && MMPROJ_ARGS=(--mmproj "${FORGE_MMPROJ}")

if [[ "${FORGE_BACKEND:-vulkan}" == "vllm" ]]; then
  export LD_LIBRARY_PATH="/opt/rocm-therock-7.13/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
  export ROCM_PATH=/opt/rocm-therock-7.13
  export HIP_PATH=/opt/rocm-therock-7.13
  exec /opt/forge/venv-vllm/bin/vllm serve "${FORGE_MODEL_PATH}" \
      --host              127.0.0.1 \
      --port              8081 \
      --served-model-name "${FORGE_MODEL_ALIAS}" \
      --max-model-len     "${FORGE_CONTEXT}" \
      "${EXTRA_ARGS[@]}"
elif [[ -n "${FORGE_LLAMA_BIN:-}" ]]; then
  LLAMA_BIN="${FORGE_LLAMA_BIN}"
elif [[ "${FORGE_BACKEND:-vulkan}" == "rocm" ]]; then
  LLAMA_BIN="/opt/forge/llama.cpp/build/bin/llama-server"
else
  LLAMA_BIN="/opt/forge/llama.cpp/build-vulkan/bin/llama-server"
fi

exec "$LLAMA_BIN" \
    --model    "${FORGE_MODEL_PATH}" \
    --alias    "${FORGE_MODEL_ALIAS}" \
    --host     127.0.0.1 \
    --port     8081 \
    --ctx-size "${FORGE_CONTEXT}" \
    -ngl       99 \
    --metrics \
    "${MMPROJ_ARGS[@]}" \
    "${EXTRA_ARGS[@]}"
