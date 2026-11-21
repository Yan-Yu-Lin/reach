#!/usr/bin/env bash
# check-reachd-ready.sh PROBE_BIN CONFIG LISTEN_ADDR
#
# PROBE_BIN must support "admin-probe-token" (the new reachd); the server
# being probed may be the old one, which accepts the token (it ignores sv).
#
# Readiness for deploy and rollback (astra P1-01): the port must accept TCP
# AND an authenticated admin request must succeed, which exercises the
# database, migrations and the auth path. TCP alone passed even when the old
# binary could not read the migrated database.
#
# The token is minted by the binary under test from the live config and is
# valid for three minutes; it is never printed.
set -Eeuo pipefail
reachd_bin="$1"; config="$2"; listen="$3"
case "$listen" in 127.0.0.1:*|localhost:*) ;; *) echo "check-reachd-ready: listen address must be loopback" >&2; exit 2 ;; esac
token="$("$reachd_bin" admin-probe-token --config "$config" 2>/dev/null)" || {
  echo "check-reachd-ready: could not mint probe token" >&2; exit 1; }
# The token goes to curl on stdin (--config -), never in argv: /proc on the
# hub is world-readable (daybreak round 5 N8). printf is a bash builtin.
code="$(printf 'header = "Authorization: Bearer %s"\nurl = "http://%s/api/admin/health"\n' "$token" "$listen" \
  | curl -s -o /dev/null -w '%{http_code}' --max-time 5 --config -)" || code=000
unset token
if [ "$code" != 200 ]; then
  echo "check-reachd-ready: authenticated GET /api/admin/health returned $code" >&2
  exit 1
fi
