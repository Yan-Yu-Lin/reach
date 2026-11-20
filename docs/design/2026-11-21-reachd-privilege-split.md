# Design: split reachd into an unprivileged API and a root helper (plan B)

Status: design only, not implemented (2026-11-21). Both security reviews
(daybreak "reachd 降權", Opus "方案 B") recommend it. This round ships the
systemd sandbox instead (plan A, `deploy/systemd/reachd.service`).

## Why

reachd runs as root, so any bug in HTTP parsing, auth, JSON handling or
SQLite becomes root on jason. Only tunnel-account management needs root, and
that happens a few times a year.

## Shape

```text
nginx ──► reachd (User=reach, no capabilities)
            │  HTTP API, auth, SQLite, SSE, health, updates
            │
            └─ unix socket /run/reach/provisioner.sock (0660 root:reach)
                   │  SO_PEERCRED: only uid reach
                   ▼
              reach-provisionerd (root, CapabilityBoundingSet limited)
```

### Helper RPC surface

It is exactly the `tunnelOS` interface introduced in this branch
(`tunnel_os.go`), plus create:

| RPC | Arguments | Re-validated in the helper |
|---|---|---|
| `Create` | unix user, port, canonical key, expires_at | user `^rt-[a-z0-9]{8}$`, port in configured range and not reserved, key via `ssh.ParseAuthorizedKey` + `MarshalAuthorizedKey`, single line |
| `DisableAuth` | unix user | user regex |
| `EnableAuth` | unix user | user regex |
| `Remove` | unix user | user regex |
| `Residue` | unix user | user regex |
| `DropInPorts` | none | none |

The helper derives every path itself (`/var/lib/reach-tunnels/<user>`,
`/etc/ssh/sshd_config.d/90-<user>.conf`). It never accepts a path, a binary,
a service name or a shell fragment. Every call is idempotent and returns an
error on partial failure, as `systemTunnelOS` already does.

Wire format: one JSON request and one JSON response per connection, 64 KiB
max, 30 s deadline. Socket activation means the helper only runs during
operations.

### Units

- `reachd.service`: `User=reach`, `CapabilityBoundingSet=`,
  `ProtectSystem=strict`, `ReadWritePaths=/var/lib/reach /var/log/reach`,
  `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK` (ss needs
  netlink).
- `reach-provisioner.socket` + `reach-provisioner.service`: root,
  `CapabilityBoundingSet=CAP_CHOWN CAP_DAC_OVERRIDE CAP_FOWNER CAP_KILL CAP_SETUID CAP_SETGID CAP_AUDIT_WRITE`,
  `PrivateNetwork=yes`, `RestrictAddressFamilies=AF_UNIX`,
  `ReadWritePaths=/etc /var/lib/reach-tunnels` (useradd rewrites
  /etc/passwd via rename, so the directory must be writable).

Full drafts are in the daybreak report (section "reachd 降權與 systemd sandbox
建議").

### Migration

1. Move tunnel homes from `/var/lib/rt-*` to `/var/lib/reach-tunnels/rt-*`
   (`usermod -d -m`, one account at a time, tunnels reconnect).
   `validateHomePath` accepts both during migration.
2. Create the `reach` user. Hand ownership of `/var/lib/reach` (DB, downloads)
   to it; `/etc/reach/config.yaml` becomes `root:reach 0640`.
3. reachd gains `provisioner_socket` in its config. When it is set, reachd uses
   an RPC-backed `tunnelOS`; otherwise it uses `systemTunnelOS` (current
   behaviour, kept for one-binary installs).
4. Health checks (`ss`, `ssh-keyscan`) already need no root.

### Effort and risk

About 400 lines of Go and three unit files. The risky part is the home
migration and the sandbox on the helper (`useradd` behaviour under
`ProtectSystem`). Test both on a VM before jason, and only while Arthur can
reach jason physically or through a non-Reach path.
