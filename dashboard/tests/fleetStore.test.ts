import { describe, expect, it } from 'vitest'
import type { MachineWithTunnels, RequestRow } from '../app/composables/useReachApi'
import { applyPatch, applySnapshot, pendingOnly, upsertMachine } from '../app/utils/fleetStore'

function item(id: string, heartbeat = '2026-11-21T00:00:00Z'): MachineWithTunnels {
  return {
    machine: { id, slug: id, status: 'online', desired_state: 'active', observed_state: 'online', desired_generation: 1, update_policy: 'manual', local_port: 22, created_at: '2026-01-01T00:00:00Z' },
    tunnels: [],
    agent_observation: { applied_generation: 1, heartbeat_at: heartbeat },
  }
}

describe('applySnapshot', () => {
  it('keeps unchanged machine objects so their rows do not re-render', () => {
    const store = new Map<string, MachineWithTunnels>()
    applySnapshot(store, [item('a'), item('b')])
    const a = store.get('a')
    const changes = applySnapshot(store, [item('a'), item('b', '2026-11-21T00:01:00Z')])
    expect(changes).toBe(1)
    expect(store.get('a')).toBe(a)
  })

  it('removes machines missing from the snapshot', () => {
    const store = new Map<string, MachineWithTunnels>()
    applySnapshot(store, [item('a'), item('b')])
    applySnapshot(store, [item('a')])
    expect([...store.keys()]).toEqual(['a'])
  })
})

describe('applyPatch', () => {
  it('updates heartbeat copy-on-write and leaves other machines alone', () => {
    const store = new Map<string, MachineWithTunnels>()
    applySnapshot(store, [item('a'), item('b')])
    const a = store.get('a')!
    const b = store.get('b')!
    expect(applyPatch(store, 'a', { heartbeat_at: '2026-11-21T00:00:30Z' })).toBe('applied')
    expect(store.get('a')).not.toBe(a)
    expect(a.agent_observation!.heartbeat_at).toBe('2026-11-21T00:00:00Z')
    expect(store.get('a')!.agent_observation!.heartbeat_at).toBe('2026-11-21T00:00:30Z')
    expect(store.get('b')).toBe(b)
  })

  it('ignores an out-of-order heartbeat, including its state', () => {
    const store = new Map([['a', item('a', '2026-11-21T00:05:00Z')]])
    expect(applyPatch(store, 'a', { heartbeat_at: '2026-11-21T00:04:00Z', observed_state: 'offline' })).toBe('noop')
    expect(store.get('a')!.machine.observed_state).toBe('online')
  })

  it('reports unknown machines so the caller can fetch them', () => {
    expect(applyPatch(new Map(), 'zzz', { heartbeat_at: '2026-11-21T00:00:00Z' })).toBe('missing')
  })

  it('applies observed state and probe changes', () => {
    const store = new Map([['a', item('a')]])
    expect(applyPatch(store, 'a', { observed_state: 'degraded', status: 'degraded', probe_state: 'unreachable' })).toBe('applied')
    expect(store.get('a')!.machine.observed_state).toBe('degraded')
    expect(store.get('a')!.hub_observation!.probe_state).toBe('unreachable')
    expect(applyPatch(store, 'a', { observed_state: 'degraded' })).toBe('noop')
  })
})

describe('upsertMachine', () => {
  it('skips identical payloads', () => {
    const store = new Map<string, MachineWithTunnels>()
    expect(upsertMachine(store, item('a'))).toBe(true)
    expect(upsertMachine(store, item('a'))).toBe(false)
  })
})

describe('pendingOnly', () => {
  it('drops history rows that old servers still return', () => {
    const rows = [
      { id: '1', name: 'x', status: 'denied', created_at: '2026-11-01T00:00:00Z', expires_at: '' },
      { id: '2', name: 'y', status: 'pending', created_at: '2026-11-20T00:00:00Z', expires_at: '' },
      { id: '3', name: 'z', status: 'pending', created_at: '2026-11-21T00:00:00Z', expires_at: '' },
    ] as RequestRow[]
    expect(pendingOnly(rows).map(r => r.id)).toEqual(['3', '2'])
    expect(pendingOnly(null)).toEqual([])
  })
})
