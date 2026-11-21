#!/usr/bin/env bash
# Regression for daybreak round 6 R6-P1-02: the early shell preflight fails
# closed (unknown phases/statuses refuse), on both the old schema without
# provision_phase and the new one. `sudo` is a pass-through stand-in.
#
# Usage (from the repo root): bash scripts/test-preflight-freeze.sh
set -Eeuo pipefail
repo="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d /tmp/reach-preflight-test.XXXXXX)"
trap 'trash "$work" 2>/dev/null || true' EXIT
mkdir -p "$work/bin"
printf '#!/bin/bash\nexec "$@"\n' > "$work/bin/sudo"
chmod +x "$work/bin/sudo"
export PATH="$work/bin:$PATH"

pf() { bash "$repo/scripts/preflight-provisioning-freeze.sh" "$1" >/dev/null 2>&1; }
expect() { # want(pass|refuse) db label
  if pf "$2"; then got=pass; else got=refuse; fi
  [ "$got" = "$1" ] || { echo "FAIL: $3: expected $1, got $got"; exit 1; }
  echo "ok: $3 -> $got"
}

old="$work/old.db"
sqlite3 "$old" "CREATE TABLE requests (id TEXT PRIMARY KEY, status TEXT DEFAULT 'pending'); CREATE TABLE machines (id TEXT PRIMARY KEY, status TEXT);
INSERT INTO requests VALUES ('a','provisioned'),('b','expired'),('c','denied'); INSERT INTO machines VALUES ('m','online');"
expect pass "$old" "old schema, finished requests"
sqlite3 "$old" "INSERT INTO requests VALUES ('d','approved')"
expect refuse "$old" "old schema, approved request"
sqlite3 "$old" "UPDATE requests SET status='some_new_status' WHERE id='d'"
expect refuse "$old" "old schema, unknown status"
sqlite3 "$old" "DELETE FROM requests WHERE id='d'; UPDATE machines SET status='provisioning'"
expect refuse "$old" "old schema, machine still provisioning"

new="$work/new.db"
sqlite3 "$new" "CREATE TABLE requests (id TEXT PRIMARY KEY, status TEXT, provision_phase TEXT); CREATE TABLE machines (id TEXT PRIMARY KEY, status TEXT);
INSERT INTO requests VALUES ('a','provisioned','delivered'),('b','expired','cleaned'),('c','provisioned',NULL);"
expect pass "$new" "new schema, terminal phases and legacy NULL"
for phase in response_pending cleanup_pending claimed os_created future_phase; do
  sqlite3 "$new" "INSERT INTO requests VALUES ('x','provisioned','$phase')"
  expect refuse "$new" "new schema, phase $phase"
  sqlite3 "$new" "DELETE FROM requests WHERE id='x'"
done
echo "PASS: preflight freeze fails closed"
