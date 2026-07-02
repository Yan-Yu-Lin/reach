#!/usr/bin/env bash
set -Eeuo pipefail

REACH_AGENT_VERSION="${REACH_AGENT_VERSION:-0.1.0-alpha}"
API_URL="${REACH_API_URL:-https://tunnels.your-domain.example}"
TRANSPORT="${REACH_TRANSPORT:-auto}"

NAME=""
TOKEN=""
TOKEN_FILE="${REACH_AUTH_CODE_FILE:-}"
TARGET_USER=""
YES=0
UPDATE_AGENT=0
ACTION="install"

log() { printf '\033[1;34m[reach]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[reach warning]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[reach error]\033[0m %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

TTY_OK=0
if [ -r /dev/tty ] && [ -w /dev/tty ] && { exec 3<>/dev/tty; } 2>/dev/null; then TTY_OK=1; fi
prompt() {
  local var="$1" msg="$2" default="${3:-}" value
  [ "$TTY_OK" = 1 ] || return 1
  if [ -n "$default" ]; then printf '%s [%s]: ' "$msg" "$default" >&3; else printf '%s: ' "$msg" >&3; fi
  IFS= read -r value <&3
  printf -v "$var" '%s' "${value:-$default}"
}

usage() {
  cat <<'USAGE'
Reach target setup

Usage:
  setup.sh [--name NAME] [--token GOD_CODE] [--target-user USER] [--transport auto|direct|websocket] [--yes]
  setup.sh --repair [--update-agent] [--version VERSION]
  setup.sh --uninstall | --reinstall

Environment:
  REACH_API_URL=https://tunnels.your-domain.example
  REACH_TRANSPORT=auto|direct|websocket
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --name) NAME="${2:-}"; shift 2 ;;
    --token|--god-code|--auth-code) TOKEN="${2:-}"; warn "auth codes passed as flags may be visible in ps; prefer REACH_AUTH_CODE_FILE or interactive prompt."; shift 2 ;;
    --token-file|--auth-code-file) TOKEN_FILE="${2:-}"; shift 2 ;;
    --target-user|--user) TARGET_USER="${2:-}"; shift 2 ;;
    --api-url) API_URL="${2:-}"; shift 2 ;;
    --transport) TRANSPORT="${2:-}"; shift 2 ;;
    --yes|-y) YES=1; shift ;;
    --update-agent) UPDATE_AGENT=1; ACTION="repair"; shift ;;
    --version) REACH_AGENT_VERSION="${2:-}"; shift 2 ;;
    --repair) ACTION="repair"; shift ;;
    --reinstall) ACTION="reinstall"; shift ;;
    --uninstall) ACTION="uninstall"; shift ;;
    --allow-no-sudo) shift ;; # compatibility: no-sudo is now automatic when sudo is unavailable/declined
    --help|-h) usage; exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done

os="$(uname -s 2>/dev/null || true)"
case "$os" in Linux|Darwin) ;; *) die "Reach setup currently supports Linux and macOS only (got ${os:-unknown})." ;; esac
case "$TRANSPORT" in auto|direct|websocket) ;; *) die "--transport must be one of: auto, direct, websocket" ;; esac
have curl || die "curl is required"
if [ -z "$TOKEN" ] && [ -n "$TOKEN_FILE" ] && [ -r "$TOKEN_FILE" ]; then
  TOKEN="$(tr -d '\r\n' < "$TOKEN_FILE")"
fi

PRIV="none"; USE_SUDO=0; SUDO=""; INSTALL_MODE="user"
if [ "$(id -u)" = 0 ]; then
  PRIV="root"; USE_SUDO=1; INSTALL_MODE="system"; SUDO=""
elif have sudo && sudo -n true 2>/dev/null; then
  PRIV="sudo-noprompt"
elif have sudo; then
  PRIV="sudo-possible"
fi

if [ "$(id -u)" != 0 ] && have sudo; then
  if sudo -n true 2>/dev/null; then
    if [ "$YES" = 1 ]; then
      ans="y"
    elif [ "$TTY_OK" = 1 ]; then
      prompt ans "Reach can use passwordless sudo for system sshd/service management. Use sudo?" "Y" || ans="n"
    else
      ans="y"
    fi
  else
    ans="n"
  fi
  ans="${ans:-y}"
  if [[ "$ans" =~ ^[Yy]$ ]]; then
    if sudo -n -v 2>/dev/null; then
      USE_SUDO=1; SUDO="sudo -n"; INSTALL_MODE="system"
    else
      warn "passwordless sudo unavailable; continuing in user mode."
    fi
  elif [ "$YES" != 1 ]; then
    log "Continuing without sudo; installing user-mode Reach agent."
  fi
fi

if [ "$INSTALL_MODE" = "system" ]; then
  INSTALL_PREFIX="/opt/reach"
  AGENT_PATH="$INSTALL_PREFIX/reach-agent"
else
  INSTALL_PREFIX="$HOME/.local/bin"
  AGENT_PATH="$INSTALL_PREFIX/reach-agent"
fi

run_priv() {
  if [ "$USE_SUDO" = 1 ]; then $SUDO "$@"; else "$@"; fi
}

repair_user() {
  [ -x "$AGENT_PATH" ] || die "$AGENT_PATH is missing; rerun setup.sh install."
  "$AGENT_PATH" check --config "$HOME/.config/reach/agent.yaml" || true
  if [ "${os:-}" = "Darwin" ] && have launchctl; then
    launchctl kickstart -k "gui/$(id -u)/dev.arthurlin.reach-agent" 2>/dev/null || true
    launchctl print "gui/$(id -u)/dev.arthurlin.reach-agent" >/dev/null 2>&1 && { log "User reach-agent restarted."; exit 0; }
  fi
  if have systemctl && systemctl --user list-units >/dev/null 2>&1; then
    systemctl --user restart reach-agent.service || true
    systemctl --user --quiet is-active reach-agent.service && { log "User reach-agent restarted."; exit 0; }
  fi
  nohup "$AGENT_PATH" run --config "$HOME/.config/reach/agent.yaml" >> "$HOME/.local/share/reach/agent.log" 2>&1 &
  log "Started user reach-agent fallback."
}

if [ "$ACTION" = "repair" ] && [ "$UPDATE_AGENT" != 1 ]; then
  if [ "$INSTALL_MODE" = "system" ] && [ -x /opt/reach/reach-agent ]; then
    log "Checking system reach-agent configuration."
    run_priv /opt/reach/reach-agent check --config /etc/reach/agent.yaml || true
    if [ "$os" = "Darwin" ] && have launchctl; then
      log "Restarting launchd Reach agent."
      run_priv launchctl kickstart -k system/dev.arthurlin.reach-agent || true
      run_priv launchctl print system/dev.arthurlin.reach-agent >/dev/null 2>&1 || die "launchd service is not loaded"
    else
      log "Restarting reach-agent.service."
      run_priv systemctl restart reach-agent.service
      run_priv systemctl --no-pager --quiet is-active reach-agent.service || die "reach-agent.service is not active; check journalctl -u reach-agent"
    fi
    log "Repair complete."
  else
    repair_user
  fi
  exit 0
fi

if [ "$ACTION" = "uninstall" ] || [ "$ACTION" = "reinstall" ]; then
  log "Uninstalling existing Reach agent/tunnel services."
  if [ "$USE_SUDO" = 1 ] && [ -x /opt/reach/reach-agent ]; then
    $SUDO /opt/reach/reach-agent uninstall --mode system --yes 2>/dev/null || true
  fi
  if [ -x "$HOME/.local/bin/reach-agent" ]; then
    "$HOME/.local/bin/reach-agent" uninstall --mode user --yes 2>/dev/null || true
  fi
  # Best-effort cleanup for very old installs or missing binaries.
  if [ "$USE_SUDO" = 1 ]; then
    if [ "$os" = "Darwin" ] && have launchctl; then
      $SUDO launchctl bootout system/dev.arthurlin.reach-agent 2>/dev/null || true
      $SUDO launchctl disable system/dev.arthurlin.reach-agent 2>/dev/null || true
      $SUDO rm -f /Library/LaunchDaemons/dev.arthurlin.reach-agent.plist
    else
      $SUDO systemctl disable --now reach-agent.service 2>/dev/null || true
      $SUDO systemctl disable --now reach-tunnel.service 2>/dev/null || true
      $SUDO rm -f /etc/systemd/system/reach-agent.service /etc/systemd/system/reach-tunnel.service
      $SUDO systemctl daemon-reload 2>/dev/null || true
    fi
  fi
  if [ "$os" = "Darwin" ] && have launchctl; then
    launchctl bootout "gui/$(id -u)/dev.arthurlin.reach-agent" 2>/dev/null || true
    launchctl disable "gui/$(id -u)/dev.arthurlin.reach-agent" 2>/dev/null || true
    rm -f "$HOME/Library/LaunchAgents/dev.arthurlin.reach-agent.plist"
  elif have systemctl; then
    systemctl --user disable --now reach-agent.service 2>/dev/null || true
    rm -f "$HOME/.config/systemd/user/reach-agent.service"
    systemctl --user daemon-reload 2>/dev/null || true
  fi
  if have crontab; then (crontab -l 2>/dev/null | grep -v 'reach-agent') | crontab - 2>/dev/null || true; fi
  pkill -f 'reach-agent run --config' 2>/dev/null || true
  [ "$ACTION" = "uninstall" ] && { log "Reach uninstall complete."; exit 0; }
fi

arch="$(uname -m 2>/dev/null || echo unknown)"
case "$os/$arch" in
  Linux/x86_64|Linux/amd64) asset="reach-agent_linux_amd64" ;;
  Linux/aarch64|Linux/arm64) asset="reach-agent_linux_arm64" ;;
  Linux/armv7l|Linux/armv7*) asset="reach-agent_linux_armv7" ;;
  Linux/armv6l|Linux/armv6*) asset="reach-agent_linux_armv6" ;;
  Linux/i386|Linux/i686) asset="reach-agent_linux_386" ;;
  Darwin/x86_64|Darwin/amd64) asset="reach-agent_darwin_amd64" ;;
  Darwin/arm64|Darwin/aarch64) asset="reach-agent_darwin_arm64" ;;
  *) die "Unsupported platform/architecture: $os/$arch" ;;
esac

base_url="${API_URL%/}/downloads/reach-agent/v${REACH_AGENT_VERSION}"
tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

log "Downloading reach-agent $REACH_AGENT_VERSION for $os/$arch."
curl -fsSL "$base_url/$asset" -o "$tmpdir/reach-agent"
curl -fsSL "$base_url/checksums.txt" -o "$tmpdir/checksums.txt"
expected="$(awk -v f="$asset" '$2 == f {print $1}' "$tmpdir/checksums.txt")"
[ -n "$expected" ] || die "No checksum found for $asset"
if have sha256sum; then actual="$(sha256sum "$tmpdir/reach-agent" | awk '{print $1}')"
elif have shasum; then actual="$(shasum -a 256 "$tmpdir/reach-agent" | awk '{print $1}')"
elif have openssl; then actual="$(openssl dgst -sha256 "$tmpdir/reach-agent" | awk '{print $NF}')"
else die "Need sha256sum, shasum, or openssl to verify reach-agent."
fi
[ "$actual" = "$expected" ] || die "reach-agent checksum mismatch: got $actual expected $expected"
chmod +x "$tmpdir/reach-agent"

if [ "$ACTION" = "repair" ] && [ "$UPDATE_AGENT" = 1 ]; then
  log "Updating existing reach-agent binary in place; this will not register, provision, or uninstall."
  if [ "$USE_SUDO" = 1 ]; then
    exec $SUDO "$tmpdir/reach-agent" update-binary --foreground --candidate "$tmpdir/reach-agent" --version "$REACH_AGENT_VERSION" --api-url "$API_URL"
  else
    exec "$tmpdir/reach-agent" update-binary --foreground --candidate "$tmpdir/reach-agent" --version "$REACH_AGENT_VERSION" --api-url "$API_URL"
  fi
fi

log "Installing reach-agent to $AGENT_PATH ($INSTALL_MODE mode; privilege: $PRIV)."
run_priv mkdir -p "$INSTALL_PREFIX"
run_priv install -m 0755 "$tmpdir/reach-agent" "$AGENT_PATH"

install_args=(install --mode "$INSTALL_MODE" --agent-path "$AGENT_PATH" --api-url "$API_URL" --transport "$TRANSPORT")
[ -n "$NAME" ] && install_args+=(--name "$NAME")
[ -n "$TOKEN" ] && install_args+=(--token "$TOKEN")
[ -n "$TARGET_USER" ] && install_args+=(--target-user "$TARGET_USER")
[ "$YES" = 1 ] && install_args+=(--yes)

log "Handing off to reach-agent install."
if [ "$USE_SUDO" = 1 ]; then
  exec $SUDO "$AGENT_PATH" "${install_args[@]}"
else
  exec "$AGENT_PATH" "${install_args[@]}"
fi
