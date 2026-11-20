# Design: internal-sshd drops privileges itself (daybreak round 3 P2-D)

Status: design only (2026-11-21). Judged not small enough to ship in the
hardening branch; see "Why not now".

## Problem

A system (root) agent starts `reach-agent internal-sshd --run-as <user>`.
Only the shell child gets the target UID/GID/groups. The process that
parses SSH packets, runs key exchange, checks public keys and handles
channel requests stays root. A memory-safety or logic bug in
`gliderlabs/ssh`, `x/crypto/ssh` or Reach's pre-shell handler would give
root, not the target user. No such bug is known; this is defence in depth.

## Design

1. **Privileged setup in the root agent** (or in internal-sshd before the
   drop):
   - open the loopback listener (high port, so no `CAP_NET_BIND_SERVICE`
     is needed; the listener fd could still be passed for symmetry);
   - read the host private key into memory;
   - open `authorized_keys` read-only and keep the fd, or move the file to
     a path the target user can read but not write (see 3).
2. **Drop.** `setgroups(target groups)`, `setgid`, `setuid` before
   `srv.Serve(ln)`. Linux: Go ≥1.16 applies these to all threads. macOS:
   `setuid` is process-wide. Refuse to continue if `getuid()==0` after the
   drop. Add `PR_SET_NO_NEW_PRIVS` on Linux.
3. **Authorized keys.** The handler re-reads the file on every attempt so
   `sync_keys` changes apply without a restart. After the drop the process
   cannot read a `root:root 0600` file. Options:
   - `root:<target-group> 0640` in a root-owned `0750` directory: readable,
     not writable by the target user (preferred; the writer's `Strict`
     ownership check must accept this owner/mode);
   - or the root agent sends key updates over a pipe (more code).
4. **Sessions.** The shell child no longer needs `SysProcAttr.Credential`
   (it inherits the dropped IDs); `runAsSysProcAttr` must not try
   `setgroups` as non-root.
5. **Host key file** stays `root 0600`; only the in-memory copy is used.

## Why not now

- Changes the ownership/mode contract of the internal-sshd key file and the
  `writeAuthorizedKeysFile` strict checks (which were just hardened).
- Existing installs need a migration (chown/chmod on upgrade) and a
  rollback path.
- Needs root-run integration tests on Linux and macOS (Docker covers
  Linux; macOS needs a real host), plus a Windows decision (internal-sshd
  there runs as the installing user already).
- Rolls out only with an agent release, which is Stage 5 anyway.

## Effort

~150 lines agent code, ~100 lines tests (Linux root container), one agent
release. Suggested as the first item after Stage 5.
