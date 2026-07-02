# Reach

Reach is a self-hosted reverse SSH tunnel manager for getting back into machines that cannot accept inbound connections.

In plain terms: a target machine runs a small `reach-agent`, the agent opens an outbound SSH tunnel to your hub server, and you connect back through that hub with normal `ssh`. The target does **not** need a public IP address, router port forwarding, or inbound firewall rules.

Reach is designed to be transparent. If someone asks “what did that setup command install on my machine?”, this README and the source should let them answer that.

> Status: early/alpha. The public repo is useful and self-hostable, but expect rough edges.

---

## What Reach is for

Reach is useful when you want SSH access to Linux or macOS machines that live behind NAT, campus/company networks, home routers, mobile hotspots, or other restrictive networks.

Typical flows:

- **Remote support:** a friend runs the setup command, you approve the request in the dashboard, then you can SSH in.
- **Your own machines:** you run setup with a pre-authorized “god code” and skip the dashboard approval step.
- **Fleet visibility:** the dashboard shows which machines are reachable, degraded, offline, disabled, or pending approval.

Reach is not a VPN. It exposes one SSH path per approved machine through a hub you control.

---

## How it works

Reach has three main pieces:

```text
Operator laptop
  └─ ssh <machine-slug>
       │
       ▼
Hub server
  ├─ reachd API + provisioner
  ├─ OpenSSH reverse-tunnel accounts
  ├─ SQLite state database
  ├─ static Nuxt dashboard
  └─ optional WebSocket carrier
       │
       ▼
Target Linux/macOS machine
  ├─ reach-agent service
  ├─ outbound SSH tunnel to the hub
  └─ local sshd / user sshd / internal Go SSH server
```

### Registration and approval

1. The target runs `setup.sh`.
2. `setup.sh` downloads `reach-agent`, verifies its SHA-256 checksum, and runs `reach-agent install`.
3. The agent registers with the hub API.
4. If a valid auth code is provided, the request is approved immediately.
5. Otherwise, the request waits in the dashboard until an operator approves or denies it.
6. After approval, the agent receives a one-time setup token, provisions the machine, installs keys/config, and starts its background service.

### Tunnel path

The target agent connects **outbound** to the hub’s SSH service and asks OpenSSH to listen on a loopback-only port on the hub:

```text
hub:127.0.0.1:<assigned-port>  →  target:127.0.0.1:<local-ssh-port>
```

The operator’s generated SSH config then connects through a `ProxyJump` host and reaches that loopback port:

```sshconfig
Host my-machine
    HostName localhost
    Port 9200
    User alice
    ProxyJump reach-hub
    HostKeyAlias reach-m_<machine-id>
    StrictHostKeyChecking accept-new
```

---

## What gets installed on a target machine

The setup script supports two install modes.

| Mode | When used | Main paths |
|---|---|---|
| System mode | Run as root, or with accepted passwordless sudo | Linux: `/opt/reach`, `/etc/reach`, `/var/lib/reach`, `/etc/systemd/system/reach-agent.service`; macOS: same Reach paths with `/Library/LaunchDaemons/dev.arthurlin.reach-agent.plist` |
| User mode | No root/sudo | Linux: `~/.local/bin/reach-agent`, `~/.config/reach`, `~/.local/share/reach`, `~/.config/systemd/user/reach-agent.service`; macOS: same Reach paths with `~/Library/LaunchAgents/dev.arthurlin.reach-agent.plist` |

### Files and directories

System install typically creates:

```text
/opt/reach/reach-agent          # target agent binary
/etc/reach/agent.yaml           # machine config; includes the agent bearer token
/etc/reach/known_hosts          # pinned hub SSH host keys
/etc/reach/install.env          # uninstall/repair metadata
/var/lib/reach/tunnel_key       # target private key for the hub tunnel
/var/lib/reach/tunnel_key.pub
/var/lib/reach/agent.log
/var/lib/reach/tunnel.log
```

User install uses the same structure under:

```text
~/.local/bin/reach-agent
~/.config/reach/agent.yaml
~/.config/reach/known_hosts
~/.config/reach/install.env
~/.local/share/reach/tunnel_key
~/.local/share/reach/agent.log
```

Depending on the target’s SSH setup, user mode may also create:

```text
~/.local/share/reach/user-sshd/       # private user-mode OpenSSH config/key/logs
~/.local/share/reach/internal-sshd/   # pure-Go fallback SSH server config/key/logs
```

### Services and persistence

Reach tries persistence in this order:

- Linux system install: systemd service, then root crontab, then detached process fallback;
- Linux user install: user systemd service, then user crontab, then detached process fallback;
- macOS system install: LaunchDaemon, then detached process fallback;
- macOS user install: LaunchAgent, then detached process fallback.

Service names/files include:

```text
reach-agent.service
/etc/systemd/system/reach-agent.service
~/.config/systemd/user/reach-agent.service
```

Older installs named `reach-tunnel.service` are disabled/removed during install or uninstall.

### SSH keys touched

Reach writes operator public keys into the target account’s `authorized_keys` file.

- Existing non-Reach key lines are preserved.
- Reach-managed lines are marked with `reach:<machine-id>`.
- For normal OpenSSH targets, Reach adds conservative restrictions such as loopback-only `from="127.0.0.1,::1"` and disables agent/X11/port forwarding for those operator keys.
- For the internal Go SSH server fallback, Reach writes bare public keys with the same `reach:<machine-id>` marker because restrictions are enforced by the server itself.

Reach does **not** copy private operator SSH keys to the target. The target’s tunnel private key is generated locally and only the public key is sent to the hub.

### Local SSH fallback modes

Reach needs something listening on the target loopback address so the reverse tunnel has a local destination.

It tries:

1. **Existing system sshd** on `127.0.0.1:22`.
2. **System-managed sshd** in system mode; on apt-based Linux systems it may install `openssh-server` if needed, and on macOS it can enable Remote Login via `systemsetup`/launchd when run with sufficient privileges.
3. **User-mode sshd** if an `sshd` binary exists but root is unavailable.
4. **Internal Go SSH server** if no usable sshd exists. This is shell-only, loopback-only, public-key-only, and has no SFTP/agent/X11/port forwarding.

### Network calls from the target

A target agent talks to the configured Reach API URL for:

- registration: `POST /api/client/register`;
- approval polling: `POST /api/client/register/<id>/poll`;
- provisioning: `POST /api/client/provision`;
- heartbeat and command reconciliation: `POST /api/client/agent/heartbeat`;
- uninstall notifications: `POST /api/client/agent/uninstall-*`.

It also opens an outbound SSH connection to the hub SSH port. If configured, it may use the WebSocket carrier over HTTPS/WSS when direct SSH is blocked.

---

## Installing a target

For a normal friend/support flow:

```bash
curl -fsSL https://tunnels.example.com/setup.sh | bash
```

Non-interactive examples:

```bash
# Use a pre-authorized code and default prompts.
curl -fsSL https://tunnels.example.com/setup.sh | bash -s -- \
  --name my-laptop \
  --target-user alice \
  --token '<one-time-or-god-code>' \
  --yes

# Force WebSocket carrier policy.
curl -fsSL https://tunnels.example.com/setup.sh | bash -s -- --transport websocket --yes
```

Repair or update an existing install:

```bash
curl -fsSL https://tunnels.example.com/setup.sh | bash -s -- --repair
curl -fsSL https://tunnels.example.com/setup.sh | bash -s -- --repair --update-agent --version 0.1.0-alpha
```

---

## Uninstalling from a target

Preferred:

```bash
curl -fsSL https://tunnels.example.com/setup.sh | bash -s -- --uninstall
```

Direct commands:

```bash
# System install
sudo /opt/reach/reach-agent uninstall --mode system --yes

# User install
~/.local/bin/reach-agent uninstall --mode user --yes
```

Uninstall stops Reach services/processes, removes Reach-managed authorized-key lines, removes local Reach config/data directories, removes the agent binary, and notifies the hub when possible so the hub can retire the machine and clean up tunnel credentials.

If the target is offline or already gone, use the dashboard “Remove” action to disable hub-side tunnel auth and retire the machine record.

---

## Dashboard

The dashboard is a static Nuxt SPA served by your hub/reverse proxy. It uses the Go API under `/api`.

It provides:

- admin login;
- pending request approval/denial;
- fleet overview and machine status;
- machine diagnostics from agent heartbeats;
- enable/disable/remove actions;
- generated SSH config;
- live updates over Server-Sent Events;
- optional process-title messages for target agents.

Open it at your configured public URL, for example:

```text
https://tunnels.example.com/
```

---

## Operator SSH config sync

Reach exposes generated SSH config at:

```text
GET /api/admin/ssh-config
```

The generated config assumes you already have a `ProxyJump` alias for the hub, usually matching `default_hub.proxyjump_alias`:

```sshconfig
Host reach-hub
    HostName hub.example.com
    Port 443
    User your-hub-user
    IdentityFile ~/.ssh/id_ed25519
```

Then include the Reach-managed file:

```sshconfig
Include ~/.ssh/reach-tunnels.conf
```

### One-shot Mac sync script

```bash
scripts/reach-sync-mac.sh --api-url https://tunnels.example.com --login
```

The script logs in, exchanges the temporary admin JWT for a `mac-agent` service token, writes `~/.ssh/reach-tunnels.conf`, and adds the `Include` line.

### Long-running Mac agent

`reach-mac-agent` does the same sync and also watches `/api/admin/events` so SSH config updates shortly after machines change state.

```bash
reach-mac-agent sample-config > ~/.config/reach/mac-agent.yaml
reach-mac-agent run --once
reach-mac-agent run
```

---

## Self-hosting

### Prerequisites

On the hub server:

- Linux with root access;
- OpenSSH server;
- Go 1.22+;
- Node.js 22+ and npm for the dashboard;
- nginx/Caddy/another reverse proxy with TLS;
- a public DNS name for the dashboard/API;
- a public SSH endpoint for the hub tunnel service.

If HTTPS and SSH share one public IP, do not bind both to the same TCP port unless you intentionally run a multiplexer. Use separate ports/hosts, or configure your network accordingly.

### 1. Build binaries

```bash
git clone https://github.com/Yan-Yu-Lin/reach.git
cd reach

mkdir -p bin
CGO_ENABLED=0 go build -o bin/reachd .
CGO_ENABLED=0 go build -o bin/reach-agent ./cmd/reach-agent
CGO_ENABLED=0 go build -o bin/reach-mac-agent ./cmd/reach-mac-agent
CGO_ENABLED=0 go build -o bin/reach-ws-carrier ./cmd/reach-ws-carrier
```

For hosted target downloads, build the Linux agent for each architecture you want to support:

```bash
VERSION=0.1.0-alpha
OUT=/var/lib/reach/downloads/reach-agent/v${VERSION}
sudo mkdir -p "$OUT"

GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /tmp/reach-agent_linux_amd64 ./cmd/reach-agent
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /tmp/reach-agent_linux_arm64 ./cmd/reach-agent
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /tmp/reach-agent_darwin_amd64 ./cmd/reach-agent
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /tmp/reach-agent_darwin_arm64 ./cmd/reach-agent

sudo install -m 0755 /tmp/reach-agent_linux_* /tmp/reach-agent_darwin_* "$OUT/"
(cd "$OUT" && sudo sha256sum reach-agent_* | sudo tee checksums.txt >/dev/null)
```

### 2. Install hub files

```bash
sudo mkdir -p /opt/reach /etc/reach /var/lib/reach /opt/reach-dashboard
sudo install -m 0755 bin/reachd /opt/reach/reachd
sudo install -m 0755 bin/reach-ws-carrier /opt/reach/reach-ws-carrier
sudo install -m 0755 bin/reach-agent /opt/reach/reach-agent
sudo install -m 0644 setup.sh /var/lib/reach/setup.sh
```

Edit `/var/lib/reach/setup.sh` so the top-level `API_URL` and `REACH_AGENT_VERSION` match your deployment.

### 3. Configure Reach

```bash
sudo cp config.example.yaml /etc/reach/config.yaml
```

Generate secret hashes:

```bash
printf 'choose-a-long-admin-password' | /opt/reach/reachd hash-secret
printf 'choose-a-long-god-code' | /opt/reach/reachd hash-secret
openssl rand -base64 32
```

Put the password hash, god-code hash, JWT secret, hub host keys, public API URL, public SSH host/port, and admin SSH public keys into `/etc/reach/config.yaml`.

Make sure your OpenSSH server includes the configured drop-in directory, for example:

```sshconfig
Include /etc/ssh/sshd_config.d/*.conf
```

Reach’s provisioner creates restricted Unix users and writes `Match User` blocks there.

### 4. Run `reachd` under systemd

`reachd` must run with enough privilege to create tunnel users, write sshd config, and reload sshd when `provisioning_enabled: true`.

```ini
# /etc/systemd/system/reachd.service
[Unit]
Description=Reach API and provisioner
After=network-online.target ssh.service sshd.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/reach/reachd serve --config /etc/reach/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now reachd
```

### 5. Build and deploy the dashboard

```bash
cd dashboard
npm install
npx nuxt generate
sudo rsync -a --delete .output/public/ /opt/reach-dashboard/
```

### 6. Reverse proxy

Example nginx shape:

```nginx
server {
    listen 443 ssl http2;
    server_name tunnels.example.com;

    root /opt/reach-dashboard;

    location /api/ {
        proxy_pass http://127.0.0.1:9300/api/;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off; # important for /api/admin/events SSE
    }

    location = /setup.sh {
        alias /var/lib/reach/setup.sh;
    }

    location /downloads/reach-agent/ {
        alias /var/lib/reach/downloads/reach-agent/;
    }

    location / {
        try_files $uri $uri/ /index.html;
    }
}
```

Optional WebSocket carrier reverse proxy:

```nginx
location /ws/tunnel/<long-random-secret>/ {
    proxy_pass http://127.0.0.1:9401/;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
}
```

The carrier path is effectively a shared secret in the current implementation. Use a high-entropy path and TLS.

### 7. Optional WebSocket carrier service

```ini
# /etc/systemd/system/reach-ws-carrier.service
[Unit]
Description=Reach WebSocket SSH carrier
After=network-online.target ssh.service sshd.service
Wants=network-online.target

[Service]
Type=simple
ExecStart=/opt/reach/reach-ws-carrier server --listen 127.0.0.1:9401 --target 127.0.0.1:22
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Enable it only if you configure `websocket_tunnel` in `/etc/reach/config.yaml`.

---

## Hub-side provisioning details

For each target tunnel, the hub provisioner:

- allocates a port from `default_hub.port_range_start` / `port_range_end`;
- creates a restricted Unix user like `rt-abcdefgh`;
- writes that user’s `authorized_keys` with reverse-forwarding restrictions;
- writes an sshd `Match User` drop-in;
- validates sshd config with `sshd -t`;
- reloads sshd;
- records machine, tunnel, request, event, heartbeat, and command state in SQLite.

The tunnel user is not a shell account. Its sshd config disables TTYs, shells, local forwarding, agent forwarding, X11, password auth, and arbitrary commands.

---

## Security model

Reach’s security relies on a few clear boundaries:

- **Admin web auth:** username/password login returns an HS256 JWT. Passwords are Argon2id-hashed.
- **God code / setup tokens:** auth codes, client secrets, and setup tokens are hashed server-side. Setup tokens are short-lived and consumed once.
- **Agent auth:** each machine gets an agent bearer token stored in `agent.yaml`; it authenticates heartbeats and uninstall notifications.
- **Tunnel auth:** each target generates its own SSH keypair. The private key stays on the target. The hub only gets the public key.
- **Hub host key pinning:** targets receive `hub_host_keys` and write them to `known_hosts` before opening the tunnel.
- **Hub sshd restrictions:** tunnel accounts can only create the assigned loopback reverse listener.
- **Operator access:** operator public keys are installed on the target account; operator private keys stay on operator machines.

Trust assumptions:

- You trust the hub server and its root/admin operators.
- Target owners trust the configured operator SSH keys.
- Compromise of a target can reveal that target’s agent token and tunnel private key.
- Compromise of an operator private key grants SSH access to reachable targets.
- The generated operator SSH config uses `StrictHostKeyChecking accept-new` with a stable `HostKeyAlias`, so target SSH host keys are trust-on-first-use unless you manage known_hosts separately.
- Hosted agent downloads are checksum-verified, but the checksums are served by the same hub; add signed releases if your threat model requires stronger supply-chain guarantees.

---

## Configuration reference

Start from:

```bash
cp config.example.yaml /etc/reach/config.yaml
```

Important fields:

| Field | Purpose |
|---|---|
| `listen_addr` | Local-only address for `reachd`; must be localhost. |
| `db_path` | SQLite database path. |
| `god_code_hash` | Argon2id hash of a pre-authorized setup code. |
| `initial_admin` | First dashboard user and public SSH keys. |
| `jwt_secret` | Secret used to sign admin JWTs. |
| `hub_host_keys` | Host key pins returned to targets. |
| `default_hub.public_host` / `ssh_port` | SSH endpoint targets connect to. |
| `default_hub.api_url` | Public dashboard/API URL. |
| `default_hub.port_range_*` | Loopback reverse-tunnel port pool. |
| `websocket_tunnel` | Optional WSS carrier fallback. |

---

## Development notes

Useful commands:

```bash
# Run API locally with a config file
go run . serve --config ./config.local.yaml

# Print an Argon2id hash
printf 'secret' | go run . hash-secret

# Generate dashboard locally
cd dashboard && npm install && npm run dev

# Build agent
go build ./cmd/reach-agent
```

The repository also contains design notes:

- [`DESIGN.md`](DESIGN.md)
- [`ARCHITECTURE.md`](ARCHITECTURE.md)
- [`EMBEDDED_SSHD_DESIGN.md`](EMBEDDED_SSHD_DESIGN.md)

---

## License

No `LICENSE` file is present yet. The project owner should choose and add a license before others rely on redistribution rights. TBD.
