import { mapSettledLimited, summarizeSettled } from '~/utils/concurrency'
import { createAuthoritativeRefreshRetry } from '~/utils/authoritativeRefreshRetry'
import { planFleetEvent } from '~/utils/fleetEvents'
import { applyPatch, applySnapshot, pendingOnly, upsertMachine } from '~/utils/fleetStore'
import { createRefreshCoordinator, type RefreshBatch } from '~/utils/refreshCoordinator'
import { groupFleet, uiStatus, type UiStatus } from '~/utils/status'

const machineRefreshConcurrency = 4
const fullRefreshThreshold = 12
/** A non-resumed hello this soon after a full snapshot started does not need another one. */
const helloDedupMs = 3_000

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
  const error = ref('')
  const selectedId = ref<string | null>(null)
  let lastFullStartedAt = 0
  let disposeSSE: (() => void) | null = null

  const authoritativeRetry = createAuthoritativeRefreshRetry(() => {
    void refreshCoordinator.requestFull(true).catch(() => {})
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
      retiring: 0, stuck: 0, failed: 0, retired: 0, unknown: 0,
    }
    for (const s of statusOf.value.values()) c[s]++
    return c
  })
  const attentionCount = computed(() => groups.value.find(g => g.key === 'attention')?.items.length || 0)
  const pendingCount = computed(() => pending.value.length)
  const selectedMachine = computed(() => (selectedId.value && byId.get(selectedId.value)) || null)

  async function loadPending() {
    pending.value = pendingOnly(await api.pendingRequests())
  }

  async function executeRefresh(batch: RefreshBatch) {
    if (batch.full) {
      lastFullStartedAt = Date.now()
      loading.value = true
      try {
        const [machineResult, requestResult] = await Promise.allSettled([api.machines(), api.pendingRequests()])
        const failures: unknown[] = []
        if (machineResult.status === 'fulfilled') applySnapshot(byId, machineResult.value || [])
        else failures.push(machineResult.reason)
        if (requestResult.status === 'fulfilled') pending.value = pendingOnly(requestResult.value)
        else failures.push(requestResult.reason)
        if (failures.length > 0) {
          const failure = failures[0] as any
          error.value = failure?.message || 'Failed to load dashboard data'
          authoritativeRetry.failed()
        } else {
          error.value = ''
          loaded.value = true
          authoritativeRetry.succeeded()
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
    if (batch.requests) {
      jobs.push(loadPending().catch((e: any) => { error.value = e?.message || 'Failed to load requests' }))
    }
    if (batch.machineIds.length > 0) {
      jobs.push((async () => {
        const updateResults = await mapSettledLimited(batch.machineIds, machineRefreshConcurrency, id => api.machine(id))
        const { values: updates, failureCount, needsFullRefresh } = summarizeSettled(updateResults)
        for (const updated of updates) upsertMachine(byId, updated)
        if (needsFullRefresh) {
          error.value = `${failureCount} machine refresh${failureCount === 1 ? '' : 'es'} failed. Refreshing fleet...`
          void refreshCoordinator.requestFull().catch(() => {})
        }
      })())
    }
    await Promise.all(jobs)
  }

  const refreshCoordinator = createRefreshCoordinator(executeRefresh)

  function refresh() {
    lastFullStartedAt = Date.now()
    return refreshCoordinator.requestFull(true)
  }

  function refreshMachine(id: string) {
    return refreshCoordinator.requestMachine(id)
  }

  function refreshRequests() {
    return refreshCoordinator.requestRequests()
  }

  function handleEvent(event: string, rawData: unknown) {
    const plan = planFleetEvent(event, rawData)
    switch (plan.kind) {
      case 'hello': {
        // Resumed with Last-Event-ID: the server already replayed what we missed.
        if (sse.wasResumed()) return
        // Fresh stream right after (or during) a snapshot: that snapshot is current enough.
        // Something could still have changed between the snapshot read and the subscribe;
        // machines heal on their next heartbeat (it carries observed_state), so only the
        // tiny pending-request list is re-checked.
        if (Date.now() - lastFullStartedAt < helloDedupMs) {
          void refreshCoordinator.requestRequests().catch(() => {})
          return
        }
        void refreshCoordinator.requestFull().catch(() => {})
        return
      }
      case 'full':
        void refreshCoordinator.requestFull().catch(() => {})
        return
      case 'requests':
        void refreshCoordinator.requestRequests().catch(() => {})
        if (plan.notifyPending) notifyPending(rawData as Record<string, any>)
        return
      case 'machine':
        void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
        return
      case 'detail':
        if (selectedId.value === plan.machineId) void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
        return
      case 'patch': {
        const result = applyPatch(byId, plan.machineId, plan.patch)
        if (result === 'missing' || (plan.refetchIfSelected && selectedId.value === plan.machineId)) {
          void refreshCoordinator.requestMachine(plan.machineId).catch(() => {})
        }
        return
      }
      default:
        return
    }
  }

  function setupSSE() {
    disposeSSE?.()
    if (!api.token.value) return () => {}

    const config = useRuntimeConfig()
    const removeHandler = sse.onEvent(handleEvent)
    const startStream = () => sse.start(api.token.value || '', config.public.apiBase as string || '/api')

    // Phones suspend background tabs. When the page comes back and the stream is not live,
    // reconnect now instead of waiting for the backoff timer. Heartbeats are live-only and
    // never replayed, so after a long absence take one fresh snapshot for the "x ago" labels.
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
    disposeSSE?.()
    authoritativeRetry.dispose()
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
    refresh,
    refreshMachine,
    refreshRequests,
    setupSSE,
    dispose,
    /** Exposed for tests and the harness. */
    handleEvent,
  }
}
