# Reach

Reach is a self-hosted reverse SSH tunnel manager. It lets an operator approve a machine registration, install a small target agent, and then connect back to the target through a hub server without opening inbound ports on the target network.

## What it does

- Runs a Go API/provisioner on your hub server.
- Serves a Nuxt dashboard for approvals, machine state, and SSH config sync.
- Installs a Linux target agent with system or user-mode persistence.
- Creates loopback-only reverse SSH tunnels through the hub.
- Stores runtime state in SQLite.

## Configuration

Copy `config.example.yaml` to your deployment host and replace the placeholder values with your own domain, hub address, admin password hash, JWT secret, and SSH public keys.

```bash
cp config.example.yaml /etc/reach/config.yaml
```

Secrets and local runtime state should stay out of git. See `.gitignore` for ignored local config, database, and build-output paths.

## Transparency

This repository is public so people running Reach can inspect the source and verify what the setup script and agent do. The example config uses placeholders; a real deployment should provide its own `/etc/reach/config.yaml`.
