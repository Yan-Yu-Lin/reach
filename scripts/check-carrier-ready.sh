#!/usr/bin/env bash
# check-carrier-ready.sh ENV_FILE [LISTEN]
#
# Protocol readiness of the WebSocket carrier (daybreak round 4 P1-04): the
# carrier must answer a WebSocket upgrade on its exact configured path with
# 101. The path is read from ENV_FILE (REACH_WS_PATH_PREFIX=...) and never
# printed. LISTEN defaults to 127.0.0.1:9401.
set -Eeuo pipefail
env_file="$1"; listen="${2:-127.0.0.1:9401}"
case "$listen" in 127.0.0.1:*) ;; *) echo "check-carrier-ready: listen must be loopback" >&2; exit 2 ;; esac
path="$(sed -n 's/^REACH_WS_PATH_PREFIX=//p' "$env_file" | head -1)"
case "$path" in /*/) ;; *) echo "check-carrier-ready: no usable REACH_WS_PATH_PREFIX in $env_file" >&2; exit 1 ;; esac
# Read only the status line; the carrier then waits for frames, so the
# connection is closed after a short timeout.
# The secret path goes to curl on stdin (--config -), never in argv
# (daybreak round 5 N8). printf is a bash builtin.
code="$(printf 'url = "http://%s%s"\n' "$listen" "$path" \
  | curl -s -o /dev/null -w '%{http_code}' --http1.1 --max-time 3 \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' --config - || true)"
unset path
if [ "$code" != 101 ]; then
  echo "check-carrier-ready: upgrade on the configured path returned ${code:-nothing}" >&2
  exit 1
fi
