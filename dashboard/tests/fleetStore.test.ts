import { describe, expect, it } from 'vitest'
import type { MachineWithTunnels, RequestRow } from '../app/composables/useReachApi'
import { applyPatch, applySnapshot, createPatchLog, knownHeartbeat, pendingOnly, upsertMachine } from '../app/utils/fleetStore'
import { connectionChecks } from '../app/utils/status'

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
    expect(applyPatch(store, 'a', { observed_state: 'degraded', status: 'degraded', probe: { state: 'unreachable', at: '2026-11-21T00:00:05Z' } })).toBe('applied')
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
    for (const e of log.patchesSince(mark, 'a')) applyPatch(store, e.id, e.patch)
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

describe('probe observation as one unit (astra R2-03, R3-03)', () => {
  const dayAgo = '2026-11-20T00:00:00Z'
  const fresh = '2026-11-21T00:00:00Z'
  function withProbe(at: string, state = 'reachable', error?: string) {
    const store = new Map([['a', item('a')]])
    store.get('a')!.hub_observation = { probe_state: state, last_probe_at: at, ...(error ? { probe_error: error } : {}) }
    return store
  }

  it('an undated result keeps the held observation whole and marks it unverified', () => {
    const store = withProbe(dayAgo, 'unreachable', 'connection refused')
    applyPatch(store, 'a', { probe: { state: 'reachable' } })
    expect(store.get('a')!.hub_observation).toEqual({ probe_state: 'unreachable', last_probe_at: dayAgo, probe_error: 'connection refused' })
    expect(store.get('a')!.probe_unverified).toBe(true)
  })

  it('an undated reachable replayed over a FRESH failure does not borrow its time (R3-03)', () => {
    const store = withProbe(fresh, 'unreachable', 'connection refused')
    applyPatch(store, 'a', { probe: { state: 'reachable' } })
    const a = store.get('a')!
    expect(a.hub_observation!.probe_state).toBe('unreachable')
    expect(a.probe_unverified).toBe(true)
    const [probe] = connectionChecks(a, Date.parse(fresh) + 60_000)
    expect(probe!.state).toBe('unknown')
  })

  it('an explicitly empty time means "no observation": no borrowed time or error (R3-03)', () => {
    const store = withProbe(fresh, 'unreachable', 'frozen timestamp fixture')
    applyPatch(store, 'a', { probe: { state: 'unknown', at: '' } })
    expect(store.get('a')!.hub_observation).toEqual({ probe_state: 'unknown', last_probe_at: '' })
    const [probe] = connectionChecks(store.get('a')!, Date.parse(fresh) + 60_000)
    expect(probe!.state).toBe('unknown')
    expect(probe!.detail).not.toContain('frozen')
  })

  it('a dated newer result replaces the observation, error included', () => {
    const store = withProbe(dayAgo, 'unreachable', 'connection refused')
    applyPatch(store, 'a', { probe: { state: 'reachable', at: fresh } })
    expect(store.get('a')!.hub_observation).toEqual({ probe_state: 'reachable', last_probe_at: fresh, last_success_at: fresh })
  })

  it('a dated older result (replayed history) does not overwrite a newer one', () => {
    const store = withProbe(fresh, 'reachable')
    applyPatch(store, 'a', { probe: { state: 'unreachable', at: dayAgo } })
    expect(store.get('a')!.hub_observation).toEqual({ probe_state: 'reachable', last_probe_at: fresh })
  })

  it('a server response clears the unverified mark', () => {
    const store = withProbe(fresh, 'unreachable')
    applyPatch(store, 'a', { probe: { state: 'reachable' } })
    const server = item('a')
    server.hub_observation = { probe_state: 'reachable', last_probe_at: fresh }
    upsertMachine(store, server)
    expect(store.get('a')!.probe_unverified).toBeFalsy()
  })
})

describe('a historical event is accepted or rejected as a whole (astra R3-04, daybreak)', () => {
  const now = '2026-11-21T00:00:00Z'
  const dayAgo = '2026-11-20T00:00:00Z'
  function fresh() {
    const store = new Map([['a', item('a', now)]])
    store.get('a')!.machine.updated_at = now
    store.get('a')!.hub_observation = { probe_state: 'reachable', last_probe_at: now }
    return store
  }

  it('an old dated health event changes neither the machine state nor the probe', () => {
    const store = fresh()
    expect(applyPatch(store, 'a', { observed_state: 'offline', status: 'offline', state_at: dayAgo, probe: { state: 'unreachable', at: dayAgo } })).toBe('noop')
    expect(store.get('a')!.machine.observed_state).toBe('online')
    expect(store.get('a')!.hub_observation!.probe_state).toBe('reachable')
  })

  it('a newer dated state with an older probe applies the state and keeps the newer probe', () => {
    const store = fresh()
    applyPatch(store, 'a', { observed_state: 'degraded', status: 'degraded', state_at: '2026-11-21T00:05:00Z', probe: { state: 'unreachable', at: dayAgo } })
    expect(store.get('a')!.machine.observed_state).toBe('degraded')
    expect(store.get('a')!.hub_observation).toEqual({ probe_state: 'reachable', last_probe_at: now })
  })

  it('an older machine state does not replace a newer probe, and a newer probe is kept by a later older state', () => {
    const store = fresh()
    applyPatch(store, 'a', { probe: { state: 'unreachable', at: '2026-11-21T00:10:00Z' }, state_at: '2026-11-21T00:10:00Z', observed_state: 'degraded', status: 'degraded' })
    applyPatch(store, 'a', { observed_state: 'online', status: 'online', state_at: '2026-11-21T00:01:00Z' })
    expect(store.get('a')!.machine.observed_state).toBe('degraded')
    expect(store.get('a')!.hub_observation!.probe_state).toBe('unreachable')
  })

  it('compares times across time zones by instant, not by string', () => {
    const store = fresh()
    expect(applyPatch(store, 'a', { observed_state: 'offline', status: 'offline', state_at: '2026-11-21T07:59:59+08:00' })).toBe('noop')
    expect(applyPatch(store, 'a', { observed_state: 'offline', status: 'offline', state_at: '2026-11-21T08:00:01+08:00' })).toBe('applied')
  })
})

describe('patch log capacity', () => {
  it('voids the open reads on overflow instead of dropping old patches', () => {
    const log = createPatchLog(10)
    const a = log.begin()
    const b = log.begin()
    for (let i = 0; i < 6; i++) log.record('m', { heartbeat_at: `2026-11-21T00:00:0${i}Z` })
    expect(log.valid(a.token)).toBe(false)
    expect(log.valid(b.token)).toBe(false)
    expect(log.size()).toBe(0)
    // a read started after the overflow is fine and keeps its own history
    const c = log.begin()
    log.record('m', { heartbeat_at: '2026-11-21T00:01:00Z' })
    expect(log.valid(c.token)).toBe(true)
    expect(log.patchesSince(c.mark)).toHaveLength(1)
    log.end(a.token); log.end(b.token); log.end(c.token)
    expect(log.size()).toBe(0)
  })

  it('is bounded even when 200 000 events arrive during one read', () => {
    const log = createPatchLog()
    const r = log.begin()
    for (let i = 0; i < 200_000; i++) log.record('m', { observed_state: 'online' })
    expect(log.size()).toBeLessThanOrEqual(10_000)
    expect(log.valid(r.token)).toBe(false)
    log.end(r.token)
  })
})

describe('replayed observed_changed (R2-03 timestamps)', () => {
  it('an old state change replayed after a newer heartbeat does not roll state back', () => {
    const store = new Map([['a', item('a', '2026-11-21T00:00:00Z')]])
    expect(applyPatch(store, 'a', { observed_state: 'offline', status: 'offline', heartbeat_at: '2026-11-19T00:00:00Z' })).toBe('noop')
    expect(store.get('a')!.machine.observed_state).toBe('online')
  })
})
