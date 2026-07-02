# CentOS 6 / kernel 2.6.32 Reach agent smoke test

Goal: prove `reach-agent` still executes and can establish a Reach tunnel on a real Linux 2.6.32 kernel before using step-4/internal-sshd fallback on machines that cannot be physically recovered.

## Important constraints

- A Docker CentOS 6 container is not sufficient. Containers use the host kernel, so they do not test Linux 2.6.32 syscall/runtime compatibility.
- Build the agent with a Go version that still supports Linux 2.6.32. Go's Linux support table lists 2.6.32 support through Go 1.23.x; do not use Go 1.24+ for this binary.
- Prefer a disposable QEMU/KVM VM. The existing `schoollabstuff` / `islabx6` machine is useful for non-destructive execution checks, but not for install/fallback experiments that might disturb a live tunnel.

## Recommended smoke path

1. Create or boot a CentOS 6 x86_64 VM with the stock `2.6.32-*` kernel.
2. Build `reach-agent` on a Linux builder with Go <= 1.23.x:

   ```sh
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
     go build -ldflags "-X main.version=centos6-smoke" \
     -o /tmp/reach-agent_linux_amd64 ./cmd/reach-agent
   ```

3. Copy only the built binary to the VM.
4. On the VM, verify the kernel and basic execution:

   ```sh
   uname -r
   ./reach-agent_linux_amd64 version
   ./reach-agent_linux_amd64 sample-config >/tmp/reach-agent.sample.yaml
   ```

5. Run an isolated user-mode install against a test Reach machine record:

   ```sh
   ./reach-agent_linux_amd64 install \
     --api-url https://tunnels.arthurlin.dev \
     --name centos6-smoke-$(date +%s) \
     --mode user \
     --transport auto \
     --config-dir "$HOME/.config/reach-smoke" \
     --data-dir "$HOME/.local/share/reach-smoke" \
     --agent-path "$PWD/reach-agent_linux_amd64" \
     --yes
   ```

6. Approve the request in the dashboard if using approval mode.
7. From the operator machine, verify the generated alias connects through the hub.
8. For internal-sshd fallback specifically, repeat in a VM state where no system sshd is reachable on `127.0.0.1:22` and no `sshd` binary is installed/in PATH. Confirm the install chooses `internal-sshd` and that shell access works through Reach.
9. Clean up:

   ```sh
   ./reach-agent_linux_amd64 uninstall --mode user --yes
   rm -rf "$HOME/.config/reach-smoke" "$HOME/.local/share/reach-smoke"
   ```

## Minimal live-machine check

For the existing CentOS 6 Reach target, keep checks non-destructive:

```sh
ssh schoollabstuff 'uname -a; command -v ssh-keygen || true; command -v sshd || true'
```

Do not overwrite the live agent on `schoollabstuff` unless explicitly doing a scheduled Reach deploy/rollback test.
