#!/bin/bash
# /usr/local/lib/forge/start-comfyui.sh
# Static launcher for ComfyUI (image/video generation, port 3001).
# SELinux: chcon -t bin_t /usr/local/lib/forge/start-comfyui.sh
COMFYUI_DIR="/opt/comfyui/app"
[[ ! -d "$COMFYUI_DIR" ]] && { echo "ERROR: ComfyUI not found: $COMFYUI_DIR"; exit 1; }
cd "$COMFYUI_DIR"
exec /opt/comfyui/venv/bin/python main.py \
    --listen 127.0.0.1 \
    --port 3001 \
    --output-directory /opt/comfyui/output
