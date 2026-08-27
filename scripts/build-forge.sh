#!/bin/bash
# build-forge.sh — cross-compiles forge for ForgeHost (linux/amd64) with a real
# v0.5.x version baked in via -ldflags, so the deploy tag stops being
# hand-typed each session (and drifting back to the old v5.x scheme, as it
# did on every deploy from the 2026-08-24 rebrand through v5.0.157).
#
# Usage:
#   ./scripts/build-forge.sh <slug>
#   e.g. ./scripts/build-forge.sh sprint0-versioning
#
# Produces ./dist/forge (ready for ./scripts/deploy-forge.sh dist/forge) and
# bumps .forge-build-number so the next build continues the sequence.
#
# NOTE: never output to a bare "forge" path at the repo root — a tracked
# source directory of that exact name already exists there (legacy
# static/templates), and `go build -o forge` silently drops the binary
# inside it as forge/forge instead of erroring.
set -euo pipefail

SLUG="${1:?usage: $0 <slug>}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
COUNTER_FILE="$ROOT/.forge-build-number"
OUT_DIR="$ROOT/dist"

if [[ ! -f "$COUNTER_FILE" ]]; then
  echo "158" > "$COUNTER_FILE"
fi

N=$(<"$COUNTER_FILE")
HASH=$(git -C "$ROOT" rev-parse --short HEAD)
DIRTY=""
git -C "$ROOT" diff --quiet || DIRTY="-dirty"
VERSION="v0.5.${N}-${SLUG}-${HASH}${DIRTY}"

mkdir -p "$OUT_DIR"
echo "[1/2] building ${VERSION}"
(cd "$ROOT/go" && GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=${VERSION}" -o "$OUT_DIR/forge" ./cmd/forge)

echo "$((N + 1))" > "$COUNTER_FILE"
echo "[2/2] built dist/forge as ${VERSION}"
echo "next build will be v0.5.$((N + 1))-..."
echo "deploy with: ./scripts/deploy-forge.sh dist/forge"
