#!/usr/bin/env bash
set -Eeuo pipefail

# Deploy Reach to a hub server from the repo checked out on that hub.
# Example: ssh <hub-alias> 'cd ~/reach && bash scripts/deploy-jason.sh'
# Or locally on the hub: cd ~/reach && bash scripts/deploy-jason.sh

cd "$(dirname "$0")/.."

export CGO_ENABLED=0
AGENT_VERSION="${REACH_AGENT_VERSION:-0.1.0-alpha}"
AGENT_LDFLAGS="-X main.version=${AGENT_VERSION}"
AGENT_DOWNLOAD_DIR="/var/lib/reach/downloads/reach-agent/v${AGENT_VERSION}"

build_agent() {
  local out="$1"
  local arch="$2"
  local arm="${3:-}"
  echo "[deploy] building reach-agent (${arch}${arm:+ GOARM=$arm})..."
  if [ -n "$arm" ]; then
    GOOS=linux GOARCH="$arch" GOARM="$arm" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
  else
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o "$out" ./cmd/reach-agent
  fi
}

echo "[deploy] pulling latest..."
git pull --ff-only

echo "[deploy] building reachd (linux/amd64 static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/reachd-build .

echo "[deploy] building reach-ws-carrier (linux/amd64 static)..."
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o /tmp/reach-ws-carrier ./cmd/reach-ws-carrier

build_agent /tmp/reach-agent-amd64 amd64
build_agent /tmp/reach-agent-arm64 arm64
build_agent /tmp/reach-agent-386 386
build_agent /tmp/reach-agent-armv6 arm 6
build_agent /tmp/reach-agent-armv7 arm 7

echo "[deploy] building reach-agent (windows/amd64)..."
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o /tmp/reach-agent-windows-amd64.exe ./cmd/reach-agent
echo "[deploy] building reach-agent (windows/arm64)..."
GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$AGENT_LDFLAGS" -o /tmp/reach-agent-windows-arm64.exe ./cmd/reach-agent

echo "[deploy] installing binaries..."
sudo install -m 0755 /tmp/reachd-build /opt/reach/reachd
sudo install -m 0755 /tmp/reach-ws-carrier /opt/reach/reach-ws-carrier
sudo install -m 0755 /tmp/reach-agent-amd64 /opt/reach/reach-agent
rm -f /tmp/reachd-build /tmp/reach-ws-carrier

echo "[deploy] updating hosted agent downloads..."
sudo mkdir -p "$AGENT_DOWNLOAD_DIR"
sudo install -m 0755 /tmp/reach-agent-amd64 "$AGENT_DOWNLOAD_DIR/reach-agent_linux_amd64"
sudo install -m 0755 /tmp/reach-agent-arm64 "$AGENT_DOWNLOAD_DIR/reach-agent_linux_arm64"
sudo install -m 0755 /tmp/reach-agent-386 "$AGENT_DOWNLOAD_DIR/reach-agent_linux_386"
sudo install -m 0755 /tmp/reach-agent-armv6 "$AGENT_DOWNLOAD_DIR/reach-agent_linux_armv6"
sudo install -m 0755 /tmp/reach-agent-armv7 "$AGENT_DOWNLOAD_DIR/reach-agent_linux_armv7"
sudo install -m 0755 /tmp/reach-agent-windows-amd64.exe "$AGENT_DOWNLOAD_DIR/reach-agent_windows_amd64.exe"
sudo install -m 0755 /tmp/reach-agent-windows-arm64.exe "$AGENT_DOWNLOAD_DIR/reach-agent_windows_arm64.exe"
rm -f /tmp/reach-agent-amd64 /tmp/reach-agent-arm64 /tmp/reach-agent-386 /tmp/reach-agent-armv6 /tmp/reach-agent-armv7 /tmp/reach-agent-windows-amd64.exe /tmp/reach-agent-windows-arm64.exe
cd "$AGENT_DOWNLOAD_DIR"
sudo sh -c 'sha256sum reach-agent_linux_* reach-agent_windows_* > checksums.txt'
cd - >/dev/null

echo "[deploy] updating setup scripts..."
CONFIG_API_URL="${REACH_API_URL:-}"
if [ -z "$CONFIG_API_URL" ] && [ -r /etc/reach/config.yaml ]; then
  CONFIG_API_URL="$(awk '
    $1 == "default_hub:" { in_hub=1; next }
    in_hub && $1 == "api_url:" { sub(/^[^:]*:[[:space:]]*/, ""); gsub(/^\"|\"$/, ""); print; exit }
    in_hub && /^[^[:space:]]/ { in_hub=0 }
  ' /etc/reach/config.yaml)"
fi
CONFIG_API_URL="${CONFIG_API_URL:-https://tunnels.your-domain.example}"
CONFIG_API_URL_SED="$(printf '%s' "$CONFIG_API_URL" | sed 's/[&|\\]/\\&/g')"
sed \
  -e "s|^REACH_AGENT_VERSION=.*|REACH_AGENT_VERSION=\"\${REACH_AGENT_VERSION:-${AGENT_VERSION}}\"|" \
  -e "s|^API_URL=.*|API_URL=\"\${REACH_API_URL:-${CONFIG_API_URL_SED}}\"|" \
  setup.sh > /tmp/reach-setup.sh
sudo install -m 0644 /tmp/reach-setup.sh /var/lib/reach/setup.sh
sed \
  -e "s|\"https://tunnels.your-domain.example\"|\"${CONFIG_API_URL_SED}\"|g" \
  -e "s|\"0.1.0-alpha\"|\"${AGENT_VERSION}\"|g" \
  setup.ps1 > /tmp/reach-setup.ps1
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

echo "[deploy] done."
