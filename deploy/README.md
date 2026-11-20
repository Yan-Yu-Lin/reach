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
