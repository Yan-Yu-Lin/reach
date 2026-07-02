# Reach — Design Overview

Reach is a self-hosted reverse SSH tunnel manager. A target machine runs the setup script, the operator approves it in the dashboard, and the target maintains an outbound reverse tunnel to a hub server.

## Use cases

1. **Remote support:** a user runs the setup script, the operator approves the request, and the operator can SSH through the hub.
2. **Owned machines:** the operator runs setup with an admin/invite token and skips manual approval.
3. **Repeatable onboarding:** new Linux machines can be added with the same flow and managed from one dashboard.

## Architecture

```text
https://your-domain.example.com
├── Nuxt dashboard + static setup script
│   ├── pending registration approvals
│   ├── live tunnel status
│   ├── machine enable/disable/remove actions
│   └── SSH config endpoint for operator machines
└── Go API/provisioner
    ├── validates registration/auth tokens
    ├── creates restricted tunnel Unix accounts on the hub
    ├── writes sshd Match blocks and authorized_keys entries
    ├── allocates reverse-tunnel ports from SQLite
    └── records machine/tunnel/audit state
```

## Auth modes

### Approval mode

1. Target runs the setup script.
2. Target submits a name and metadata to the API.
3. The API returns a request ID and client secret.
4. Target polls until the operator approves or denies the request.
5. Approval mints a short-lived setup token for provisioning.

### Token mode

The operator can provide a pre-authorized invite/admin token to skip the approval wait. Tokens are verified server-side and should be high entropy. Long-lived secrets are stored as hashes only.

## Target setup flow

```text
curl -fsSL https://your-domain.example.com/setup.sh | bash

1. Detect Linux/macOS environment and install mode.
2. Collect machine name, target user, and auth token or approval request.
3. Generate a tunnel keypair locally.
4. Send only the tunnel public key to the API.
5. Receive assigned hub port, tunnel account, host key pins, and operator public keys.
6. Install operator public keys into the selected target account.
7. Install and start the reach-agent service or user service.
8. Agent keeps the reverse tunnel and heartbeat alive.
```

The target machine's tunnel private key never leaves the target. The operator's private SSH key never leaves the operator's machine.

## Hub-side provisioning flow

```text
1. Validate setup token or approved request.
2. Sanitize the requested slug.
3. Allocate a port from the configured pool in a SQLite transaction.
4. Create a restricted hub Unix user for the tunnel.
5. Write authorized_keys with port-forwarding restrictions.
6. Write a Match User sshd config block.
7. Validate sshd config and reload sshd.
8. Persist machine/tunnel state and return the provisioning payload.
```

Important sshd constraints:

```text
AllowTcpForwarding remote
PermitListen 127.0.0.1:<assigned-port>
PermitOpen none
GatewayPorts no
PermitTTY no
PermitTunnel no
AllowAgentForwarding no
X11Forwarding no
PasswordAuthentication no
KbdInteractiveAuthentication no
ForceCommand /bin/false
```

## Key model

- **Tunnel key:** generated on the target. The public key authenticates the target to the hub tunnel account.
- **Operator access key:** configured on the hub and installed on the target account so the operator can SSH through the reverse tunnel.
- **Hub host keys:** pinned on the target to prevent connecting to an unexpected hub.

## SSH config sync

Operator machines can fetch generated SSH config entries from the dashboard API and include them from their main SSH config:

```sshconfig
Include ~/.ssh/reach-tunnels.conf
```

Generated entries use the configured `proxyjump_alias`, target slug, target user, and assigned port.

## Runtime state

SQLite is the source of truth for machines, tunnels, users, access keys, requests, service tokens, and audit events. The target agent reports heartbeat and observed state; the server computes the dashboard and generated SSH config from that state.
