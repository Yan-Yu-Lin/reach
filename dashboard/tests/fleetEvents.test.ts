import { describe, expect, it } from 'vitest'
import { planFleetEvent } from '../app/utils/fleetEvents'

describe('planFleetEvent', () => {
  it('requests an authoritative full snapshot for every connection hello', () => {
    expect(planFleetEvent('hello', { ok: true })).toEqual({
      fullRefresh: true,
      machineId: null,
      notifyPending: false,
    })
  })

  it('keeps machine invalidations targeted when an id is present', () => {
    expect(planFleetEvent('machine.updated', { machine_id: 'm-1' })).toEqual({
      fullRefresh: false,
      machineId: 'm-1',
      notifyPending: false,
    })
  })

  it('falls back to a full snapshot for malformed machine invalidations', () => {
    expect(planFleetEvent('agent.heartbeat', {})).toEqual({
      fullRefresh: true,
      machineId: null,
      notifyPending: false,
    })
  })

  it('refreshes and notifies for a newly pending request', () => {
    expect(planFleetEvent('request.created', { status: 'pending' })).toEqual({
      fullRefresh: true,
      machineId: null,
      notifyPending: true,
    })
  })

  it('ignores unrelated events so hello cannot cause an event loop', () => {
    expect(planFleetEvent('snapshot.completed', {})).toEqual({
      fullRefresh: false,
      machineId: null,
      notifyPending: false,
    })
  })
})
