#!/usr/bin/env bash
# Measure the 8c74086 bundle and the redesigned bundle with the same harness.
#   BEFORE_SITE=<dir> AFTER_SITE=<dir> OUTDIR=<dir> CHROME=<headless shell> ./run-compare.sh [mode]
# Runs three configurations, one at a time on port 9388:
#   before        old bundle + old server (heartbeats are durable events with ids)
#   after         new bundle + new server (live-only heartbeats, ?status=pending, observed_ip)
#   after-oldsrv  new bundle + old server (front end deployed before the backend)
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
MODE="${1:-all}"
: "${BEFORE_SITE:?}" "${AFTER_SITE:?}" "${OUTDIR:?}" "${CHROME:?}"
mkdir -p "$OUTDIR"

if curl -fsS -m 2 http://127.0.0.1:9388/__stats >/dev/null 2>&1; then
  echo "port 9388 is already in use; stop the other mock first" >&2
  exit 1
fi

run() {
  local name="$1" site="$2" hbfmt="$3" v2="$4"
  SITE="$site" PORT=9388 HBFMT="$hbfmt" V2="$v2" node "$HERE/mock.mjs" &
  local pid=$!
  trap 'kill $pid 2>/dev/null || true' RETURN
  sleep 0.3
  kill -0 "$pid" 2>/dev/null || { echo "mock failed to start" >&2; exit 1; }
  for _ in $(seq 1 50); do curl -fsS http://127.0.0.1:9388/__stats >/dev/null 2>&1 && break; sleep 0.1; done
  CHROME="$CHROME" OUT="$OUTDIR/$name-$MODE.json" node "$HERE/measure.mjs" "$MODE" >/dev/null
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  echo "wrote $OUTDIR/$name-$MODE.json"
}

run before "$BEFORE_SITE" old 0
run after "$AFTER_SITE" new 1
run after-oldsrv "$AFTER_SITE" old 0
