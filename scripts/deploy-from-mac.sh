#!/usr/bin/env bash
set -Eeuo pipefail

# Full deploy from a workstation to a configured hub.
# Set REACH_DEPLOY_HOST to the SSH alias for your hub.
# Builds/signs agent release artifacts locally (private key stays off the hub),
# builds dashboard locally (needs Node 22+), uploads artifacts, and runs deploy
# on the hub from its own git clone.

cd "$(dirname "$0")/.."

export CGO_ENABLED=0
DEPLOY_HOST="${REACH_DEPLOY_HOST:-reach-hub}"

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
  local arch="$2"
  local arm="${3:-}"
  echo "[mac] building reach-agent (${arch}${arm:+ GOARM=$arm})..."
  if [ -n "$arm" ]; then
    GOOS=linux GOARCH="$arch" GOARM="$arm" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
  else
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
  fi
}

AGENT_VERSION="${REACH_AGENT_VERSION:-$(version_from_git)}"
AGENT_LDFLAGS="-X main.version=${AGENT_VERSION}"
RELEASE_KEY="${REACH_RELEASE_KEY:-$HOME/.minisign/reach-release.key}"
RELEASE_PASSWORD_FILE="${REACH_RELEASE_KEY_PASSWORD_FILE:-}"
if [ -z "$RELEASE_PASSWORD_FILE" ] && [ -f "$HOME/.minisign/reach-release.pass" ]; then
  RELEASE_PASSWORD_FILE="$HOME/.minisign/reach-release.pass"
fi

[ -f "$RELEASE_KEY" ] || { echo "[mac] ERROR: release signing key not found at $RELEASE_KEY" >&2; exit 1; }

echo "[mac] agent version: $AGENT_VERSION"

ARTIFACT_DIR="$(mktemp -d)"
DASHBOARD_TAR="/tmp/reach-dashboard-deploy.tar.gz"
AGENT_TAR="/tmp/reach-agent-artifacts-${AGENT_VERSION}.tar.gz"
REMOTE_ARTIFACT_DIR="/tmp/reach-agent-artifacts-${AGENT_VERSION}-$(date -u +%Y%m%dT%H%M%SZ)"
trap 'rm -rf "$ARTIFACT_DIR" "$DASHBOARD_TAR" "$AGENT_TAR"' EXIT

build_agent "$ARTIFACT_DIR/reach-agent_linux_amd64" amd64
build_agent "$ARTIFACT_DIR/reach-agent_linux_arm64" arm64
build_agent "$ARTIFACT_DIR/reach-agent_linux_386" 386
build_agent "$ARTIFACT_DIR/reach-agent_linux_armv6" arm 6
build_agent "$ARTIFACT_DIR/reach-agent_linux_armv7" arm 7
(
  cd "$ARTIFACT_DIR"
  shasum -a 256 reach-agent_linux_* | awk '{print $1 "  " $2}' > checksums.txt
)
go run ./cmd/reach-release manifest --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$(git rev-parse HEAD)"
SIGN_ARGS=(--key "$RELEASE_KEY")
if [ -n "$RELEASE_PASSWORD_FILE" ]; then
  SIGN_ARGS+=(--password-file "$RELEASE_PASSWORD_FILE")
fi
go run ./cmd/reach-release sign "${SIGN_ARGS[@]}" --manifest "$ARTIFACT_DIR/manifest.json" --out "$ARTIFACT_DIR/manifest.json.minisig" --trusted-comment "reach-agent version=$AGENT_VERSION commit=$(git rev-parse HEAD)"

tar czf "$AGENT_TAR" -C "$ARTIFACT_DIR" .

echo "[mac] building dashboard..."
cd dashboard
npx nuxt generate 2>&1 | tail -1
cd ..

echo "[mac] uploading dashboard to ${DEPLOY_HOST}..."
tar czf "$DASHBOARD_TAR" -C dashboard/.output/public .
scp "$DASHBOARD_TAR" "${DEPLOY_HOST}:/tmp/"
ssh "${DEPLOY_HOST}" "sudo tar xzf /tmp/reach-dashboard-deploy.tar.gz -C /opt/reach-dashboard/"

echo "[mac] uploading signed agent artifacts to ${DEPLOY_HOST}..."
scp "$AGENT_TAR" "${DEPLOY_HOST}:/tmp/"
ssh "${DEPLOY_HOST}" "mkdir -p '$REMOTE_ARTIFACT_DIR' && tar xzf '/tmp/$(basename "$AGENT_TAR")' -C '$REMOTE_ARTIFACT_DIR'"

echo "[mac] running deploy on ${DEPLOY_HOST}..."
ssh "${DEPLOY_HOST}" "cd ~/reach && git pull --ff-only && REACH_AGENT_VERSION='$AGENT_VERSION' REACH_AGENT_ARTIFACT_DIR='$REMOTE_ARTIFACT_DIR' CGO_ENABLED=0 bash scripts/deploy-jason.sh"

echo "[mac] done. Published reach-agent v${AGENT_VERSION}."
