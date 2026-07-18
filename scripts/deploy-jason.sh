#!/usr/bin/env bash
set -Eeuo pipefail

# Deploy Reach to a hub server from the repo checked out on that hub.
# Example: ssh <hub-alias> 'cd ~/reach && bash scripts/deploy-jason.sh'
# Or locally on the hub: cd ~/reach && bash scripts/deploy-jason.sh
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
LOCK_DIR="${XDG_RUNTIME_DIR:-$HOME/.cache/reach}"
mkdir -p "$LOCK_DIR"
if [ "${REACH_DEPLOY_LOCK_HELD:-0}" = 1 ]; then
  if ! flock -n 9; then
    echo "[deploy] ERROR: inherited deployment lock is not held" >&2
    exit 1
  fi
else
  exec 9>"$LOCK_DIR/deploy.lock"
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
  go run ./cmd/reach-release manifest --dir "$dir" --version "$AGENT_VERSION" --commit "$(git rev-parse HEAD)"
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
if [ -n "${REACH_DEPLOY_COMMIT:-}" ]; then
  echo "[deploy] checking out explicit commit ${REACH_DEPLOY_COMMIT}..."
  git fetch origin
  if ! git cat-file -e "${REACH_DEPLOY_COMMIT}^{commit}" 2>/dev/null; then
    echo "[deploy] ERROR: commit ${REACH_DEPLOY_COMMIT} is unavailable after fetching origin" >&2
    exit 1
  fi
  deploy_commit="$(git rev-parse "${REACH_DEPLOY_COMMIT}^{commit}")"
  if [ -z "$(git for-each-ref --format='%(refname)' --contains "$deploy_commit" refs/remotes/origin/)" ]; then
    echo "[deploy] ERROR: commit ${deploy_commit} is not reachable from an origin branch" >&2
    exit 1
  fi
  git checkout --detach "$deploy_commit"
  test "$(git rev-parse HEAD)" = "$deploy_commit"
else
  if ! current_branch="$(git symbolic-ref --quiet --short HEAD)"; then
    default_branch="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)"
    default_branch="${default_branch#origin/}"
    if [ -z "$default_branch" ]; then
      default_branch=main
    fi
    echo "[deploy] detached HEAD; switching to ${default_branch}..."
    if git show-ref --verify --quiet "refs/heads/$default_branch"; then
      git switch "$default_branch"
    else
      git switch --track -c "$default_branch" "origin/$default_branch"
    fi
  fi
  echo "[deploy] pulling latest..."
  git pull --ff-only
  deploy_commit="$(git rev-parse HEAD)"
fi

AGENT_VERSION="${REACH_AGENT_VERSION:-$(version_from_git)}"
bash scripts/validate-release-version.sh "$AGENT_VERSION" || {
  echo "[deploy] ERROR: invalid agent version" >&2
  exit 1
}
AGENT_LDFLAGS=("-X" "main.version=${AGENT_VERSION}")
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
REACHD_ROLLBACK=""
REACHD_EXISTED=0
CARRIER_ROLLBACK=""
CARRIER_EXISTED=0
CARRIER_UNIT_BACKUP=""
CARRIER_UNIT_EXISTED=0
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
    sudo systemctl disable --now reach-ws-carrier.service 2>/dev/null || true
    if [ "$CARRIER_UNIT_EXISTED" = 1 ] && sudo test -f "$CARRIER_UNIT_BACKUP"; then
      sudo cp -a "$CARRIER_UNIT_BACKUP" /etc/systemd/system/reach-ws-carrier.service.rollback-candidate
      sudo mv -f /etc/systemd/system/reach-ws-carrier.service.rollback-candidate /etc/systemd/system/reach-ws-carrier.service
    elif sudo test -e /etc/systemd/system/reach-ws-carrier.service; then
      sudo mv /etc/systemd/system/reach-ws-carrier.service "/etc/systemd/system/reach-ws-carrier.service.failed.$DEPLOY_TS"
    fi
    if [ "$CARRIER_EXISTED" = 1 ] && sudo test -f "$CARRIER_ROLLBACK"; then
      sudo install -m 0755 "$CARRIER_ROLLBACK" /opt/reach/reach-ws-carrier.rollback-candidate
      sudo mv -f /opt/reach/reach-ws-carrier.rollback-candidate /opt/reach/reach-ws-carrier
    elif sudo test -e /opt/reach/reach-ws-carrier; then
      sudo mv /opt/reach/reach-ws-carrier "/opt/reach/reach-ws-carrier.failed.$DEPLOY_TS"
    fi
    sudo systemctl daemon-reload
    if [ "$CARRIER_WAS_ENABLED" = 1 ]; then sudo systemctl enable reach-ws-carrier.service; else sudo systemctl disable reach-ws-carrier.service 2>/dev/null || true; fi
    if [ "$CARRIER_WAS_ACTIVE" = 1 ]; then sudo systemctl start reach-ws-carrier.service; else sudo systemctl stop reach-ws-carrier.service 2>/dev/null || true; fi
    if [ "$LEGACY_WAS_ENABLED" = 1 ]; then sudo systemctl enable reach-wstunnel.service; else sudo systemctl disable reach-wstunnel.service 2>/dev/null || true; fi
    if [ "$LEGACY_WAS_ACTIVE" = 1 ]; then sudo systemctl start reach-wstunnel.service; else sudo systemctl stop reach-wstunnel.service 2>/dev/null || true; fi
  fi
  if [ "$ROLLBACK_ARMED" = 1 ]; then
    echo "[deploy] ERROR: deployment failed; restoring previous reachd binary" >&2
    sudo systemctl stop reachd
    if [ "$DB_MAY_HAVE_MIGRATED" = 1 ]; then
      failed_db_dir="$(dirname "$DB_PATH")/backups/failed.${DEPLOY_TS}"
      echo "[deploy] preserving failed database files in $failed_db_dir" >&2
      sudo install -d -m 0700 "$failed_db_dir"
      for db_file in "$DB_PATH" "$DB_PATH-wal" "$DB_PATH-shm"; do
        if sudo test -e "$db_file"; then
          sudo cp -a "$db_file" "$failed_db_dir/"
        fi
      done
    fi
    if [ "$REACHD_EXISTED" = 1 ] && [ -n "$REACHD_ROLLBACK" ] && sudo test -f "$REACHD_ROLLBACK"; then
      sudo install -m 0755 "$REACHD_ROLLBACK" /opt/reach/reachd.rollback-candidate
      sudo mv -f /opt/reach/reachd.rollback-candidate /opt/reach/reachd
      sudo systemctl start reachd
    else
      echo "[deploy] removing partial fresh reachd installation" >&2
      sudo systemctl stop reachd 2>/dev/null || true
      if sudo test -e /opt/reach/reachd; then sudo mv /opt/reach/reachd "/opt/reach/reachd.failed.$DEPLOY_TS"; fi
      rollback_ready=1
    fi
    rollback_ready="${rollback_ready:-0}"
    if [ "$REACHD_EXISTED" = 1 ]; then
      for attempt in $(seq 1 30); do
        if systemctl is-active --quiet reachd \
          && "$UV_BIN" run --script scripts/check-local-service.py "$LISTEN_ADDR"; then
          rollback_ready=1
          break
        fi
        sleep 1
      done
    fi
    if [ "$rollback_ready" != 1 ]; then
      sudo systemctl stop reachd
      echo "[deploy] ERROR: previous reachd cannot become ready with the migrated database; leaving reachd stopped" >&2
      echo "[deploy] ERROR: predeploy database backup: $DB_BACKUP_PATH" >&2
      if [ "$DB_MAY_HAVE_MIGRATED" = 1 ]; then
        echo "[deploy] ERROR: failed database forensic set: $failed_db_dir" >&2
      fi
      echo "[deploy] ERROR: database was not restored automatically; operator approval is required" >&2
      sudo journalctl -u reachd -n 80 --no-pager >&2 || true
    fi
  elif [ "$SERVICE_STOPPED" = 1 ]; then
    if [ "$REACHD_EXISTED" = 1 ]; then
      echo "[deploy] ERROR: deployment failed while reachd was stopped; restarting previous service" >&2
      sudo systemctl start reachd
    else
      sudo systemctl stop reachd 2>/dev/null || true
    fi
  fi
  exit "$exit_code"
}

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
    --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$deploy_commit"
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
    --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$deploy_commit"
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
if [ -r /etc/reach/config.yaml ]; then
  DB_PATH="$("$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml db_path)"
  LISTEN_ADDR="$("$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml listen_addr)"
else
  DB_PATH="$(sudo "$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml db_path)"
  LISTEN_ADDR="$(sudo "$UV_BIN" run --script scripts/read-reach-config.py --config /etc/reach/config.yaml listen_addr)"
fi
case "$DB_PATH" in
  /*) ;;
  *) echo "[deploy] ERROR: db_path must be absolute: $DB_PATH" >&2; exit 1 ;;
esac
case "$LISTEN_ADDR" in
  127.0.0.1:*|localhost:*) ;;
  *) echo "[deploy] ERROR: refusing listen_addr not accepted by Reach: $LISTEN_ADDR" >&2; exit 1 ;;
esac

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
sudo tee /etc/systemd/system/reach-ws-carrier.service >/dev/null <<'EOF'
[Unit]
Description=Reach Go WebSocket SSH carrier
After=network-online.target ssh.service sshd.service
Wants=network-online.target

[Service]
Type=simple
User=reach-wstunnel
Group=reach-wstunnel
ExecStart=/opt/reach/reach-ws-carrier server --listen 127.0.0.1:9401 --target 127.0.0.1:22
Restart=always
RestartSec=5
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl disable --now reach-wstunnel.service 2>/dev/null || true
sudo systemctl enable --now reach-ws-carrier.service

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

echo "[deploy] stopping reachd for a consistent database backup..."
if sudo test -f /opt/reach/reachd; then
  REACHD_EXISTED=1
fi
SERVICE_STOPPED=1
sudo systemctl stop reachd
if systemctl is-active --quiet reachd; then
  echo "[deploy] ERROR: reachd did not stop cleanly" >&2
  false
fi
DB_BACKUP_DIR="$(dirname "$DB_PATH")/backups"
DB_BACKUP_PATH="${DB_BACKUP_DIR}/$(basename "$DB_PATH").${DEPLOY_TS}"
sudo install -d -m 0700 "$DB_BACKUP_DIR"
sudo sqlite3 "$DB_PATH" ".timeout 10000" ".backup '$DB_BACKUP_PATH'"
if [ "$(sudo sqlite3 "$DB_BACKUP_PATH" 'PRAGMA quick_check;')" != "ok" ]; then
  echo "[deploy] ERROR: backup database failed PRAGMA quick_check" >&2
  false
fi
if [ "$(sudo sqlite3 "$DB_PATH" 'PRAGMA quick_check;')" != "ok" ]; then
  echo "[deploy] ERROR: live database failed PRAGMA quick_check" >&2
  false
fi
echo "[deploy] database backup: $DB_BACKUP_PATH"

REACHD_ROLLBACK="/opt/reach/reachd.rollback.${DEPLOY_TS}"
ROLLBACK_ARMED=1
if [ "$REACHD_EXISTED" = 1 ]; then
  sudo cp -a /opt/reach/reachd "$REACHD_ROLLBACK"
fi
sudo install -m 0755 /tmp/reachd-build /opt/reach/reachd.candidate
sudo mv -f /opt/reach/reachd.candidate /opt/reach/reachd
rm -f /tmp/reachd-build

DB_MAY_HAVE_MIGRATED=1
sudo systemctl start reachd
SERVICE_STOPPED=0
for attempt in $(seq 1 30); do
  if systemctl is-active --quiet reachd && "$UV_BIN" run --script scripts/check-local-service.py "$LISTEN_ADDR"; then
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

echo "[deploy] restarting WebSocket carrier..."
sudo systemctl restart reach-ws-carrier.service
systemctl is-active reach-ws-carrier.service

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
  sed \
    -e "s|^REACH_AGENT_VERSION=.*|REACH_AGENT_VERSION=\"\${REACH_AGENT_VERSION:-${AGENT_VERSION_SED}}\"|" \
    -e "s|^API_URL=.*|API_URL=\"\${REACH_API_URL:-${CONFIG_API_URL_SED}}\"|" \
    -e "s|https://tunnels.your-domain.example|${CONFIG_API_URL_SED}|g" \
    setup.sh > /tmp/reach-setup.sh
  sed \
    -e "s|\"https://tunnels.your-domain.example\"|\"${CONFIG_API_URL_SED}\"|g" \
    -e "s|\"0.1.0-alpha\"|\"${AGENT_VERSION_SED}\"|g" \
    setup.ps1 > /tmp/reach-setup.ps1
  grep -Fq "$CONFIG_API_URL" /tmp/reach-setup.sh
  grep -Fq "$CONFIG_API_URL" /tmp/reach-setup.ps1
  grep -Fq "$AGENT_VERSION" /tmp/reach-setup.sh
  grep -Fq "$AGENT_VERSION" /tmp/reach-setup.ps1
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
  '/var/lib/reach/deploy-backups/agent-publication.*' \
  "$AGENT_DOWNLOAD_ROOT/v*.replaced.*" \
  "$DB_BACKUP_DIR/failed.*" \
  "$DB_BACKUP_DIR/$(basename "$DB_PATH").*"; then
  echo "[deploy] WARNING: deployment succeeded, but backup pruning failed" >&2
fi

if [ "$PUBLISH_AGENT_RELEASE" = 1 ]; then
  echo "[deploy] done. Published reach-agent v${AGENT_VERSION}."
else
  echo "[deploy] done. reachd and dashboard updated; reach-agent release unchanged."
fi
