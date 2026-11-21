#!/usr/bin/env bash
# preflight-provisioning-freeze.sh DB_PATH
#
# Stage 4 precondition (daybreak round 5): provisioning is frozen from the
# deploy fence until Stage 5 is done. Refuses (exit 1) when any enrollment
# is in flight: requests pending/approved/provisioning, or provisions in
# response_pending/cleanup_pending (the last two only exist after this
# release). Read-only (sqlite3 -readonly).
set -Eeuo pipefail
db="$1"
q() { sudo sqlite3 -readonly "$db" "$1"; }
has_phase="$(q "SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name='provision_phase'")"
open_requests="$(q "SELECT COUNT(*) FROM requests WHERE status IN ('pending','approved','provisioning')")"
open_phases=0
if [ "$has_phase" = 1 ]; then
  open_phases="$(q "SELECT COUNT(*) FROM requests WHERE provision_phase IN ('response_pending','cleanup_pending','claimed','os_created')")"
fi
if [ "$open_requests" != 0 ] || [ "$open_phases" != 0 ]; then
  echo "[preflight] ERROR: provisioning is not frozen: $open_requests open request(s), $open_phases provision(s) in flight" >&2
  echo "[preflight] ERROR: deny or let them expire, and do not add or reinstall machines until Stage 5" >&2
  exit 1
fi
echo "[preflight] provisioning frozen: no open requests or provisions"
