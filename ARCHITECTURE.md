# Reach — Architecture & Implementation Plan

## Tech stack

```text
Client setup script: bash/sh (Linux), PowerShell (Windows admin-first)
Target agent:        Go
API + provisioner:   Go (single binary, privileged on the hub)
Dashboard:           Nuxt 4 SPA
Database:            SQLite
Hub:                 your own Linux server reachable by targets
```

## Deployment layout

Example deployment paths:

```text
/opt/reach/reachd               # Go API + provisioner binary
/etc/reach/config.yaml          # deployment-specific config and hashes
/var/lib/reach/reach.db         # SQLite database
/var/lib/reach/setup.sh         # Linux setup script served by the web frontend
/var/lib/reach/setup.ps1        # Windows setup script served by the web frontend
/var/log/reach/                 # logs

/opt/reach-dashboard/           # static dashboard build output
```

System services:

```text
reachd.service                  # API on a localhost-only address
reach-dashboard.service         # optional dashboard server/static frontend
```

Reverse proxy routes:

```text
/api/client/*   -> Reach API
/api/admin/*    -> Reach API
/setup.sh       -> static Linux setup script
/setup.ps1      -> static Windows setup script
everything else -> dashboard
```

## Why Go + Nuxt

- Go owns privileged operations: user creation, sshd config, service reloads, and DB transactions.
- The dashboard is UI only and does not mutate host files directly.
- The API enforces auth itself and does not rely solely on the reverse proxy.
- The target agent is a static Go binary that can run on old Linux distributions.

## Core model

Reach separates five concepts:

1. **Machine** — a managed target device.
2. **Hub** — a bastion server that accepts reverse tunnels.
3. **Tunnel** — a specific connection from a machine to a hub.
4. **User** — an admin/operator account.
5. **Access key** — an SSH public key belonging to an operator device.

This allows multiple hubs, multiple operator keys, and clean revocation without coupling everything to one hostname or one machine name.

## Important config values

Deployment-specific values live in `/etc/reach/config.yaml`, not in source code:

```yaml
default_hub:
  id: primary
  name: Primary Hub
  public_host: your-server-ip-or-hostname
  ssh_port: 443
  proxyjump_alias: reach-hub
  api_url: https://your-domain.example.com
  port_range_start: 9200
  port_range_end: 9499
```

The example config intentionally uses placeholders. Real IPs, domains, host keys, JWT secrets, admin password hashes, and invite/admin codes should be supplied only in private deployment config.

## Database schema overview

Main tables:

- `machines`: identity, slug, target user, desired/observed state, expiry, cleanup state.
- `hubs`: hub host, API URL, SSH port, ProxyJump alias, managed port range.
- `tunnels`: hub Unix user, assigned port, tunnel public key, tunnel state.
- `users`: dashboard/operator accounts with password hashes.
- `access_keys`: operator SSH public keys distributed to targets.
- `requests`: registration requests and short-lived setup tokens.
- `service_tokens`: long-lived dashboard automation tokens.
- `audit_log`: administrative and provisioning events.

## Request/provisioning flow

```text
Target setup script
  -> POST /api/client/register
  <- request_id + client_secret + pending/approved status
  -> poll with client_secret until approved
  <- setup_token
  -> POST /api/client/provision with setup_token + tunnel pubkey
  <- machine, tunnel, hub, agent token, operator keys, hub host keys
```

All raw one-time secrets are returned once and stored hashed server-side.

## Reverse tunnel constraints

The hub provisions one restricted Unix user per tunnel by default. The generated sshd config limits that user to a single loopback reverse listen address and disables shells, TTYs, local forwarding, agent forwarding, X11, password auth, and arbitrary commands.

The target agent connects outward to the hub and exposes the target's local SSH service only on the hub's loopback interface. The operator then connects through the configured ProxyJump entry.

## Target agent responsibilities

- Maintain the reverse SSH tunnel.
- Fall back between direct SSH and optional WebSocket carrier transport.
- Install or run a local SSH server when required by install mode.
- Reconcile desired state from heartbeats.
- Sync operator public keys and remove Reach-managed keys on disable/uninstall.
- Report observed state and capability information to the API.

## Security notes

- Bind API services to localhost behind a reverse proxy.
- Store passwords, invite/admin codes, service tokens, and setup tokens as hashes.
- Never log raw secrets or bearer tokens.
- Pin hub SSH host keys on targets.
- Use per-machine tunnel users for strong revocation and audit boundaries.
- Keep private deployment config, DB files, build outputs, and token files out of git.
