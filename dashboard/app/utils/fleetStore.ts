// Pure helpers for the id-keyed fleet store. They work on any Map, so the composable
// can pass a reactive Map and the tests a plain one.
import type { MachineWithTunnels, RequestRow } from '~/composables/useReachApi'
import type { MachinePatch } from '~/utils/fleetEvents'

type Store = Map<string, MachineWithTunnels>

function sameJSON(a: unknown, b: unknown) {
  return JSON.stringify(a) === JSON.stringify(b)
}

/**
 * Replace the store contents with a fresh snapshot, but keep the old object for every
 * machine that did not change so Vue does not re-render rows that are identical.
 * Returns how many entries were added, replaced or removed.
 */
export function applySnapshot(store: Store, snapshot: readonly MachineWithTunnels[]): number {
  let changes = 0
  const seen = new Set<string>()
  for (const item of snapshot) {
    const id = item.machine.id
    seen.add(id)
    const current = store.get(id)
    if (!current || !sameJSON(current, item)) {
      store.set(id, item)
      changes++
    }
  }
  for (const id of [...store.keys()]) {
    if (!seen.has(id)) {
      store.delete(id)
      changes++
    }
  }
  return changes
}

/** Insert or replace a single machine. Returns false when nothing changed. */
export function upsertMachine(store: Store, item: MachineWithTunnels): boolean {
  const current = store.get(item.machine.id)
  if (current && sameJSON(current, item)) return false
  store.set(item.machine.id, item)
  return true
}

function newer(next: string, current?: string) {
  if (!current) return true
  const a = Date.parse(next)
  const b = Date.parse(current)
  if (!Number.isFinite(a) || !Number.isFinite(b)) return next !== current
  return a > b
}

/**
 * Apply an SSE patch copy-on-write: the changed machine gets a new object (so a
 * shallowReactive Map re-renders exactly that row) and untouched machines keep theirs.
 * Returns 'missing' when the machine is not in the store (the caller should fetch it),
 * 'applied' or 'noop'.
 */
export function applyPatch(store: Store, id: string, patch: MachinePatch): 'missing' | 'applied' | 'noop' {
  const item = store.get(id)
  if (!item) return 'missing'
  let next: MachineWithTunnels = item

  if (patch.heartbeat_at) {
    const obs = item.agent_observation
    if (obs && !newer(patch.heartbeat_at, obs.heartbeat_at)) {
      // An out-of-order heartbeat must not roll observed state back either.
      return 'noop'
    }
    next = {
      ...next,
      agent_observation: obs
        ? { ...obs, heartbeat_at: patch.heartbeat_at }
        : { applied_generation: 0, heartbeat_at: patch.heartbeat_at },
    }
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
  if (patch.probe_state && item.hub_observation?.probe_state !== patch.probe_state) {
    next = {
      ...next,
      hub_observation: item.hub_observation
        ? { ...item.hub_observation, probe_state: patch.probe_state }
        : { probe_state: patch.probe_state, last_probe_at: '' },
    }
  }
  if (next === item) return 'noop'
  store.set(id, next)
  return 'applied'
}

/** Pending requests only; older servers ignore ?status=pending and return history too. */
export function pendingOnly(rows: readonly RequestRow[] | null | undefined): RequestRow[] {
  return (rows || [])
    .filter(r => r.status === 'pending')
    .sort((a, b) => b.created_at.localeCompare(a.created_at))
}
