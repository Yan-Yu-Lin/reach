#!/usr/bin/env bash
set -Eeuo pipefail

API_URL="${REACH_API_URL:-https://tunnels.your-domain.example}"
OUT_FILE="${REACH_SSH_CONFIG:-$HOME/.ssh/reach-tunnels.conf}"
TOKEN_FILE="${REACH_TOKEN_FILE:-$HOME/.config/reach/admin-token}"
SSH_CONFIG="${REACH_MAIN_SSH_CONFIG:-$HOME/.ssh/config}"
FORCE_LOGIN=0
INSTALL_INCLUDE=1

log() { printf '\033[1;34m[reach-sync]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[reach-sync warning]\033[0m %s\n' "$*" >&2; }
die() { printf '\033[1;31m[reach-sync error]\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
  cat <<'USAGE'
Sync Reach SSH aliases to ~/.ssh/reach-tunnels.conf.

On login, this stores a non-expiring mac-agent service token (not a 24h JWT).

Usage:
  reach-sync-mac.sh [--api-url URL] [--out FILE] [--login] [--no-include]

Environment:
  REACH_API_URL          default https://tunnels.your-domain.example
  REACH_SSH_CONFIG       default ~/.ssh/reach-tunnels.conf
  REACH_TOKEN_FILE       default ~/.config/reach/admin-token
  REACH_MAIN_SSH_CONFIG  default ~/.ssh/config
USAGE
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --api-url) API_URL="${2:-}"; shift 2 ;;
    --out) OUT_FILE="${2:-}"; shift 2 ;;
    --token-file) TOKEN_FILE="${2:-}"; shift 2 ;;
    --login) FORCE_LOGIN=1; shift ;;
    --no-include) INSTALL_INCLUDE=0; shift ;;
    --help|-h) usage; exit 0 ;;
    *) die "Unknown argument: $1" ;;
  esac
done

command -v curl >/dev/null 2>&1 || die "curl is required"
PYTHON=""
if command -v python3 >/dev/null 2>&1; then PYTHON=python3; elif command -v python >/dev/null 2>&1; then PYTHON=python; else die "python3/python is required for JSON parsing"; fi

json_get() {
  "$PYTHON" - "$1" "$2" <<'PY'
import json, sys
with open(sys.argv[1]) as f: obj=json.load(f)
for p in sys.argv[2].split('.'):
    if not p: continue
    obj = obj[int(p)] if isinstance(obj, list) else obj.get(p)
    if obj is None:
        sys.exit(1)
print(obj)
PY
}

login() {
  mkdir -p "$(dirname "$TOKEN_FILE")"
  chmod 700 "$(dirname "$TOKEN_FILE")" 2>/dev/null || true
  local username password req resp status
  local username_default="${REACH_USERNAME_DEFAULT:-admin}"
  read -r -p "Reach username [$username_default]: " username
  username="${username:-$username_default}"
  read -r -s -p "Reach password: " password; echo
  req=$(mktemp); resp=$(mktemp)
  trap 'rm -f "$req" "$resp"' RETURN
  REACH_USERNAME="$username" REACH_PASSWORD="$password" "$PYTHON" > "$req" <<'PY'
import json, os
print(json.dumps({"username": os.environ["REACH_USERNAME"], "password": os.environ["REACH_PASSWORD"]}))
PY
  status=$(curl -sS -o "$resp" -w '%{http_code}' -X POST "$API_URL/api/admin/login" -H 'Content-Type: application/json' --data-binary "@$req") || die "Could not reach $API_URL/api/admin/login"
  case "$status" in
    2*) ;;
    *) printf 'Login failed (%s): ' "$status" >&2; cat "$resp" >&2; printf '\n' >&2; exit 1 ;;
  esac
  local admin_token token_req token_resp
  admin_token="$(json_get "$resp" token)"
  token_req=$(mktemp); token_resp=$(mktemp)
  trap 'rm -f "$req" "$resp" "$token_req" "$token_resp"' RETURN
  REACH_SERVICE_LABEL="reach-mac-agent:$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo mac)" "$PYTHON" > "$token_req" <<'PY'
import json, os
print(json.dumps({"label": os.environ["REACH_SERVICE_LABEL"][:64], "role": "mac-agent"}))
PY
  status=$(curl -sS -o "$token_resp" -w '%{http_code}' -X POST "$API_URL/api/admin/service-tokens" -H 'Content-Type: application/json' -H "Authorization: Bearer $admin_token" --data-binary "@$token_req") || die "Could not issue Mac agent service token"
  case "$status" in
    2*) json_get "$token_resp" token > "$TOKEN_FILE" ;;
    *) printf 'Service token issue failed (%s): ' "$status" >&2; cat "$token_resp" >&2; printf '\n' >&2; exit 1 ;;
  esac
  chmod 600 "$TOKEN_FILE"
  log "Saved non-expiring Mac agent service token to $TOKEN_FILE"
}

if [ "$FORCE_LOGIN" = 1 ] || [ ! -s "$TOKEN_FILE" ]; then
  login
fi
TOKEN="$(cat "$TOKEN_FILE")"
[ -n "$TOKEN" ] || die "Token file is empty: $TOKEN_FILE"

mkdir -p "$(dirname "$OUT_FILE")"
chmod 700 "$(dirname "$OUT_FILE")" 2>/dev/null || true
TMP=$(mktemp)
HTTP=$(curl -sS -o "$TMP" -w '%{http_code}' "$API_URL/api/admin/ssh-config" -H "Authorization: Bearer $TOKEN") || die "Could not reach $API_URL/api/admin/ssh-config"
if [ "$HTTP" = 401 ] || [ "$HTTP" = 403 ]; then
  warn "Stored token was rejected; logging in again."
  rm -f "$TOKEN_FILE"
  login
  TOKEN="$(cat "$TOKEN_FILE")"
  HTTP=$(curl -sS -o "$TMP" -w '%{http_code}' "$API_URL/api/admin/ssh-config" -H "Authorization: Bearer $TOKEN") || die "Could not reach $API_URL/api/admin/ssh-config"
fi
case "$HTTP" in
  2*) ;;
  *) printf 'SSH config sync failed (%s): ' "$HTTP" >&2; cat "$TMP" >&2; printf '\n' >&2; exit 1 ;;
esac
install -m 600 "$TMP" "$OUT_FILE"
rm -f "$TMP"
log "Wrote $OUT_FILE"

if [ "$INSTALL_INCLUDE" = 1 ]; then
  mkdir -p "$(dirname "$SSH_CONFIG")"
  touch "$SSH_CONFIG"
  chmod 600 "$SSH_CONFIG" 2>/dev/null || true
  include_line="Include $OUT_FILE"
  if ! grep -Fxq "$include_line" "$SSH_CONFIG" 2>/dev/null; then
    printf '\n%s\n' "$include_line" >> "$SSH_CONFIG"
    log "Added Include line to $SSH_CONFIG"
  fi
fi

log "Done. Try: ssh <reach-machine-slug>"
