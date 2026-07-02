#!/usr/bin/env bash
set -Eeuo pipefail

# Deploy Reach to a hub server from the repo checked out on that hub.
# Example: ssh <hub-alias> 'cd ~/reach && bash scripts/deploy-jason.sh'
# Or locally on the hub: cd ~/reach && bash scripts/deploy-jason.sh
#
# Agent release versioning:
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
    GOOS="$goos" GOARCH="$arch" GOARM="$arm" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
  else
    GOOS="$goos" GOARCH="$arch" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
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

echo "[deploy] pulling latest..."
git pull --ff-only

AGENT_VERSION="${REACH_AGENT_VERSION:-$(version_from_git)}"
AGENT_LDFLAGS="-X main.version=${AGENT_VERSION}"
AGENT_DOWNLOAD_ROOT="/var/lib/reach/downloads/reach-agent"
AGENT_DOWNLOAD_DIR="${AGENT_DOWNLOAD_ROOT}/v${AGENT_VERSION}"
AGENT_STAGING_DIR="${AGENT_DOWNLOAD_ROOT}/.v${AGENT_VERSION}.tmp.$$"
ARTIFACT_DIR="${REACH_AGENT_ARTIFACT_DIR:-}"
BUILD_ARTIFACT_DIR=""

echo "[deploy] agent version: ${AGENT_VERSION}"

if [ -z "$ARTIFACT_DIR" ]; then
  BUILD_ARTIFACT_DIR="$(mktemp -d)"
  ARTIFACT_DIR="$BUILD_ARTIFACT_DIR"
  trap 'rm -rf "$BUILD_ARTIFACT_DIR"' EXIT

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
  if sign_manifest_if_configured "$ARTIFACT_DIR"; then
    echo "[deploy] signed release manifest"
  elif [ "${REACH_REQUIRE_SIGNED_MANIFEST:-0}" = 1 ]; then
    echo "[deploy] ERROR: REACH_REQUIRE_SIGNED_MANIFEST=1 but REACH_RELEASE_KEY is not set" >&2
    exit 1
  else
    echo "[deploy] WARNING: manifest.json.minisig not created; update-binary will reject this release"
  fi
else
  require_agent_artifacts "$ARTIFACT_DIR"
fi

if [ -e "$AGENT_DOWNLOAD_DIR" ] && [ "${REACH_OVERWRITE_VERSION:-0}" != 1 ]; then
  echo "[deploy] ERROR: $AGENT_DOWNLOAD_DIR already exists; refusing to overwrite immutable release" >&2
  echo "[deploy] Set REACH_OVERWRITE_VERSION=1 only for emergency rebuilds of the same version." >&2
  exit 1
fi

echo "[deploy] building reachd (linux/amd64 static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/reachd-build .

echo "[deploy] building reach-ws-carrier (linux/amd64 static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/reach-ws-carrier ./cmd/reach-ws-carrier

echo "[deploy] installing binaries..."
sudo install -m 0755 /tmp/reachd-build /opt/reach/reachd
sudo install -m 0755 /tmp/reach-ws-carrier /opt/reach/reach-ws-carrier
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_amd64" /opt/reach/reach-agent
rm -f /tmp/reachd-build /tmp/reach-ws-carrier

echo "[deploy] publishing hosted agent downloads..."
sudo mkdir -p "$AGENT_DOWNLOAD_ROOT"
if [ -e "$AGENT_STAGING_DIR" ]; then
  sudo mv "$AGENT_STAGING_DIR" "$AGENT_STAGING_DIR.abandoned.$(date -u +%Y%m%dT%H%M%SZ)"
fi
sudo mkdir -p "$AGENT_STAGING_DIR"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_amd64" "$AGENT_STAGING_DIR/reach-agent_linux_amd64"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_arm64" "$AGENT_STAGING_DIR/reach-agent_linux_arm64"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_386" "$AGENT_STAGING_DIR/reach-agent_linux_386"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_armv6" "$AGENT_STAGING_DIR/reach-agent_linux_armv6"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_linux_armv7" "$AGENT_STAGING_DIR/reach-agent_linux_armv7"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_darwin_amd64" "$AGENT_STAGING_DIR/reach-agent_darwin_amd64"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_darwin_arm64" "$AGENT_STAGING_DIR/reach-agent_darwin_arm64"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_windows_amd64.exe" "$AGENT_STAGING_DIR/reach-agent_windows_amd64.exe"
sudo install -m 0755 "$ARTIFACT_DIR/reach-agent_windows_arm64.exe" "$AGENT_STAGING_DIR/reach-agent_windows_arm64.exe"
sudo install -m 0644 "$ARTIFACT_DIR/checksums.txt" "$AGENT_STAGING_DIR/checksums.txt"
sudo install -m 0644 "$ARTIFACT_DIR/manifest.json" "$AGENT_STAGING_DIR/manifest.json"
if [ -f "$ARTIFACT_DIR/manifest.json.minisig" ]; then
  sudo install -m 0644 "$ARTIFACT_DIR/manifest.json.minisig" "$AGENT_STAGING_DIR/manifest.json.minisig"
fi
if [ -e "$AGENT_DOWNLOAD_DIR" ]; then
  sudo mv "$AGENT_DOWNLOAD_DIR" "$AGENT_DOWNLOAD_DIR.replaced.$(date -u +%Y%m%dT%H%M%SZ)"
fi
sudo mv "$AGENT_STAGING_DIR" "$AGENT_DOWNLOAD_DIR"
printf '%s\n' "$AGENT_VERSION" > /tmp/reach-latest-version
sudo install -m 0644 /tmp/reach-latest-version "$AGENT_DOWNLOAD_ROOT/latest.txt"
cat > /tmp/reach-latest.json <<EOF
{"version":"${AGENT_VERSION}","api_url":"${REACH_API_URL:-}","created_at":"$(date -u +%Y-%m-%dT%H:%M:%SZ)"}
EOF
sudo install -m 0644 /tmp/reach-latest.json "$AGENT_DOWNLOAD_ROOT/latest.json"
rm -f /tmp/reach-latest-version /tmp/reach-latest.json

echo "[deploy] updating setup scripts..."
CONFIG_API_URL="${REACH_API_URL:-}"
read_reach_api_url_awk='
  $1 == "default_hub:" { in_hub=1; next }
  in_hub && $1 == "api_url:" {
    sub(/^[^:]*:[[:space:]]*/, "")
    sub(/[[:space:]]+#.*$/, "")
    gsub(/^[[:space:]]+|[[:space:]]+$/, "")
    gsub(/^\"|\"$/, "")
    print
    exit
  }
  in_hub && /^[^[:space:]]/ { in_hub=0 }
'
if [ -z "$CONFIG_API_URL" ]; then
  if [ -r /etc/reach/config.yaml ]; then
    CONFIG_API_URL="$(awk "$read_reach_api_url_awk" /etc/reach/config.yaml)"
  elif sudo -n test -r /etc/reach/config.yaml 2>/dev/null; then
    CONFIG_API_URL="$(sudo awk "$read_reach_api_url_awk" /etc/reach/config.yaml)"
  fi
fi
if [ -z "$CONFIG_API_URL" ] || [ "$CONFIG_API_URL" = "https://tunnels.your-domain.example" ]; then
  echo "[deploy] ERROR: could not determine real Reach API URL. Set REACH_API_URL or configure default_hub.api_url in /etc/reach/config.yaml." >&2
  exit 1
fi
case "$CONFIG_API_URL" in
  http://*|https://*) ;;
  *) echo "[deploy] ERROR: invalid Reach API URL: $CONFIG_API_URL" >&2; exit 1 ;;
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
if grep -q 'tunnels.your-domain.example' /tmp/reach-setup.sh; then
  echo "[deploy] ERROR: setup.sh still contains placeholder API URL after rendering" >&2
  exit 1
fi
if grep -q 'tunnels.your-domain.example' /tmp/reach-setup.ps1; then
  echo "[deploy] ERROR: setup.ps1 still contains placeholder API URL after rendering" >&2
  exit 1
fi
if ! grep -Fq "$CONFIG_API_URL" /tmp/reach-setup.sh; then
  echo "[deploy] ERROR: setup.sh does not contain rendered API URL $CONFIG_API_URL" >&2
  exit 1
fi
if ! grep -Fq "$CONFIG_API_URL" /tmp/reach-setup.ps1; then
  echo "[deploy] ERROR: setup.ps1 does not contain rendered API URL $CONFIG_API_URL" >&2
  exit 1
fi
if ! grep -Fq "$AGENT_VERSION" /tmp/reach-setup.sh; then
  echo "[deploy] ERROR: setup.sh does not contain rendered agent version $AGENT_VERSION" >&2
  exit 1
fi
if ! grep -Fq "$AGENT_VERSION" /tmp/reach-setup.ps1; then
  echo "[deploy] ERROR: setup.ps1 does not contain rendered agent version $AGENT_VERSION" >&2
  exit 1
fi
sudo install -m 0644 /tmp/reach-setup.sh /var/lib/reach/setup.sh
sudo install -m 0644 /tmp/reach-setup.ps1 /var/lib/reach/setup.ps1
rm -f /tmp/reach-setup.sh /tmp/reach-setup.ps1

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

echo "[deploy] deploying dashboard..."
export FNM_PATH="$HOME/.local/share/fnm"
if [ -d "$FNM_PATH" ]; then
  export PATH="$FNM_PATH:$PATH"
  eval "$(fnm env --shell bash)"
  fnm use 22 --silent-if-unchanged 2>/dev/null || true
fi
cd dashboard
npm install --silent 2>/dev/null
npx nuxt generate 2>&1 | tail -1
if [ -f .output/public/index.html ]; then
  sudo cp -r .output/public/* /opt/reach-dashboard/
  echo "[deploy] dashboard built and deployed"
else
  echo "[deploy] ERROR: dashboard build failed, index.html missing!"
  exit 1
fi
cd ..

echo "[deploy] restarting services..."
sudo systemctl restart reachd
sudo systemctl restart reach-ws-carrier.service
sleep 2
systemctl is-active reachd
systemctl is-active reach-ws-carrier.service

echo "[deploy] done. Published reach-agent v${AGENT_VERSION}."
