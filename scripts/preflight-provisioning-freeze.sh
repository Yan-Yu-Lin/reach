#!/usr/bin/env bash
# preflight-provisioning-freeze.sh DB_PATH
#
# Early warning only. This is a point-in-time check while reachd is still
# serving, not a switch that stays on: a client can register in the minutes
# before reachd is stopped. The binding check runs again inside
# `reachd deploy-prepare` (reachd stopped, writers lock held, before fence and
# snapshot; daybreak round 6 R6-P1-02). To keep enrollment closed between
# Stage 4 and Stage 5, set `enrollment_frozen: true` in config.yaml.
#
# Fails closed, like deploy-prepare: only finished states pass (requests
# provisioned/expired/denied with provision_phase NULL, delivered or cleaned;
# no machine in status provisioning). Read-only (sqlite3 -readonly).
set -Eeuo pipefail
db="$1"
q() { sudo sqlite3 -readonly "$db" "$1"; }
has_phase="$(q "SELECT COUNT(*) FROM pragma_table_info('requests') WHERE name='provision_phase'")"
phase_expr=NULL
[ "$has_phase" = 1 ] && phase_expr=provision_phase
open_requests="$(q "SELECT COUNT(*) FROM requests WHERE COALESCE(status,'') NOT IN ('provisioned','expired','denied') OR ($phase_expr IS NOT NULL AND $phase_expr NOT IN ('delivered','cleaned'))")"
open_machines="$(q "SELECT COUNT(*) FROM machines WHERE status='provisioning'")"
if [ "$open_requests" != 0 ] || [ "$open_machines" != 0 ]; then
  echo "[preflight] ERROR: provisioning is not frozen: $open_requests unfinished request(s), $open_machines machine(s) still provisioning" >&2
  echo "[preflight] ERROR: deny them or let them expire and finish, and do not add or reinstall machines until Stage 5" >&2
  exit 1
fi
echo "[preflight] provisioning frozen right now (deploy-prepare checks again after reachd stops)"
