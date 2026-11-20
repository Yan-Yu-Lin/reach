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
Sync Reach SSH aliases to ~/.ssh/reach-tunnels.conf using reach-mac-agent
(structured data, fixed template, ssh -G validation).

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

# Login sends the admin password and receives a service token: only over
# https, or plain http to this machine (daybreak round 3 P2-B). Exact host
# match after parsing; prefixes like http://localhost.evil.example fail.
validate_api_url() {
  local rest host
  case "$1" in
    https://*) rest="${1#https://}" ;;
    http://*) rest="${1#http://}" ;;
    *) return 1 ;;
  esac
  rest="${rest%%/*}"
  case "$rest" in *@*|'') return 1 ;; esac
  case "$1" in https://*) return 0 ;; esac
  case "$rest" in
    \[*\]*) host="${rest#[}"; host="${host%%]*}" ;;
    *) host="${rest%%:*}" ;;
  esac
  case "$host" in localhost|127.0.0.1|::1) return 0 ;; esac
  return 1
}
validate_api_url "$API_URL" || die "API URL must be https:// (plain http only for localhost/127.0.0.1/[::1]): $API_URL"
# curl below runs without -L: redirects are not followed.
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

# The SSH config is written only by reach-mac-agent, which renders it from
# structured data with a fixed template and validates it with ssh -G. This
# script used to install the server's raw text as-is, which let a
# compromised hub run commands on the Mac (daybreak P1-2).
AGENT="${REACH_MAC_AGENT:-}"
if [ -z "$AGENT" ]; then
  AGENT="$(command -v reach-mac-agent 2>/dev/null || true)"
fi
[ -z "$AGENT" ] && [ -x "$HOME/.local/bin/reach-mac-agent" ] && AGENT="$HOME/.local/bin/reach-mac-agent"
[ -n "$AGENT" ] && [ -x "$AGENT" ] || die "reach-mac-agent not found; build it with: go build -o ~/.local/bin/reach-mac-agent ./cmd/reach-mac-agent"

CFG="$(mktemp)"
trap 'rm -f "$CFG"' EXIT
include=false
[ "$INSTALL_INCLUDE" = 1 ] && include=true
"$PYTHON" - "$CFG" "$API_URL" "$TOKEN_FILE" "$OUT_FILE" "$SSH_CONFIG" "$include" <<'PY'
import json, sys
cfg, api, token_file, out_file, ssh_config, include = sys.argv[1:7]
# JSON is valid YAML; json.dumps quotes every value safely.
with open(cfg, "w") as f:
    json.dump({"api_url": api, "token_file": token_file, "out_file": out_file,
               "ssh_config_file": ssh_config, "install_include": include == "true"}, f)
PY
if ! "$AGENT" run --once --config "$CFG"; then
  warn "sync failed; if the token was rejected, rerun with --login"
  exit 1
fi
log "Wrote $OUT_FILE via reach-mac-agent"

log "Done. Try: ssh <reach-machine-slug>"
