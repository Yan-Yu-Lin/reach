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
    expect(planFleetEvent('machine.health_changed', { machine_id: 'm-1', observed_state: 'degraded', status: 'degraded', probe_state: 'unreachable' })).toEqual({
      kind: 'patch',
      machineId: 'm-1',
      patch: { observed_state: 'degraded', status: 'degraded', probe_state: 'unreachable' },
      refetchIfSelected: 'now',
    })
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
