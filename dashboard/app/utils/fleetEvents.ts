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

export type MachinePatch = {
  heartbeat_at?: string
  observed_state?: string
  status?: string
  probe_state?: string
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
  | { kind: 'patch'; machineId: string; patch: MachinePatch; refetchIfSelected: SelectedRefetch }
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

export function planFleetEvent(event: string, rawData: unknown): FleetEventPlan {
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
      if (probe) patch.probe_state = probe
      if (!observed && !status) return { kind: 'machine', machineId }
      return { kind: 'patch', machineId, patch, refetchIfSelected: 'now' }
    }
    return { kind: 'machine', machineId }
  }

  return { kind: 'none' }
}
