import { describe, expect, it } from 'vitest'
import type { MachineWithTunnels, RequestRow } from '../app/composables/useReachApi'
import { applyPatch, applySnapshot, createPatchLog, knownHeartbeat, pendingOnly, upsertMachine } from '../app/utils/fleetStore'

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
    expect(store.get('a')!.live_heartbeat_at).toBe('2026-11-21T00:00:30Z')
    // The observation (tunnel/sshd/version) is NOT re-dated by a heartbeat (F04).
    expect(store.get('a')!.agent_observation!.heartbeat_at).toBe('2026-11-21T00:00:00Z')
    expect(knownHeartbeat(store.get('a')!)).toBe('2026-11-21T00:00:30Z')
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

describe('HTTP response vs newer SSE patch (astra F03)', () => {
  // Same sequence useFleet runs: begin -> SSE patch arrives -> stale response -> replay.
  function staleResponseScenario(apply: (store: Map<string, MachineWithTunnels>, stale: MachineWithTunnels) => void) {
    const store = new Map<string, MachineWithTunnels>()
    applySnapshot(store, [item('a', '2026-11-21T00:00:00Z')])
    const log = createPatchLog()
    const { token, mark } = log.begin()
    const patch = { heartbeat_at: '2026-11-21T00:02:00Z', observed_state: 'degraded', status: 'degraded' }
    log.record('a', patch)
    applyPatch(store, 'a', patch)
    const stale = item('a', '2026-11-21T00:00:00Z') // read before the patch, arrives after it
    apply(store, stale)
    for (const e of log.patchesSince(mark, 'a')) applyPatch(store, e.id, e.patch, e.receivedAt)
    log.end(token)
    return store.get('a')!
  }

  it('a stale full snapshot does not roll back state or heartbeat', () => {
    const a = staleResponseScenario((store, stale) => applySnapshot(store, [stale]))
    expect(a.machine.observed_state).toBe('degraded')
    expect(knownHeartbeat(a)).toBe('2026-11-21T00:02:00Z')
  })

  it('a stale single-machine GET does not roll back state or heartbeat', () => {
    const a = staleResponseScenario((store, stale) => upsertMachine(store, stale))
    expect(a.machine.observed_state).toBe('degraded')
    expect(knownHeartbeat(a)).toBe('2026-11-21T00:02:00Z')
  })

  it('keeps fields the patch did not carry from the HTTP response', () => {
    const a = staleResponseScenario((store, stale) => {
      stale.agent_observation!.agent_version = 'v2'
      upsertMachine(store, stale)
    })
    expect(a.agent_observation!.agent_version).toBe('v2')
  })

  it('does not delete a machine an event touched after the snapshot request began', () => {
    const store = new Map<string, MachineWithTunnels>()
    applySnapshot(store, [item('a'), item('b')])
    const log = createPatchLog()
    const { token, mark } = log.begin()
    log.record('b') // e.g. machine.created / desired_changed for b
    applySnapshot(store, [item('a')], log.touchedSince(mark))
    log.end(token)
    expect(store.has('b')).toBe(true)
  })

  it('forgets patches once no request is in flight', () => {
    const log = createPatchLog()
    log.record('a', { heartbeat_at: '2026-11-21T00:00:00Z' })
    expect(log.size()).toBe(0)
    const { token } = log.begin()
    log.record('a', { heartbeat_at: '2026-11-21T00:00:01Z' })
    expect(log.size()).toBe(2)
    log.end(token)
    expect(log.size()).toBe(0)
  })
})
