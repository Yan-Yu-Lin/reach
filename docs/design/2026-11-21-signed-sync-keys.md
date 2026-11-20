# Design: signed operator key sets (`sync_keys`)

Status: design only, not implemented (2026-11-21). Addresses daybreak P1-3
(target side) and Opus P2-6.

## Problem

`sync_keys` makes the target agent (root in system mode) write whatever
`admin_pubkeys` the hub returns into the target account's `authorized_keys`.
Whoever controls jason (root, or just the reachd DB) can add a key and log in
to omarchy through the Reach port. Agent binaries are already protected by an
offline Minisign key; the key set is not.

## Goal

A compromised hub can stop syncing or withhold keys. It cannot add a key the
operator never signed, and it cannot replay an older key set.

## Design

### Signed document

The operator signs a small JSON document on the Mac with a Minisign key that
never leaves the Mac. Reusing the release key is possible, but a separate
`reach-keys` key is better: it can rotate without a release and it can't be
used to sign binaries.

```json
{
  "schema": 1,
  "project": "reach-keyset",
  "generation": 7,
  "created_at": "2026-11-21T00:00:00Z",
  "expires_at": "2027-05-21T00:00:00Z",
  "scope": "*",
  "keys": [
    {"label": "mac", "public_key": "ssh-ed25519 AAAA... mac"}
  ]
}
```

- `generation` is monotonic. The agent stores the highest one it accepted in
  its root-owned data dir and refuses anything lower (anti-rollback).
- `scope` is either `*` or a list of machine IDs, so a key can be limited to
  some machines.
- `expires_at` forces re-signing at least twice a year, so a signature stolen
  from a backup eventually stops working.

### Flow

1. The operator runs `reach-keys sign --keys ~/.ssh/reach-operator-keys.txt`
   on the Mac. It bumps the generation, signs, and uploads with
   `POST /api/admin/keyset`.
2. reachd stores the document and signature verbatim. It does **not** derive
   `admin_pubkeys` from the `access_keys` table any more.
3. The heartbeat response carries `desired_config.keyset` and `keyset_sig`.
4. The agent verifies the signature with a public key pinned in its config at
   install time (and compiled-in as a fallback). It checks project, scope,
   expiry and generation, and only then rewrites `authorized_keys`.
5. Any failure leaves the existing Reach-managed lines untouched, and the
   failure is reported in the heartbeat as `keyset_error`.

### Migration

- Agents built before this keep the current behaviour. New agents accept
  unsigned key sets only until a signed set has been seen once
  (trust-on-first-use of the *feature*), after which unsigned sets are
  refused. A config flag `require_signed_keys: true` turns that on at install
  time for new machines.
- Disabling a machine (`sync_keys` with `enabled=false`) removes keys and
  needs no signature: removal can only reduce access.

### Effort

About 300 lines of Go: signer CLI, server endpoint and storage, agent
verification, and tests. One new key to protect on the Mac. Install changes
pin the public key into `agent.yaml`.

### Why not now

It needs a new offline key, a Mac-side signing step for every key change, and
a rollout plan per target. That is a separate project from this hardening
round. The highest-value part of P1-3, closing the Mac command-execution path,
is done in this branch (structured SSH config).
