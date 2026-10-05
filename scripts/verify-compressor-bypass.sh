#!/usr/bin/env bash
# verify-compressor-bypass.sh — read-only proof that no traffic flows through the
# forge-compress proxies (WS-A, C7). Run from a machine on the tailnet.
#
# It samples each compressor's own /metrics `compress_requests_total` twice,
# INTERVAL seconds apart, and fails if any counter moved. It also prints the
# value of the `compressor.passthrough_all` setting.
#
# Read-only: it only runs curl against loopback /metrics, a grep for the
# COMPRESS_PORT key (never other env keys — the env file holds a token), and
# `forge config get`. It never toggles anything.
#
# Usage:  scripts/verify-compressor-bypass.sh [-i SECONDS] [-n SAMPLES]
#   -i  seconds between samples (default 60)
#   -n  number of intervals to check (default 1; n+1 snapshots)
# Env:    FORGE_HOST (REQUIRED: the tailnet name of the machine running forge), COMPRESSORS (default "local deepseek external"),
#         FORGE_DB (default /var/lib/forge/forge.db)
#
# Exit codes: 0 = no movement (PASS), 1 = counter moved or reset (FAIL),
#             2 = could not measure (ssh/curl failure, nothing sampled).
#
# NOTE: PASS is only meaningful while a0 traffic is actually flowing. During
# idle time "unchanged" is trivially true; look at the passthrough_all line and
# re-run under load (e.g. an OpenCode session) for a real proof.
set -u
INTERVAL=60
SAMPLES=1
while getopts "i:n:h" o; do
  case $o in
    i) INTERVAL=$OPTARG ;;
    n) SAMPLES=$OPTARG ;;
    *) sed -n '2,24p' "$0"; exit 2 ;;
  esac
done
TS=${TAILSCALE_BIN:-tailscale}
HOST=${FORGE_HOST:?set FORGE_HOST to the tailnet name of the host running forge (no default is baked in)}
NAMES=${COMPRESSORS:-"local deepseek external"}
DB=${FORGE_DB:-/var/lib/forge/forge.db}

# Remote snippet: emits one line per compressor: "<name> <state> <requests_total|->".
# Reads only the COMPRESS_PORT line from each env file.
snapshot() {
  "$TS" ssh "$HOST" "bash -s" -- $NAMES <<'REMOTE'
for n in "$@"; do
  st=$(systemctl is-active "forge-compress@$n" 2>/dev/null || true)
  if [ "$st" != active ]; then echo "$n ${st:-unknown} -"; continue; fi
  port=$(grep -m1 '^COMPRESS_PORT=' "/var/lib/forge/compress/$n.env" 2>/dev/null | cut -d= -f2)
  v=$(curl -sf --max-time 5 "http://127.0.0.1:${port}/metrics" 2>/dev/null | awk '$1=="compress_requests_total"{print $2}')
  echo "$n active ${v:--}"
done
REMOTE
}

flag=$("$TS" ssh "$HOST" "/opt/forge/forge config get -db $DB compressor.passthrough_all" 2>/dev/null | tr -d '[:space:]')
echo "compressor.passthrough_all = ${flag:-<unreadable>}"
[ "$flag" = true ] || echo "WARN: passthrough_all is not true; per-proxy bypass flags are not checked here."

prev=$(snapshot) || { echo "ERROR: ssh/snapshot failed" >&2; exit 2; }
[ -n "$prev" ] || { echo "ERROR: empty snapshot" >&2; exit 2; }
echo "snapshot 0 ($(date +%T)):"; echo "$prev" | sed 's/^/  /'

rc=0; measured=0
for ((k=1; k<=SAMPLES; k++)); do
  sleep "$INTERVAL"
  cur=$(snapshot) || { echo "ERROR: ssh/snapshot failed" >&2; exit 2; }
  echo "snapshot $k ($(date +%T)):"; echo "$cur" | sed 's/^/  /'
  while read -r name _ a; do
    b=$(echo "$cur" | awk -v n="$name" '$1==n{print $3}')
    if [ "$a" = "-" ] || [ "${b:--}" = "-" ]; then
      echo "  $name: not sampled (inactive or no metrics)"; continue
    fi
    measured=$((measured+1))
    if [ "$b" -gt "$a" ]; then
      echo "  FAIL $name: compress_requests_total $a -> $b (+$((b-a))) — traffic is still going through the compressor"; rc=1
    elif [ "$b" -lt "$a" ]; then
      echo "  FAIL $name: counter went DOWN $a -> $b (unit restarted; inconclusive)"; rc=1
    else
      echo "  ok   $name: unchanged at $b"
    fi
  done <<< "$prev"
  prev=$cur
done

if [ "$measured" -eq 0 ]; then echo "ERROR: no compressor could be measured" >&2; exit 2; fi
if [ $rc -eq 0 ]; then echo "PASS: no compressor traffic over $((SAMPLES*INTERVAL))s"; else echo "FAIL: see above"; fi
exit $rc
