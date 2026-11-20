// Pure helpers for the id-keyed fleet store. They work on any Map, so the composable
// can pass a reactive Map and the tests a plain one.
//
// Two sources write into the store and they are not ordered with each other:
//   - HTTP responses (full snapshot, single machine) describe the server at the moment the
//     request was read, which can be older than SSE events that arrived while it was in flight;
//   - SSE patches (heartbeat, observed/health changes) describe a later moment.
// createPatchLog() records the patches so an HTTP response can be applied and the patches
// that arrived after the request started replayed on top (see useFleet).
import type { MachineWithTunnels, RequestRow } from '~/composables/useReachApi'
import type { MachinePatch } from '~/utils/fleetEvents'

type Store = Map<string, MachineWithTunnels>

function sameJSON(a: unknown, b: unknown) {
  return JSON.stringify(a) === JSON.stringify(b)
}

function time(value?: string | null): number {
  if (!value) return Number.NEGATIVE_INFINITY
  const t = Date.parse(value)
  return Number.isFinite(t) ? t : Number.NEGATIVE_INFINITY
}

/** Latest heartbeat we know of: the full observation's or a newer live-only one. */
export function knownHeartbeat(item: MachineWithTunnels): string | undefined {
  const obs = item.agent_observation?.heartbeat_at
  const live = item.live_heartbeat_at
  if (!live) return obs
  if (!obs) return live
  return time(live) > time(obs) ? live : obs
}

/**
 * Server data for one machine, keeping a live heartbeat that is newer than the payload's
 * observation (a heartbeat is published right after it is stored, so normally the payload
 * already contains it; this only matters when events and responses interleave).
 */
function fromServer(current: MachineWithTunnels | undefined, incoming: MachineWithTunnels): MachineWithTunnels {
  const live = current?.live_heartbeat_at
  if (live && time(live) > time(incoming.agent_observation?.heartbeat_at)) {
    return { ...incoming, live_heartbeat_at: live }
  }
  return incoming
}

/**
 * Replace the store contents with a fresh snapshot, but keep the old object for every
 * machine that did not change so Vue does not re-render rows that are identical.
 * `keep` lists machines missing from the snapshot that must not be deleted yet, because
 * an event touched them after the snapshot was read (the caller re-checks them one by one).
 * Returns how many entries were added, replaced or removed.
 */
export function applySnapshot(store: Store, snapshot: readonly MachineWithTunnels[], keep: ReadonlySet<string> = new Set()): number {
  let changes = 0
  const seen = new Set<string>()
  for (const raw of snapshot) {
    const id = raw.machine.id
    seen.add(id)
    const current = store.get(id)
    const item = fromServer(current, raw)
    if (!current || !sameJSON(current, item)) {
      store.set(id, item)
      changes++
    }
  }
  for (const id of [...store.keys()]) {
    if (!seen.has(id) && !keep.has(id)) {
      store.delete(id)
      changes++
    }
  }
  return changes
}

/** Insert or replace a single machine. Returns false when nothing changed. */
export function upsertMachine(store: Store, raw: MachineWithTunnels): boolean {
  const current = store.get(raw.machine.id)
  const item = fromServer(current, raw)
  if (current && sameJSON(current, item)) return false
  store.set(item.machine.id, item)
  return true
}

/**
 * Apply an SSE patch copy-on-write: the changed machine gets a new object (so a
 * shallowReactive Map re-renders exactly that row) and untouched machines keep theirs.
 *
 * A heartbeat only says "alive at T" (plus aggregate state on newer servers). It is stored
 * as live_heartbeat_at and does NOT touch agent_observation, whose fields (tunnel state,
 * version, errors) are only as fresh as agent_observation.heartbeat_at.
 *
 * Returns 'missing' when the machine is not in the store (the caller should fetch it),
 * 'applied' or 'noop'.
 */
export function applyPatch(store: Store, id: string, patch: MachinePatch, receivedAt = new Date().toISOString()): 'missing' | 'applied' | 'noop' {
  const item = store.get(id)
  if (!item) return 'missing'
  let next: MachineWithTunnels = item

  if (patch.heartbeat_at) {
    const incoming = time(patch.heartbeat_at)
    const known = time(knownHeartbeat(item))
    // An out-of-order (older) heartbeat must not roll observed state back either. An equal
    // one is the same heartbeat again (replayed after an HTTP response): re-apply its state.
    if (incoming < known) return 'noop'
    if (incoming > known) next = { ...next, live_heartbeat_at: patch.heartbeat_at }
  }
  const machineChanges: Partial<MachineWithTunnels['machine']> = {}
  if (patch.observed_state && item.machine.observed_state !== patch.observed_state) {
    machineChanges.observed_state = patch.observed_state
  }
  if (patch.status && item.machine.status !== patch.status) {
    machineChanges.status = patch.status
  }
  if (Object.keys(machineChanges).length > 0) {
    next = { ...next, machine: { ...item.machine, ...machineChanges } }
  }
  if (patch.probe_state) {
    // health_changed is sent when a probe just produced a new result: that is a fresh probe.
    next = {
      ...next,
      hub_observation: { ...(item.hub_observation || {}), probe_state: patch.probe_state, last_probe_at: receivedAt },
    }
  }
  if (next === item) return 'noop'
  store.set(id, next)
  return 'applied'
}

type LoggedPatch = { seq: number; id: string; patch: MachinePatch; receivedAt: string }

/**
 * Sequence of SSE events and the machine patches among them, kept while any HTTP request
 * is in flight. An HTTP request remembers mark() when it starts; when its response arrives
 * the caller applies it and then replays patchesSince(mark) so newer SSE data wins.
 * touchedSince() lists machines any event mentioned (including ones that only triggered a
 * GET), so a snapshot does not delete them on stale evidence.
 */
export function createPatchLog() {
  let seq = 0
  let entries: LoggedPatch[] = []
  let touched: Array<{ seq: number; id: string }> = []
  const open = new Map<number, number>() // token -> start seq
  let nextToken = 0

  function prune() {
    if (open.size === 0) {
      entries = []
      touched = []
      return
    }
    const oldest = Math.min(...open.values())
    entries = entries.filter(e => e.seq > oldest)
    touched = touched.filter(e => e.seq > oldest)
  }

  return {
    /** Record that an event about machine `id` arrived (with its patch, if it carried one). */
    record(id: string, patch?: MachinePatch, receivedAt = new Date().toISOString()) {
      seq++
      if (open.size === 0) return
      touched.push({ seq, id })
      if (patch) entries.push({ seq, id, patch, receivedAt })
    },
    /** Start of an HTTP read. Call end(token) when its response has been applied. */
    begin(): { token: number; mark: number } {
      const token = nextToken++
      open.set(token, seq)
      return { token, mark: seq }
    },
    end(token: number) {
      open.delete(token)
      prune()
    },
    patchesSince(mark: number, id?: string): LoggedPatch[] {
      return entries.filter(e => e.seq > mark && (id === undefined || e.id === id))
    },
    touchedSince(mark: number): Set<string> {
      return new Set(touched.filter(e => e.seq > mark).map(e => e.id))
    },
    /** For tests. */
    size: () => entries.length + touched.length,
  }
}

/** Pending requests only; older servers ignore ?status=pending and return history too. */
export function pendingOnly(rows: readonly RequestRow[] | null | undefined): RequestRow[] {
  return (rows || [])
    .filter(r => r.status === 'pending')
    .sort((a, b) => b.created_at.localeCompare(a.created_at))
}
