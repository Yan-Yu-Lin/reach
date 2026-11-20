# Design: bootstrap trust root independent of the hub (daybreak P2-13)

Status: design only (2026-11-21).

## Problem

Targets install with `curl https://<hub>/setup.sh | bash` (or
`irm .../setup.ps1 | iex`). The script runs as root/SYSTEM and contains the
Minisign public key and pinned hashes that verify the agent. If the hub (or
its nginx) is compromised, the attacker serves a different script: it
never needs to touch the binary or the signature. Signing the binary
protects updates and partial compromises of the download directory, not a
compromised script origin.

## Options

1. **Script from an independent origin (recommended).** Publish
   `setup.sh`/`setup.ps1` as release assets on GitHub (immutable tag), and
   document the install line as
   `curl -fsSL https://github.com/Yan-Yu-Lin/reach/releases/download/vX/setup.sh | bash -s -- --api-url https://tunnels.example`.
   The script embeds the Minisign key; the hub only serves the binaries and
   signed manifest, which the script verifies. Compromising jason then no
   longer yields code execution at install time. Cost: release workflow
   uploads the scripts; the hub URL becomes a parameter.
2. **Pinned script hash.** Show the install line with a hash:
   `curl -fsSL https://hub/setup.sh -o s && echo "<sha256>  s" | sha256sum -c && bash s`.
   The hash must come from a source the installer trusts (README on GitHub,
   a message from Arthur). Cheap, but awkward to type.
3. **Tiny verifier.** Ship a minimal verifier (Go, ~200 KB) in an OS package
   or GitHub release; it fetches the script and its Minisign signature from
   the hub and refuses to run it unless the signature verifies. Same trust
   as option 1 with more moving parts.

## Recommendation

Option 1, with option 2 printed alongside for people who prefer the hub
URL. The dashboard "Setup" card should show the GitHub-hosted line.
