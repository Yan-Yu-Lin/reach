# Embedded SSH Server (self-exec `internal-sshd`) — Implementation Design

Status: **approved for implementation.** This is the Phase D step-4 last-resort local SSH server for Reach.

Audience: the implementing agent (pi). This is a build spec, not a review. Read the referenced code before writing.

## Decision (already made — do not relitigate)

When a target machine has **no usable sshd, no sudo, and no existing `sshd` binary on disk**, Reach's last resort is a local SSH server. We are **not** shipping Dropbear or OpenSSH. Instead, `reach-agent` (already on the box, already running the tunnel) **re-execs itself** in a dedicated `internal-sshd` mode as a supervised child process, using a pure-Go SSH server.

Why this over shipping a C binary (context for the implementer, so you make consistent choices):
- Zero extra download / checksum / per-arch matrix / hosting surface — the child *is* `reach-agent`.
- Zero glibc/NSS/`getpwnam`/static-linking pain — the server does its own pubkey auth and never resolves a system user.
- Runtime already proven on the target — if `reach-agent` runs there, the server runs there (no separate "is Dropbear ok on kernel 2.6.32" question).
- Crash isolation — it's a **separate process**, so a panic / OOM / fd-exhaustion in SSH handling kills the child, not the tunnel+heartbeat in the parent. The parent supervises and restarts it.
- One process *name* in `ps` (two `reach-agent` procs, both settable via existing process-title machinery), closer to the "one thing visible" goal than `reach-agent` + `dropbear`.

## Key invariant that makes this simple: step 4 is always single-user, no-privilege

Confirmed from code, rely on it:
- Step 4 is only reachable on the **no-sudo path**. In `prepareLocalSSH` (`cmd/reach-agent/install.go:371`), the sudo branch (`opt.InstallMode == "system"`) installs a system sshd; steps 3/4 are the `else` (user mode) only.
- User mode is hard-locked to the current user: `install.go:181-182` rejects user-mode installs where `TargetUser != currentUsername()` (*"user-mode install can only configure the current user"*).
- User-mode persistence runs the daemon as that same user (`systemd --user` / cron / detached — `install.go:849+`, `install.go:940`).

**Therefore the internal sshd never needs privilege escalation, never needs `setuid`, never needs to resolve a different user.** It runs as the current user and execs that user's shell as itself. A non-root SSH server physically cannot log you in as anyone else anyway — same model the existing `user-sshd` path already relies on.

---

## 1. Self-exec subprocess model

### 1a. New subcommand in the dispatch

`run(ctx, args)` at `cmd/reach-agent/main.go:264` switches on `args[0]` (`install`, `run`, `daemon`, `check`, `ws-client`, …). Add:

```go
case "internal-sshd":
    return internalSSHDCommand(ctx, args[1:])
```

`internalSSHDCommand` parses a small, **non-secret** flag set (nothing sensitive on argv; the host *private* key lives in a file, only its path is passed):

```go
func internalSSHDCommand(ctx context.Context, args []string) error {
    fs := flag.NewFlagSet("internal-sshd", flag.ContinueOnError)
    port     := fs.Int("port", 0, "loopback TCP port to bind")
    authFile := fs.String("auth-file", "", "authorized_keys path (bare pubkeys)")
    hostKey  := fs.String("host-key", "", "server host private key path (PEM, ed25519)")
    shell    := fs.String("shell", "", "login shell to exec (optional; auto-detected)")
    if err := fs.Parse(args); err != nil { return err }
    // validate port in [1024,65535], authFile/hostKey non-empty, files exist
    return runInternalSSHD(ctx, internalSSHDOptions{
        Port: *port, AuthFile: *authFile, HostKeyPath: *hostKey, Shell: *shell,
    })
}
```

There is an existing self-exec precedent in this codebase — read `maybeReexecForProcessTitle` (`main.go:255`, `process_title.go`) for how the binary re-invokes itself. Mirror its `os.Executable()` / arg-rebuild approach.

### 1b. Spawning + supervision — reuse the existing probe loop, do NOT invent a new supervisor

The daemon already has a supervision loop for the local SSH server. **Hook into it; don't build a parallel one.**

- `checkLocalSSH` (`main.go:724`) runs every `LocalSSH.probeDur` (default 15s, `main.go:494`). It probes `LocalSSH.Host:Port`; if the probe fails and `LocalSSH.Manage` is true, it calls `startLocalSSH`. **This loop is the supervisor** — if the child dies, the next probe fails and it is restarted.
- `startLocalSSH` (`main.go:752`) currently branches: `if d.cfg.LocalSSH.UserSSHD { return d.startUserSSHD(ctx) }` else systemd/service. **Add a branch above it:**

```go
func (d *Daemon) startLocalSSH(ctx context.Context) error {
    if d.cfg.LocalSSH.InternalSSHD {
        return d.startInternalSSHD(ctx)
    }
    if d.cfg.LocalSSH.UserSSHD { ... }   // existing
    ...
}
```

- `startInternalSSHD` mirrors `startUserSSHD` (`main.go:780`) almost exactly — spawn, log pid, `go cmd.Wait()`, return. The only differences: the binary is *self* and the args are the `internal-sshd` flags.

```go
func (d *Daemon) startInternalSSHD(ctx context.Context) error {
    self := d.cfg.Install.AgentPath
    if self == "" {
        var err error
        if self, err = os.Executable(); err != nil { return err }
    }
    ls := d.cfg.LocalSSH
    if ls.Port == 0 || ls.AuthFile == "" || ls.HostKeyPath == "" {
        return errors.New("internal-sshd not fully configured")
    }
    args := []string{"internal-sshd",
        "--port", strconv.Itoa(ls.Port),
        "--auth-file", ls.AuthFile,
        "--host-key", ls.HostKeyPath,
    }
    logPath := firstNonEmpty(ls.SSHDLog, d.cfg.Tunnel.LogPath+".internal-sshd.log")
    logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
    if err != nil { return err }
    defer logFile.Close()
    cmd := exec.CommandContext(ctx, self, args...)   // ctx = daemon ctx → child dies on daemon shutdown
    cmd.Stdout, cmd.Stderr = logFile, logFile
    if err := cmd.Start(); err != nil { return err }
    d.logger.Printf("started internal sshd pid=%d port=%d", cmd.Process.Pid, ls.Port)
    go func() { _ = cmd.Wait() }()
    time.Sleep(time.Second)   // same settle as startUserSSHD
    return nil
}
```

Follow `startUserSSHD`'s `serviceEnsured` interaction exactly — no changes needed there; `checkLocalSSH` already restarts on probe failure regardless of `serviceEnsured`.

`localSSHMode` (`main.go:1023`) — add an `internal-sshd` case so status/heartbeat report the mode correctly:

```go
if cfg.LocalSSH.InternalSSHD { return "internal-sshd" }
```

### 1c. Config the daemon needs (add to `LocalSSHConfig`)

`LocalSSHConfig` at `main.go:64` already has `Port`, `AuthFile`, `TargetUser`, `Manage`, `SSHDLog`. Add two fields:

```go
InternalSSHD bool   `yaml:"internal_sshd" json:"internal_sshd"`
HostKeyPath  string `yaml:"host_key_path" json:"host_key_path"`
```

For step-4 installs the agent config carries:
`LocalSSH.Manage=true`, `LocalSSH.InternalSSHD=true`, `LocalSSH.Port=<loopback port>`, `LocalSSH.AuthFile=<DataDir>/internal-sshd/authorized_keys`, `LocalSSH.HostKeyPath=<DataDir>/internal-sshd/host_ed25519_key`.

That's all the daemon needs to start it on every future run — no code path other than the existing probe loop is involved.

---

## 2. SSH server implementation

### 2a. Libraries

- **`github.com/gliderlabs/ssh`** — server framework over `golang.org/x/crypto/ssh` (already a dep, `go.mod:7`). Gives clean session/pubkey/pty handling and — importantly — **forwarding is off unless you register the forward callbacks**, so "no forwarding" is the default.
- **`github.com/creack/pty`** — PTY allocation (pure Go, uses `/dev/ptmx`; fine on old kernels).

`go get github.com/gliderlabs/ssh github.com/creack/pty`. Both are small, pure-Go, static-friendly.

(Hand-rolling directly on `x/crypto/ssh` is possible to avoid the gliderlabs dep, but you'd re-implement session/pty-req/window-change/shell/exec request handling by hand. Use gliderlabs unless there's a reason not to.)

### 2b. Server assembly

```go
func runInternalSSHD(ctx context.Context, opt internalSSHDOptions) error {
    signer, err := loadOrRequireHostKey(opt.HostKeyPath)   // host key already persisted by installer; just load
    if err != nil { return err }
    authorized, err := loadAuthorizedKeys(opt.AuthFile)     // []ssh.PublicKey
    if err != nil { return err }

    srv := &ssh.Server{
        Addr: fmt.Sprintf("127.0.0.1:%d", opt.Port),        // loopback-only bind = the real containment
        PublicKeyHandler: func(_ ssh.Context, key ssh.PublicKey) bool {
            for _, ak := range authorized {
                if ssh.KeysEqual(key, ak) { return true }
            }
            return false
        },
        // No PasswordHandler, no KeyboardInteractiveHandler  → password auth disabled.
        // No LocalPortForwardingCallback / ReversePortForwardingCallback → all forwarding denied.
        // No SubsystemHandlers → no sftp.
        Handler: func(s ssh.Session) {
            defer func() {
                if r := recover(); r != nil {               // isolate a panic to one session
                    log.Printf("internal-sshd session panic: %v", r)
                    _ = s.Exit(1)
                }
            }()
            handleShellSession(s, opt.Shell)
        },
    }
    srv.AddHostKey(signer)

    ln, err := net.Listen("tcp", srv.Addr)                  // explicit loopback listener
    if err != nil { return err }
    go func() { <-ctx.Done(); _ = srv.Close() }()
    return srv.Serve(ln)
}
```

`loadAuthorizedKeys`: read the file, for each non-blank line call `ssh.ParseAuthorizedKey` (tolerates any options/comment), collect the pubkeys. Exact-match auth via `ssh.KeysEqual` (compares marshaled bytes) — we do **not** interpret `from=` or other options; loopback binding is the enforcement (see §5).

### 2c. Shell session + PTY

Canonical gliderlabs+creack/pty shape:

```go
func handleShellSession(s ssh.Session, shellOverride string) {
    shell := resolveShell(shellOverride)                    // $SHELL → /bin/bash → /bin/sh
    cmd := exec.Command(shell)
    cmd.Env = append(minimalEnv(shell), s.Environ()...)     // HOME,USER,PATH,SHELL,LOGNAME; TERM from ptyReq
    ptyReq, winCh, isPty := s.Pty()
    if isPty {
        cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
        f, err := pty.Start(cmd)
        if err != nil { _ = s.Exit(1); return }
        defer f.Close()
        go func() { for w := range winCh { setWinsize(f, w.Width, w.Height) } }()
        go func() { _, _ = io.Copy(f, s) }()                // stdin → pty
        _, _ = io.Copy(s, f)                                // pty → stdout
        _ = cmd.Wait()
        _ = s.Exit(exitCode(cmd))
        return
    }
    // No-PTY (e.g. `ssh host somecmd` or piped): wire stdio directly.
    cmd.Stdin, cmd.Stdout, cmd.Stderr = s, s, s.Stderr()
    if len(s.Command()) > 0 { cmd.Args = append([]string{shell, "-c"}, strings.Join(s.Command(), " ")) }
    _ = cmd.Run()
    _ = s.Exit(exitCode(cmd))
}
```

`resolveShell`: try `shellOverride`, then `os.Getenv("SHELL")`, then `/bin/bash`, then `/bin/sh`. Optionally read the current user's shell via `os/user.Current()` (Go's pure-Go path parses `/etc/passwd` directly — no cgo/NSS), but the env→`/bin/sh` fallback is enough and avoids depending on `/etc/passwd` being sane on a broken box.

### 2d. Security posture (equivalents to the existing user-sshd hardening at `install.go:412-431`)

| Property | How |
|---|---|
| Loopback-only reachability | `net.Listen("tcp", "127.0.0.1:PORT")` — **this is the primary containment** |
| No password auth | omit `PasswordHandler` / `KeyboardInteractiveHandler` |
| No forwarding (local/remote/agent/X11) | omit the forwarding callbacks (gliderlabs denies by default) |
| No sftp | register no subsystem handler |
| No root login | N/A — non-root process, can only be the current user |
| Persistent host key | loaded from `HostKeyPath` (installer generates+persists, §3c) |
| Panic containment | `recover()` in the session handler **and** the process is separate (parent restarts) |
| Optional: cap concurrent sessions | a counter/semaphore in `PublicKeyHandler`/`Handler` — low priority; port is auth-gated + loopback so DoS is effectively self-inflicted |

Also wrap any long-lived goroutines you add with `recover()` — note the existing embedded *client* goroutines (`embedded_ssh.go` `forward`/`keepalive`) currently have none; don't copy that gap into the server.

---

## 3. Integration into `install.go` Phase D, step 4

### 3a. Trigger point

Today `prepareLocalSSH` errors when no sshd and no binary is found — `install.go:387-389`:

```go
bin := findSSHD()
if bin == "" {
    return localSSHPlan{}, fmt.Errorf("no sshd is running ... install openssh-server or rerun with sudo")
}
```

Replace that error with the step-4 plan (this is the "no sudo, no sshd, no binary" branch — exactly step 4):

```go
bin := findSSHD()
if bin == "" {
    return prepareInternalSSHD(ctx, opt, compat)
}
```

`prepareInternalSSHD` allocates the loopback port, ensures the host key exists (persist), and returns a plan:

```go
func prepareInternalSSHD(ctx context.Context, opt installOptions, compat SSHCompatConfig) (localSSHPlan, error) {
    port, err := findFreePort(22220, 22320)                 // reuse existing helper (install.go:451)
    if err != nil { return localSSHPlan{}, err }
    dir := filepath.Join(opt.DataDir, "internal-sshd")
    if err := os.MkdirAll(dir, 0o700); err != nil { return localSSHPlan{}, err }
    hostKey := filepath.Join(dir, "host_ed25519_key")
    if err := ensureInternalHostKey(hostKey); err != nil { return localSSHPlan{}, err }  // §3c, pure-Go
    authFile := filepath.Join(dir, "authorized_keys")
    fmt.Printf("[reach] no sshd and no sudo; using reach-agent internal ssh server on 127.0.0.1:%d\n", port)
    return localSSHPlan{
        Mode: "internal-sshd", LocalPort: port, AuthFile: authFile,
        InternalSSHD: true, HostKeyPath: hostKey,
    }, nil
}
```

Add `InternalSSHD bool` and `HostKeyPath string` to the `localSSHPlan` struct (`install.go:~95`, near `UserSSHD`/`SSHDBinary`).

### 3b. Writing the agent config

Where the plan is translated to config (`install.go:799-821`, the `cfg.LocalSSH.*` assignments), set for the internal-sshd plan:

```go
cfg.LocalSSH.Manage       = true
cfg.LocalSSH.InternalSSHD = plan.InternalSSHD
cfg.LocalSSH.HostKeyPath  = plan.HostKeyPath
// Port / AuthFile / TargetUser already assigned from plan+opt as today
```

Note `cfg.LocalSSH.Manage` is currently `opt.InstallMode == "system" || plan.UserSSHD` (`install.go:807`) — extend it to also be true for `plan.InternalSSHD`.

### 3c. Host-key persistence — **pure Go, not `ssh-keygen`**

Critical: on a step-4 box `ssh-keygen` may not exist (that's part of why we're here). **Do not** use the existing `generateSSHKey` helper (`ssh_compat.go:168`) — it shells out to `ssh-keygen`. Generate in Go:

```go
func ensureInternalHostKey(path string) error {
    if st, err := os.Stat(path); err == nil && !st.IsDir() { return nil } // reuse across restarts
    _, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil { return err }
    pem, err := ssh.MarshalPrivateKey(priv, "reach-internal-sshd")        // x/crypto/ssh, returns *pem.Block
    if err != nil { return err }
    return os.WriteFile(path, pem.EncodeToMemory-equivalent(pem), 0o600)  // write PEM, mode 0600
}
```

Stat-check-before-generate mirrors the existing user-sshd host-key handling at `install.go:404`. Persisting the key is what keeps the operator's `HostKeyAlias`/`known_hosts` stable across agent/machine restarts — a fragile box is the most likely to restart, so this matters.

(Related, out of scope but flag to team-lead: the tunnel key gen in Phase E1 also uses `ssh-keygen` via `generateSSHKey`. On a genuinely toolless step-4 box that would fail too. Worth a follow-up to give tunnel-key gen the same pure-Go path — not part of this task.)

---

## 4. Bugs to fix regardless of the server choice

### 4a. Admin-key-type filter rejects ed25519 on a step-4 box — **must fix or provisioning aborts**

`installAdminKeys` (`install.go:677-708`) filters admin keys through `keyTypeSupported(fields[0], supportedKeyTypes)` (`install.go:686`) and **aborts if none pass** (`install.go:698-700`). The `supportedKeyTypes` it's given (`install.go:221`, `sshCompat.SupportedAuthorizedKeyTypes`) comes from `supportedKeygenTypes` (`ssh_compat.go:101`), which **probes the local `ssh-keygen`**. On a step-4 box `ssh-keygen` is likely absent → `supportedKeygenTypes` returns nil → falls back to `["ssh-rsa"]` (`ssh_compat.go:44`) → an ed25519 admin key is **rejected** and the install fails with *"add an ssh-rsa admin key."*

Fix: in internal-sshd mode, the supported set is defined by **our Go server**, not by a local `ssh-keygen`. Hardcode it:

```go
// internal-sshd (and user-sshd where our server/host key type is known) supports these regardless of local tooling:
internalSupported := []string{"ssh-ed25519", "ecdsa-sha2-nistp256", "ssh-rsa"}
```

Pass that into `installAdminKeys` when `plan.InternalSSHD` (prefer ed25519). Do not gate admin-key acceptance on `supportedKeygenTypes` in this mode.

### 4b. Host-key type default

`prepareLocalSSH` defaults the host-key type to `ssh-rsa` when compat is empty (`install.go:399-403`). That default exists for **ancient-OpenSSH-client** compatibility and is unrelated to what *our* server should use. The internal sshd uses an **ed25519** host key unconditionally (§3c). Keep "client is ancient" (which drives `ClientOptions`, kex/cipher downgrades) and "our server host key type" as independent decisions — don't let the ssh-rsa client-compat default leak into the internal-sshd host key.

### 4c. Host-key persistence

Covered in §3c — stat-before-generate, persist in `DataDir/internal-sshd/`, reuse across restarts. Reference the existing pattern at `install.go:404`.

---

## 5. authorized_keys simplification for internal-sshd mode

The current writer (`install.go:695`) emits:

```
from="127.0.0.1,::1",no-agent-forwarding,no-X11-forwarding,no-port-forwarding <pub> <marker>
```

For internal-sshd mode, write a **bare** line — pubkey + reach marker, no options:

```
<pub> <marker>
```

Rationale:
- `from=` was never the real boundary — the existing comment at `install.go:690-694` already says target keys are "loopback-only" and expiry is enforced on the hub. Our server binds loopback (§2d), which *is* the containment. There is nothing to gain from `from=`.
- Options are actively risky on a last-resort binary: the same failure mode the existing comment warns about (an unsupported option silently voids the whole key line). Our server does its own exact-match auth and enforces every restriction at the process/server level, so options in the file buy nothing and could only hurt.
- **Keep the trailing `reach:<machine-id>` marker** — the uninstall/idempotency path finds and removes admin keys by that marker (Phase D3 "remove reach:<id> keys", `ARCHITECTURE.md:344`). Don't drop it.

Concretely: branch the `Fprintf` in `installAdminKeys` on `plan.Mode == "internal-sshd"` to emit `"%s %s\n"` (pub, marker) instead of the options-prefixed form. `loadAuthorizedKeys` in the server tolerates either form anyway (it parses via `ssh.ParseAuthorizedKey`), but write bare lines for cleanliness.

---

## 6. Explicitly do NOT build (and document as known limitations)

Do not implement, and note each in the install summary / capability report so the operator isn't surprised:

- **No sftp subsystem.** `sftp <name>` and sftp-based `scp` won't work in internal-sshd mode (the existing user-sshd offers `internal-sftp`, `install.go:429`; we don't). Legacy `scp` (old protocol, execs remote `scp`) still works if an `scp` binary exists on the target. If sftp is ever wanted, gliderlabs can add it via `github.com/pkg/sftp` — but not now.
- **No port/agent/X11 forwarding through this local server.** Not needed — reach-agent's embedded *client* (`embedded_ssh.go`) handles the reverse tunnel; this local server only provides a shell.
- **No multi-user.** Serves exactly the current user (guaranteed == target_user, see the invariant above). No `setuid`, no user switching, no `getpwnam`.
- **No PAM, no password/keyboard-interactive auth, no root login.** Pubkey-only.
- **No host-based auth, no CA-signed certs** (Reach's SSH-CA plan, `ARCHITECTURE.md:419/435`, is future and orthogonal).

Report line suggestion for Phase G (`ARCHITECTURE.md:333`): `mode=internal-sshd (shell only; no sftp; loopback-only)`.

---

## 7. Reference map (existing code to mirror, don't reinvent)

| Need | Existing code to copy/adapt |
|---|---|
| Self-exec of the binary | `maybeReexecForProcessTitle` (`main.go:255`, `process_title.go`) |
| Subprocess spawn + `go Wait()` supervision | `startUserSSHD` (`main.go:780`) |
| Supervision loop (restart on probe fail) | `checkLocalSSH` (`main.go:724`) — already covers internal-sshd once `startLocalSSH` branches |
| `startLocalSSH` branch point | `main.go:752` |
| Local port probe / free-port pick | `probeSSH` (`main.go:725` caller), `findFreePort` (`install.go:451`) |
| Step-4 detection point | `prepareLocalSSH` `findSSHD()=="" ` branch (`install.go:387`) |
| Existing hardened sshd config (for parity of flags) | `install.go:412-431` |
| Admin-key writing + marker | `installAdminKeys` (`install.go:677`) |
| Config→daemon plumbing | `install.go:799-821`, `LocalSSHConfig` (`main.go:64`) |
| SSH client patterns (config, host-key cb, keepalive) | `embedded_ssh.go` |

---

## 8. Testing checklist (do these before calling it done)

1. **Unit**: `loadAuthorizedKeys` parses bare + options-prefixed lines; `KeysEqual` accepts the right key, rejects a stranger.
2. **Local end-to-end**: run `reach-agent internal-sshd --port 22200 --auth-file <bare-ed25519> --host-key <gen>` standalone; `ssh -p 22200 -i <priv> 127.0.0.1` → interactive shell; `ssh ... 127.0.0.1 whoami` (no-PTY exec path); confirm exit codes propagate; confirm password auth is refused; confirm a non-listed key is refused; confirm the listener is **not** reachable on a non-loopback interface.
3. **Supervision**: `kill` the child; within one probe interval (~15s) the daemon respawns it; tunnel+heartbeat never drop.
4. **Persistence**: restart the daemon; host key is reused (client `known_hosts`/`HostKeyAlias` does not complain).
5. **The bug fixes**: on a box with **no `ssh-keygen`**, an **ed25519** admin key is accepted (4a) and the ed25519 host key is generated in-process (3c) — simulate by pointing PATH away from `ssh-keygen`.
6. **Ancient target**: since the runtime is `reach-agent`'s own (already required to run on the target), a real ancient-userland test is less critical than for a shipped C binary — but still smoke-test on the oldest target you can (glibc/kernel), and confirm `net.Listen`, `/dev/ptmx` PTY, and shell exec work there. Fail **loudly** (non-zero exit, clear log line) if the server can't bind or the host key can't be created — step 4 has no fallback behind it, so a silent half-install on a remote-only box is the worst outcome.

---

## Summary of changes by file

- `cmd/reach-agent/main.go`: `LocalSSHConfig` +`InternalSSHD`,`HostKeyPath`; dispatch `case "internal-sshd"`; `startLocalSSH` branch; `startInternalSSHD`; `localSSHMode` case.
- `cmd/reach-agent/internal_sshd.go` (new): `internalSSHDCommand`, `runInternalSSHD`, `handleShellSession`, `loadAuthorizedKeys`, `resolveShell`, `ensureInternalHostKey` (or put the last in install.go).
- `cmd/reach-agent/install.go`: `localSSHPlan` +fields; `prepareInternalSSHD`; replace the `findSSHD()==""` error; config plumbing (`install.go:799-821`, `Manage` at `:807`); bare-key branch in `installAdminKeys`; hardcoded supported-key set for internal-sshd mode (bug 4a).
- `go.mod`: add `github.com/gliderlabs/ssh`, `github.com/creack/pty`.
- Docs: add the `mode=internal-sshd` limitations to the Phase G report / capability summary.
