# Reach Roadmap

Current status and planned work, roughly ordered by priority within each category.

Legend: `🔴` blocking / broken — `🟡` important but not broken — `🟢` nice to have — `⚪` future / exploratory

---

## Critical fixes

| # | Item | Why | Status |
|---|------|-----|--------|
| 1 | **Pure-Go tunnel key generation** | Phase E1 shells out to `ssh-keygen` for tunnel keypair. On machines with no SSH tools (the exact machines the embedded sshd fallback is built for), this fails before the tunnel is even established. The embedded sshd fallback is effectively dead without this. | 🔴 Not started |
| 2 | **Deploy pending features to jason** | `session-refresh` and `agent-repair-update` are built and pushed but never deployed. Jason is running an older build that's missing JWT lifetime config, 401 redirect handling, and the `update-binary` command on the server side. | 🔴 Code ready, needs deploy |
| 3 | **`embedded_ssh.go` audit** | We found a real host-key-algorithm negotiation bug by accident (broke rtxws-colin during a live update). One bug found by accident suggests more may exist in that file. Needs a deliberate review pass, not just waiting for the next surprise. | 🟡 Not started |

## Development workflow

| # | Item | Why | Status |
|---|------|-----|--------|
| 4 | **Establish public repo as working repo** | Private archive (`reach-private-archive`) is frozen. Public repo (`reach`) has a fresh single commit. Need to set up the local clone, make sure deploy scripts point to the right remote, and confirm jason pulls from the public repo going forward. | 🔴 Not done |
| 5 | **Version numbering** | Everything is `v0.1.0-alpha`. Every deploy overwrites the same download path. Can't tell which version a machine is running or whether an update actually changed anything. Need a real versioning scheme (semver or date-based) and `deploy-jason.sh` should create a new version directory each time. | 🟡 Not started |
| 6 | **CI — at minimum `go test` on push** | No automated testing. Every change is manually validated by the implementing agent. A GitHub Actions workflow running `go test ./...` + `go vet ./...` + `go build ./...` on push would catch regressions before deploy. | 🟢 Not started |

## Transparency

| # | Item | Why | Status |
|---|------|-----|--------|
| 7 | **Local README on target machines** | When someone runs `ps aux` and sees `Reach-Agent`, or finds files under `~/.config/reach/`, there should be a readable file right there explaining: what this is, who installed it, where the source code is (link to the public repo), and how to uninstall. This was Arthur's original transparency vision. | 🟡 Not started |
| 8 | **Show public repo URL in setup.sh output** | The install flow should print the GitHub URL so the person running it knows where to verify the code. One line in setup.sh. | 🟢 Not started |

## Security hardening

| # | Item | Why | Status |
|---|------|-----|--------|
| 9 | **Signed release manifests** | `update-binary` currently verifies checksums only. Checksums are served from the same host as the binary — if that host is compromised, both can be tampered together. Embedding a public signing key in the agent and signing the manifest with a separate private key (kept off the server) would close this gap. | 🟡 Not started |
| 10 | **CentOS 6 / kernel 2.6.32 smoke test** | The embedded sshd and the Dropbear research both flagged this: step-4 targets are by definition the most fragile machines, and we've never actually tested the new binary on a real ancient kernel. Need one real test before trusting step 4 on a machine Arthur can't physically reach. | 🟡 Not started |

## Feature completions (designed but not built)

| # | Item | Why | Status |
|---|------|-----|--------|
| 11 | **Dashboard update button** | `update-binary` exists as a CLI command but there's no way to trigger it from the dashboard. The server already tracks `agent_version` per machine. Add a button in MachineDetail to push an update command, and show current vs. latest version. | ✅ Implemented |
| 12 | **Friend mode notifications** | When someone requests access (pending approval), the operator has no notification — they have to be watching the dashboard. Email or push notification on new requests would make the friend flow actually usable without the operator babysitting. | ✅ Implemented |
| 13 | **Per-machine update policy** | Currently all updates are manual. Some machines (Arthur's own) could safely auto-update; friend machines should stay manual. The `CanSelfUpdate` field and `AgentUpdateHint` struct already exist as stubs. | ✅ Implemented |

## Future features

| # | Item | Why | Status |
|---|------|-----|--------|
| 14 | **Multi-hub support** | ARCHITECTURE.md designed for multiple hubs (DGX, Pi5lin as secondary bastions). DB schema supports it. Not wired up. | ⚪ Schema ready, not wired |
| 15 | **Windows target** | PowerShell installer + Task Scheduler persistence. DESIGN.md mentions this. Would expand Reach beyond Linux-only. | ⚪ Not started |
| 16 | **Mac target** | launchd persistence. DESIGN.md mentions this alongside Windows. | 🟡 Implemented in code; needs deploy + real Mac install smoke test |
| 17 | **Multiple operator access keys** | Support multiple SSH keys per operator across devices (Mac, phone, tablet). Schema supports it via `access_keys` table. | ⚪ Schema ready, not wired |
| 18 | **SSH CA** | Replace direct key distribution with certificate-based auth. Long-term goal from ARCHITECTURE.md. | ⚪ Not started |
| 19 | **Reach Assist mode** | tmate-like temporary shell sharing — no sshd needed on target at all, pure relay through the hub. Mentioned in ARCHITECTURE.md as long-term. | ⚪ Not started |

---

## Notes

- Items 1-4 should be done before starting anything else — they're either broken or block a sane development workflow.
- The private archive repo (`reach-private-archive`) retains full git history and all removed internal docs. It stays private and frozen.
- This roadmap lives in the public repo and should be updated as items are completed or reprioritized.
