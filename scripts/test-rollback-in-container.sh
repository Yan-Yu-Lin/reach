#!/usr/bin/env bash
# End-to-end test of the deploy rollback in a throwaway Ubuntu 24.04
# container. It loads the REAL transaction steps from scripts/deploy-jason.sh
# (everything between "BEGIN reachd transaction steps" and "END ..."), so
# install, probe and rollback use the production pathnames and variables
# (astra R2-01: an earlier version picked its own probe path and hid a bug).
# Only systemctl, journalctl and (for scenario E) sudo are stand-ins.
#
# Scenarios:
#   A. failure after the new reachd migrated a legacy Argon2 agent token
#      -> DB snapshot restored, old binary back, legacy heartbeat 200.
#   B. an operator disabled a machine on the new version first
#      -> no automatic DB restore; reachd left stopped.
#   C. the probe/fence binary is missing at rollback time
#      -> no restore, reachd stopped, never reported as restored.
#   D. "systemctl stop reachd" fails (astra R2-03)
#      -> DB, WAL and binary untouched; the new process keeps running.
#   E. the forensic copy of the failed DB fails (astra R2-05)
#      -> live DB not overwritten; reachd stopped; no partial failed set.
#   F. revoke-all on the new version while audit_log inserts fail
#      (daybreak P1-A / astra R2-02) -> the journal blocks the restore.
#   G. carrier rollback restores /etc/reach/ws-carrier.env, or moves away
#      the one this deploy added (astra R2-06).
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
[ -n "${REACH_RBTEST_TRACE:-}" ] && sed -i "" "s/^set -Eeuo pipefail$/set -Eeuxo pipefail/" "$work/scripts/rehearse-rollback.sh"

cat > "$work/run.sh" <<'EOS'
#!/usr/bin/env bash
set -Eeuo pipefail
apt-get -qq update >/dev/null && apt-get -qq install -y sqlite3 curl python3 sudo procps >/dev/null
# Work on the container's own filesystem: SQLite locking is unreliable on
# the bind-mounted host volume.
cp -R /w /work
W=/work
# --- fake systemctl: one reachd process found by its command line ---------
cat > /usr/local/bin/systemctl <<'EOF'
#!/bin/bash
pat='/opt/reach/reachd serve --config'
case "$*" in
  "stop reachd") [ -e /tmp/fail-stop ] && exit 70
    pkill -f "$pat"; for i in $(seq 1 50); do pgrep -f "$pat" >/dev/null || break; sleep 0.1; done ;;
  "start reachd") setsid /opt/reach/reachd serve --config /etc/reach/config.yaml >>/var/log/reachd.log 2>&1 < /dev/null &
    for i in $(seq 1 50); do curl -s -o /dev/null http://127.0.0.1:9300/ && break; sleep 0.1; done ;;
  "is-active --quiet reachd") pgrep -f "$pat" >/dev/null ;;
  "show -p MainPID --value reachd") p="$(pgrep -f "$pat" | head -1)"; echo "${p:-0}" ;;
  *) exit 0 ;;
esac
EOF
chmod +x /usr/local/bin/systemctl
printf '#!/bin/bash\nexit 0\n' > /usr/local/bin/journalctl; chmod +x /usr/local/bin/journalctl
# sudo wrapper: fails copies into the forensic set when asked (scenario E).
cat > /usr/local/bin/sudo <<'EOF'
#!/bin/bash
if [ -e /tmp/fail-forensic ] && [ "$1" = cp ]; then
  case "$*" in */backups/failed.*) echo forced-forensic-copy-failure >&2; exit 70 ;; esac
fi
exec /usr/bin/sudo "$@"
EOF
chmod +x /usr/local/bin/sudo
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
token_kind() { sqlite3 /var/lib/reach/reach.db "SELECT substr(agent_token_hash,1,7) FROM machines WHERE id='m_legacy'"; }

cd $W
UV_BIN=/usr/local/bin/uvstub; printf '#!/bin/bash\nshift 2; exec python3 "$@"\n' > $UV_BIN; chmod +x $UV_BIN
# The real transaction steps, verbatim.
eval "$(sed -n '/^# BEGIN reachd transaction steps/,/^# END reachd transaction steps/p' scripts/deploy-jason.sh)"

fail() { echo "FAIL $1"; exit 1; }

scenario() { # name kind
  systemctl stop reachd || true
  /usr/bin/rm -f /tmp/fail-stop /tmp/fail-forensic /var/lib/reach/reach.db* /var/log/reachd.log /opt/reach/reachd*
  for d in /var/lib/reach/backups /var/lib/reach/deploy-backups; do [ -e "$d" ] && mv "$d" "$d.old.$1.$RANDOM"; done
  mkdir -p /var/lib/reach/backups
  install -m 0755 $W/reachd-old /opt/reach/reachd
  systemctl start reachd; systemctl stop reachd   # the old version creates its schema
  h="$(printf '%s' "$token" | $W/reachd-old hash-secret)"
  sqlite3 /var/lib/reach/reach.db "INSERT INTO machines (id,slug,original_slug,status,desired_state,observed_state,desired_generation,agent_token_hash,created_at) VALUES ('m_legacy','legacy-box','legacy-box','offline','active','offline',1,'$h','2026-01-01T00:00:00Z');"
  systemctl start reachd

  # --- deploy-jason.sh, from "stop reachd" to "new binary running" --------
  DEPLOY_TS=test$1; DB_PATH=/var/lib/reach/reach.db; LISTEN_ADDR=127.0.0.1:9300
  REACHD_EXISTED=1 ROLLBACK_ARMED=0 DEPLOY_COMMITTED=0 SERVICE_STOPPED=1 DB_MAY_HAVE_MIGRATED=0 PROBE_BIN="" REACHD_ROLLBACK=""
  PUBLICATION_ARMED=0 DASHBOARD_SWAP_ARMED=0 CARRIER_STATE_SNAPSHOTTED=0 REACHD_UNIT_MUTATED=0 LIMITS_MUTATED=0
  stop_reachd_verified || fail "$1: initial stop"
  DB_BACKUP_DIR=/var/lib/reach/backups; DB_BACKUP_PATH=$DB_BACKUP_DIR/reach.db.$DEPLOY_TS
  sqlite3 $DB_PATH ".backup '$DB_BACKUP_PATH'"
  cp $W/reachd-new /tmp/reachd-build
  bash scripts/rehearse-rollback.sh /opt/reach/reachd /tmp/reachd-build "$DB_BACKUP_PATH" /etc/reach/config.yaml | tail -1
  DEPLOY_FENCE=$DB_BACKUP_DIR/deploy-fence.$DEPLOY_TS.json
  /tmp/reachd-build deploy-fence record --config /etc/reach/config.yaml --fence $DEPLOY_FENCE
  install_new_reachd
  [ "$PROBE_BIN" = /var/lib/reach/deploy-backups/reachd-probe.$DEPLOY_TS ] || fail "$1: unexpected PROBE_BIN $PROBE_BIN"
  [ ! -e /tmp/reachd-build ] || fail "$1: build binary not consumed"
  DB_MAY_HAVE_MIGRATED=1
  systemctl start reachd; SERVICE_STOPPED=0
  for i in $(seq 1 30); do reachd_ready 2>/dev/null && break; sleep 0.3; done
  reachd_ready || fail "$1: new reachd never became ready"
  echo "new version heartbeat: $(heartbeat) token now $(token_kind)"
  adm="$("$PROBE_BIN" admin-probe-token --config /etc/reach/config.yaml)"
  case "$2" in
    durable)
      echo "operator disable on new version: $(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:9300/api/admin/machines/m_legacy/disable -H "Authorization: Bearer $adm")" ;;
    noprobe) /usr/bin/rm -f "$PROBE_BIN" ;;
    stopfail) touch /tmp/fail-stop ;;
    forensic) touch /tmp/fail-forensic ;;
    auditfail)
      sqlite3 $DB_PATH "CREATE TRIGGER fail_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT,'injected audit failure'); END;"
      echo "revoke-all with failing audit: $(curl -s -o /dev/null -w '%{http_code}' -X POST http://127.0.0.1:9300/api/admin/sessions/revoke-all -H "Authorization: Bearer $adm")" ;;
  esac
  new_pid="$(pgrep -f '/opt/reach/reachd serve --config' | head -1)"

  # --- a later deploy step fails: run the real rollback --------------------
  set +e
  out="$( ( ROLLBACK_EXIT_CODE=1 rollback_reachd ) 2>&1 )"
  set -e
  printf '%s\n' "$out" | grep -E 'restoring|NOT|operator|durable|could not|ready|missing|ERROR: (systemctl|reachd)' | head -6 || true
  if systemctl is-active --quiet reachd; then result="running/$(heartbeat)"; else result="stopped"; fi
  echo "after rollback: $result"
  claimed=0; printf '%s' "$out" | grep -q 'previous reachd restored and ready' && claimed=1
  case "$2" in
    lazy)
      [ "$result" = running/200 ] || fail "A: expected old reachd running with legacy heartbeat 200"
      cmp -s /opt/reach/reachd $W/reachd-old || fail "A: old binary not restored"
      [ -f /opt/reach/reachd.rollback.$DEPLOY_TS ] || fail "A: rollback backup was consumed"
      [ "$claimed" = 1 ] || fail "A: success not reported"
      [ -s $DB_BACKUP_DIR/failed.$DEPLOY_TS/reach.db ] || fail "A: forensic set missing" ;;
    durable|auditfail|noprobe|forensic)
      [ "$result" = stopped ] || fail "$1: expected reachd left stopped"
      [ "$claimed" = 0 ] || fail "$1: rollback claimed success"
      [ "$(token_kind)" = "sha256:" ] || fail "$1: live DB was replaced by the snapshot" ;;
    stopfail)
      [ "$result" != stopped ] || fail "D: the process should still be running"
      [ "$(pgrep -f '/opt/reach/reachd serve --config' | head -1)" = "$new_pid" ] || fail "D: process changed"
      cmp -s /opt/reach/reachd $W/reachd-new || fail "D: on-disk binary swapped under the running process"
      [ "$(token_kind)" = "sha256:" ] || fail "D: DB replaced under the running process"
      [ ! -e $DB_BACKUP_DIR/failed.$DEPLOY_TS ] || fail "D: forensic copy taken from a live DB"
      [ "$claimed" = 0 ] || fail "D: rollback claimed success"
      /usr/bin/rm -f /tmp/fail-stop ;;
  esac
  if [ "$2" = forensic ]; then
    [ ! -e $DB_BACKUP_DIR/failed.$DEPLOY_TS ] || fail "E: partial forensic set published"
  fi
}

carrier_env() { # existed(0/1)
  local f=/etc/reach/ws-carrier.env
  /usr/bin/rm -f $f /etc/reach/ws-carrier.env.failed.*
  mkdir -p /var/lib/reach/deploy-backups /etc/systemd/system
  printf old-unit > /var/lib/reach/deploy-backups/reach-ws-carrier.service.envtest
  printf old-binary > /opt/reach/reach-ws-carrier.rollback.envtest
  printf new-unit > /etc/systemd/system/reach-ws-carrier.service
  printf new-binary > /opt/reach/reach-ws-carrier
  DEPLOY_TS=envtest CARRIER_ENV_EXISTED=$1 CARRIER_ENV_BACKUP=/var/lib/reach/deploy-backups/ws-carrier.env.envtest
  /usr/bin/rm -f $CARRIER_ENV_BACKUP
  [ "$1" = 1 ] && printf 'REACH_WS_PATH_PREFIX=/ws/tunnel/old/\n' > $CARRIER_ENV_BACKUP
  printf 'REACH_WS_PATH_PREFIX=/ws/tunnel/new/\n' > $f
  ( DEPLOY_COMMITTED=0 PUBLICATION_ARMED=0 DASHBOARD_SWAP_ARMED=0 CARRIER_STATE_SNAPSHOTTED=1 \
    CARRIER_UNIT_EXISTED=1 CARRIER_UNIT_BACKUP=/var/lib/reach/deploy-backups/reach-ws-carrier.service.envtest \
    CARRIER_EXISTED=1 CARRIER_ROLLBACK=/opt/reach/reach-ws-carrier.rollback.envtest \
    CARRIER_WAS_ENABLED=1 CARRIER_WAS_ACTIVE=1 LEGACY_WAS_ENABLED=0 LEGACY_WAS_ACTIVE=0 \
    LIMITS_MUTATED=0 REACHD_UNIT_MUTATED=0 ROLLBACK_ARMED=0 SERVICE_STOPPED=0 ROLLBACK_EXIT_CODE=1 rollback_reachd ) >/dev/null 2>&1 || true
  if [ "$1" = 1 ]; then
    grep -q /ws/tunnel/old/ $f || fail "G: old env not restored"
  else
    [ ! -e $f ] || fail "G: env added by the deploy was left in place"
  fi
  grep -q old-binary /opt/reach/reach-ws-carrier || fail "G: carrier binary not restored"
  echo "carrier env rollback (existed=$1): ok"
}

echo "== A: failure after lazy token migration ==";  scenario A lazy
echo "== B: failure after an operator disable ==";   scenario B durable
echo "== C: probe/fence binary missing ==";          scenario C noprobe
echo "== D: systemctl stop fails ==";                scenario D stopfail
echo "== E: forensic copy fails ==";                 scenario E forensic
echo "== F: revoke-all while audit inserts fail =="; scenario F auditfail
echo "== G: carrier env rollback ==";                carrier_env 1; carrier_env 0
echo "PASS: rollback scenarios A-G"
EOS
chmod +x "$work/run.sh"
docker run --rm -v "$work:/w" ubuntu:24.04 bash /w/run.sh
