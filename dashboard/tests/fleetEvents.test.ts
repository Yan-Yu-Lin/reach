import { describe, expect, it } from 'vitest'
import { planFleetEvent } from '../app/utils/fleetEvents'

describe('planFleetEvent', () => {
  it('leaves the snapshot decision for hello to the caller', () => {
    expect(planFleetEvent('hello', { ok: true })).toEqual({ kind: 'hello' })
  })

  it('patches heartbeats locally; only the open machine is re-fetched, throttled (A5, F04)', () => {
    expect(planFleetEvent('agent.heartbeat', { machine_id: 'm-1', heartbeat_at: '2026-11-21T00:00:00Z' })).toEqual({
      kind: 'patch',
      machineId: 'm-1',
      patch: { heartbeat_at: '2026-11-21T00:00:00Z' },
      refetchIfSelected: 'throttled',
    })
  })

  it('carries observed_state and status from the new heartbeat payload', () => {
    const plan = planFleetEvent('agent.heartbeat', {
      machine_id: 'm-1', heartbeat_at: '2026-11-21T00:00:00Z', observed_state: 'online', status: 'online',
    })
    expect(plan).toMatchObject({ kind: 'patch', patch: { observed_state: 'online', status: 'online' } })
  })

  it('falls back to a single-machine fetch for a heartbeat without a timestamp', () => {
    expect(planFleetEvent('agent.heartbeat', { machine_id: 'm-1' })).toEqual({ kind: 'machine', machineId: 'm-1' })
  })

  it('falls back to a full snapshot for malformed machine invalidations', () => {
    expect(planFleetEvent('agent.heartbeat', {})).toEqual({ kind: 'full' })
    expect(planFleetEvent('machine.desired_changed', {})).toEqual({ kind: 'full' })
  })

  it('refreshes only the affected machine for ssh_config.changed (A7)', () => {
    expect(planFleetEvent('ssh_config.changed', { machine_id: 'm-9', reason: 'disable' })).toEqual({ kind: 'machine', machineId: 'm-9' })
    expect(planFleetEvent('ssh_config.changed', { reason: 'bulk' })).toEqual({ kind: 'full' })
  })

  it('re-fetches only the request list for request events (A7)', () => {
    expect(planFleetEvent('request.created', { status: 'pending' })).toEqual({ kind: 'requests', notifyPending: true })
    expect(planFleetEvent('request.approved', { request_id: 'r' })).toEqual({ kind: 'requests', notifyPending: false })
  })

  it('patches state changes and only re-fetches the open machine', () => {
    expect(planFleetEvent('machine.health_changed', { machine_id: 'm-1', observed_state: 'degraded', status: 'degraded', probe_state: 'unreachable', last_probe_at: '2026-11-21T00:00:00Z', event_at: '2026-11-21T00:00:00Z' })).toEqual({
      kind: 'patch',
      machineId: 'm-1',
      patch: { observed_state: 'degraded', status: 'degraded', state_at: '2026-11-21T00:00:00Z', probe: { state: 'unreachable', at: '2026-11-21T00:00:00Z' } },
      refetchIfSelected: 'now',
    })
  })

  it('dates observed_changed by its heartbeat on newer servers', () => {
    expect(planFleetEvent('machine.observed_changed', { machine_id: 'm-1', observed_state: 'offline', status: 'offline', heartbeat_at: '2026-11-19T00:00:00Z', event_at: '2026-11-19T00:05:00Z' })).toMatchObject({
      kind: 'patch',
      patch: { observed_state: 'offline', status: 'offline', heartbeat_at: '2026-11-19T00:00:00Z' },
    })
  })

  it('asks the server when a probe result has no server time (R2-03, older reachd)', () => {
    expect(planFleetEvent('machine.health_changed', { machine_id: 'm-1', observed_state: 'online', status: 'online', probe_state: 'reachable' })).toMatchObject({
      kind: 'patch',
      patch: { probe: { state: 'reachable' } },
      needsAuthoritative: true,
    })
    // a LIVE undated state change (older reachd) is the newest the server has: no fetch
    expect(planFleetEvent('machine.observed_changed', { machine_id: 'm-1', observed_state: 'online', status: 'online' })).not.toHaveProperty('needsAuthoritative')
  })

  it('keeps "no time" and "explicitly empty time" apart (R3-03)', () => {
    const absent = planFleetEvent('machine.health_changed', { machine_id: 'm-1', observed_state: 'online', status: 'online', probe_state: 'reachable', event_at: '2026-11-21T00:00:00Z' })
    const empty = planFleetEvent('machine.health_changed', { machine_id: 'm-1', observed_state: 'online', status: 'online', probe_state: 'unknown', last_probe_at: '', event_at: '2026-11-21T00:00:00Z' })
    expect(absent).toMatchObject({ patch: { probe: { state: 'reachable' } }, needsAuthoritative: true })
    expect((absent as any).patch.probe).not.toHaveProperty('at')
    expect(empty).toMatchObject({ patch: { probe: { state: 'unknown', at: '' } } })
    expect(empty).not.toHaveProperty('needsAuthoritative')
  })

  it('drops the state of undated REPLAYED history and asks the server (R3-04, older reachd)', () => {
    const plan = planFleetEvent('machine.health_changed', { machine_id: 'm-1', observed_state: 'offline', status: 'offline', probe_state: 'unreachable' }, { replay: true })
    expect(plan).toMatchObject({ kind: 'patch', needsAuthoritative: true })
    expect((plan as any).patch).not.toHaveProperty('observed_state')
    expect((plan as any).patch).not.toHaveProperty('status')
  })

  it('ignores machine.<state> aliases that duplicate observed_changed', () => {
    expect(planFleetEvent('machine.online', { machine_id: 'm-1', slug: 'x' })).toEqual({ kind: 'none' })
    expect(planFleetEvent('machine.gone', { machine_id: 'm-1', slug: 'x' })).toEqual({ kind: 'none' })
  })

  it('re-fetches one machine for desired-state changes', () => {
    expect(planFleetEvent('machine.desired_changed', { machine_id: 'm-1', desired_state: 'disabled' })).toEqual({ kind: 'machine', machineId: 'm-1' })
    expect(planFleetEvent('machine.cleanup_failed', { machine_id: 'm-1' })).toEqual({ kind: 'machine', machineId: 'm-1' })
  })

  it('treats command results as detail-only', () => {
    expect(planFleetEvent('agent.command_acked', { machine_id: 'm-1' })).toEqual({ kind: 'detail', machineId: 'm-1' })
  })

  it('takes a full snapshot when the server asks to resync', () => {
    expect(planFleetEvent('machine.resync_required', { reason: 'subscriber_overflow' })).toEqual({ kind: 'full' })
  })

  it('ignores unrelated events so hello cannot cause an event loop', () => {
    expect(planFleetEvent('snapshot.completed', {})).toEqual({ kind: 'none' })
  })
})
