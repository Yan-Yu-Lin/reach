#!/usr/bin/env bash
# rehearse-rollback.sh OLD_REACHD NEW_REACHD SNAPSHOT_DB CONFIG
#
# Proves, before the new reachd touches the live database, that the old
# binary can serve a database in the state a rollback would leave it in
# (astra P1-01). Everything runs on a private copy of the pre-deploy
# snapshot; the live DB and dashboard are never touched:
#
#   1. copy SNAPSHOT_DB to a 0700 temp dir;
#   2. inject a canary machine whose agent token is stored as Argon2 (the
#      format the old binary verifies);
#   3. start NEW_REACHD on the copy (migrates it, rewrites the canary token
#      to sha256 on its first heartbeat, exactly like production would);
#   4. stop it, restore the copy from the untouched snapshot (the rollback
#      step), start OLD_REACHD on the restored copy;
#   5. the canary heartbeat must return 200 and an authenticated admin
#      request must succeed.
#
# It also shows what would happen WITHOUT the DB restore (old binary on the
# migrated copy) and reports it, so the danger stays visible.
set -Eeuo pipefail
old_bin="$1"; new_bin="$2"; snapshot="$3"; config="$4"
here="$(cd "$(dirname "$0")" && pwd)"
work="$(mktemp -d /tmp/reach-rehearsal.XXXXXX)"
chmod 0700 "$work"
pids=()
cleanup() {
  for p in "${pids[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
  if command -v trash >/dev/null; then trash "$work" 2>/dev/null || true; else find "$work" -depth -delete 2>/dev/null || true; fi
}
trap cleanup EXIT

port="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])')"
listen="127.0.0.1:$port"
# Same config, but private DB path, listen address, and no OS provisioning.
python3 - "$config" "$work/config.yaml" "$work/reach.db" "$listen" <<'EOF'
import sys, re
src, dst, db, listen = sys.argv[1:5]
out = []
for line in open(src).read().splitlines():
    if re.match(r'^(db_path|listen_addr|provisioning_enabled|health_interval|maintenance_interval):', line):
        continue
    out.append(line)
out += [f"db_path: {db}", f"listen_addr: {listen}", "provisioning_enabled: false",
        "health_interval: 1h", "maintenance_interval: 1h"]
open(dst, "w").write("\n".join(out) + "\n")
EOF
chmod 0600 "$work/config.yaml"

canary_token="at_rehearsal$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
# Random ids so the canary can never collide with a real machine row.
canary_suffix="$(head -c 6 /dev/urandom | od -An -tx1 | tr -d ' \n')"
canary_id="m_rehearsal_${canary_suffix}"
canary_slug="rehearsal-canary-${canary_suffix}"
canary_hash="$(printf '%s' "$canary_token" | "$old_bin" hash-secret)"
cp "$snapshot" "$work/snapshot.db"
cp "$snapshot" "$work/reach.db"
now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
sqlite3 "$work/snapshot.db" "INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at) VALUES ('$canary_id','$canary_slug','$canary_slug','offline','active','offline',1,'$canary_hash','$now');"
cp "$work/snapshot.db" "$work/reach.db"

# start BIN: launch reachd on the private copy and set SERVER_PID. Must not
# run in a subshell: the background process has to be our own child so
# stop() can kill and wait for it (otherwise it keeps the DB locked).
SERVER_PID=""
start() {
  "$1" serve --config "$work/config.yaml" >>"$work/serve.log" 2>&1 &
  SERVER_PID=$!
  pids+=("$SERVER_PID")
  for _ in $(seq 1 50); do
    if curl -s -o /dev/null "http://$listen/api/admin/health"; then return 0; fi
    sleep 0.2
  done
  echo "rehearsal: $1 did not listen" >&2; tail -20 "$work/serve.log" >&2; return 1
}
stop() {
  kill "$SERVER_PID" 2>/dev/null || true
  wait "$SERVER_PID" 2>/dev/null || true
  for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://$listen/" || return 0
    sleep 0.1
  done
}
heartbeat() {
  curl -s -o /dev/null -w '%{http_code}' -X POST "http://$listen/api/client/agent/heartbeat" \
    -H "Authorization: Bearer $canary_token" -H 'Content-Type: application/json' \
    -d '{"machine_id":"'"$canary_id"'","agent_version":"rehearsal","applied_generation":1,"observed":{"local_ssh":{"state":"healthy"},"tunnel":{"state":"stopped"},"persistence":{},"keys":{}}}'
}
restore_copy() { rm -f "$work/reach.db-wal" "$work/reach.db-shm"; cp "$work/snapshot.db" "$work/reach.db.tmp"; mv -f "$work/reach.db.tmp" "$work/reach.db"; }

start "$new_bin"
code="$(heartbeat)"; stop
[ "$code" = 200 ] || { echo "rehearsal: new reachd rejected the canary heartbeat ($code)" >&2; exit 1; }
migrated="$(sqlite3 "$work/reach.db" "SELECT substr(agent_token_hash,1,7) FROM machines WHERE id='$canary_id'")"

start "$old_bin"
without_restore="$(heartbeat)"; stop

restore_copy
start "$old_bin"
with_restore="$(heartbeat)"
ready=0
bash "$here/check-reachd-ready.sh" "$new_bin" "$work/config.yaml" "$listen" && ready=1
stop

echo "rehearsal: canary token after new reachd: ${migrated}..."
echo "rehearsal: old reachd on migrated DB (binary-only rollback): heartbeat $without_restore"
echo "rehearsal: old reachd on restored snapshot: heartbeat $with_restore, authenticated admin probe ready=$ready"
[ "$with_restore" = 200 ] && [ "$ready" = 1 ] || { echo "rehearsal: FAILED - rollback would not restore service" >&2; exit 1; }
echo "rehearsal: OK"
