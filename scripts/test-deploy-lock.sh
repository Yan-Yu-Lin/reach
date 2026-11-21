#!/usr/bin/env bash
# Regression for daybreak round 5 N7: the deploy lock helper refuses a
# symlinked lock file and a group/other-writable lock directory, and opens
# a good lock without truncating anything.
# Usage: bash scripts/test-deploy-lock.sh
set -Eeuo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d /tmp/reach-lock-test.XXXXXX)"
trap 'trash "$work" 2>/dev/null || true' EXIT
eval "$(sed -n '/^open_deploy_lock() {/,/^}/p' "$repo/scripts/deploy-jason.sh")"
fail() { echo "FAIL: $*"; exit 1; }

( open_deploy_lock "$work/ok" ) || fail "a private lock directory was refused"

mkdir -p -m 0700 "$work/sym"
printf 'keep me\n' > "$work/victim"
ln -s "$work/victim" "$work/sym/deploy.lock"
if ( open_deploy_lock "$work/sym" ) 2>/dev/null; then fail "symlinked lock file accepted"; fi
grep -q 'keep me' "$work/victim" || fail "symlink target was truncated"

mkdir -p "$work/loose"
chmod 0775 "$work/loose"
if ( open_deploy_lock "$work/loose" ) 2>/dev/null; then fail "group-writable lock directory accepted"; fi

ln -s "$work/ok" "$work/dirlink"
if ( open_deploy_lock "$work/dirlink" ) 2>/dev/null; then fail "symlinked lock directory accepted"; fi
echo "PASS: deploy lock helper refuses unsafe locks"
