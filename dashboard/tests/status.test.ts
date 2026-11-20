import { describe, expect, it } from 'vitest'
import type { MachineWithTunnels } from '../app/composables/useReachApi'
import { connectionChecks, DAY_MS, groupFleet, statusSentence, uiStatus } from '../app/utils/status'

const NOW = Date.parse('2026-11-21T12:00:00Z')
const ago = (ms: number) => new Date(NOW - ms).toISOString()

function machine(over: Partial<MachineWithTunnels['machine']> = {}, extra: Partial<MachineWithTunnels> = {}): MachineWithTunnels {
  return {
    machine: {
      id: over.id || `m_${Math.random().toString(36).slice(2, 8)}`,
      slug: over.slug || 'box',
      status: 'online',
      desired_state: 'active',
      observed_state: 'online',
      desired_generation: 1,
      update_policy: 'manual',
      local_port: 22,
      created_at: ago(90 * DAY_MS),
      ...over,
    },
    tunnels: [],
    ...extra,
  }
}

const hb = (msAgo: number) => ({ agent_observation: { applied_generation: 1, heartbeat_at: ago(msAgo) } })

describe('uiStatus', () => {
  it('splits offline into recent (attention) and dormant at 7 days (A9)', () => {
    expect(uiStatus(machine({ observed_state: 'offline', status: 'offline' }, hb(2 * DAY_MS)), NOW)).toBe('offline')
    expect(uiStatus(machine({ observed_state: 'gone', status: 'gone' }, hb(3 * DAY_MS)), NOW)).toBe('offline')
    expect(uiStatus(machine({ observed_state: 'offline', status: 'offline' }, hb(8 * DAY_MS)), NOW)).toBe('dormant')
    expect(uiStatus(machine({ observed_state: 'gone', status: 'gone' }, hb(60 * DAY_MS)), NOW)).toBe('dormant')
  })

  it('treats a never-seen machine created long ago as dormant', () => {
    expect(uiStatus(machine({ observed_state: 'unknown', status: 'unknown' }), NOW)).toBe('dormant')
  })

  it('marks retiring older than one day as stuck (A9)', () => {
    expect(uiStatus(machine({ desired_state: 'retiring', status: 'retiring', desired_changed_at: ago(2 * 3600_000) }), NOW)).toBe('retiring')
    expect(uiStatus(machine({ desired_state: 'retiring', status: 'retiring', desired_changed_at: ago(2 * DAY_MS) }), NOW)).toBe('stuck')
  })

  it('uses the server verdict for timed-out and failed cleanups', () => {
    expect(uiStatus(machine({ desired_state: 'retiring', cleanup_state: 'timed_out', desired_changed_at: ago(60_000) }), NOW)).toBe('stuck')
    expect(uiStatus(machine({ desired_state: 'retiring', status: 'cleanup_failed' }), NOW)).toBe('failed')
    expect(uiStatus(machine({ desired_state: 'disabled', status: 'disable_failed' }), NOW)).toBe('disable_failed')
  })

  it('tells the user to retry a failed disable, not to force retire', () => {
    const item = machine({ desired_state: 'disabled', status: 'disable_failed', cleanup_state: 'disable_failed' })
    expect(uiStatus(item, NOW)).toBe('disable_failed')
    expect(statusSentence(item, 'disable_failed', NOW)).toMatch(/Retry disable/)
  })

  it('keeps disabled distinct from failure states (A10)', () => {
    expect(uiStatus(machine({ desired_state: 'disabled', status: 'disabled', observed_state: 'offline' }, hb(90 * DAY_MS)), NOW)).toBe('disabled')
  })
})

describe('groupFleet', () => {
  it('keeps long-dead machines out of Needs attention and hides retired', () => {
    const fleet = [
      machine({ slug: 'omarchy' }, hb(10_000)),
      machine({ slug: 'jack', observed_state: 'offline', status: 'offline' }, hb(DAY_MS)),
      ...Array.from({ length: 20 }, (_, i) => machine({ slug: `dead-${i}`, observed_state: 'gone', status: 'gone' }, hb(40 * DAY_MS))),
      machine({ slug: 'stuck-1', desired_state: 'retiring', status: 'retiring', desired_changed_at: ago(30 * DAY_MS) }),
      machine({ slug: 'off', desired_state: 'disabled', status: 'disabled' }),
      machine({ slug: 'old', desired_state: 'retired', status: 'retired' }),
    ]
    const groups = Object.fromEntries(groupFleet(fleet, NOW).map(g => [g.key, g.items.map(i => i.machine.slug)]))
    expect(groups.attention).toEqual(['jack', 'stuck-1'])
    expect(groups.online).toEqual(['omarchy'])
    expect(groups.dormant).toHaveLength(20)
    expect(groups.disabled).toEqual(['off'])
    expect(Object.values(groups).flat()).not.toContain('old')
  })
})

describe('connectionChecks', () => {
  it('reports unknown, not OK, when there is no probe data', () => {
    const [probe] = connectionChecks(machine({}, hb(10_000)), NOW)
    expect(probe!.state).toBe('unknown')
  })

  it('does not trust a stale agent report', () => {
    const item = machine({}, {
      agent_observation: { applied_generation: 1, heartbeat_at: ago(3 * DAY_MS), tunnel_state: 'connected', local_ssh_state: 'healthy' },
      hub_observation: { probe_state: 'unreachable', last_probe_at: ago(10_000), probe_error: 'connection refused' },
    })
    const [probe, tunnel, sshd] = connectionChecks(item, NOW)
    expect(probe).toMatchObject({ state: 'fail', detail: 'connection refused' })
    expect(tunnel!.state).toBe('unknown')
    expect(sshd!.state).toBe('unknown')
  })

  it('reports all three OK for a healthy machine', () => {
    const item = machine({}, {
      agent_observation: { applied_generation: 1, heartbeat_at: ago(5_000), tunnel_state: 'connected', local_ssh_state: 'healthy' },
      hub_observation: { probe_state: 'reachable', last_probe_at: ago(10_000) },
    })
    expect(connectionChecks(item, NOW).map(c => c.state)).toEqual(['ok', 'ok', 'ok'])
  })
})
