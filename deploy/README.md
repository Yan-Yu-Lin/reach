# Deployment configuration (hub)

**Deploy only the exact pushed tip of `release/reach-2026-11`** (backend and
dashboard merged; one deploy swaps reachd, carrier, dashboard and drop-in).
`deploy-from-mac.sh`, the hub side it starts, and a direct
`deploy-jason.sh` run all fetch that branch and refuse unless HEAD equals
`origin/release/reach-2026-11` exactly (an older commit of the branch is
refused too). There is no override (`scripts/release-gate.sh`), and a detached
HEAD is never switched to `main`. For a direct run on the hub:
`git fetch origin && git checkout --detach origin/release/reach-2026-11`
first. The deploy scripts on `fix/dashboard-2026-11` are older: they would
downgrade reachd and publish an agent release. Do not run them, and do not
deploy the dashboard separately.

Stage 4 preconditions: `fuser` (psmisc) installed, and provisioning frozen
(`scripts/preflight-provisioning-freeze.sh` runs automatically and refuses
while any request is pending/approved/provisioning or any provision is in
flight). Do not add or reinstall machines from the fence until Stage 5.

Nothing in this directory is applied by default. Each file states how it is
installed and what to test first.

| File | What | Applied by |
|---|---|---|
| `systemd/reachd.service.d/10-reach-limits.conf` | `MemoryMax=1G`, `MemoryHigh`, `TasksMax=256`, `LimitNOFILE=4096`. Resource ceiling only, **not** privilege containment (reachd stays root) | **installed by default** on every deploy; backed up and restored on failed deploy |
| `systemd/reachd.service` | reachd sandbox (plan A): `ProtectSystem=strict`, capability allowlist, `MemoryMax=1G`, `TasksMax=256` | `deploy-jason.sh` / `deploy-from-mac.sh` only with `REACH_INSTALL_REACHD_UNIT=1`; backed up and restored on failed deploy |
| `nginx/reach-tunnels.conf` | vhost template: gzip, `/_nuxt/` immutable cache, `limit_req` on `/api/client/` and login, `limit_conn` on the carrier route | manual (`nginx -t` then reload) |
| `haproxy/2026-11-21-bk_ssh-conn-limit.suggested.diff` | per-IP SSH connection limits on the public 443 mux | manual, after review |

The WebSocket carrier unit (written by `deploy-jason.sh`) now has
`MemoryMax=256M`, `TasksMax`, `LimitNOFILE`, and reads optional
`/etc/reach/ws-carrier.env` (`REACH_WS_PATH_PREFIX=/ws/tunnel/<secret>/`) so the
carrier enforces the exact path itself.

Syntax checks run on a Mac (2026-11-21): `nginx -t` on the template and
`haproxy -c` on the suggested fragment both pass. `systemd-analyze verify`
needs Linux and runs inside `deploy-jason.sh` before the unit is installed.

Before turning on the sandboxed unit, test once with jason reachable another
way (Headscale or physical): register + provision a test machine, disable,
enable, retire, then `systemd-analyze security reachd`.

## Rollback of a failed reachd deploy (`deploy-jason.sh`)

The steps are the functions between `BEGIN/END reachd transaction steps` in
`scripts/deploy-jason.sh`; `scripts/test-rollback-in-container.sh` runs those
same functions in Ubuntu 24.04 (scenarios A–G).

1. reachd is stopped and the stop is proven (unit inactive, old PID gone,
   nobody holding the DB), then the DB snapshot and the deploy fence
   (`MAX(durable_actions.id)`) are taken.
2. The old binary is copied to `/opt/reach/reachd.rollback.<ts>`; a private
   copy of the new binary goes to
   `/var/lib/reach/deploy-backups/reachd-probe.<ts>` (readiness token and
   `deploy-fence check`; the old binary has neither command).
3. On failure, the snapshot is restored automatically only if: reachd is
   provably stopped, the failed DB/WAL/SHM are fully preserved in
   `backups/failed.<ts>`, and the journal has no row after the fence (no
   operator/enrollment decision, no OS intent, on the new version).
   Otherwise reachd stays stopped and the script prints what is on disk.
4. `/etc/reach/ws-carrier.env`, the carrier unit/binary, the limits
   drop-in and an opt-in reachd unit are restored (or removed if new).

Old fence files (without journal fields) never allow an automatic restore.

### Round 4 changes (fence order, writers lock, JWT secret, carrier)

- `reachd deploy-prepare` clears retired machines' tokens, records the
  fence, writes the snapshot and rotates `jwt_secret` (always, on every
  deploy), all in one process holding
  `<dbdir>/locks/writers.lock` exclusively; `reachd deploy-restore` does the
  forensic copy, fence check and snapshot restore the same way. reachd and every CLI writer
  hold it shared, so CLI writes during that step fail with "deployment in
  progress".
- `fuser` (package `psmisc`) is required.
- Before the new binary first starts, `deploy-prepare` replaces
  `jwt_secret` in config.yaml **unconditionally** (backup in
  `deploy-backups/config.yaml.<ts>`). It no longer infers "already rotated"
  from the DB migration marker: a newer CLI can create that marker while
  the secret is unchanged (daybreak round 6 R6-P1-03). Rollback does not
  restore config.yaml, so pre-deploy admin JWTs stay invalid. Every admin
  logs in again after every deploy; a conditional rotation would need a
  rollback-proof marker bound to the secret itself (not built).
- The carrier must answer a WebSocket upgrade with 101 on its configured
  path (`scripts/check-carrier-ready.sh`), on deploy and on rollback. Any
  rollback step that fails is reported as `rollback_incomplete` and the
  rollback never prints success.
