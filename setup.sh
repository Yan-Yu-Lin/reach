#!/usr/bin/env bash
set -Eeuo pipefail

REACH_AGENT_VERSION="${REACH_AGENT_VERSION:-0.1.0-alpha}"
API_URL="${REACH_API_URL:-https://tunnels.your-domain.example}"
TRANSPORT="${REACH_TRANSPORT:-auto}"
SOURCE_URL="${REACH_SOURCE_URL:-https://github.com/Yan-Yu-Lin/reach}"
# Offline release-signing key; its private half never lives on the hub. Same
# key as releaseManifestPublicKeys in cmd/reach-agent/update_binary.go.
REACH_RELEASE_PUBKEY="RWQ+MT09E8yd1V1tf3J3NI3Eb7L9DgMgNrN4SiqdrTs1Y2C61++bpyYY"
# Filled in by scripts/deploy-jason.sh from the signature-verified manifest of
# the published release; used only when no Ed25519 verifier is available.
REACH_PINNED_VERSION=""
REACH_PINNED_SHA256S=""

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

if [ "$ACTION" = "repair" ] && [ "$UPDATE_AGENT" = 1 ]; then
  # The already-installed (trusted) agent downloads and verifies the signed
  # release itself; a freshly downloaded candidate is never executed here.
  log "Updating existing reach-agent binary in place; this will not register, provision, or uninstall."
  if [ "$INSTALL_MODE" = "system" ] && [ -x /opt/reach/reach-agent ]; then
    exec $SUDO /opt/reach/reach-agent update-binary --version "$REACH_AGENT_VERSION" --api-url "$API_URL"
  elif [ -x "$HOME/.local/bin/reach-agent" ]; then
    exec "$HOME/.local/bin/reach-agent" update-binary --version "$REACH_AGENT_VERSION" --api-url "$API_URL"
  fi
  die "No installed reach-agent found to perform a verified update; rerun setup.sh without --repair."
fi

base_url="${API_URL%/}/downloads/reach-agent/v${REACH_AGENT_VERSION}"
# validate_api_url accepts https://HOST[:PORT][/PATH] for any DNS host, and
# plain http only when HOST is exactly 127.0.0.1, ::1 or localhost. The
# authority is parsed, so prefixes like http://localhost.evil.example and
# userinfo tricks (https://good@evil) are rejected (astra P2-11).
validate_api_url() {
  local url="$1" scheme rest authority host port
  case "$url" in
    https://*) scheme=https; rest="${url#https://}" ;;
    http://*) scheme=http; rest="${url#http://}" ;;
    *) return 1 ;;
  esac
  authority="${rest%%/*}"
  case "$authority" in ''|*@*|*\\*|*' '*|*'?'*|*'#'*) return 1 ;; esac
  case "$authority" in
    \[*\]) host="$authority"; port="" ;;
    \[*\]:*) host="${authority%]:*}]"; port="${authority##*]:}" ;;
    *:*) host="${authority%:*}"; port="${authority##*:}" ;;
    *) host="$authority"; port="" ;;
  esac
  if [ -n "$port" ]; then
    case "$port" in *[!0-9]*) return 1 ;; esac
  fi
  if [ "$scheme" = http ]; then
    case "$host" in 127.0.0.1|localhost|'[::1]') return 0 ;; *) return 1 ;; esac
  fi
  case "$host" in *[!A-Za-z0-9.-]*|.*|*..*) return 1 ;; esac
  return 0
}
validate_api_url "$API_URL" || die "API URL must be https:// (plain http only for exactly 127.0.0.1 or localhost): $API_URL"
tmpdir="$(mktemp -d)"
trap 'rm -r -f "$tmpdir"' EXIT

# Pick curl flags this curl supports. --proto/--proto-redir appeared in
# 7.21.0; CentOS 6 ships 7.19.7 (astra P2-06). Without them redirects are
# not followed at all (no -L), so a redirect cannot switch protocol or host.
CURL_ARGS=(-fsS)
curl_supports() { curl --help all 2>/dev/null | grep -q -- "$1" || curl --help 2>/dev/null | grep -q -- "$1"; }
if curl_supports --proto-redir; then
  CURL_ARGS+=(-L --proto '=https,http' --proto-redir '=https')
else
  warn "curl $(curl --version 2>/dev/null | awk 'NR==1{print $2}') lacks --proto; downloading without following redirects."
fi
if curl_supports --max-filesize; then
  CURL_MAXSIZE=1
else
  CURL_MAXSIZE=0
fi
fetch() {
  local url="$1" out="$2" max="$3"
  validate_api_url "$url" || die "refusing to download from $url"
  if [ "$CURL_MAXSIZE" = 1 ]; then
    curl "${CURL_ARGS[@]}" --max-filesize "$max" "$url" -o "$out"
  else
    curl "${CURL_ARGS[@]}" "$url" -o "$out"
  fi
  [ "$(wc -c < "$out" | tr -d ' ')" -le "$max" ] || die "download from $url exceeds $max bytes"
}

sha256_of() {
  if have sha256sum; then sha256sum "$1" | awk '{print $1}'
  elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
  elif have openssl; then openssl dgst -sha256 "$1" | awk '{print $NF}'
  else die "Need sha256sum, shasum, or openssl to verify reach-agent."
  fi
}

# find_ed25519_openssl prints an openssl that can verify raw Ed25519
# (OpenSSL >= 3.0, pkeyutl -rawin). macOS LibreSSL cannot.
find_ed25519_openssl() {
  local c
  for c in openssl /opt/homebrew/opt/openssl@3/bin/openssl /usr/local/opt/openssl@3/bin/openssl openssl3; do
    command -v "$c" >/dev/null 2>&1 || continue
    if "$c" version 2>/dev/null | grep -Eq '^OpenSSL [3-9]\.'; then printf '%s\n' "$c"; return 0; fi
  done
  return 1
}

# verify_minisign_openssl MSG SIG: check a minisign Ed25519 signature and its
# trusted-comment signature against REACH_RELEASE_PUBKEY.
verify_minisign_openssl() {
  local msg="$1" sig="$2" d="$tmpdir/ms" tc
  mkdir -p "$d"
  printf '%s' "$REACH_RELEASE_PUBKEY" | "$OSSL" base64 -d -A > "$d/pk.bin" || return 1
  [ "$(wc -c < "$d/pk.bin" | tr -d ' ')" = 42 ] || return 1
  sed -n 2p "$sig" | tr -d '\r\n' | "$OSSL" base64 -d -A > "$d/sig.bin" || return 1
  [ "$(wc -c < "$d/sig.bin" | tr -d ' ')" = 74 ] || return 1
  [ "$(head -c 2 "$d/sig.bin")" = "Ed" ] || { warn "unsupported minisign signature algorithm"; return 1; }
  head -c 10 "$d/pk.bin" | tail -c 8 > "$d/pk.id"
  head -c 10 "$d/sig.bin" | tail -c 8 > "$d/sig.id"
  cmp -s "$d/pk.id" "$d/sig.id" || { warn "manifest signed by an unknown key"; return 1; }
  tail -c 32 "$d/pk.bin" > "$d/pk.raw"
  { printf '\060\052\060\005\006\003\053\145\160\003\041\000'; cat "$d/pk.raw"; } > "$d/pk.der"
  "$OSSL" pkey -pubin -inform DER -in "$d/pk.der" -out "$d/pk.pem" 2>/dev/null || return 1
  tail -c 64 "$d/sig.bin" > "$d/sig.raw"
  "$OSSL" pkeyutl -verify -pubin -inkey "$d/pk.pem" -rawin -in "$msg" -sigfile "$d/sig.raw" >/dev/null 2>&1 || return 1
  tc="$(sed -n 3p "$sig" | tr -d '\r')"
  case "$tc" in "trusted comment: "*) ;; *) return 1 ;; esac
  { cat "$d/sig.raw"; printf '%s' "${tc#trusted comment: }"; } > "$d/global.msg"
  sed -n 4p "$sig" | tr -d '\r\n' | "$OSSL" base64 -d -A > "$d/global.sig" || return 1
  "$OSSL" pkeyutl -verify -pubin -inkey "$d/pk.pem" -rawin -in "$d/global.msg" -sigfile "$d/global.sig" >/dev/null 2>&1
}

# manifest_asset_field ASSET KEY reads sha256/size for ASSET from the signed
# manifest (indented JSON written by reach-release).
manifest_asset_field() {
  awk -v a="\"$1\": {" -v k="\"$2\":" '
    index($0, a) { inside=1; next }
    inside && index($0, k) { v=$0; sub(/.*: */, "", v); gsub(/[",[:space:]]/, "", v); print v; exit }
    inside && /}/ { exit }' "$tmpdir/manifest.json"
}

log "Source code: $SOURCE_URL"
log "Downloading reach-agent $REACH_AGENT_VERSION for $os/$arch."
fetch "$base_url/$asset" "$tmpdir/reach-agent" 134217728
actual="$(sha256_of "$tmpdir/reach-agent")"
verified=""
OSSL=""
if have minisign || OSSL="$(find_ed25519_openssl)"; then
  fetch "$base_url/manifest.json" "$tmpdir/manifest.json" 1048576
  fetch "$base_url/manifest.json.minisig" "$tmpdir/manifest.json.minisig" 16384
  if have minisign; then
    minisign -Vm "$tmpdir/manifest.json" -x "$tmpdir/manifest.json.minisig" -P "$REACH_RELEASE_PUBKEY" -q || die "release manifest signature verification failed"
    verified="minisign"
  else
    verify_minisign_openssl "$tmpdir/manifest.json" "$tmpdir/manifest.json.minisig" || die "release manifest signature verification failed"
    verified="Ed25519 via $OSSL"
  fi
  grep -Eq '^  "project": "reach-agent",?$' "$tmpdir/manifest.json" || die "signed manifest is not for reach-agent"
  grep -Fq "\"version\": \"$REACH_AGENT_VERSION\"," "$tmpdir/manifest.json" || die "signed manifest is for a different version"
  expected="$(manifest_asset_field "$asset" sha256)"
  size="$(manifest_asset_field "$asset" size)"
  [ -n "$expected" ] && [ -n "$size" ] || die "signed manifest has no entry for $asset"
  [ "$(wc -c < "$tmpdir/reach-agent" | tr -d ' ')" = "$size" ] || die "reach-agent size does not match the signed manifest"
  [ "$actual" = "$expected" ] || die "reach-agent checksum does not match the signed manifest"
  log "Verified reach-agent against the signed release manifest ($verified)."
elif [ -n "$REACH_PINNED_SHA256S" ] && [ "$REACH_PINNED_VERSION" = "$REACH_AGENT_VERSION" ]; then
  expected=""
  for pin in $REACH_PINNED_SHA256S; do
    [ "${pin%%=*}" = "$asset" ] && expected="${pin#*=}"
  done
  [ -n "$expected" ] || die "setup.sh has no pinned hash for $asset"
  [ "$actual" = "$expected" ] || die "reach-agent checksum does not match the hash pinned in setup.sh"
  log "Verified reach-agent against the hash pinned in setup.sh (no Ed25519 verifier on this machine)."
elif [ "${REACH_ALLOW_UNVERIFIED:-0}" = 1 ]; then
  warn "REACH_ALLOW_UNVERIFIED=1: checking only the same-origin checksums.txt."
  fetch "$base_url/checksums.txt" "$tmpdir/checksums.txt" 65536
  expected="$(awk -v f="$asset" '$2 == f {print $1}' "$tmpdir/checksums.txt")"
  [ -n "$expected" ] && [ "$actual" = "$expected" ] || die "reach-agent checksum mismatch"
else
  die "Cannot verify the reach-agent release signature: install minisign or OpenSSL 3, or set REACH_ALLOW_UNVERIFIED=1 if you trust this server."
fi
chmod +x "$tmpdir/reach-agent"

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
