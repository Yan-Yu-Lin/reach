// Decides what one SSE event means for the local fleet store.
//
// Event contract: docs/EVENTS.md (backend). Summary of what the dashboard relies on:
// - agent.heartbeat: live-only (no id, not replayed). data has machine_id, heartbeat_at and,
//   on newer servers, observed_state + status. Patched locally. The machine whose detail is
//   open is also re-fetched (throttled), because a heartbeat does not carry the diagnostics.
// - machine.observed_changed / machine.health_changed: carry observed_state/status
//   (+ probe_state). Patched locally; the machine open in the detail view is re-fetched now.
// - machine.<observed_state> (machine.online, machine.offline, ...): duplicates of
//   observed_changed with only a slug, ignored.
// - other machine.* and ssh_config.changed with machine_id: re-fetch that one machine.
// - request.*: re-fetch the pending request list only.
// - hello: the stream is (re)connected. Always followed by a full snapshot (see useFleet).
// - machine.resync_required, or a machine event without machine_id: full snapshot.

/**
 * What one event says about a machine. Fields that belong together travel together:
 *
 * - heartbeat_at: a heartbeat (live, or the one behind observed_changed).
 * - observed_state/status: the aggregate state, as of `state_at` when the server says so
 *   (event_at, 2026-11-21+ servers). Without state_at the event cannot be ordered against
 *   other state changes and is applied as "newest seen".
 * - probe: one hub observation, as a whole. `at` is the server's stored probe time:
 *     undefined: the server did not say (older reachd) -> the time is unknown;
 *     '':        the server says there is no observation (never probed);
 *     a time:    the probe ran then.
 */
export type ProbeReport = { state: string; at?: string }
export type MachinePatch = {
  heartbeat_at?: string
  observed_state?: string
  status?: string
  /** Server time of the aggregate state above (event_at). */
  state_at?: string
  probe?: ProbeReport
}

/** When the machine open in the detail view should be re-fetched after a patch. */
export type SelectedRefetch = 'now' | 'throttled'

export type FleetEventPlan =
  | { kind: 'none' }
  | { kind: 'hello' }
  | { kind: 'full' }
  | { kind: 'requests'; notifyPending: boolean }
  | { kind: 'machine'; machineId: string }
  /** Apply locally, then re-fetch the machine if its detail is open. */
  | {
    kind: 'patch'
    machineId: string
    patch: MachinePatch
    refetchIfSelected: SelectedRefetch
    /** The event lacks the server time needed to judge it; ask the server for this machine. */
    needsAuthoritative?: boolean
  }
  /** Only matters for the detail view (e.g. agent command results). */
  | { kind: 'detail'; machineId: string }

const OBSERVED_ALIASES = new Set([
  'machine.online',
  'machine.offline',
  'machine.degraded',
  'machine.gone',
  'machine.unknown',
])

function str(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined
}

/** Like str() but keeps the difference between "absent" (undefined) and "empty" (''). */
function strOrEmpty(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined
}

/**
 * `replay`: the event arrived before this connection's `hello`, i.e. it is durable history
 * the server replayed for our Last-Event-ID (docs/EVENTS.md: hello marks the end of replay).
 */
export function planFleetEvent(event: string, rawData: unknown, { replay = false }: { replay?: boolean } = {}): FleetEventPlan {
  const data = rawData && typeof rawData === 'object'
    ? rawData as Record<string, unknown>
    : {}
  const machineId = str(data.machine_id)

  if (event === 'hello') return { kind: 'hello' }
  if (event === 'machine.resync_required') return { kind: 'full' }

  if (event.startsWith('request.')) {
    return { kind: 'requests', notifyPending: event === 'request.created' && data.status === 'pending' }
  }

  if (event === 'ssh_config.changed') {
    return machineId ? { kind: 'machine', machineId } : { kind: 'full' }
  }

  if (event === 'agent.heartbeat') {
    if (!machineId) return { kind: 'full' }
    const heartbeatAt = str(data.heartbeat_at)
    if (!heartbeatAt) return { kind: 'machine', machineId }
    const patch: MachinePatch = { heartbeat_at: heartbeatAt }
    const observed = str(data.observed_state)
    const status = str(data.status)
    if (observed) patch.observed_state = observed
    if (status) patch.status = status
    return { kind: 'patch', machineId, patch, refetchIfSelected: 'throttled' }
  }

  if (event.startsWith('agent.')) {
    // agent.command_* results only change the command list shown in the detail view.
    return machineId ? { kind: 'detail', machineId } : { kind: 'none' }
  }

  if (event.startsWith('machine.')) {
    if (!machineId) {
      return OBSERVED_ALIASES.has(event) ? { kind: 'none' } : { kind: 'full' }
    }
    if (OBSERVED_ALIASES.has(event)) return { kind: 'none' }
    if (event === 'machine.observed_changed' || event === 'machine.health_changed') {
      const patch: MachinePatch = {}
      const observed = str(data.observed_state)
      const status = str(data.status)
      const probe = str(data.probe_state)
      if (observed) patch.observed_state = observed
      if (status) patch.status = status
      const eventAt = str(data.event_at)
      if (eventAt && (observed || status)) patch.state_at = eventAt
      // observed_changed on 2026-11-21+ servers names the heartbeat behind it, so a replayed
      // old state change cannot roll back a newer one (applyPatch ignores older heartbeats).
      const hbAt = event === 'machine.observed_changed' ? str(data.heartbeat_at) : undefined
      if (hbAt) patch.heartbeat_at = hbAt
      if (probe) {
        const at = strOrEmpty(data.last_probe_at)
        patch.probe = at === undefined ? { state: probe } : { state: probe, at }
      }
      if (!observed && !status) return { kind: 'machine', machineId }
      // An event that cannot be dated (older reachd: no last_probe_at, no event_at) may be a
      // replay from days ago. It is applied cautiously (see applyPatch) and the machine is
      // fetched from the server (R2-03, R3-03). NOT inferred from the SSE id: live durable
      // events have ids too.
      const undated = !eventAt && !hbAt
      // Replayed history without a time (older reachd) cannot be ordered against what we
      // hold; the snapshot that follows hello is the authority for the state, so the state
      // part is not applied at all.
      if (replay && undated) {
        delete patch.observed_state
        delete patch.status
      }
      // A LIVE undated state change is the newest the server has (one stream, publish order),
      // so it needs no fetch; an undated probe result or replayed undated history does.
      const needsAuthoritative = (!!patch.probe && patch.probe.at === undefined) || (replay && undated)
      return { kind: 'patch', machineId, patch, refetchIfSelected: 'now', ...(needsAuthoritative ? { needsAuthoritative } : {}) }
    }
    return { kind: 'machine', machineId }
  }

  return { kind: 'none' }
}
