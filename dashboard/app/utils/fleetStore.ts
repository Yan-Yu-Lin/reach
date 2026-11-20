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
  // Client-only marks (probe_unverified, state_at) are not carried over: the server row is
  // the authority. Events that arrived while this response was in flight are replayed on
  // top of it by the caller.
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
 * Server time up to which the held aggregate state (observed_state/status) is known to be
 * valid: the newest of the last event_at applied, the row's updated_at (reachd bumps it on
 * every observed change) and the newest heartbeat (reachd recomputes state on each one).
 */
export function stateClock(item: MachineWithTunnels): number {
  return Math.max(time(item.state_at), time(item.machine.updated_at), time(knownHeartbeat(item)))
}

/**
 * Apply an SSE patch copy-on-write: the changed machine gets a new object (so a
 * shallowReactive Map re-renders exactly that row) and untouched machines keep theirs.
 *
 * The event is judged FIRST and then applied as a whole or not at all (astra R3-04):
 * its state clock (event_at, else heartbeat_at) older than stateClock(item) means the
 * event is history - its state AND its probe are dropped; only a newer heartbeat time
 * (liveness) still counts. Inside an accepted event the machine state and the hub
 * observation are each ordered by their own clock (daybreak: an older machine state must
 * not replace a newer probe or the other way round).
 *
 * A heartbeat only says "alive at T" (plus aggregate state on newer servers). It is stored
 * as live_heartbeat_at and does NOT touch agent_observation, whose fields (tunnel state,
 * version, errors) are only as fresh as agent_observation.heartbeat_at.
 *
 * Probe results (astra R3-03) are one observation: state, time and error together.
 *  - with a time: replaces the observation if not older than the one held;
 *  - with an explicit empty time: "no observation" (never probed) -> no time, no error;
 *  - without a time (older reachd): the held observation stays as it is but is marked
 *    unverified, so the UI says Unknown until the server confirms (the caller fetches it).
 *
 * Returns 'missing' when the machine is not in the store (the caller should fetch it),
 * 'applied' or 'noop'.
 */
export function applyPatch(store: Store, id: string, patch: MachinePatch): 'missing' | 'applied' | 'noop' {
  const item = store.get(id)
  if (!item) return 'missing'

  let next: MachineWithTunnels = item
  const known = time(knownHeartbeat(item))
  const hb = patch.heartbeat_at ? time(patch.heartbeat_at) : null
  if (hb !== null && hb > known) next = { ...next, live_heartbeat_at: patch.heartbeat_at }

  // ---- 1. accept or reject the rest of the event as a whole
  const clockRaw = patch.state_at ?? patch.heartbeat_at
  const historical = clockRaw !== undefined && time(clockRaw) < stateClock(item)
  if (!historical) {
    // ---- 2. apply every part of the accepted event
    if (patch.state_at && time(patch.state_at) > time(item.state_at)) next = { ...next, state_at: patch.state_at }
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

    const probe = patch.probe
    if (probe) {
      const held = item.hub_observation
      if (probe.at === undefined) {
        // Undated (older reachd): it may be newer or days old. Keep the held observation
        // intact and mark it unverified until the server is asked.
        if (held?.probe_state !== probe.state) next = { ...next, probe_unverified: true }
      } else if (probe.at === '') {
        // The server says: no observation. Nothing of an earlier observation may be borrowed.
        next = { ...next, hub_observation: { probe_state: probe.state, last_probe_at: '' }, probe_unverified: false }
      } else if (time(probe.at) >= time(held?.last_probe_at)) {
        // A dated observation at least as new as ours replaces it as a whole. The event has
        // no error text: keep the held one only if it is this very observation.
        const same = !!held && held.last_probe_at === probe.at && held.probe_state === probe.state
        next = {
          ...next,
          hub_observation: same
            ? { ...held! }
            : { probe_state: probe.state, last_probe_at: probe.at, ...(probe.state === 'reachable' ? { last_success_at: probe.at } : held?.last_success_at ? { last_success_at: held.last_success_at } : {}) },
          probe_unverified: false,
        }
      }
      // else: an older dated observation is history and leaves the newer one alone.
    }
  }

  if (next === item || sameJSON(next, item)) return 'noop'
  store.set(id, next)
  return 'applied'
}

type LoggedPatch = { seq: number; id: string; patch: MachinePatch }

/**
 * Hard cap on what the log keeps while requests are in flight. Normally a few dozen entries
 * at most (requests time out after 15 s). When it is exceeded the log does NOT drop old
 * patches (that would bring back astra F03); it marks itself overflowed, every read that
 * was open at that moment must discard its response, and the caller schedules a fresh
 * authoritative read instead.
 */
export const PATCH_LOG_LIMIT = 5_000

/**
 * Sequence of SSE events and the machine patches among them, kept while any HTTP request
 * is in flight. An HTTP request remembers mark() when it starts; when its response arrives
 * the caller applies it and then replays patchesSince(mark) so newer SSE data wins.
 * touchedSince() lists machines any event mentioned (including ones that only triggered a
 * GET), so a snapshot does not delete them on stale evidence.
 */
export function createPatchLog(limit = PATCH_LOG_LIMIT) {
  let seq = 0
  let entries: LoggedPatch[] = []
  let touched: Array<{ seq: number; id: string }> = []
  const open = new Map<number, number>() // token -> start seq
  const voided = new Set<number>() // tokens whose response must be thrown away
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

  function overflow() {
    // Every open read loses the history it would need; void them all and start empty.
    for (const token of open.keys()) voided.add(token)
    open.clear()
    entries = []
    touched = []
  }

  return {
    /** Record that an event about machine `id` arrived (with its patch, if it carried one). */
    record(id: string, patch?: MachinePatch) {
      seq++
      if (open.size === 0) return
      touched.push({ seq, id })
      if (patch) entries.push({ seq, id, patch })
      if (entries.length + touched.length > limit) overflow()
    },
    /** Start of an HTTP read. Call end(token) when its response has been applied. */
    begin(): { token: number; mark: number } {
      const token = nextToken++
      open.set(token, seq)
      return { token, mark: seq }
    },
    /** False if the log overflowed while this read was open: discard its response. */
    valid(token: number) {
      return !voided.has(token)
    },
    end(token: number) {
      open.delete(token)
      voided.delete(token)
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
