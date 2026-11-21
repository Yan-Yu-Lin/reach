#!/usr/bin/env bash
set -Eeuo pipefail

# Deploy Reach to a hub server from the repo checked out on that hub.
# Normally started by scripts/deploy-from-mac.sh. For a direct run on the hub:
#   cd ~/reach && git fetch origin && git checkout --detach origin/release/reach-2026-11
#   bash scripts/deploy-jason.sh
# It refuses anything but the exact tip of the release branch (release-gate.sh).
#
# - Existing deployments leave hosted reach-agent artifacts unchanged by default.
#   Set REACH_PUBLISH_AGENT_RELEASE=1 to build/install/publish an agent release.
# - REACH_AGENT_VERSION overrides everything.
# - Exact git tag vX.Y.Z[...suffix] publishes version X.Y.Z[...suffix].
# - Untagged deploys publish REACH_VERSION_BASE-dev.YYYYmmddTHHMMSSZ.g<sha>.
#
# Signed agent artifacts:
# - Preferred: pass REACH_AGENT_ARTIFACT_DIR containing pre-built binaries,
#   manifest.json, manifest.json.minisig, and checksums.txt. This keeps the
#   release signing private key off the hub.
# - Fallback: if building on the hub, set REACH_RELEASE_KEY plus optional
#   REACH_RELEASE_KEY_PASSWORD_FILE/REACH_RELEASE_KEY_PASSWORD to sign there.

cd "$(dirname "$0")/.."

export CGO_ENABLED=0

UV_BIN="$(command -v uv 2>/dev/null || true)"
if [ -z "$UV_BIN" ] && [ -x "$HOME/.local/bin/uv" ]; then
  UV_BIN="$HOME/.local/bin/uv"
fi
if [ -z "$UV_BIN" ] || [ ! -x "$UV_BIN" ]; then
  echo "[deploy] ERROR: uv is required (checked PATH and $HOME/.local/bin/uv)" >&2
  exit 1
fi
UV_BIN="$(cd "$(dirname "$UV_BIN")" && pwd)/$(basename "$UV_BIN")"

command -v flock >/dev/null || { echo "[deploy] ERROR: flock is required" >&2; exit 1; }
# Safe deploy lock (daybreak round 5 N7): the lock directory must be a real
# directory owned by us and not group/other writable, and the lock file a
# regular file (not a symlink); it is opened without truncation, so a
# planted symlink can neither redirect the lock nor clobber a file.
open_deploy_lock() {
  local dir="$1" f="$1/deploy.lock" owner mode
  mkdir -p -m 0700 "$dir"
  owner="$(stat -c %u "$dir" 2>/dev/null || stat -f %u "$dir")"
  mode="$(stat -c %a "$dir" 2>/dev/null || stat -f %Lp "$dir")"
  if [ -L "$dir" ] || [ ! -d "$dir" ] || [ "$owner" != "$(id -u)" ]; then
    echo "ERROR: deploy lock directory $dir is not a private directory of ours" >&2; return 1
  fi
  if [ $(( 8#$mode & 8#022 )) -ne 0 ]; then
    echo "ERROR: deploy lock directory $dir is group/other writable" >&2; return 1
  fi
  if [ -L "$f" ] || { [ -e "$f" ] && [ ! -f "$f" ]; }; then
    echo "ERROR: deploy lock $f is not a regular file" >&2; return 1
  fi
  exec 9<>"$f"
}
LOCK_DIR="${XDG_RUNTIME_DIR:-$HOME/.cache/reach}/reach"
if [ "${REACH_DEPLOY_LOCK_HELD:-0}" = 1 ]; then
  if ! flock -n 9; then
    echo "[deploy] ERROR: inherited deployment lock is not held" >&2
    exit 1
  fi
else
  open_deploy_lock "$LOCK_DIR" || exit 1
  if ! flock -n 9; then
    echo "[deploy] ERROR: another Reach deployment is running" >&2
    exit 1
  fi
fi

version_from_git() {
  local tag sha ts base
  if tag="$(git describe --tags --exact-match --match 'v[0-9]*' 2>/dev/null)"; then
    printf '%s\n' "${tag#v}"
    return 0
  fi
  sha="$(git rev-parse --short=12 HEAD)"
  ts="$(date -u +%Y%m%dT%H%M%SZ)"
  base="${REACH_VERSION_BASE:-0.1.0}"
  printf '%s-dev.%s.g%s\n' "$base" "$ts" "$sha"
}

build_agent() {
  local out="$1"
  local goos="$2"
  local arch="$3"
  local arm="${4:-}"
  echo "[deploy] building reach-agent (${goos}/${arch}${arm:+ GOARM=$arm})..."
  if [ -n "$arm" ]; then
    GOOS="$goos" GOARCH="$arch" GOARM="$arm" CGO_ENABLED=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=status.showUntrackedFiles GIT_CONFIG_VALUE_0=no go build -ldflags "${AGENT_LDFLAGS[*]}" -o "$out" ./cmd/reach-agent
  else
    GOOS="$goos" GOARCH="$arch" CGO_ENABLED=0 GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=status.showUntrackedFiles GIT_CONFIG_VALUE_0=no go build -ldflags "${AGENT_LDFLAGS[*]}" -o "$out" ./cmd/reach-agent
  fi
}

write_checksums() {
  local dir="$1"
  (
    cd "$dir"
    sha256sum reach-agent_* > checksums.txt
  )
}

create_manifest() {
  local dir="$1"
  go run ./cmd/reach-release manifest --dir "$dir" --version "$AGENT_VERSION" --commit "$(git rev-parse HEAD)" --sequence "$RELEASE_SEQUENCE"
}

sign_manifest_if_configured() {
  local dir="$1"
  local key_args=()
  if [ -z "${REACH_RELEASE_KEY:-}" ]; then
    return 1
  fi
  key_args+=(--key "$REACH_RELEASE_KEY")
  if [ -n "${REACH_RELEASE_KEY_PASSWORD_FILE:-}" ]; then
    key_args+=(--password-file "$REACH_RELEASE_KEY_PASSWORD_FILE")
  fi
  go run ./cmd/reach-release sign "${key_args[@]}" --manifest "$dir/manifest.json" --out "$dir/manifest.json.minisig" --trusted-comment "reach-agent version=$AGENT_VERSION commit=$(git rev-parse HEAD)"
}

require_agent_artifacts() {
  local dir="$1"
  local f
  for f in \
    reach-agent_linux_amd64 \
    reach-agent_linux_arm64 \
    reach-agent_linux_386 \
    reach-agent_linux_armv6 \
    reach-agent_linux_armv7 \
    reach-agent_darwin_amd64 \
    reach-agent_darwin_arm64 \
    reach-agent_windows_amd64.exe \
    reach-agent_windows_arm64.exe \
    checksums.txt \
    manifest.json \
    manifest.json.minisig; do
    [ -f "$dir/$f" ] || { echo "[deploy] ERROR: missing artifact $dir/$f" >&2; exit 1; }
  done
}

require_clean_tree() {
  local status
  status="$(git status --porcelain --untracked-files=normal)"
  status="$(printf '%s\n' "$status" | grep -v '^?? \.claude/$' || true)"
  if [ -n "$status" ]; then
    echo "[deploy] ERROR: product tree has tracked or nonignored untracked changes" >&2
    printf '%s\n' "$status" >&2
    exit 1
  fi
}

require_clean_tree
# Release gate (daybreak round 6 R6-P1-01): deploy only the exact pushed tip
# of the one release branch. This script never switches branches or pulls:
# the checkout must already be that tip (deploy-from-mac.sh checks it out
# before starting this script; for a direct run on the hub, run
#   git fetch origin && git checkout --detach origin/<release branch>
# first). A detached HEAD is never moved to main.
# shellcheck source=release-gate.sh
. scripts/release-gate.sh
require_release_head "[deploy]" || exit 1
deploy_commit="$(git rev-parse HEAD)"
if [ -n "${REACH_DEPLOY_COMMIT:-}" ] && [ "$REACH_DEPLOY_COMMIT" != "$deploy_commit" ]; then
  echo "[deploy] ERROR: REACH_DEPLOY_COMMIT ${REACH_DEPLOY_COMMIT} is not the checked-out release tip ${deploy_commit}" >&2
  exit 1
fi
echo "[deploy] deploying ${REACH_RELEASE_BRANCH} at ${deploy_commit}"

AGENT_VERSION="${REACH_AGENT_VERSION:-$(version_from_git)}"
bash scripts/validate-release-version.sh "$AGENT_VERSION" || {
  echo "[deploy] ERROR: invalid agent version" >&2
  exit 1
}
# One release sequence (Unix seconds) shared by the binaries and the signed
# manifest; agents seed their anti-rollback floor from the embedded value
# on first start (daybreak P2-1).
RELEASE_SEQUENCE="${REACH_RELEASE_SEQUENCE:-$(date -u +%s)}"
AGENT_LDFLAGS=("-X" "main.version=${AGENT_VERSION}" "-X" "main.releaseSequence=${RELEASE_SEQUENCE}")
AGENT_DOWNLOAD_ROOT="/var/lib/reach/downloads/reach-agent"
AGENT_DOWNLOAD_DIR="${AGENT_DOWNLOAD_ROOT}/v${AGENT_VERSION}"
AGENT_STAGING_DIR="${AGENT_DOWNLOAD_ROOT}/.v${AGENT_VERSION}.tmp.$$"
ARTIFACT_DIR="${REACH_AGENT_ARTIFACT_DIR:-}"
BUILD_ARTIFACT_DIR=""
MUTATION_STARTED=0
DEPLOY_COMMITTED=0
ROLLBACK_ARMED=0
SERVICE_STOPPED=0
DB_MAY_HAVE_MIGRATED=0
DB_BACKUP_PATH=""
DEPLOY_FENCE=""
PROBE_BIN=""
REACHD_ROLLBACK=""
REACHD_EXISTED=0
CARRIER_ROLLBACK=""
CARRIER_EXISTED=0
CARRIER_UNIT_BACKUP=""
CARRIER_UNIT_EXISTED=0
CARRIER_ENV_BACKUP=""
CARRIER_ENV_EXISTED=0
CARRIER_WAS_ENABLED=0
CARRIER_WAS_ACTIVE=0
LEGACY_WAS_ENABLED=0
LEGACY_WAS_ACTIVE=0
CARRIER_STATE_SNAPSHOTTED=0
CARRIER_ROLLBACK_ARMED=0
DASHBOARD_EXISTED=0
DASHBOARD_SWAP_ARMED=0
DASHBOARD_MUTATED=0
DASHBOARD_STAGE=""
DASHBOARD_BACKUP=""
PUBLISH_AGENT_RELEASE="${REACH_PUBLISH_AGENT_RELEASE:-0}"
BACKUP_RETAIN="${REACH_BACKUP_RETAIN:-5}"
PUBLICATION_ARMED=0
AGENT_BIN_BACKUP=""
SETUP_SH_BACKUP=""
SETUP_PS1_BACKUP=""
LATEST_TXT_BACKUP=""
LATEST_JSON_BACKUP=""
AGENT_VERSION_BACKUP=""
AGENT_VERSION_EXISTED=0
AGENT_BIN_EXISTED=0
SETUP_SH_EXISTED=0
SETUP_PS1_EXISTED=0
LATEST_TXT_EXISTED=0
LATEST_JSON_EXISTED=0

cleanup() {
  if [ -n "$BUILD_ARTIFACT_DIR" ] && [ -d "$BUILD_ARTIFACT_DIR" ]; then
    if command -v trash >/dev/null; then
      trash "$BUILD_ARTIFACT_DIR"
    else
      rm -rf "$BUILD_ARTIFACT_DIR"
    fi
  fi
  if [ -n "$DASHBOARD_STAGE" ] && sudo test -e "$DASHBOARD_STAGE"; then
    sudo mv "$DASHBOARD_STAGE" "${DASHBOARD_STAGE}.abandoned" 2>/dev/null || true
  fi
  # The probe copy of the new binary (deploy-backups/reachd-probe.*) is kept:
  # after a failed rollback it is the only binary that can run
  # "deploy-fence check" for the operator. Backup pruning removes old ones.
}

# BEGIN reachd transaction steps
# Everything between the BEGIN/END markers is also loaded by
# scripts/test-rollback-in-container.sh, so the rollback test runs the real
# steps with the real pathnames (astra R2-01).

# stop_reachd_verified: stop reachd and prove it is gone before anyone
# touches the database (astra R2-03). Returns non-zero when systemctl fails,
# the unit is still active, the old main PID is still alive, fuser is
# missing, or some process still holds the database or its WAL/SHM (astra
# R3-03, daybreak round 4).
stop_reachd_verified() {
  local pid
  pid="$(systemctl show -p MainPID --value reachd 2>/dev/null || true)"
  case "$pid" in ''|*[!0-9]*) pid=0 ;; esac
  if ! sudo systemctl stop reachd; then
    echo "[deploy] ERROR: systemctl stop reachd failed" >&2
    return 1
  fi
  if systemctl is-active --quiet reachd; then
    echo "[deploy] ERROR: reachd is still active after stop" >&2
    return 1
  fi
  if [ "$pid" -gt 0 ]; then
    local i
    for i in $(seq 1 50); do
      sudo kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    if sudo kill -0 "$pid" 2>/dev/null; then
      echo "[deploy] ERROR: reachd process $pid is still alive after stop" >&2
      return 1
    fi
  fi
  if [ -n "${DB_PATH:-}" ]; then
    if ! command -v fuser >/dev/null 2>&1; then
      echo "[deploy] ERROR: fuser (psmisc) is required to prove nobody holds the database" >&2
      return 1
    fi
    local f
    for f in "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"; do
      if sudo test -e "$f" && sudo fuser "$f" >/dev/null 2>&1; then
        echo "[deploy] ERROR: a process still has $f open" >&2
        return 1
      fi
    done
  fi
  return 0
}

# install_new_reachd: keep the old binary as an immutable rollback copy,
# keep a private copy of the NEW binary for readiness probes and the deploy
# fence (the old binary has neither admin-probe-token nor deploy-fence, and
# rollback puts the old binary back at /opt/reach/reachd: astra R2-01,
# daybreak P1-B), then install the new binary.
install_new_reachd() {
  REACHD_ROLLBACK="/opt/reach/reachd.rollback.${DEPLOY_TS}"
  PROBE_BIN="/var/lib/reach/deploy-backups/reachd-probe.${DEPLOY_TS}"
  sudo install -d -m 0700 /var/lib/reach/deploy-backups
  sudo install -m 0700 /tmp/reachd-build "$PROBE_BIN"
  sudo test -x "$PROBE_BIN"
  ROLLBACK_ARMED=1
  if [ "$REACHD_EXISTED" = 1 ]; then
    sudo cp -a /opt/reach/reachd "$REACHD_ROLLBACK"
    sudo cmp -s /opt/reach/reachd "$REACHD_ROLLBACK"
  fi
  sudo install -m 0755 /tmp/reachd-build /opt/reach/reachd.candidate
  sudo mv -f /opt/reach/reachd.candidate /opt/reach/reachd
  rm -f /tmp/reachd-build
}

# The forensic copy, fence check and snapshot restore are done by
# "reachd deploy-restore" under the writers lock (deploy_txn.go).

# handoff_args: what deploy-restore / deploy-handoff put at /opt/reach/reachd
# before unlocking: the rollback copy, or (fresh install) nothing.
handoff_args() {
  if [ "$REACHD_EXISTED" = 1 ]; then
    printf -- '--binary-target /opt/reach/reachd --rollback-binary %s' "$REACHD_ROLLBACK"
  else
    printf -- '--binary-target /opt/reach/reachd --remove-binary'
  fi
}


# reachd_ready: TCP plus an authenticated admin request, the token minted by
# the private probe copy of the new binary.
reachd_ready() {
  if [ -z "$PROBE_BIN" ] || ! sudo test -x "$PROBE_BIN"; then
    echo "[deploy] ERROR: readiness probe binary ${PROBE_BIN:-<unset>} is missing" >&2
    return 1
  fi
  systemctl is-active --quiet reachd \
    && "$UV_BIN" run --script scripts/check-local-service.py "$LISTEN_ADDR" \
    && sudo bash scripts/check-reachd-ready.sh "$PROBE_BIN" /etc/reach/config.yaml "$LISTEN_ADDR"
}

# ROLLBACK_INCOMPLETE collects every rollback step that failed (daybreak
# round 4 P1-04, astra R3-04). Any entry means the rollback never reports
# success, whatever reachd's own state.
ROLLBACK_INCOMPLETE=""
rollback_incomplete() {
  ROLLBACK_INCOMPLETE="${ROLLBACK_INCOMPLETE:+$ROLLBACK_INCOMPLETE; }$1"
  echo "[deploy] ERROR: rollback step failed: $1" >&2
}
report_incomplete() {
  if [ -n "$ROLLBACK_INCOMPLETE" ]; then
    echo "[deploy] ERROR: rollback_incomplete: $ROLLBACK_INCOMPLETE" >&2
  fi
}
# rollback_finish MESSAGE EXIT: print MESSAGE only if every step succeeded.
rollback_finish() {
  if [ -n "$ROLLBACK_INCOMPLETE" ]; then
    report_incomplete
    echo "[deploy] ERROR: rollback is INCOMPLETE; check the components above by hand" >&2
  elif [ -n "$1" ]; then
    echo "[deploy] $1" >&2
  fi
  exit "$2"
}

# restore_file_checked BACKUP DEST MODE: copy BACKUP over DEST atomically
# and verify the content.
restore_file_checked() {
  local backup="$1" dest="$2" mode="$3"
  sudo test -f "$backup" || { echo "[deploy] ERROR: backup $backup is missing" >&2; return 1; }
  sudo install -m "$mode" "$backup" "$dest.rollback-candidate" || return 1
  sudo mv -f "$dest.rollback-candidate" "$dest" || return 1
  sudo cmp -s "$backup" "$dest"
}
# move_away_checked PATH: move a file this deploy added out of the way.
move_away_checked() {
  if sudo test -e "$1"; then
    sudo mv "$1" "$1.failed.$DEPLOY_TS" || return 1
  fi
  ! sudo test -e "$1"
}

# restore_carrier_state: stop the carrier, restore unit, env and binary
# (each checked), and only then bring back the previous enabled/active
# state and verify it, including a WebSocket upgrade on the configured
# path. Any failure leaves the carrier stopped and returns non-zero.
restore_carrier_state() {
  local ok=1
  sudo systemctl disable --now reach-ws-carrier.service 2>/dev/null || true
  if [ "$CARRIER_UNIT_EXISTED" = 1 ]; then
    restore_file_checked "$CARRIER_UNIT_BACKUP" /etc/systemd/system/reach-ws-carrier.service 0644 || ok=0
  else
    move_away_checked /etc/systemd/system/reach-ws-carrier.service || ok=0
  fi
  if [ "${CARRIER_ENV_EXISTED:-0}" = 1 ]; then
    restore_file_checked "$CARRIER_ENV_BACKUP" /etc/reach/ws-carrier.env 0640 || ok=0
  else
    move_away_checked /etc/reach/ws-carrier.env || ok=0
  fi
  if [ "$CARRIER_EXISTED" = 1 ]; then
    restore_file_checked "$CARRIER_ROLLBACK" /opt/reach/reach-ws-carrier 0755 || ok=0
  else
    move_away_checked /opt/reach/reach-ws-carrier || ok=0
  fi
  sudo systemctl daemon-reload || ok=0
  if [ "$ok" != 1 ]; then
    echo "[deploy] ERROR: carrier files could not be restored; leaving the carrier stopped" >&2
    sudo systemctl stop reach-ws-carrier.service 2>/dev/null || true
    return 1
  fi
  if [ "$CARRIER_WAS_ENABLED" = 1 ]; then
    sudo systemctl enable reach-ws-carrier.service || ok=0
  else
    sudo systemctl disable reach-ws-carrier.service 2>/dev/null || true
  fi
  if [ "$CARRIER_WAS_ACTIVE" = 1 ]; then
    sudo systemctl start reach-ws-carrier.service || ok=0
    local ready=0 i
    for i in $(seq 1 10); do
      if systemctl is-active --quiet reach-ws-carrier.service; then
        if ! sudo test -f /etc/reach/ws-carrier.env; then
          ready=1   # older carrier without a path file: unit state only
          break
        fi
        if sudo bash scripts/check-carrier-ready.sh /etc/reach/ws-carrier.env >/dev/null 2>&1; then
          ready=1
          break
        fi
      fi
      sleep 1
    done
    [ "$ready" = 1 ] || { echo "[deploy] ERROR: previous carrier is not active/answering" >&2; ok=0; }
  else
    sudo systemctl stop reach-ws-carrier.service 2>/dev/null || true
    if systemctl is-active --quiet reach-ws-carrier.service; then ok=0; fi
  fi
  if [ "$LEGACY_WAS_ENABLED" = 1 ]; then sudo systemctl enable reach-wstunnel.service || ok=0; else sudo systemctl disable reach-wstunnel.service 2>/dev/null || true; fi
  if [ "$LEGACY_WAS_ACTIVE" = 1 ]; then
    sudo systemctl start reach-wstunnel.service || ok=0
    systemctl is-active --quiet reach-wstunnel.service || ok=0
  else
    sudo systemctl stop reach-wstunnel.service 2>/dev/null || true
  fi
  [ "$ok" = 1 ]
}

# restore_reachd_units: put the resource drop-in and an opt-in reachd unit
# back (or move ours away), reload, and verify systemd loaded the unit.
restore_reachd_units() {
  local ok=1
  if [ "${LIMITS_MUTATED:-0}" = 1 ]; then
    if [ -n "${LIMITS_BACKUP:-}" ]; then
      restore_file_checked "$LIMITS_BACKUP" "$LIMITS_DROPIN" 0644 || ok=0
    else
      move_away_checked "$LIMITS_DROPIN" || ok=0
    fi
  fi
  if [ "${REACHD_UNIT_MUTATED:-0}" = 1 ]; then
    if [ -n "${REACHD_UNIT_BACKUP:-}" ]; then
      echo "[deploy] restoring previous reachd.service" >&2
      restore_file_checked "$REACHD_UNIT_BACKUP" /etc/systemd/system/reachd.service 0644 || ok=0
    else
      # There was no /etc override before (vendor unit or fresh host):
      # move ours away instead of leaving it active (astra P2-12).
      echo "[deploy] removing reachd.service override added by this deploy" >&2
      move_away_checked /etc/systemd/system/reachd.service || ok=0
    fi
  fi
  sudo systemctl daemon-reload || ok=0
  if [ "$(systemctl show -p LoadState --value reachd 2>/dev/null)" != "loaded" ]; then
    echo "[deploy] ERROR: reachd.service is not loaded after restoring units" >&2
    ok=0
  fi
  [ "$ok" = 1 ]
}

rollback_reachd() {
  local exit_code="${ROLLBACK_EXIT_CODE:-$?}"
  if [ "$DEPLOY_COMMITTED" = 1 ]; then
    exit "$exit_code"
  fi
  if [ "${ROLLBACK_RUNNING:-0}" = 1 ]; then
    exit "$exit_code"
  fi
  ROLLBACK_RUNNING=1
  trap - ERR INT TERM HUP
  set +e
  if [ "$PUBLICATION_ARMED" = 1 ]; then
    echo "[deploy] restoring previous agent publication" >&2
    for record in \
      "$AGENT_BIN_EXISTED|$AGENT_BIN_BACKUP|/opt/reach/reach-agent" \
      "$SETUP_SH_EXISTED|$SETUP_SH_BACKUP|/var/lib/reach/setup.sh" \
      "$SETUP_PS1_EXISTED|$SETUP_PS1_BACKUP|/var/lib/reach/setup.ps1" \
      "$LATEST_TXT_EXISTED|$LATEST_TXT_BACKUP|$AGENT_DOWNLOAD_ROOT/latest.txt" \
      "$LATEST_JSON_EXISTED|$LATEST_JSON_BACKUP|$AGENT_DOWNLOAD_ROOT/latest.json"; do
      existed="${record%%|*}"
      remainder="${record#*|}"
      backup="${remainder%%|*}"
      destination="${remainder#*|}"
      if [ "$existed" = 1 ]; then
        if [ -n "$backup" ] && sudo test -f "$backup"; then
          sudo cp -a "$backup" "$destination.rollback-candidate"
          sudo mv -f "$destination.rollback-candidate" "$destination"
        fi
      elif sudo test -e "$destination"; then
        sudo mv "$destination" "$destination.failed.$DEPLOY_TS"
      fi
    done
    if [ -n "$AGENT_VERSION_BACKUP" ] && sudo test -d "$AGENT_VERSION_BACKUP"; then
      if sudo test -d "$AGENT_DOWNLOAD_DIR"; then
        sudo mv "$AGENT_DOWNLOAD_DIR" "$AGENT_DOWNLOAD_DIR.failed.$DEPLOY_TS"
      fi
      sudo mv "$AGENT_VERSION_BACKUP" "$AGENT_DOWNLOAD_DIR"
    elif [ "$AGENT_VERSION_EXISTED" = 0 ] && sudo test -d "$AGENT_DOWNLOAD_DIR"; then
      sudo mv "$AGENT_DOWNLOAD_DIR" "$AGENT_DOWNLOAD_DIR.failed.$DEPLOY_TS"
    fi
  fi
  if [ "$DASHBOARD_SWAP_ARMED" = 1 ]; then
    local dashboard_rollback_source=""
    if [ "$DASHBOARD_MUTATED" != 1 ]; then
      :
    elif [ -n "$DASHBOARD_BACKUP" ] && sudo test -d "$DASHBOARD_BACKUP"; then
      dashboard_rollback_source="$DASHBOARD_BACKUP"
    elif [ -n "$DASHBOARD_STAGE" ] && sudo test -d "$DASHBOARD_STAGE"; then
      dashboard_rollback_source="$DASHBOARD_STAGE"
    fi
    if [ "$DASHBOARD_EXISTED" = 1 ] && [ -n "$dashboard_rollback_source" ]; then
      echo "[deploy] restoring previous dashboard" >&2
      sudo "$UV_BIN" run --script scripts/atomic-directory-swap.py "$dashboard_rollback_source" /opt/reach-dashboard
    elif [ "$DASHBOARD_MUTATED" = 1 ] && [ "$DASHBOARD_EXISTED" = 0 ] && sudo test -e /opt/reach-dashboard; then
      echo "[deploy] removing partial fresh dashboard installation" >&2
      sudo mv /opt/reach-dashboard "/opt/reach-dashboard.failed.$DEPLOY_TS"
    fi
  fi
  if [ "$CARRIER_STATE_SNAPSHOTTED" = 1 ]; then
    echo "[deploy] restoring previous WebSocket carrier state" >&2
    restore_carrier_state || rollback_incomplete "WebSocket carrier"
  fi
  if [ "${LIMITS_MUTATED:-0}" = 1 ] || [ "${REACHD_UNIT_MUTATED:-0}" = 1 ]; then
    restore_reachd_units || rollback_incomplete "reachd unit / resource drop-in"
  fi
  if [ "$ROLLBACK_ARMED" = 1 ]; then
    echo "[deploy] ERROR: deployment failed; rolling back reachd (binary + database)" >&2
    failed_db_dir=""
    # Nothing below may touch the database or the binary unless reachd is
    # provably stopped (astra R2-03).
    if ! stop_reachd_verified; then
      report_incomplete
      echo "[deploy] ERROR: could not prove reachd stopped; NOT touching the database or binary" >&2
      echo "[deploy] ERROR: manual action required: stop reachd, then decide between the new binary and snapshot $DB_BACKUP_PATH + $REACHD_ROLLBACK" >&2
      exit "$exit_code"
    fi
    db_restore_ok=1
    if [ "$DB_MAY_HAVE_MIGRATED" = 1 ]; then
      failed_db_dir="$(dirname "$DB_PATH")/backups/failed.${DEPLOY_TS}"
      # The old binary cannot read what the new one may have written
      # (migrated sha256 agent tokens, schema). Restore the pre-deploy
      # snapshot only when (1) the failed set is fully preserved, and (2)
      # the durable action journal shows no durable action or open
      # OS intent since the fence (daybreak P1-A). The check runs with the
      # private probe copy of the new binary, which knows the journal.
      # One process, holding the writers lock exclusively, does the
      # forensic copy, the fence check and the restore: no CLI writer can
      # commit between the check and the restore (astra R4-01).
      if [ -z "$DEPLOY_FENCE" ] || [ -z "$PROBE_BIN" ] || ! sudo test -x "$PROBE_BIN"; then
        echo "[deploy] ERROR: no deploy fence or fence checker; NOT restoring the database automatically" >&2
        db_restore_ok=0
      elif ! sudo "$PROBE_BIN" deploy-restore --config /etc/reach/config.yaml --fence "$DEPLOY_FENCE" --snapshot "$DB_BACKUP_PATH" --forensic "$failed_db_dir" $(handoff_args) >&2; then
        echo "[deploy] ERROR: database NOT restored automatically, or the previous binary could not be put back (durable actions after the fence, a writer holds the lock, or a restore/handoff step failed)" >&2
        db_restore_ok=0
      else
        binary_handed_off=1
      fi
    fi
    if [ "$db_restore_ok" != 1 ]; then
      report_incomplete
      echo "[deploy] ERROR: leaving reachd stopped; operator decision required" >&2
      echo "[deploy] ERROR: on disk now: new reachd at /opt/reach/reachd, live DB as the new binary left it (unless the restore step above failed part-way)" >&2
      echo "[deploy] ERROR: predeploy snapshot: $DB_BACKUP_PATH; failed set: ${failed_db_dir:-none}; old binary: $REACHD_ROLLBACK" >&2
      echo "[deploy] ERROR: options: start the new binary as is, or restore the snapshot (losing the actions listed above) and the old binary" >&2
      exit "$exit_code"
    fi
    # The previous binary goes back to /opt/reach/reachd while the writers
    # lock is still held (inside deploy-restore, or deploy-handoff when the
    # DB was not touched): nobody can start the new binary on the restored
    # database in between (astra R5-01). The service is started only after
    # the lock is released, so a previous reachd that takes the lock shared
    # does not lock itself out.
    if [ "${binary_handed_off:-0}" != 1 ]; then
      if [ -z "$PROBE_BIN" ] || ! sudo test -x "$PROBE_BIN" || ! sudo "$PROBE_BIN" deploy-handoff --config /etc/reach/config.yaml $(handoff_args) >&2; then
        rollback_incomplete "previous reachd binary not put back"
        report_incomplete
        echo "[deploy] ERROR: leaving reachd stopped; the binary at /opt/reach/reachd may still be the new one" >&2
        exit "$exit_code"
      fi
    fi
    rollback_ready=0
    if [ "$REACHD_EXISTED" = 1 ]; then
      if sudo cmp -s "$REACHD_ROLLBACK" /opt/reach/reachd && sudo systemctl start reachd; then
        for attempt in $(seq 1 30); do
          # TCP plus an authenticated admin request against the restored DB,
          # token minted by the probe copy (the old binary cannot).
          if reachd_ready; then
            rollback_ready=1
            break
          fi
          sleep 1
        done
      else
        echo "[deploy] ERROR: could not put the previous reachd binary back" >&2
      fi
    else
      echo "[deploy] partial fresh reachd installation moved away" >&2
      if sudo test -e /opt/reach/reachd; then rollback_incomplete "new reachd still at /opt/reach/reachd"; else rollback_ready=1; fi
    fi
    if [ "$rollback_ready" != 1 ]; then
      sudo systemctl stop reachd
      report_incomplete
      echo "[deploy] ERROR: previous reachd did not become ready; leaving reachd stopped" >&2
      echo "[deploy] ERROR: predeploy database backup: $DB_BACKUP_PATH" >&2
      if [ -n "$failed_db_dir" ]; then
        echo "[deploy] ERROR: failed database forensic set: $failed_db_dir" >&2
      fi
      echo "[deploy] ERROR: operator approval is required" >&2
      sudo journalctl -u reachd -n 80 --no-pager >&2 || true
      exit "$exit_code"
    fi
    rollback_finish "previous reachd restored and ready" "$exit_code"
  elif [ "$SERVICE_STOPPED" = 1 ]; then
    if [ "$REACHD_EXISTED" = 1 ]; then
      echo "[deploy] ERROR: deployment failed while reachd was stopped; restarting previous service" >&2
      local ok=0
      if sudo systemctl start reachd; then
        for attempt in $(seq 1 30); do
          if systemctl is-active --quiet reachd && "$UV_BIN" run --script scripts/check-local-service.py "$LISTEN_ADDR" >/dev/null 2>&1; then
            ok=1
            break
          fi
          sleep 1
        done
      fi
      [ "$ok" = 1 ] || rollback_incomplete "previous reachd did not come back after restart"
      rollback_finish "previous reachd running again" "$exit_code"
    else
      sudo systemctl stop reachd 2>/dev/null || true
      if systemctl is-active --quiet reachd; then rollback_incomplete "partial fresh reachd still active"; fi
    fi
  fi
  rollback_finish "" "$exit_code"
}
# END reachd transaction steps

rollback_signal() {
  local signal="$1"
  if [ "$MUTATION_STARTED" != 1 ] || [ "$DEPLOY_COMMITTED" = 1 ]; then
    echo "[deploy] ERROR: received $signal outside deployment transaction" >&2
    case "$signal" in HUP) exit 129 ;; INT) exit 130 ;; TERM) exit 143 ;; esac
  fi
  echo "[deploy] ERROR: received $signal; rolling back" >&2
  case "$signal" in
    HUP) ROLLBACK_EXIT_CODE=129 ;;
    INT) ROLLBACK_EXIT_CODE=130 ;;
    TERM) ROLLBACK_EXIT_CODE=143 ;;
  esac
  rollback_reachd
}

trap cleanup EXIT
trap rollback_reachd ERR
trap 'rollback_signal HUP' HUP
trap 'rollback_signal INT' INT
trap 'rollback_signal TERM' TERM

echo "[deploy] agent version: ${AGENT_VERSION}"

case "$PUBLISH_AGENT_RELEASE" in
  0|1) ;;
  *) echo "[deploy] ERROR: REACH_PUBLISH_AGENT_RELEASE must be 0 or 1" >&2; exit 1 ;;
esac
case "$BACKUP_RETAIN" in
  ''|*[!0-9]*|0) echo "[deploy] ERROR: REACH_BACKUP_RETAIN must be a positive integer" >&2; exit 1 ;;
esac

# The published release's manifest, so a new release must carry a strictly
# greater anti-rollback sequence (astra P2-04).
PREVIOUS_MANIFEST=""
if [ "$PUBLISH_AGENT_RELEASE" = 1 ] && [ -r "$AGENT_DOWNLOAD_ROOT/latest.txt" ]; then
  previous_version="$(tr -d '\r\n' < "$AGENT_DOWNLOAD_ROOT/latest.txt")"
  if [ -n "$previous_version" ] && [ "$previous_version" != "$AGENT_VERSION" ] && [ -r "$AGENT_DOWNLOAD_ROOT/v$previous_version/manifest.json" ]; then
    PREVIOUS_MANIFEST="$AGENT_DOWNLOAD_ROOT/v$previous_version/manifest.json"
  fi
fi

if [ "$PUBLISH_AGENT_RELEASE" = 1 ] && [ -z "$ARTIFACT_DIR" ]; then
  if [ -z "${REACH_RELEASE_KEY:-}" ]; then
    echo "[deploy] ERROR: publishing hub-built artifacts requires REACH_RELEASE_KEY" >&2
    exit 1
  fi
  BUILD_ARTIFACT_DIR="$(mktemp -d)"
  ARTIFACT_DIR="$BUILD_ARTIFACT_DIR"

  build_agent "$ARTIFACT_DIR/reach-agent_linux_amd64" linux amd64
  build_agent "$ARTIFACT_DIR/reach-agent_linux_arm64" linux arm64
  build_agent "$ARTIFACT_DIR/reach-agent_linux_386" linux 386
  build_agent "$ARTIFACT_DIR/reach-agent_linux_armv6" linux arm 6
  build_agent "$ARTIFACT_DIR/reach-agent_linux_armv7" linux arm 7
  build_agent "$ARTIFACT_DIR/reach-agent_darwin_amd64" darwin amd64
  build_agent "$ARTIFACT_DIR/reach-agent_darwin_arm64" darwin arm64
  build_agent "$ARTIFACT_DIR/reach-agent_windows_amd64.exe" windows amd64
  build_agent "$ARTIFACT_DIR/reach-agent_windows_arm64.exe" windows arm64
  write_checksums "$ARTIFACT_DIR"
  create_manifest "$ARTIFACT_DIR"
  "$UV_BIN" run --script scripts/verify-release-artifacts.py \
    --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$deploy_commit" \
    ${PREVIOUS_MANIFEST:+--previous-manifest "$PREVIOUS_MANIFEST"}
  if sign_manifest_if_configured "$ARTIFACT_DIR"; then
    echo "[deploy] signed release manifest"
  else
    echo "[deploy] ERROR: failed to sign release manifest" >&2
    exit 1
  fi
  go run ./cmd/reach-release verify --manifest "$ARTIFACT_DIR/manifest.json" --artifacts-dir "$ARTIFACT_DIR"
elif [ "$PUBLISH_AGENT_RELEASE" = 1 ]; then
  require_agent_artifacts "$ARTIFACT_DIR"
  "$UV_BIN" run --script scripts/verify-release-artifacts.py \
    --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$deploy_commit" \
    ${PREVIOUS_MANIFEST:+--previous-manifest "$PREVIOUS_MANIFEST"}
  go run ./cmd/reach-release verify --manifest "$ARTIFACT_DIR/manifest.json" --artifacts-dir "$ARTIFACT_DIR"
fi

if [ "$PUBLISH_AGENT_RELEASE" = 1 ] && [ -e "$AGENT_DOWNLOAD_DIR" ] && [ "${REACH_OVERWRITE_VERSION:-0}" != 1 ]; then
  echo "[deploy] ERROR: $AGENT_DOWNLOAD_DIR already exists; refusing to overwrite immutable release" >&2
  echo "[deploy] Set REACH_OVERWRITE_VERSION=1 only for emergency rebuilds of the same version." >&2
  exit 1
fi

echo "[deploy] building reachd (linux/amd64 static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/reachd-build .

echo "[deploy] building reach-ws-carrier (linux/amd64 static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/reach-ws-carrier ./cmd/reach-ws-carrier

DEPLOY_TS="$(date -u +%Y%m%dT%H%M%SZ)"
DASHBOARD_STAGE="/opt/.reach-dashboard.staged.${DEPLOY_TS}.$$"
DASHBOARD_BACKUP="/opt/reach-dashboard.backup.${DEPLOY_TS}"

command -v sqlite3 >/dev/null || { echo "[deploy] ERROR: sqlite3 is required for safe backups" >&2; exit 1; }
command -v fuser >/dev/null || { echo "[deploy] ERROR: fuser (apt install psmisc) is required to prove nobody holds the database" >&2; exit 1; }
if [ -r /etc/reach/config.yaml ]; then
  DB_PATH="$("$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml db_path)"
  LISTEN_ADDR="$("$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml listen_addr)"
else
  DB_PATH="$(sudo "$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml db_path)"
  LISTEN_ADDR="$(sudo "$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml listen_addr)"
fi
case "$DB_PATH" in
  *\?*) echo "[deploy] ERROR: db_path must not contain ?: $DB_PATH" >&2; exit 1 ;;
  /*) ;;
  *) echo "[deploy] ERROR: db_path must be absolute: $DB_PATH" >&2; exit 1 ;;
esac
case "$LISTEN_ADDR" in
  127.0.0.1:*|localhost:*) ;;
  *) echo "[deploy] ERROR: refusing listen_addr not accepted by Reach: $LISTEN_ADDR" >&2; exit 1 ;;
esac
# Stage 4 precondition: no enrollment in flight (daybreak round 5).
# Read-only; runs before anything is changed.
if sudo test -f "$DB_PATH"; then
  bash scripts/preflight-provisioning-freeze.sh "$DB_PATH"
fi

export FNM_PATH="$HOME/.local/share/fnm"
if [ -d "$FNM_PATH" ]; then
  export PATH="$FNM_PATH:$PATH"
  eval "$(fnm env --shell bash)"
  fnm use 22 --silent-if-unchanged
fi
if [ "$(node --version | cut -d. -f1)" != "v22" ]; then
  echo "[deploy] ERROR: dashboard build requires Node.js 22" >&2
  exit 1
fi

echo "[deploy] building and staging dashboard..."
(
  cd dashboard
  npm ci
  npm run typecheck
  npm test
  npm run generate
  test -f .output/public/index.html
)
sudo install -d -m 0755 "$DASHBOARD_STAGE"
sudo cp -a dashboard/.output/public/. "$DASHBOARD_STAGE/"

echo "[deploy] staging auxiliary binary..."
MUTATION_STARTED=1
CARRIER_ROLLBACK="/opt/reach/reach-ws-carrier.rollback.${DEPLOY_TS}"
CARRIER_UNIT_BACKUP="/var/lib/reach/deploy-backups/reach-ws-carrier.service.${DEPLOY_TS}"
sudo install -d -m 0700 /var/lib/reach/deploy-backups
if sudo test -f /opt/reach/reach-ws-carrier; then
  CARRIER_EXISTED=1
  sudo cp -a /opt/reach/reach-ws-carrier "$CARRIER_ROLLBACK"
fi
if sudo test -f /etc/systemd/system/reach-ws-carrier.service; then
  CARRIER_UNIT_EXISTED=1
  sudo cp -a /etc/systemd/system/reach-ws-carrier.service "$CARRIER_UNIT_BACKUP"
fi
CARRIER_ENV_BACKUP="/var/lib/reach/deploy-backups/ws-carrier.env.${DEPLOY_TS}"
if sudo test -f /etc/reach/ws-carrier.env; then
  CARRIER_ENV_EXISTED=1
  sudo cp -a /etc/reach/ws-carrier.env "$CARRIER_ENV_BACKUP"
fi
# Every backup the carrier rollback needs must exist and match before the
# deploy changes anything of the carrier (astra R3-04).
[ "$CARRIER_EXISTED" != 1 ] || sudo cmp -s /opt/reach/reach-ws-carrier "$CARRIER_ROLLBACK"
[ "$CARRIER_UNIT_EXISTED" != 1 ] || sudo cmp -s /etc/systemd/system/reach-ws-carrier.service "$CARRIER_UNIT_BACKUP"
[ "$CARRIER_ENV_EXISTED" != 1 ] || sudo cmp -s /etc/reach/ws-carrier.env "$CARRIER_ENV_BACKUP"
if systemctl is-enabled --quiet reach-ws-carrier.service 2>/dev/null; then CARRIER_WAS_ENABLED=1; fi
if systemctl is-active --quiet reach-ws-carrier.service 2>/dev/null; then CARRIER_WAS_ACTIVE=1; fi
if systemctl is-enabled --quiet reach-wstunnel.service 2>/dev/null; then LEGACY_WAS_ENABLED=1; fi
if systemctl is-active --quiet reach-wstunnel.service 2>/dev/null; then LEGACY_WAS_ACTIVE=1; fi
CARRIER_STATE_SNAPSHOTTED=1
CARRIER_ROLLBACK_ARMED=1
sudo install -m 0755 /tmp/reach-ws-carrier /opt/reach/reach-ws-carrier.candidate
sudo mv -f /opt/reach/reach-ws-carrier.candidate /opt/reach/reach-ws-carrier
rm -f /tmp/reach-ws-carrier

if [ "$PUBLISH_AGENT_RELEASE" != 1 ]; then
  echo "[deploy] skipping reach-agent release (set REACH_PUBLISH_AGENT_RELEASE=1 to publish one)"
fi

echo "[deploy] ensuring Go WebSocket carrier service..."
if ! id reach-wstunnel >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin reach-wstunnel
fi
# The carrier fails closed without its path (daybreak P2-11): derive it from
# websocket_tunnel.path_prefix in config.yaml. If the WebSocket fallback is
# not configured, the carrier is not started.
WS_PREFIX="$(sudo "$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml websocket_tunnel.path_prefix || true)"
CARRIER_ENABLED=1
if [ -z "$WS_PREFIX" ]; then
  echo "[deploy] websocket_tunnel.path_prefix not set; WebSocket carrier will be disabled"
  CARRIER_ENABLED=0
else
  case "$WS_PREFIX" in *[!A-Za-z0-9/_.-]*) echo "[deploy] ERROR: unexpected characters in websocket_tunnel.path_prefix" >&2; false ;; esac
  # Same normalisation as the agent (carrierURL): "/" + trim("/") + "/".
  ws_path="/${WS_PREFIX#/}"
  ws_path="${ws_path%/}/"
  printf 'REACH_WS_PATH_PREFIX=%s\n' "$ws_path" | sudo install -m 0640 -o root -g reach-wstunnel /dev/stdin /etc/reach/ws-carrier.env
fi
sudo tee /etc/systemd/system/reach-ws-carrier.service >/dev/null <<'EOF'
[Unit]
Description=Reach Go WebSocket SSH carrier
After=network-online.target ssh.service sshd.service
Wants=network-online.target

[Service]
Type=simple
User=reach-wstunnel
Group=reach-wstunnel
# REACH_WS_PATH_PREFIX=/ws/tunnel/<secret>/ (written by deploy from
# websocket_tunnel.path_prefix; kept out of argv and git). The carrier
# refuses to start without it and accepts only that exact path.
EnvironmentFile=/etc/reach/ws-carrier.env
ExecStart=/opt/reach/reach-ws-carrier server --listen 127.0.0.1:9401 --target 127.0.0.1:22
Restart=always
RestartSec=5
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
MemoryMax=256M
TasksMax=128
LimitNOFILE=1024

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl disable --now reach-wstunnel.service 2>/dev/null || true
if [ "$CARRIER_ENABLED" = 1 ]; then
  sudo systemctl enable --now reach-ws-carrier.service
else
  sudo systemctl disable --now reach-ws-carrier.service 2>/dev/null || true
fi

echo "[deploy] swapping dashboard into place..."
if sudo test -d /opt/reach-dashboard; then
  DASHBOARD_EXISTED=1
  DASHBOARD_SWAP_ARMED=1
  DASHBOARD_MUTATED=1
  sudo "$UV_BIN" run --script scripts/atomic-directory-swap.py "$DASHBOARD_STAGE" /opt/reach-dashboard
  sudo mv "$DASHBOARD_STAGE" "$DASHBOARD_BACKUP"
  DASHBOARD_STAGE=""
else
  DASHBOARD_SWAP_ARMED=1
  DASHBOARD_MUTATED=1
  sudo "$UV_BIN" run --script scripts/atomic-directory-swap.py "$DASHBOARD_STAGE" /opt/reach-dashboard
  DASHBOARD_STAGE=""
fi

# Optional sandboxed reachd unit (deploy/systemd/reachd.service). Off by default:
# it must be tested once with a real provision/disable/retire cycle while the
# hub can be reached without Reach. REACH_INSTALL_REACHD_UNIT=1 enables it.
# Resource-limit drop-in: installed by default on every deploy (daybreak P1-6).
# It bounds memory/tasks/fds; it is not privilege containment.
LIMITS_DROPIN=/etc/systemd/system/reachd.service.d/10-reach-limits.conf
LIMITS_BACKUP=""
LIMITS_MUTATED=0
if ! sudo cmp -s deploy/systemd/reachd.service.d/10-reach-limits.conf "$LIMITS_DROPIN" 2>/dev/null; then
  echo "[deploy] installing reachd resource-limit drop-in..."
  if sudo test -f "$LIMITS_DROPIN"; then
    LIMITS_BACKUP="/var/lib/reach/deploy-backups/10-reach-limits.conf.${DEPLOY_TS}"
    sudo cp -a "$LIMITS_DROPIN" "$LIMITS_BACKUP"
  fi
  LIMITS_MUTATED=1
  sudo install -d -m 0755 /etc/systemd/system/reachd.service.d
  sudo install -m 0644 deploy/systemd/reachd.service.d/10-reach-limits.conf "$LIMITS_DROPIN"
  sudo systemctl daemon-reload
fi

REACHD_UNIT_BACKUP=""
REACHD_UNIT_MUTATED=0
if [ "${REACH_INSTALL_REACHD_UNIT:-0}" = 1 ]; then
  echo "[deploy] installing sandboxed reachd.service from deploy/systemd..."
  if sudo test -f /etc/systemd/system/reachd.service; then
    REACHD_UNIT_BACKUP="/var/lib/reach/deploy-backups/reachd.service.${DEPLOY_TS}"
    sudo cp -a /etc/systemd/system/reachd.service "$REACHD_UNIT_BACKUP"
  fi
  REACHD_UNIT_MUTATED=1
  if command -v systemd-analyze >/dev/null; then
    systemd-analyze verify deploy/systemd/reachd.service
  fi
  sudo install -m 0644 deploy/systemd/reachd.service /etc/systemd/system/reachd.service
  sudo systemctl daemon-reload
fi

echo "[deploy] stopping reachd for a consistent database backup..."
if sudo test -f /opt/reach/reachd; then
  REACHD_EXISTED=1
fi
SERVICE_STOPPED=1
# The snapshot and the fence baseline are both taken with reachd verifiably
# stopped, so no reachd write can land between them (daybreak P1-A).
if ! stop_reachd_verified; then
  echo "[deploy] ERROR: reachd did not stop cleanly" >&2
  false
fi
DB_BACKUP_DIR="$(dirname "$DB_PATH")/backups"
DB_BACKUP_PATH="${DB_BACKUP_DIR}/$(basename "$DB_PATH").${DEPLOY_TS}"
sudo install -d -m 0700 "$DB_BACKUP_DIR"
if [ "$(sudo sqlite3 "$DB_PATH" 'PRAGMA quick_check;')" != "ok" ]; then
  echo "[deploy] ERROR: live database failed PRAGMA quick_check" >&2
  false
fi
# Fence first, then snapshot, both under the exclusive writers lock that
# every reachd/CLI writer of this version holds shared (daybreak round 4
# P1-02, astra R3-03). A write slipping in between would be in the snapshot
# AND above the fence, so a rollback would refuse to restore: never lost.
DEPLOY_FENCE="${DB_BACKUP_DIR}/deploy-fence.${DEPLOY_TS}.json"
sudo cp -a /etc/reach/config.yaml "/var/lib/reach/deploy-backups/config.yaml.${DEPLOY_TS}"
# One locked process: clear retired tokens, fence, snapshot, and rotate
# jwt_secret (always), all under the exclusive writers lock (astra R4-01,
# daybreak round 5 N6, daybreak round 6 R6-P1-03).
sudo /tmp/reachd-build deploy-prepare --config /etc/reach/config.yaml --fence "$DEPLOY_FENCE" --snapshot "$DB_BACKUP_PATH"
if [ "$(sudo sqlite3 "$DB_BACKUP_PATH" 'PRAGMA quick_check;')" != "ok" ]; then
  echo "[deploy] ERROR: backup database failed PRAGMA quick_check" >&2
  false
fi
echo "[deploy] database backup: $DB_BACKUP_PATH"

# Prove the rollback path on a private copy of the snapshot before the new
# binary touches the live DB (astra P1-01): old binary on the restored copy
# must accept a legacy Argon2 agent token heartbeat. Skipped on fresh installs.
if [ "$REACHD_EXISTED" = 1 ] && [ "${REACH_SKIP_ROLLBACK_REHEARSAL:-0}" != 1 ]; then
  echo "[deploy] rehearsing rollback on a private copy of the snapshot..."
  sudo bash scripts/rehearse-rollback.sh /opt/reach/reachd /tmp/reachd-build "$DB_BACKUP_PATH" /etc/reach/config.yaml
fi

install_new_reachd

# jwt_secret was rotated by deploy-prepare, unconditionally (daybreak
# round 6 R6-P1-03): config.yaml is not part of the rollback, so pre-upgrade
# JWTs stay invalid even after a snapshot restore (daybreak round 4 P1-03).

DB_MAY_HAVE_MIGRATED=1
sudo systemctl start reachd
SERVICE_STOPPED=0
for attempt in $(seq 1 30); do
  if reachd_ready; then
    break
  fi
  if [ "$attempt" -eq 30 ]; then
    echo "[deploy] ERROR: reachd did not become healthy at $LISTEN_ADDR" >&2
    sudo journalctl -u reachd -n 80 --no-pager >&2 || true
    false
  fi
  sleep 1
done
echo "[deploy] running reachd database readiness check..."
sudo /opt/reach/reachd db-check --config /etc/reach/config.yaml

if [ "$CARRIER_ENABLED" = 1 ]; then
  echo "[deploy] restarting WebSocket carrier..."
  sudo systemctl restart reach-ws-carrier.service
  carrier_ok=0
  for attempt in $(seq 1 10); do
    if systemctl is-active --quiet reach-ws-carrier.service \
      && sudo bash scripts/check-carrier-ready.sh /etc/reach/ws-carrier.env; then
      carrier_ok=1
      break
    fi
    sleep 1
  done
  if [ "$carrier_ok" != 1 ]; then
    echo "[deploy] ERROR: carrier is not active or does not answer a WebSocket upgrade on its path" >&2
    false
  fi
fi

if [ "$PUBLISH_AGENT_RELEASE" = 1 ]; then
  echo "[deploy] staging reach-agent v${AGENT_VERSION} publication..."
  sudo install -d -m 0755 "$AGENT_DOWNLOAD_ROOT"
  if sudo test -e "$AGENT_STAGING_DIR"; then
    sudo mv "$AGENT_STAGING_DIR" "$AGENT_STAGING_DIR.abandoned.$DEPLOY_TS"
  fi
  sudo install -d -m 0755 "$AGENT_STAGING_DIR"
  for artifact in \
    reach-agent_linux_amd64 reach-agent_linux_arm64 reach-agent_linux_386 \
    reach-agent_linux_armv6 reach-agent_linux_armv7 reach-agent_darwin_amd64 \
    reach-agent_darwin_arm64 reach-agent_windows_amd64.exe reach-agent_windows_arm64.exe; do
    sudo install -m 0755 "$ARTIFACT_DIR/$artifact" "$AGENT_STAGING_DIR/$artifact"
  done
  sudo install -m 0644 "$ARTIFACT_DIR/checksums.txt" "$AGENT_STAGING_DIR/checksums.txt"
  sudo install -m 0644 "$ARTIFACT_DIR/manifest.json" "$AGENT_STAGING_DIR/manifest.json"
  sudo install -m 0644 "$ARTIFACT_DIR/manifest.json.minisig" "$AGENT_STAGING_DIR/manifest.json.minisig"

  CONFIG_API_URL="${REACH_API_URL:-}"
  if [ -z "$CONFIG_API_URL" ]; then
    if [ -r /etc/reach/config.yaml ]; then
      CONFIG_API_URL="$("$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml default_hub.api_url)"
    else
      CONFIG_API_URL="$(sudo "$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml default_hub.api_url)"
    fi
  fi
  if [ -z "$CONFIG_API_URL" ] || [ "$CONFIG_API_URL" = "https://tunnels.your-domain.example" ]; then
    echo "[deploy] ERROR: could not determine real Reach API URL" >&2
    false
  fi
  case "$CONFIG_API_URL" in
    http://*|https://*) ;;
    *) echo "[deploy] ERROR: invalid Reach API URL: $CONFIG_API_URL" >&2; false ;;
  esac
  CONFIG_API_URL_SED="$(printf '%s' "$CONFIG_API_URL" | sed 's/[&|\\]/\\&/g')"
  AGENT_VERSION_SED="$(printf '%s' "$AGENT_VERSION" | sed 's/[&|\\]/\\&/g')"
  # Pin the agent hashes from the signature-verified manifest into setup.sh
  # for targets without minisign/OpenSSL 3 (daybreak P1-5).
  PINNED_SHA256S="$(go run ./cmd/reach-release pins --manifest "$ARTIFACT_DIR/manifest.json")"
  case "$PINNED_SHA256S" in *[!A-Za-z0-9_.=\ -]*|'') echo "[deploy] ERROR: invalid pinned hashes" >&2; false ;; esac
  sed \
    -e "s|^REACH_AGENT_VERSION=.*|REACH_AGENT_VERSION=\"\${REACH_AGENT_VERSION:-${AGENT_VERSION_SED}}\"|" \
    -e "s|^API_URL=.*|API_URL=\"\${REACH_API_URL:-${CONFIG_API_URL_SED}}\"|" \
    -e "s|^REACH_PINNED_VERSION=.*|REACH_PINNED_VERSION=\"${AGENT_VERSION_SED}\"|" \
    -e "s|^REACH_PINNED_SHA256S=.*|REACH_PINNED_SHA256S=\"${PINNED_SHA256S}\"|" \
    -e "s|https://tunnels.your-domain.example|${CONFIG_API_URL_SED}|g" \
    setup.sh > /tmp/reach-setup.sh
  sed \
    -e "s|\"https://tunnels.your-domain.example\"|\"${CONFIG_API_URL_SED}\"|g" \
    -e "s|\"0.1.0-alpha\"|\"${AGENT_VERSION_SED}\"|g" \
    -e "s|^\\\$ReachPinnedVersion = .*|\\\$ReachPinnedVersion = '${AGENT_VERSION_SED}'|" \
    -e "s|^\\\$ReachPinnedSha256s = .*|\\\$ReachPinnedSha256s = '${PINNED_SHA256S}'|" \
    setup.ps1 > /tmp/reach-setup.ps1
  grep -Fq "$CONFIG_API_URL" /tmp/reach-setup.sh
  grep -Fq "$CONFIG_API_URL" /tmp/reach-setup.ps1
  grep -Fq "$AGENT_VERSION" /tmp/reach-setup.sh
  grep -Fq "$AGENT_VERSION" /tmp/reach-setup.ps1
  grep -Fq "REACH_PINNED_SHA256S=\"reach-agent_" /tmp/reach-setup.sh
  grep -Fq "\$ReachPinnedSha256s = 'reach-agent_" /tmp/reach-setup.ps1
  printf '%s\n' "$AGENT_VERSION" > /tmp/reach-latest-version
  printf '{"version":"%s","api_url":"%s","created_at":"%s"}\n' \
    "$AGENT_VERSION" "$CONFIG_API_URL" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > /tmp/reach-latest.json

  PUBLICATION_BACKUP_DIR="/var/lib/reach/deploy-backups/agent-publication.${DEPLOY_TS}"
  sudo install -d -m 0700 "$PUBLICATION_BACKUP_DIR"
  for source in \
    /opt/reach/reach-agent /var/lib/reach/setup.sh /var/lib/reach/setup.ps1 \
    "$AGENT_DOWNLOAD_ROOT/latest.txt" "$AGENT_DOWNLOAD_ROOT/latest.json"; do
    if sudo test -f "$source"; then
      case "$source" in
        /opt/reach/reach-agent) AGENT_BIN_EXISTED=1 ;;
        /var/lib/reach/setup.sh) SETUP_SH_EXISTED=1 ;;
        /var/lib/reach/setup.ps1) SETUP_PS1_EXISTED=1 ;;
        "$AGENT_DOWNLOAD_ROOT/latest.txt") LATEST_TXT_EXISTED=1 ;;
        "$AGENT_DOWNLOAD_ROOT/latest.json") LATEST_JSON_EXISTED=1 ;;
      esac
      sudo cp -a "$source" "$PUBLICATION_BACKUP_DIR/$(basename "$source")"
    fi
  done
  AGENT_BIN_BACKUP="$PUBLICATION_BACKUP_DIR/reach-agent"
  SETUP_SH_BACKUP="$PUBLICATION_BACKUP_DIR/setup.sh"
  SETUP_PS1_BACKUP="$PUBLICATION_BACKUP_DIR/setup.ps1"
  LATEST_TXT_BACKUP="$PUBLICATION_BACKUP_DIR/latest.txt"
  LATEST_JSON_BACKUP="$PUBLICATION_BACKUP_DIR/latest.json"

  sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_amd64" /opt/reach/reach-agent.candidate
  sudo install -m 0644 /tmp/reach-setup.sh /var/lib/reach/setup.sh.candidate
  sudo install -m 0644 /tmp/reach-setup.ps1 /var/lib/reach/setup.ps1.candidate
  sudo install -m 0644 /tmp/reach-latest-version "$AGENT_DOWNLOAD_ROOT/latest.txt.candidate"
  sudo install -m 0644 /tmp/reach-latest.json "$AGENT_DOWNLOAD_ROOT/latest.json.candidate"

  if sudo test -e "$AGENT_DOWNLOAD_DIR"; then
    AGENT_VERSION_EXISTED=1
    AGENT_VERSION_BACKUP="$AGENT_DOWNLOAD_DIR.replaced.$DEPLOY_TS"
  fi
  PUBLICATION_ARMED=1
  if [ "$AGENT_VERSION_EXISTED" = 1 ]; then
    sudo mv "$AGENT_DOWNLOAD_DIR" "$AGENT_VERSION_BACKUP"
  fi
  sudo mv "$AGENT_STAGING_DIR" "$AGENT_DOWNLOAD_DIR"
  sudo mv -f /opt/reach/reach-agent.candidate /opt/reach/reach-agent
  sudo mv -f /var/lib/reach/setup.sh.candidate /var/lib/reach/setup.sh
  sudo mv -f /var/lib/reach/setup.ps1.candidate /var/lib/reach/setup.ps1
  sudo mv -f "$AGENT_DOWNLOAD_ROOT/latest.txt.candidate" "$AGENT_DOWNLOAD_ROOT/latest.txt"
  sudo mv -f "$AGENT_DOWNLOAD_ROOT/latest.json.candidate" "$AGENT_DOWNLOAD_ROOT/latest.json"
  rm -f /tmp/reach-latest-version /tmp/reach-latest.json /tmp/reach-setup.sh /tmp/reach-setup.ps1
fi

DEPLOY_COMMITTED=1
ROLLBACK_ARMED=0
CARRIER_STATE_SNAPSHOTTED=0
CARRIER_ROLLBACK_ARMED=0
DASHBOARD_SWAP_ARMED=0
PUBLICATION_ARMED=0

echo "[deploy] pruning deployment backups (retain $BACKUP_RETAIN)..."
if ! sudo "$UV_BIN" run --script scripts/prune-deploy-backups.py --retain "$BACKUP_RETAIN" \
  '/opt/reach-dashboard.backup.*' \
  '/opt/reach/reachd.rollback.*' \
  '/opt/reach/reach-ws-carrier.rollback.*' \
  '/var/lib/reach/deploy-backups/reach-ws-carrier.service.*' \
  '/var/lib/reach/deploy-backups/reachd.service.*' \
  '/var/lib/reach/deploy-backups/10-reach-limits.conf.*' \
  '/var/lib/reach/deploy-backups/ws-carrier.env.*' \
  '/var/lib/reach/deploy-backups/agent-publication.*' \
  "$AGENT_DOWNLOAD_ROOT/v*.replaced.*"; then
  echo "[deploy] WARNING: deployment succeeded, but backup pruning failed" >&2
fi
# The rollback set of one deploy (fence, snapshot, probe binary, config
# backup, forensic set) is pruned as a group (daybreak round 5 N9).
if ! sudo "$UV_BIN" run --script scripts/prune-deploy-backups.py --retain "$BACKUP_RETAIN" \
  --group '([0-9]{8}T[0-9]{6}Z)' \
  "$DB_BACKUP_DIR/deploy-fence.*.json" \
  "$DB_BACKUP_DIR/$(basename "$DB_PATH").*" \
  "$DB_BACKUP_DIR/failed.*" \
  '/var/lib/reach/deploy-backups/reachd-probe.*' \
  '/var/lib/reach/deploy-backups/config.yaml.*'; then
  echo "[deploy] WARNING: deployment succeeded, but rollback-set pruning failed" >&2
fi

if [ "$PUBLISH_AGENT_RELEASE" = 1 ]; then
  echo "[deploy] done. Published reach-agent v${AGENT_VERSION}."
else
  echo "[deploy] done. reachd and dashboard updated; reach-agent release unchanged."
fi
