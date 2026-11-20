#!/usr/bin/env bash
# End-to-end test of the deploy rollback (astra P1-01) in a throwaway Ubuntu
# 24.04 container, using the REAL rollback_reachd function from
# scripts/deploy-jason.sh with a tiny systemctl stand-in.
#
# Scenarios:
#   A. deploy fails after the new reachd migrated a legacy Argon2 agent token
#      -> DB snapshot restored, old binary back, the legacy heartbeat is 200.
#   B. same, but an operator disabled a machine on the new version first
#      -> no automatic DB restore; reachd left stopped for a human.
#
# Usage (from the repo root, Docker running): bash scripts/test-rollback-in-container.sh
set -Eeuo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d /tmp/reach-rbtest.XXXXXX)"
trap 'trash "$work" 2>/dev/null || true' EXIT
base="${REACH_ROLLBACK_BASE:-8c74086}"
git -C "$repo" archive "$base" | (mkdir -p "$work/old" && tar -x -C "$work/old")
(cd "$work/old" && GOOS=linux GOARCH="$(go env GOARCH)" CGO_ENABLED=0 go build -o "$work/reachd-old" .)
(cd "$repo" && GOOS=linux GOARCH="$(go env GOARCH)" CGO_ENABLED=0 go build -o "$work/reachd-new" .)
cp -R "$repo/scripts" "$work/scripts"

cat > "$work/run.sh" <<'EOS'
#!/usr/bin/env bash
set -Eeuo pipefail
apt-get -qq update >/dev/null && apt-get -qq install -y sqlite3 curl python3 sudo procps >/dev/null
# Work on the container's own filesystem: SQLite locking is unreliable on
# the bind-mounted host volume.
cp -R /w /work
W=/work
# --- fake systemctl: one reachd process managed by pid file ---------------
cat > /usr/local/bin/systemctl <<'EOF'
#!/bin/bash
# Minimal stand-in: one "reachd serve" process, found by name.
case "$1 $2" in
  "stop reachd") pkill -f '/opt/reach/reachd serve --config'; for i in $(seq 1 50); do pgrep -f '/opt/reach/reachd serve --config' >/dev/null || break; sleep 0.1; done ;;
  "start reachd") setsid /opt/reach/reachd serve --config /etc/reach/config.yaml >>/var/log/reachd.log 2>&1 < /dev/null &
    for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:9300/ && break; sleep 0.1; done ;;
  "is-active --quiet") pgrep -f '/opt/reach/reachd serve --config' >/dev/null ;;
  *) exit 0 ;;
esac
EOF
chmod +x /usr/local/bin/systemctl
printf '#!/bin/bash\nexit 0\n' > /usr/local/bin/journalctl; chmod +x /usr/local/bin/journalctl
mkdir -p /opt/reach /etc/reach /var/lib/reach/backups
pw="$(printf pw | $W/reachd-new hash-secret)"
cat > /etc/reach/config.yaml <<EOF
listen_addr: 127.0.0.1:9300
db_path: /var/lib/reach/reach.db
jwt_secret: container-secret-container-secret-12
provisioning_enabled: false
health_interval: 1h
maintenance_interval: 1h
initial_admin:
  username: arthur
  password_hash: "$pw"
EOF
token=at_legacycontainer000000000000001
hb='{"machine_id":"m_legacy","agent_version":"t","applied_generation":1,"observed":{"local_ssh":{"state":"healthy"},"tunnel":{"state":"stopped"},"persistence":{},"keys":{}}}'
heartbeat() { curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:9300/api/client/agent/heartbeat -H "Authorization: Bearer $token" -d "$hb"; }

scenario() { # name, durable_action(0/1)
  systemctl stop reachd || true
  rm -f /var/lib/reach/reach.db* /var/log/reachd.log
  install -m 0755 $W/reachd-old /opt/reach/reachd
  systemctl start reachd; systemctl stop reachd   # old version creates schema
  echo "procs after stop: $(pgrep -af reachd || echo none)"; tail -3 /var/log/reachd.log || true
  h="$(printf '%s' "$token" | $W/reachd-old hash-secret)"
  sqlite3 /var/lib/reach/reach.db "INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at) VALUES ('m_legacy','legacy-box','legacy-box','offline','active','offline',1,'$h','2026-01-01T00:00:00Z');"

  # --- the deploy steps up to "new binary running", as in deploy-jason.sh --
  cd $W
  UV_BIN=/usr/local/bin/uvstub; printf '#!/bin/bash\nshift 2; exec python3 "$@"\n' > $UV_BIN; chmod +x $UV_BIN
  DEPLOY_TS=test$1; DB_PATH=/var/lib/reach/reach.db; LISTEN_ADDR=127.0.0.1:9300
  DB_BACKUP_DIR=/var/lib/reach/backups; DB_BACKUP_PATH=$DB_BACKUP_DIR/reach.db.$DEPLOY_TS
  sqlite3 $DB_PATH ".backup '$DB_BACKUP_PATH'"
  cp $W/reachd-new /tmp/reachd-build
  bash scripts/rehearse-rollback.sh /opt/reach/reachd /tmp/reachd-build "$DB_BACKUP_PATH" /etc/reach/config.yaml | tail -2
  DEPLOY_FENCE=$DB_BACKUP_DIR/fence.$DEPLOY_TS.json
  /tmp/reachd-build deploy-fence record --config /etc/reach/config.yaml --fence $DEPLOY_FENCE
  cp /opt/reach/reachd /opt/reach/reachd.rollback.$DEPLOY_TS
  install -m 0755 /tmp/reachd-build /opt/reach/reachd
  sleep 1.1   # writes below land strictly after the fence second
  systemctl start reachd
  echo "new version heartbeat: $(heartbeat) token now $(sqlite3 $DB_PATH "SELECT substr(agent_token_hash,1,7) FROM machines WHERE id='m_legacy'")"
  if [ "$2" = 1 ]; then
    adm="$(/opt/reach/reachd admin-probe-token --config /etc/reach/config.yaml)"
    echo "operator disable on new version: $(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:9300/api/admin/machines/m_legacy/disable -H "Authorization: Bearer $adm")"
  fi

  # --- simulate a failure later in the deploy: run the real rollback ------
  eval "$(sed -n '/^rollback_reachd() {/,/^}/p' scripts/deploy-jason.sh)"
  DEPLOY_COMMITTED=0 ROLLBACK_ARMED=1 DB_MAY_HAVE_MIGRATED=1 REACHD_EXISTED=1
  REACHD_ROLLBACK=/opt/reach/reachd.rollback.$DEPLOY_TS PROBE_BIN=/tmp/reachd-build
  PUBLICATION_ARMED=0 DASHBOARD_SWAP_ARMED=0 CARRIER_STATE_SNAPSHOTTED=0 REACHD_UNIT_MUTATED=0 SERVICE_STOPPED=0
  ( ROLLBACK_EXIT_CODE=1 rollback_reachd ) 2>&1 | grep -E 'restoring|NOT restoring|operator|durable|audit' | head -5 || true
  if systemctl is-active --quiet reachd; then
    echo "after rollback: reachd running, legacy heartbeat $(heartbeat)"
  else
    echo "after rollback: reachd stopped (manual decision)"
  fi
}
echo "== A: failure after lazy token migration =="; scenario A 0
echo "== B: failure after an operator disable  =="; scenario B 1
EOS
chmod +x "$work/run.sh"
docker run --rm -v "$work:/w" ubuntu:24.04 bash /w/run.sh
