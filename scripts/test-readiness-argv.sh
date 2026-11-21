#!/usr/bin/env bash
# Regression for daybreak round 5 N8: the readiness probe's admin JWT and
# the carrier's secret path must never appear in a process argv (the hub's
# /proc is readable by every local user). A fake curl records its argv and
# its stdin; the secrets must be only in stdin.
# Usage: bash scripts/test-readiness-argv.sh
set -Eeuo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d /tmp/reach-argv-test.XXXXXX)"
trap 'trash "$work" 2>/dev/null || true' EXIT
mkdir -p "$work/bin"
cat > "$work/bin/curl" <<'EOF'
#!/bin/bash
printf '%s\n' "$*" >> "$TEST_WORK/argv"
cat >> "$TEST_WORK/stdin"
case "$*" in *Upgrade*) printf 101 ;; *) printf 200 ;; esac
EOF
chmod +x "$work/bin/curl"
export TEST_WORK="$work"
token="eyJTESTSECRETTOKEN.part.sig"
cat > "$work/fake-reachd" <<EOF
#!/bin/bash
[ "\$1" = admin-probe-token ] && echo "$token"
EOF
chmod +x "$work/fake-reachd"
secret_path="/ws/tunnel/SECRETPATHVALUE123/"
printf 'REACH_WS_PATH_PREFIX=%s\n' "$secret_path" > "$work/ws.env"

PATH="$work/bin:$PATH" bash "$repo/scripts/check-reachd-ready.sh" "$work/fake-reachd" /dev/null 127.0.0.1:9300
PATH="$work/bin:$PATH" bash "$repo/scripts/check-carrier-ready.sh" "$work/ws.env"

fail() { echo "FAIL: $*"; exit 1; }
if grep -q "TESTSECRETTOKEN" "$work/argv"; then fail "admin JWT in curl argv"; fi
if grep -q "SECRETPATHVALUE" "$work/argv"; then fail "carrier path in curl argv"; fi
grep -q "TESTSECRETTOKEN" "$work/stdin" || fail "token not passed on stdin"
grep -q "SECRETPATHVALUE" "$work/stdin" || fail "path not passed on stdin"
echo "PASS: readiness secrets only on stdin"
