import { mapSettledLimited } from '~/utils/concurrency'
import { createAuthoritativeRefreshRetry } from '~/utils/authoritativeRefreshRetry'
import { planFleetEvent, type MachinePatch } from '~/utils/fleetEvents'
import { applyPatch, applySnapshot, createPatchLog, pendingOnly, upsertMachine } from '~/utils/fleetStore'
import { createRefreshCoordinator, type RefreshBatch } from '~/utils/refreshCoordinator'
import { groupFleet, uiStatus, type UiStatus } from '~/utils/status'

const machineRefreshConcurrency = 4
const fullRefreshThreshold = 12
/** If the event stream has not said hello by then, load a snapshot without it. */
const bootstrapFallbackMs = 2_500
/** The open machine is re-fetched on its heartbeats at most this often. */
const selectedRefetchMinMs = 15_000

/*
 * Consistency rules (see docs/review/2026-11-21-dashboard.md, astra F02-F06):
 *
 * 1. A full snapshot is taken after every `hello`, resumed or not. The stream is subscribed
 *    by then, so nothing that happens after the snapshot is read can be missed. Durable
 *    replay does not cover live-only heartbeats, so a resumed stream is not "complete".
 * 2. HTTP responses can be older than SSE events that arrived while they were in flight.
 *    Every machine event is logged with a sequence number; after applying a response the
 *    patches that arrived since the request started are replayed on top, and machines that
 *    any event touched are not deleted on the snapshot's word (they are re-checked).
 * 3. Each lane (snapshot, pending requests, single machines) retries its own failure with a
 *    bounded backoff and clears its own error on success.
 */
export function useFleet() {
  const api = useReachApi()
  const sse = useSSE()
  const now = useNow()

  // id -> machine. shallowReactive: Vue tracks set/delete per key, not deep mutations.
  // fleetStore replaces a machine object whenever it changes, so only that row re-renders.
  const byId = shallowReactive(new Map<string, MachineWithTunnels>())
  const pending = shallowRef<RequestRow[]>([])
  const loading = ref(true)
  const loaded = ref(false)
  const selectedId = ref<string | null>(null)
  const laneErrors = reactive({ machines: '', requests: '', machine: '' })
  const error = computed(() => laneErrors.machines || laneErrors.requests || laneErrors.machine)
  const patchLog = createPatchLog()
  let disposeSSE: (() => void) | null = null
  let bootstrapTimer: ReturnType<typeof setTimeout> | null = null
  let lastSelectedFetch = 0

  const fullRetry = createAuthoritativeRefreshRetry(() => {
    void refreshCoordinator.requestFull(true).catch(() => {})
  })
  const requestsRetry = createAuthoritativeRefreshRetry(() => {
    void refreshCoordinator.requestRequests().catch(() => {})
  })

  const machines = computed(() => [...byId.values()])
  const statusOf = computed(() => {
    const out = new Map<string, UiStatus>()
    for (const item of byId.values()) out.set(item.machine.id, uiStatus(item, now.value))
    return out
  })
  const groups = computed(() => groupFleet(byId.values(), now.value))
  const counts = computed(() => {
    const c: Record<UiStatus, number> = {
      online: 0, degraded: 0, offline: 0, dormant: 0, pending: 0, disabled: 0,
      retiring: 0, stuck: 0, failed: 0, disable_failed: 0, retired: 0, unknown: 0,
    }
    for (const s of statusOf.value.values()) c[s]++
    return c
  })
  const attentionCount = computed(() => groups.value.find(g => g.key === 'attention')?.items.length || 0)
  const pendingCount = computed(() => pending.value.length)
  const selectedMachine = computed(() => (selectedId.value && byId.get(selectedId.value)) || null)

  function replaySince(mark: number, id?: string) {
    for (const e of patchLog.patchesSince(mark, id)) applyPatch(byId, e.id, e.patch, e.receivedAt)
  }

  async function loadPendingLane() {
    try {
      pending.value = pendingOnly(await api.pendingRequests())
      laneErrors.requests = ''
      requestsRetry.succeeded()
    } catch (e: any) {
      laneErrors.requests = e?.message || 'Failed to load requests'
      requestsRetry.failed()
    }
  }

  async function loadMachinesLane() {
    const { token, mark } = patchLog.begin()
    try {
      const snapshot = await api.machines()
      const touched = patchLog.touchedSince(mark)
      applySnapshot(byId, snapshot || [], touched)
      replaySince(mark)
      // Machines an event mentioned but the snapshot did not list: ask about each directly
      // instead of trusting a list that may predate them.
      const listed = new Set((snapshot || []).map(m => m.machine.id))
      for (const id of touched) if (!listed.has(id)) void refreshCoordinator.requestMachine(id).catch(() => {})
      laneErrors.machines = ''
      loaded.value = true
      return true
    } catch (e: any) {
      laneErrors.machines = e?.message || 'Failed to load machines'
      return false
    } finally {
      patchLog.end(token)
    }
  }

  async function loadMachineLane(ids: string[]) {
    const { token, mark } = patchLog.begin()
    try {
      const results = await mapSettledLimited(ids, machineRefreshConcurrency, id => api.machine(id))
      let failures = 0
      results.forEach((r, i) => {
        const id = ids[i]!
        if (r.status === 'fulfilled') {
          upsertMachine(byId, r.value)
          replaySince(mark, id)
        } else if ((r.reason as any)?.status === 404) {
          // Gone on the server (retired machines are no longer listed).
          byId.delete(id)
        } else {
          failures++
        }
      })
      if (failures > 0) {
        laneErrors.machine = `${failures} machine refresh${failures === 1 ? '' : 'es'} failed. Refreshing fleet...`
        void refreshCoordinator.requestFull().catch(() => {})
      } else {
        laneErrors.machine = ''
      }
    } finally {
      patchLog.end(token)
    }
  }

  async function executeRefresh(batch: RefreshBatch) {
    if (batch.full) {
      loading.value = true
      try {
        // Each half is applied as soon as it lands; a slow request list must not hold a
        // machine snapshot back (that would widen the window it can be stale in).
        const [machinesOk] = await Promise.all([loadMachinesLane(), loadPendingLane()])
        if (machinesOk) {
          laneErrors.machine = ''
          fullRetry.succeeded()
        } else {
          fullRetry.failed()
        }
      } finally {
        loading.value = false
      }
      return
    }

    if (batch.machineIds.length >= fullRefreshThreshold) {
      await executeRefresh({ full: true, machineIds: [], requests: false })
      return
    }

    const jobs: Promise<unknown>[] = []
    if (batch.requests) jobs.push(loadPendingLane())
    if (batch.machineIds.length > 0) jobs.push(loadMachineLane(batch.machineIds))
    await Promise.all(jobs)
  }

  const refreshCoordinator = createRefreshCoordinator(executeRefresh)

  function refresh() {
    return refreshCoordinator.requestFull(true)
  }

  function refreshMachine(id: string) {
    return refreshCoordinator.requestMachine(id)
  }

  function refreshRequests() {
    return refreshCoordinator.requestRequests()
  }

  function clearBootstrapTimer() {
    if (bootstrapTimer) clearTimeout(bootstrapTimer)
    bootstrapTimer = null
  }

  function recordMachineEvent(id: string, patch?: MachinePatch) {
    patchLog.record(id, patch)
  }

  function handleEvent(event: string, rawData: unknown) {
    const plan = planFleetEvent(event, rawData)
    switch (plan.kind) {
      case 'hello':
        // Subscribed now: a snapshot read from here on cannot miss anything (rule 1).
        clearBootstrapTimer()
        void refreshCoordinator.requestFull(true).catch(() => {})
        return
      case 'full':
        void refreshCoordinator.requestFull().catch(() => {})
        return
      case 'requests':
        void refreshCoordinator.requestRequests().catch(() => {})
        if (plan.notifyPending) notifyPending(rawData as Record<string, any>)
        return
      case 'machine':
        recordMachineEvent(plan.machineId)
        void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
        return
      case 'detail':
        recordMachineEvent(plan.machineId)
        if (selectedId.value === plan.machineId) void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
        return
      case 'patch': {
        recordMachineEvent(plan.machineId, plan.patch)
        const result = applyPatch(byId, plan.machineId, plan.patch)
        if (result === 'missing') {
          // Before the first snapshot every machine is "missing"; that snapshot is coming
          // (hello requested it) and this patch is replayed on top of it, so no GET.
          if (loaded.value) void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
          return
        }
        if (selectedId.value !== plan.machineId) return
        if (plan.refetchIfSelected === 'now' || Date.now() - lastSelectedFetch >= selectedRefetchMinMs) {
          lastSelectedFetch = Date.now()
          void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
        }
        return
      }
      default:
        return
    }
  }

  // Opening a machine always shows its current details, not the list snapshot's (F04).
  watch(selectedId, (id) => {
    if (!id || !import.meta.client) return
    lastSelectedFetch = Date.now()
    void refreshCoordinator.requestMachine(id).catch(() => {})
  })

  /** Connect the event stream; its first hello loads the snapshot. */
  function start() {
    setupSSE()
    clearBootstrapTimer()
    // If the stream cannot connect (proxy trouble, server restarting), still show data.
    bootstrapTimer = setTimeout(() => {
      bootstrapTimer = null
      if (!loaded.value) void refreshCoordinator.requestFull(true).catch(() => {})
    }, bootstrapFallbackMs)
  }

  function setupSSE() {
    disposeSSE?.()
    if (!api.token.value) return () => {}

    const config = useRuntimeConfig()
    const removeHandler = sse.onEvent(handleEvent)
    const startStream = () => sse.start(api.token.value || '', config.public.apiBase as string || '/api')

    // Phones suspend background tabs. When the page comes back and the stream is not live,
    // reconnect now instead of waiting for the backoff timer (its hello brings a snapshot).
    // If it stayed live, live-only heartbeats may still have been dropped while frozen.
    let hiddenAt = 0
    const onVisible = () => {
      if (document.visibilityState === 'hidden') {
        hiddenAt = Date.now()
        return
      }
      if (!api.token.value) return
      const awayMs = hiddenAt ? Date.now() - hiddenAt : 0
      hiddenAt = 0
      if (sse.status.value !== 'live') startStream()
      else if (awayMs > 60_000) void refreshCoordinator.requestFull().catch(() => {})
    }
    document.addEventListener('visibilitychange', onVisible)

    startStream()
    disposeSSE = () => {
      document.removeEventListener('visibilitychange', onVisible)
      removeHandler()
      sse.stop()
      disposeSSE = null
    }
    return disposeSSE
  }

  function dispose() {
    clearBootstrapTimer()
    disposeSSE?.()
    fullRetry.dispose()
    requestsRetry.dispose()
    refreshCoordinator.dispose()
  }

  /** Only ever shows a notification; asking for permission happens in Settings, on a tap. */
  function notifyPending(data: Record<string, any>) {
    if (!import.meta.client || !('Notification' in window)) return
    if (Notification.permission !== 'granted') return
    try {
      new Notification('Reach: new machine request', {
        body: `${data.name || 'Unknown'} wants to connect`,
        tag: 'reach-pending',
      })
    } catch {
      // Some mobile browsers only allow notifications from a service worker.
    }
  }

  watch(pendingCount, (count) => {
    if (import.meta.client) document.title = count > 0 ? `(${count}) Reach` : 'Reach'
  })

  return {
    byId,
    machines,
    pending,
    loading,
    loaded,
    error,
    selectedId,
    selectedMachine,
    statusOf,
    groups,
    counts,
    attentionCount,
    pendingCount,
    start,
    refresh,
    refreshMachine,
    refreshRequests,
    setupSSE,
    dispose,
    /** Exposed for tests and the harness. */
    handleEvent,
  }
}
