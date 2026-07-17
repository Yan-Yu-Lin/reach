#!/usr/bin/env bash
set -Eeuo pipefail

# Full deploy from a workstation to a configured hub.
# Set REACH_DEPLOY_HOST to the SSH alias for your hub.
# Builds/signs every supported agent artifact locally, uploads them, and runs
# deploy on the hub from its own git clone. The hub builds/swaps its dashboard.

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
  local goos="$2"
  local arch="$3"
  local arm="${4:-}"
  echo "[mac] building reach-agent (${goos}/${arch}${arm:+ GOARM=$arm})..."
  if [ -n "$arm" ]; then
    GOOS="$goos" GOARCH="$arch" GOARM="$arm" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
  else
    GOOS="$goos" GOARCH="$arch" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
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
AGENT_TAR="/tmp/reach-agent-artifacts-${AGENT_VERSION}.tar.gz"
REMOTE_ARTIFACT_DIR="/tmp/reach-agent-artifacts-${AGENT_VERSION}-$(date -u +%Y%m%dT%H%M%SZ)"
trap 'trash "$ARTIFACT_DIR" "$AGENT_TAR" 2>/dev/null || true' EXIT

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
go run ./cmd/reach-release manifest --dir "$ARTIFACT_DIR" --version "$AGENT_VERSION" --commit "$(git rev-parse HEAD)"
SIGN_ARGS=(--key "$RELEASE_KEY")
if [ -n "$RELEASE_PASSWORD_FILE" ]; then
  SIGN_ARGS+=(--password-file "$RELEASE_PASSWORD_FILE")
fi
go run ./cmd/reach-release sign "${SIGN_ARGS[@]}" --manifest "$ARTIFACT_DIR/manifest.json" --out "$ARTIFACT_DIR/manifest.json.minisig" --trusted-comment "reach-agent version=$AGENT_VERSION commit=$(git rev-parse HEAD)"

tar czf "$AGENT_TAR" -C "$ARTIFACT_DIR" .

echo "[mac] uploading signed agent artifacts to ${DEPLOY_HOST}..."
scp "$AGENT_TAR" "${DEPLOY_HOST}:/tmp/"
ssh "${DEPLOY_HOST}" "mkdir -p '$REMOTE_ARTIFACT_DIR' && tar xzf '/tmp/$(basename "$AGENT_TAR")' -C '$REMOTE_ARTIFACT_DIR'"

DEPLOY_COMMIT="$(git rev-parse HEAD)"
echo "[mac] running deploy of ${DEPLOY_COMMIT} on ${DEPLOY_HOST}..."
ssh "${DEPLOY_HOST}" "cd ~/reach && REACH_DEPLOY_COMMIT='$DEPLOY_COMMIT' REACH_PUBLISH_AGENT_RELEASE=1 REACH_AGENT_VERSION='$AGENT_VERSION' REACH_AGENT_ARTIFACT_DIR='$REMOTE_ARTIFACT_DIR' CGO_ENABLED=0 bash scripts/deploy-jason.sh"

echo "[mac] done. Published reach-agent v${AGENT_VERSION}."
