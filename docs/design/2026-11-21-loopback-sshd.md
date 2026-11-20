# Design: loopback-only SSH on targets (daybreak P1-5)

Status: minimal fix shipped 2026-11-21; full design below not implemented.

## Problem

Reach only needs an SSH server on `127.0.0.1` of the target. System installs
used to install `openssh-server` (Linux), enable Remote Login (macOS) or
install Windows OpenSSH Server, and enable them at boot. Those listen on all
interfaces, so Reach could add a LAN/public port 22 that bypasses Reach's
disable/retire and audit.

## Shipped now

- Linux/macOS system install: if an sshd already answers on `127.0.0.1:22`
  it is reused and the installer warns when it also listens beyond loopback
  (`ss` / `lsof`). If none answers, the agent's built-in internal-sshd is used
  on a loopback high port. A root agent runs internal-sshd sessions as the
  target user (`--run-as`; root is refused; identity/loader variables from the
  client are dropped). Shell only: no sftp, no port forwarding, no pty on
  Windows.
- The daemon's local-SSH repair only `systemctl start`s an sshd unit that is
  already enabled; it never enables one.
- Windows system install: Windows OpenSSH Server is installed only with
  `--install-openssh` / `setup.ps1 -InstallOpenSSH`; otherwise the install
  stops with instructions (or run without admin for the built-in server).
- Existing installs are unaffected (their agent.yaml already names the mode).

## Full design (not implemented)

Run a dedicated OpenSSH instance per target, loopback only:

- `reach-sshd.service` (Linux) / LaunchDaemon (macOS) with its own config:
  `ListenAddress 127.0.0.1`, high port, `AuthorizedKeysFile` under
  `/var/lib/reach`, `PermitRootLogin no`, `AllowUsers <target>`, host keys in
  `/var/lib/reach/sshd`, sftp enabled.
- Gives full OpenSSH features (sftp, pty, port forwarding policy) without
  touching the system sshd.
- Needs: per-OS unit generation, SELinux/AppArmor labels on RHEL/Ubuntu,
  macOS `sshd-keygen-wrapper` differences, upgrade path from internal-sshd.
- Windows equivalent: a second `sshd.exe` service with its own
  `sshd_config` and `ListenAddress 127.0.0.1`.
