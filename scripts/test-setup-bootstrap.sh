#!/usr/bin/env bash
# Offline checks for setup.sh bootstrap logic (astra P2-06, P2-11).
# Uses stub curl binaries; makes no network requests and installs nothing.
# Run: bash scripts/test-setup-bootstrap.sh
set -u
cd "$(dirname "$0")/.."
fail=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got $2 want $3"; fail=1; fi; }

# --- validate_api_url, extracted from setup.sh ------------------------------
eval "$(sed -n '/^validate_api_url() {/,/^}/p' setup.sh)"
for good in https://tunnels.arthurlin.dev https://tunnels.arthurlin.dev/ https://h.example:8443/x \
            http://127.0.0.1:9300 http://localhost http://localhost:3000/api 'http://[::1]:9300'; do
  validate_api_url "$good"; check "accept $good" "$?" 0
done
for bad in http://localhost.attacker.invalid http://127.0.0.1.attacker.invalid http://remote.invalid \
           'https://good.example@evil.example' 'http://localhost@evil.example' 'https://evil.example\@good' \
           'https://exa mple.com' ftp://x.example 'https://' 'https://.evil' 'https://a..b' \
           'http://localhost:80x' 'https://h.example?x=1' 'http://[::1].evil'; do
  validate_api_url "$bad"; check "reject $bad" "$?" 1
done

# --- curl feature detection with an old curl ---------------------------------
stub=$(mktemp -d)
cat > "$stub/curl" <<'EOF'
#!/bin/bash
# curl 7.19.7 stand-in: no --proto/--proto-redir/--max-filesize in help;
# rejects those flags like the real one.
case "$1" in
  --help) printf ' -f, --fail\n -L, --location\n -o, --output FILE\n -s, --silent\n'; exit 0 ;;
  --version) echo "curl 7.19.7 (x86_64-redhat-linux-gnu)"; exit 0 ;;
esac
for a in "$@"; do
  case "$a" in --proto|--proto-redir|--max-filesize|-L) echo "curl: option $a: is unknown" >&2; exit 2 ;; esac
done
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
printf 'payload' > "$out"
echo "$*" >> "$(dirname "$0")/argv.log"
EOF
chmod +x "$stub/curl"
fetch_block="$(sed -n '/^# Pick curl flags this curl supports/,/^}/p' setup.sh)"
out=$(PATH="$stub:$PATH" bash -c '
  warn() { :; }; die() { echo "DIE $*"; exit 9; }
  '"$(sed -n '/^validate_api_url() {/,/^}/p' setup.sh)"'
  '"$fetch_block"'
  fetch https://h.example/x "'"$stub"'/o" 100 && echo FETCHED' 2>&1)
check "old curl fetch works without --proto" "$(echo "$out" | tail -1)" FETCHED
check "old curl not given -L/--proto" "$(grep -c -- '--proto\|-L' "$stub/argv.log")" 0
out=$(PATH="$stub:$PATH" bash -c '
  warn() { :; }; die() { echo "DIE $*"; exit 9; }
  '"$(sed -n '/^validate_api_url() {/,/^}/p' setup.sh)"'
  '"$fetch_block"'
  fetch https://h.example/x "'"$stub"'/o" 3' 2>&1)
check "size cap enforced without --max-filesize" "$(echo "$out" | grep -c 'exceeds 3 bytes')" 1
trash "$stub" 2>/dev/null || true

exit $fail
