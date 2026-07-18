#!/usr/bin/env bash
set -Eeuo pipefail

# Full deploy from a workstation to a configured hub.
# Set REACH_DEPLOY_HOST to the SSH alias for your hub.
# Builds/signs every supported agent artifact locally, uploads them, and runs
# deploy on the hub from its own git clone. The hub builds/swaps its dashboard.

cd "$(dirname "$0")/.."

SOURCE_ROOT="$PWD"
export CGO_ENABLED=0
DEPLOY_HOST="${REACH_DEPLOY_HOST:-reach-hub}"

require_clean_product_tree() {
  local status
  status="$(git -C "$SOURCE_ROOT" status --porcelain --untracked-files=normal)"
  status="$(printf '%s\n' "$status" | grep -v '^?? \.claude/$' || true)"
  if [ -n "$status" ]; then
    echo "[mac] ERROR: product tree has tracked or nonignored untracked changes" >&2
    printf '%s\n' "$status" >&2
    exit 1
  fi
}

require_clean_product_tree
DEPLOY_BRANCH="$(git symbolic-ref --quiet --short HEAD)" || {
  echo "[mac] ERROR: deploy-from-mac requires a branch, not detached HEAD" >&2
  exit 1
}
DEPLOY_COMMIT="$(git rev-parse HEAD)"
echo "[mac] fetching origin/${DEPLOY_BRANCH} and verifying ${DEPLOY_COMMIT} is pushed..."
git fetch origin "$DEPLOY_BRANCH"
if ! git rev-parse --verify --quiet "origin/$DEPLOY_BRANCH^{commit}" >/dev/null || \
   ! git merge-base --is-ancestor "$DEPLOY_COMMIT" "origin/$DEPLOY_BRANCH"; then
  echo "[mac] ERROR: HEAD ${DEPLOY_COMMIT} is not reachable from origin/${DEPLOY_BRANCH}" >&2
  exit 1
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
  echo "[mac] building reach-agent (${goos}/${arch}${arm:+ GOARM=$arm})..."
  if [ -n "$arm" ]; then
    GOOS="$goos" GOARCH="$arch" GOARM="$arm" CGO_ENABLED=0 go build -ldflags "${AGENT_LDFLAGS[*]}" -o "$out" ./cmd/reach-agent
  else
    GOOS="$goos" GOARCH="$arch" CGO_ENABLED=0 go build -ldflags "${AGENT_LDFLAGS[*]}" -o "$out" ./cmd/reach-agent
  fi
}

AGENT_VERSION="${REACH_AGENT_VERSION:-$(version_from_git)}"
bash scripts/validate-release-version.sh "$AGENT_VERSION" || {
  echo "[mac] ERROR: invalid agent version" >&2
  exit 1
}
AGENT_LDFLAGS=("-X" "main.version=${AGENT_VERSION}")
RELEASE_KEY="${REACH_RELEASE_KEY:-$HOME/.minisign/reach-release.key}"
RELEASE_PASSWORD_FILE="${REACH_RELEASE_KEY_PASSWORD_FILE:-}"
if [ -z "$RELEASE_PASSWORD_FILE" ] && [ -f "$HOME/.minisign/reach-release.pass" ]; then
  RELEASE_PASSWORD_FILE="$HOME/.minisign/reach-release.pass"
fi

[ -f "$RELEASE_KEY" ] || { echo "[mac] ERROR: release signing key not found at $RELEASE_KEY" >&2; exit 1; }

echo "[mac] agent version: $AGENT_VERSION"

RUN_ID="$(printf '%s' "$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM" | tr -cd 'A-Za-z0-9._-')"
BUILD_WORKTREE="$SOURCE_ROOT/.claude/worktrees/deploy-$RUN_ID"
git worktree add --detach "$BUILD_WORKTREE" "$DEPLOY_COMMIT" >/dev/null
cd "$BUILD_WORKTREE"
ARTIFACT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/reach-agent-artifacts.XXXXXX")"
AGENT_TAR="${ARTIFACT_DIR}.tar.gz"
REMOTE_ARTIFACT_DIR=""
REMOTE_AGENT_TAR=""
cleanup() {
  trash "$ARTIFACT_DIR" "$AGENT_TAR" 2>/dev/null || true
  git -C "$SOURCE_ROOT" worktree remove --force "$BUILD_WORKTREE" >/dev/null 2>&1 || true
  if [ -n "$REMOTE_ARTIFACT_DIR" ] && [ -n "$REMOTE_AGENT_TAR" ]; then
    ssh "$DEPLOY_HOST" "bash -s" -- "$REMOTE_ARTIFACT_DIR" "$REMOTE_AGENT_TAR" <<'REMOTE_CLEANUP' >/dev/null 2>&1 || true
set -u
artifact_dir="$1"
artifact_tar="$2"
case "$artifact_dir" in /tmp/reach-agent-artifacts.*) ;; *) exit 1 ;; esac
case "$artifact_tar" in /tmp/reach-agent-artifacts.*.tar.gz) ;; *) exit 1 ;; esac
if command -v trash >/dev/null 2>&1; then
  trash "$artifact_dir" "$artifact_tar" 2>/dev/null || true
else
  if [ -d "$artifact_dir" ]; then
    find "$artifact_dir" -depth -delete
  fi
  if [ -f "$artifact_tar" ]; then
    find "$artifact_tar" -delete
  fi
fi
REMOTE_CLEANUP
  fi
}
trap cleanup EXIT

build_agent "$ARTIFACT_DIR/reach-agent_linux_amd64" linux amd64
build_agent "$ARTIFACT_DIR/reach-agent_linux_arm64" linux arm64
build_agent "$ARTIFACT_DIR/reach-agent_linux_386" linux 386
build_agent "$ARTIFACT_DIR/reach-agent_linux_armv6" linux arm 6
build_agent "$ARTIFACT_DIR/reach-agent_linux_armv7" linux arm 7
build_agent "$ARTIFACT_DIR/reach-agent_darwin_amd64" darwin amd64
build_agent "$ARTIFACT_DIR/reach-agent_darwin_arm64" darwin arm64
build_agent "$ARTIFACT_DIR/reach-agent_windows_amd64.exe" windows amd64
build_agent "$ARTIFACT_DIR/reach-agent_windows_arm64.exe" windows arm64
(
  cd "$ARTIFACT_DIR"
  shasum -a 256 reach-agent_* | awk '{print $1 "  " $2}' > checksums.txt
)
go run ./cmd/reach-release manifest --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$DEPLOY_COMMIT"
uv run --script scripts/verify-release-artifacts.py --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$DEPLOY_COMMIT"
SIGN_ARGS=(--key "$RELEASE_KEY")
if [ -n "$RELEASE_PASSWORD_FILE" ]; then
  SIGN_ARGS+=(--password-file "$RELEASE_PASSWORD_FILE")
fi
go run ./cmd/reach-release sign "${SIGN_ARGS[@]}" --manifest "$ARTIFACT_DIR/manifest.json" --out "$ARTIFACT_DIR/manifest.json.minisig" --trusted-comment "reach-agent version=$AGENT_VERSION commit=$DEPLOY_COMMIT"
go run ./cmd/reach-release verify --manifest "$ARTIFACT_DIR/manifest.json" --artifacts-dir "$ARTIFACT_DIR"

tar czf "$AGENT_TAR" -C "$ARTIFACT_DIR" .

require_clean_product_tree
test "$(git -C "$SOURCE_ROOT" rev-parse HEAD)" = "$DEPLOY_COMMIT" || {
  echo "[mac] ERROR: source HEAD changed while building artifacts" >&2
  exit 1
}
REMOTE_PATHS="$(ssh "$DEPLOY_HOST" 'artifact_dir=$(mktemp -d /tmp/reach-agent-artifacts.XXXXXX); artifact_tar="${artifact_dir}.tar.gz"; : > "$artifact_tar"; printf "%s\n%s\n" "$artifact_dir" "$artifact_tar"')"
REMOTE_ARTIFACT_DIR="${REMOTE_PATHS%%$'\n'*}"
REMOTE_AGENT_TAR="$(printf '%s\n' "$REMOTE_PATHS" | tail -n 1 | tr -d '\r\n')"
case "$REMOTE_ARTIFACT_DIR" in /tmp/reach-agent-artifacts.*) ;; *) echo "[mac] ERROR: unsafe remote artifact directory" >&2; exit 1 ;; esac
case "$REMOTE_AGENT_TAR" in /tmp/reach-agent-artifacts.*.tar.gz) ;; *) echo "[mac] ERROR: unsafe remote artifact tar" >&2; exit 1 ;; esac

echo "[mac] uploading signed agent artifacts to ${DEPLOY_HOST}..."
scp "$AGENT_TAR" "${DEPLOY_HOST}:${REMOTE_AGENT_TAR}"

# Do not trust whatever deploy script the hub happened to have before this run.
# The remote bootstrap first fetches and checks out the exact pushed commit, then
# runs that commit's deployment script. No source files are copied from the Mac.
ssh "${DEPLOY_HOST}" "bash -s" -- "$DEPLOY_COMMIT" "$REMOTE_ARTIFACT_DIR" "$REMOTE_AGENT_TAR" "$AGENT_VERSION" <<'REMOTE_DEPLOY'
set -Eeuo pipefail
deploy_commit="$1"
artifact_dir="$2"
artifact_tar="$3"
agent_version="$4"
cleanup_remote() {
  case "$artifact_dir" in /tmp/reach-agent-artifacts.*) ;; *) return 1 ;; esac
  case "$artifact_tar" in /tmp/reach-agent-artifacts.*.tar.gz) ;; *) return 1 ;; esac
  if command -v trash >/dev/null 2>&1; then
    trash "$artifact_dir" "$artifact_tar" 2>/dev/null || true
  else
    if [ -d "$artifact_dir" ]; then find "$artifact_dir" -depth -delete; fi
    if [ -f "$artifact_tar" ]; then find "$artifact_tar" -delete; fi
  fi
}
trap cleanup_remote EXIT
cd "$HOME"
command -v flock >/dev/null || { echo "[mac] ERROR: flock is required on hub" >&2; exit 1; }
lock_dir="${XDG_RUNTIME_DIR:-$HOME/.cache/reach}"
mkdir -p "$lock_dir"
exec 9>"$lock_dir/deploy.lock"
if ! flock -n 9; then
  echo "[mac] ERROR: another Reach deployment is running" >&2
  exit 1
fi
if [ ! -d "$HOME/reach/.git" ]; then
  command -v gh >/dev/null || { echo "[mac] ERROR: hub clone missing at ~/reach and gh is unavailable" >&2; exit 1; }
  gh repo clone Yan-Yu-Lin/reach "$HOME/reach"
fi
cd "$HOME/reach"
status="$(git status --porcelain --untracked-files=normal)"
status="$(printf '%s\n' "$status" | grep -v '^?? \.claude/$' || true)"
if [ -n "$status" ]; then
  echo "[mac] ERROR: hub product tree has tracked or nonignored untracked changes" >&2
  printf '%s\n' "$status" >&2
  exit 1
fi
git fetch origin
if ! git cat-file -e "${deploy_commit}^{commit}" 2>/dev/null; then
  echo "[mac] ERROR: pushed commit ${deploy_commit} is unavailable on hub" >&2
  exit 1
fi
if [ -z "$(git for-each-ref --format='%(refname)' --contains "$deploy_commit" refs/remotes/origin/)" ]; then
  echo "[mac] ERROR: commit ${deploy_commit} is not reachable from an origin branch on hub" >&2
  exit 1
fi
git checkout --detach "$deploy_commit"
test "$(git rev-parse HEAD)" = "$deploy_commit"
mkdir -p "$artifact_dir"
tar xzf "$artifact_tar" -C "$artifact_dir"
REACH_DEPLOY_COMMIT="$deploy_commit" \
REACH_PUBLISH_AGENT_RELEASE=1 \
REACH_AGENT_VERSION="$agent_version" \
REACH_AGENT_ARTIFACT_DIR="$artifact_dir" \
REACH_DEPLOY_LOCK_HELD=1 \
CGO_ENABLED=0 \
bash scripts/deploy-jason.sh 9>&9
REMOTE_DEPLOY

echo "[mac] done. Published reach-agent v${AGENT_VERSION}."
