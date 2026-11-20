# Deployment configuration (hub)

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

- `deploy-fence record --snapshot` clears retired machines' tokens, then
  records the fence, then writes the snapshot, all while holding
  `<dbdir>/locks/writers.lock` exclusively. reachd and every CLI writer
  hold it shared, so CLI writes during that step fail with "deployment in
  progress".
- `fuser` (package `psmisc`) is required.
- Before the new binary first starts, `reachd rotate-jwt-secret
  --if-session-migration-pending` replaces `jwt_secret` in config.yaml
  (backup in `deploy-backups/config.yaml.<ts>`). Rollback does not
  restore config.yaml, so pre-deploy admin JWTs stay invalid.
- The carrier must answer a WebSocket upgrade with 101 on its configured
  path (`scripts/check-carrier-ready.sh`), on deploy and on rollback. Any
  rollback step that fails is reported as `rollback_incomplete` and the
  rollback never prints success.
