# Design: per-machine WebSocket carrier credentials (daybreak P2-11)

Status: design only (2026-11-21). Shipped meanwhile: exact path match,
mandatory path (fail closed), connection/size/idle limits, path secret kept
out of argv.

## Problem

Every WebSocket target receives the same `/ws/tunnel/<secret>/` path. The
path is the only thing standing between the internet and a raw TCP pipe to
jason's loopback sshd. A compromised target leaks it to anyone; disabling or
retiring that machine does not revoke it. Rotating it breaks every other
WebSocket target.

## Design

1. **Credential.** reachd issues each WebSocket-capable machine a carrier
   token on heartbeat: `HMAC-SHA256(carrier_key, machine_id || exp)`,
   base64url, 15-minute lifetime, plus `machine_id` and `exp`. `carrier_key`
   is a 32-byte secret shared only between reachd and reach-ws-carrier
   (`/etc/reach/carrier.key`, `root:reach-wstunnel 0640`).
2. **Handshake.** The agent sends it in the upgrade request:
   `Sec-WebSocket-Protocol: reach.v1, reach-auth.<machine_id>.<exp>.<mac>`.
   (A header, not the path: paths end up in nginx logs.)
3. **Carrier check, before dialling sshd.** Verify the MAC in constant time
   and `exp > now`; then look up revocation: reachd writes
   `/run/reach/carrier-revoked` (machine IDs disabled/retired in the last
   15 minutes) which the carrier re-reads on change. Reject with 401.
4. **Limits.** Per-machine concurrent connection cap (2) in addition to the
   per-IP/global caps that exist today.
5. **Rotation.** `carrier_key` rotation = issue tokens with the new key,
   accept both for 15 minutes, drop the old one.

## Rollout

- Old agents send no credential. Phase 1: carrier accepts either the
  credential or (legacy) the shared path, logs which. Phase 2, once every
  WebSocket machine runs an agent that sends credentials: require the
  credential; the shared path becomes routing only.
- jason today: check `agent_observations.transport='websocket'` to see
  which machines depend on the carrier before phase 2.

## Effort

reachd (issue tokens, revoked list) ~80 lines; carrier verify ~60 lines;
agent handshake header ~20 lines; tests. Needs an agent release on every
WebSocket target before enforcement.
