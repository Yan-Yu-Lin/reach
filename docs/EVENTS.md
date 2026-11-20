# Reach event stream (`GET /api/admin/events`)

Server-Sent Events for the dashboard and reach-mac-agent. Auth: admin JWT or
`mac-agent` service token (`Authorization: Bearer ...`).

Last updated 2026-11-21 (branch `fix/hardening-2026-11`).

## Wire format

```text
retry: 2000

id: 861234
event: machine.desired_changed
data: {"machine_id":"m_abc123","desired_state":"disabled","generation":4}

event: agent.heartbeat
data: {"machine_id":"m_abc123","heartbeat_at":"2026-11-21T03:04:05Z","observed_state":"online","status":"online"}

: keepalive 2026-11-21T03:04:30Z
```

- `data` is the payload object only. The envelope's machine id is not
  repeated outside `data`; every machine event carries `machine_id` inside
  `data`.
- A keepalive comment arrives every 25 s. Treat 60 s of silence as a dead
  stream and reconnect.
- The server re-validates the bearer token before every event and keepalive,
  and immediately after any logout, revoke-all or service-token revocation.
  A revoked or expired session's stream is closed without further data; the
  next reconnect gets 401, and the client should go to login.

## Two kinds of events

### Durable events (have `id:`)

Stored in the `events` table, resumable with `Last-Event-ID`. Everything
except `agent.heartbeat` and `hello` is durable.

### Live-only events (no `id:` line)

- **`agent.heartbeat`** is pushed to connected subscribers only. It is never
  stored, never replayed, and does not move the client's cursor.
  - data: `machine_id`, `heartbeat_at` (RFC 3339 UTC), `observed_state`,
    `status` (all four are always present).
  - The client should patch `agent_observation.heartbeat_at`,
    `machine.observed_state` and `machine.status` in place and **not** issue
    a GET. A dropped heartbeat (slow consumer) is harmless; the next one
    arrives within the heartbeat interval.
- **`hello`** marks the end of replay; `data: {"ok":true}`.

## Resuming and resync

- Without a cursor the stream is live-only (no history).
- With `Last-Event-ID: N` the server replays durable events after N, then
  sends `hello`.
- The replay window is **72 hours** (time-based; it used to be 500 events,
  which heartbeats exhausted in about an hour). A cursor older than the
  window, newer than the newest event, or more than 5000 events behind gets:

```text
id:
event: machine.resync_required
data: {"reason":"cursor_out_of_range"}
```

  and the stream closes. The empty `id:` line clears the browser's
  EventSource cursor. The client must drop its cursor, refetch
  `/api/admin/machines` and `/api/admin/requests`, and reconnect without
  `Last-Event-ID`. Other `reason` values: `invalid_cursor`, `replay_overflow`,
  `replay_failed`, `subscriber_overflow`, `event_persistence_failed`,
  `event_encode_failed`.

## Durable event types

| type | data | notes |
|---|---|---|
| `request.created` | `request_id`, `name`, `status` | `status` is `pending` or `approved` (god code / invite) |
| `request.approved` / `request.denied` / `request.expired` | `request_id` | |
| `request.provisioned` | `request_id`, `machine_id`, `slug` | |
| `machine.created` | `machine_id`, `slug` | |
| `machine.desired_changed` | `machine_id`, `desired_state`, `generation`, optional `reason` | |
| `machine.enabled` / `machine.disabled` / `machine.retiring` / `machine.retired` | `machine_id`, `slug` (admin) or `machine_id`, `reason` (agent uninstall) | |
| `machine.observed_changed` | `machine_id`, `observed_state`, `status` | |
| `machine.online` / `machine.degraded` / `machine.offline` / `machine.gone` / `machine.unknown` | `machine_id`, `slug` | emitted with `machine.observed_changed` |
| `machine.health_changed` | `machine_id`, `observed_state`, `status`, `probe_state` | only when the hub probe result changes |
| `machine.cleanup_failed` | `machine_id`, `cleanup_state`, `desired_state` | **new.** Hub-side revoke/cleanup failed; see below |
| `machine.retiring_timed_out` | `machine_id`, `cleanup_state` (`timed_out`) | **new.** Agent never confirmed uninstall |
| `machine.update_policy_changed` | `machine_id`, `slug`, `update_policy` | |
| `machine.process_title_changed` | `machine_id`, `slug` | |
| `agent.update_queued` | `machine_id`, `version`, `reason` | |
| `agent.command_acked` / `agent.command_failed` | `machine_id`, `command_id`, `status`, `message` | only for a real pending/sent to acked/failed transition |
| `ssh_config.changed` | optional `machine_id`, `reason` | refresh that machine if `machine_id` is set |

## Machine status values used by the dashboard

`machine.desired_state` is still one of `active`, `disabled`, `retiring`,
`retired`.

`machine.status` (effective status) may now also be:

- `disable_failed`: disable or remove could not revoke hub access (rename of
  `authorized_keys`, process kill). The machine's key may still work. Retry
  the action.
- `cleanup_failed`: retire could not remove the Unix user, home, sshd
  drop-in or processes. The tunnel row is kept as a tombstone (status
  `cleanup_failed`, port and user reserved). `retire-now` retries.

`machine.cleanup_state` values: `pending` (waiting for the agent to
uninstall), `timed_out` (retiring for longer than `retiring_timeout`,
default 24h; operator decides with retire-now), `disable_failed`,
`cleanup_failed`, `complete` (hub side verified clean), `server-only`
(legacy rows).

Admin actions that fail this way return **HTTP 409** with an error message
instead of 200.

## Related API changes in this branch

- `GET /api/admin/machines` omits retired machines; add
  `?include_retired=1` for history.
- `GET /api/admin/requests?status=pending|approved|denied|expired|provisioned|provisioning`
  filters server-side. Each row has top-level `observed_ip` (address the
  server saw; `null` on rows created before this change), `slug` (the exact
  alias provision will use), `target_user`, `mode`, `local_port`. Everything
  under `metadata` is self-reported by the requester.
- `POST /api/admin/sessions/revoke-all` with optional
  `{"include_service_tokens": true}` invalidates every dashboard session of
  the account, including the caller's. The client should return to login.
- `GET /api/admin/ssh-config.json` returns structured hosts (schema 1) for
  reach-mac-agent; `/api/admin/ssh-config` (text) is kept for older agents.
