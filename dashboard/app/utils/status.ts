// One status language for the whole dashboard. Every dot, badge, group and
// counter goes through uiStatus() so the same machine never shows two colours.
import type { MachineWithTunnels } from '~/composables/useReachApi'

export type UiStatus =
  | 'online'
  | 'degraded'
  | 'offline'
  | 'dormant'
  | 'pending'
  | 'disabled'
  | 'retiring'
  | 'stuck'
  | 'failed'
  | 'disable_failed'
  | 'retired'
  | 'unknown'

export const DAY_MS = 86_400_000
/** Offline longer than this is "dormant": probably never coming back. */
export const DORMANT_AFTER_MS = 7 * DAY_MS
/** Retiring longer than this without the agent confirming is "stuck". */
export const RETIRING_STUCK_AFTER_MS = DAY_MS

export const STATUS_LABEL: Record<UiStatus, string> = {
  online: 'Online',
  degraded: 'Degraded',
  offline: 'Offline',
  dormant: 'Dormant',
  pending: 'Pending',
  disabled: 'Disabled',
  retiring: 'Retiring',
  stuck: 'Retire stuck',
  failed: 'Cleanup failed',
  disable_failed: 'Disable failed',
  retired: 'Retired',
  unknown: 'Unknown',
}

function parseTime(value?: string | null): number | null {
  if (!value) return null
  const t = Date.parse(value)
  return Number.isFinite(t) ? t : null
}

/** Best evidence of when the machine was last alive (including live-only heartbeats). */
export function lastSeenAt(item: MachineWithTunnels): number | null {
  const candidates = [
    parseTime(item.agent_observation?.heartbeat_at),
    parseTime(item.live_heartbeat_at),
    parseTime(item.hub_observation?.last_success_at),
    ...item.tunnels.map(t => parseTime(t.last_seen_at)),
  ].filter((t): t is number => t !== null)
  if (candidates.length > 0) return Math.max(...candidates)
  return null
}

export function uiStatus(item: MachineWithTunnels, now = Date.now()): UiStatus {
  const m = item.machine
  const cleanup = m.cleanup_state || ''
  if (m.desired_state === 'retired') return 'retired'
  // Disable failing and cleanup failing need different actions (retry disable vs force
  // retire), so they are different statuses even though both are red.
  if (m.status === 'disable_failed' || cleanup === 'disable_failed') return 'disable_failed'
  if (m.status === 'cleanup_failed' || cleanup === 'cleanup_failed') return 'failed'
  if (m.desired_state === 'retiring') {
    if (cleanup === 'timed_out') return 'stuck'
    const since = parseTime(m.desired_changed_at) ?? parseTime(m.updated_at)
    if (since !== null && now - since > RETIRING_STUCK_AFTER_MS) return 'stuck'
    return 'retiring'
  }
  if (m.desired_state === 'disabled') return 'disabled'
  if (m.status === 'pending') return 'pending'
  if (m.observed_state === 'online') return 'online'
  if (m.observed_state === 'degraded') return 'degraded'

  // offline, gone or unknown: decide between "just dropped" and "long dead".
  const seen = lastSeenAt(item) ?? parseTime(m.created_at)
  if (seen !== null && now - seen > DORMANT_AFTER_MS) return 'dormant'
  if (m.observed_state === 'unknown' && lastSeenAt(item) === null) return 'unknown'
  return 'offline'
}

export type FleetGroupKey = 'attention' | 'online' | 'dormant' | 'disabled'

export type FleetGroup = {
  key: FleetGroupKey
  label: string
  hint: string
  collapsible: boolean
  items: MachineWithTunnels[]
}

// Retired machines are history: the server stops listing them by default and the
// dashboard never shows them in the fleet.
const GROUP_OF: Record<UiStatus, FleetGroupKey | null> = {
  offline: 'attention',
  degraded: 'attention',
  stuck: 'attention',
  failed: 'attention',
  disable_failed: 'attention',
  unknown: 'attention',
  pending: 'attention',
  online: 'online',
  dormant: 'dormant',
  disabled: 'disabled',
  retiring: 'disabled',
  retired: null,
}

// Inside a group, show the most urgent first, then by name. A machine that just dropped is
// what Arthur most likely needs to act on; stuck removals can wait for a day at home.
const URGENCY: Record<UiStatus, number> = {
  offline: 0, degraded: 1, disable_failed: 2, failed: 2, stuck: 3, pending: 4, unknown: 5,
  online: 6, retiring: 7, disabled: 8, dormant: 9, retired: 10,
}

export function groupFleet(items: Iterable<MachineWithTunnels>, now = Date.now()): FleetGroup[] {
  const buckets: Record<FleetGroupKey, Array<{ item: MachineWithTunnels; status: UiStatus }>> = {
    attention: [], online: [], dormant: [], disabled: [],
  }
  for (const item of items) {
    const status = uiStatus(item, now)
    const key = GROUP_OF[status]
    if (key) buckets[key].push({ item, status })
  }
  const sort = (rows: Array<{ item: MachineWithTunnels; status: UiStatus }>) =>
    rows
      .sort((a, b) => URGENCY[a.status] - URGENCY[b.status] || a.item.machine.slug.localeCompare(b.item.machine.slug))
      .map(r => r.item)

  const groups: FleetGroup[] = [
    { key: 'attention', label: 'Needs attention', hint: 'Dropped in the last 7 days, degraded, or stuck', collapsible: false, items: sort(buckets.attention) },
    { key: 'online', label: 'Online', hint: 'Reachable now', collapsible: false, items: sort(buckets.online) },
    { key: 'dormant', label: 'Dormant', hint: 'Offline for more than 7 days', collapsible: true, items: sort(buckets.dormant) },
    { key: 'disabled', label: 'Disabled & retiring', hint: 'Turned off by you', collapsible: true, items: sort(buckets.disabled) },
  ]
  return groups.filter(g => g.items.length > 0)
}

export function formatAge(ms: number): string {
  if (ms < 0) ms = 0
  const s = Math.floor(ms / 1000)
  if (s < 60) return `${s}s`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m`
  const h = Math.floor(m / 60)
  if (h < 48) return `${h}h`
  return `${Math.floor(h / 24)}d`
}

/** One plain-language sentence for the top of the detail page. */
export function statusSentence(item: MachineWithTunnels, status: UiStatus, now = Date.now()): string {
  const seen = lastSeenAt(item)
  const age = seen === null ? null : formatAge(now - seen)
  const agent = item.agent_observation
  const probe = item.hub_observation?.probe_state
  switch (status) {
    case 'online':
      return 'Tunnel is up. You can ssh in.'
    case 'degraded': {
      const tunnelUp = agent?.tunnel_state === 'connected' || agent?.tunnel_state === 'running'
      if (tunnelUp && probe !== 'reachable') return 'Agent says the tunnel is up, but the hub cannot reach the target sshd. ssh may fail.'
      if (!tunnelUp && probe === 'reachable') return 'The hub can reach the target, but the agent reports a problem. ssh probably works.'
      return 'Partly working. ssh may fail; check the connection check below.'
    }
    case 'offline':
      return age ? `No heartbeat for ${age}. The tunnel is down; ssh will not work.` : 'No heartbeat. The tunnel is down; ssh will not work.'
    case 'dormant':
      return age ? `Not seen for ${age}. Probably not coming back; consider disabling it.` : 'Never seen online. Consider disabling it.'
    case 'pending':
      return 'Waiting for setup to finish.'
    case 'disabled':
      return 'You disabled this machine. Its tunnel key is blocked. Enable brings it back.'
    case 'retiring':
      return 'Removal requested. Waiting for the agent to confirm the uninstall.'
    case 'stuck':
      return 'Removal started but the agent never confirmed. Force retire finishes the hub-side cleanup.'
    case 'disable_failed':
      return item.machine.desired_state === 'disabled'
        ? 'Disable did not finish: the hub could not block the tunnel key, so it may still work. Retry disable.'
        : 'Removal could not block the tunnel key on the hub, so it may still work. Force retire tries again.'
    case 'failed':
      return 'Hub-side cleanup failed, so the tunnel account was kept. Force retire tries the cleanup again.'
    case 'retired':
      return 'Removed.'
    default:
      return 'No heartbeat yet.'
  }
}

export type CheckState = 'ok' | 'fail' | 'unknown' | 'na'
export type ConnectionCheck = { key: string; label: string; state: CheckState; detail: string }

const OK_TUNNEL = new Set(['connected', 'running'])
const OK_SSH = new Set(['healthy', 'ok', 'running'])

/**
 * How old a probe result may be and still count as "now". The hub probes every 30-45 s but,
 * since the 2026-11 backend, only rewrites an unchanged row every 10 minutes
 * (hubObservationRefresh), so anything under ~15 minutes is a live probe.
 */
export const PROBE_FRESH_MS = 15 * 60_000
/** Agent detail fields (tunnel/sshd state) are as fresh as the observation they came in. */
export const AGENT_REPORT_FRESH_MS = 10 * 60_000

const NOT_ACTIVE_NOTE: Record<string, string> = {
  disabled: 'Not checked: the machine is disabled',
  retiring: 'Not checked: the machine is being removed',
  retired: 'Not checked: the machine is retired',
}

/** Hub probe -> tunnel -> target sshd, each with its own verdict and reason. */
export function connectionChecks(item: MachineWithTunnels, now = Date.now()): ConnectionCheck[] {
  const hub = item.hub_observation
  const agent = item.agent_observation
  const lifecycle = item.machine.desired_state

  // The hub stops probing disabled/retiring/retired tunnels and the agent stops (or is told
  // to stop) the tunnel, so whatever was stored last is history, not the current state.
  if (lifecycle !== 'active') {
    const note = NOT_ACTIVE_NOTE[lifecycle] || `Not checked: the machine is ${lifecycle}`
    const last = (label: string, at?: string | null) => at ? `${note}. ${label} ${formatAge(now - (parseTime(at) ?? now))} ago.` : `${note}.`
    return [
      { key: 'probe', label: 'Hub probe', state: 'na', detail: last('Last probe', hub?.last_probe_at) },
      { key: 'tunnel', label: 'Tunnel', state: 'na', detail: last('Last agent report', agent?.heartbeat_at) },
      { key: 'sshd', label: 'Target sshd', state: 'na', detail: `${note}.` },
    ]
  }

  const probeAt = parseTime(hub?.last_probe_at)
  const probeStale = probeAt === null || now - probeAt > PROBE_FRESH_MS
  let probe: ConnectionCheck
  if (!hub || !hub.probe_state) probe = { key: 'probe', label: 'Hub probe', state: 'unknown', detail: 'No probe data yet' }
  else if (item.probe_unverified) {
    // An undated probe result arrived (older reachd, possibly replayed history): what we
    // hold may no longer be current and the new one cannot be dated. Wait for the server.
    probe = { key: 'probe', label: 'Hub probe', state: 'unknown', detail: 'Unknown: a newer probe result could not be dated; checking with the hub' }
  } else if (hub.probe_state === 'unknown' && !hub.last_probe_at) {
    probe = { key: 'probe', label: 'Hub probe', state: 'unknown', detail: 'Not probed yet' }
  } else if (probeStale) {
    const age = probeAt === null ? 'unknown time' : `${formatAge(now - probeAt)} ago`
    probe = { key: 'probe', label: 'Hub probe', state: 'unknown', detail: `Unknown: last probe ${age} said ${hub.probe_state}` }
  } else if (hub.probe_state === 'reachable') probe = { key: 'probe', label: 'Hub probe', state: 'ok', detail: 'Hub reached the target sshd through the tunnel' }
  else probe = { key: 'probe', label: 'Hub probe', state: 'fail', detail: hub.probe_error || `Probe says ${hub.probe_state}` }

  // Judge tunnel/sshd by when THIS observation was reported, never by a later live heartbeat:
  // a heartbeat proves the agent is alive, not that its old tunnel_state is still true.
  const hbAt = parseTime(agent?.heartbeat_at)
  const agentStale = hbAt === null || now - hbAt > AGENT_REPORT_FRESH_MS
  const staleNote = hbAt === null ? 'no agent report yet' : `last agent report ${formatAge(now - hbAt)} ago`

  let tunnel: ConnectionCheck
  if (!agent?.tunnel_state) tunnel = { key: 'tunnel', label: 'Tunnel', state: 'unknown', detail: 'Agent has not reported' }
  else if (agentStale) tunnel = { key: 'tunnel', label: 'Tunnel', state: 'unknown', detail: `Unknown: ${staleNote}` }
  else if (OK_TUNNEL.has(agent.tunnel_state)) tunnel = { key: 'tunnel', label: 'Tunnel', state: 'ok', detail: `Agent reports ${agent.tunnel_state}` }
  else tunnel = { key: 'tunnel', label: 'Tunnel', state: 'fail', detail: agent.tunnel_error || `Agent reports ${agent.tunnel_state}` }

  let sshd: ConnectionCheck
  if (!agent?.local_ssh_state) sshd = { key: 'sshd', label: 'Target sshd', state: 'unknown', detail: 'Agent has not reported' }
  else if (agentStale) sshd = { key: 'sshd', label: 'Target sshd', state: 'unknown', detail: `Unknown: ${staleNote}` }
  else if (OK_SSH.has(agent.local_ssh_state)) sshd = { key: 'sshd', label: 'Target sshd', state: 'ok', detail: `Port ${item.machine.local_port || 22} answers` }
  else sshd = { key: 'sshd', label: 'Target sshd', state: 'fail', detail: agent.local_ssh_error || `Agent reports ${agent.local_ssh_state}` }

  return [probe, tunnel, sshd]
}
