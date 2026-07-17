import { mapSettledLimited, summarizeSettled } from '~/utils/concurrency'
import { createRefreshCoordinator, type RefreshBatch } from '~/utils/refreshCoordinator'

const machineRefreshConcurrency = 4
const fullRefreshThreshold = 12

export function useFleet() {
  const api = useReachApi()
  const sse = useSSE()
  const machines = ref<MachineWithTunnels[]>([])
  const requests = ref<RequestRow[]>([])
  const loading = ref(true)
  const error = ref('')
  const selectedId = ref<string | null>(null)
  let disposeSSE: (() => void) | null = null

  const pendingRequests = computed(() =>
    requests.value.filter(r => r.status === 'pending')
  )

  const pendingCount = computed(() => pendingRequests.value.length)

  const onlineCount = computed(() =>
    machines.value.filter(m =>
      m.machine.desired_state === 'active' && m.machine.observed_state === 'online'
    ).length
  )

  const degradedCount = computed(() =>
    machines.value.filter(m =>
      m.machine.desired_state === 'active' && m.machine.observed_state === 'degraded'
    ).length
  )

  const offlineCount = computed(() =>
    machines.value.filter(m =>
      m.machine.desired_state === 'active' &&
      (m.machine.observed_state === 'offline' || m.machine.observed_state === 'gone')
    ).length
  )

  type FleetGroup = {
    label: string
    machines: MachineWithTunnels[]
    collapsed?: boolean
  }

  const grouped = computed((): FleetGroup[] => {
    const needsAttention: MachineWithTunnels[] = []
    const activeFleet: MachineWithTunnels[] = []
    const inactive: MachineWithTunnels[] = []

    for (const item of machines.value) {
      const m = item.machine
      if (m.desired_state === 'disabled' || m.desired_state === 'retired' || m.desired_state === 'retiring') {
        inactive.push(item)
      } else if (
        m.observed_state === 'offline' ||
        m.observed_state === 'gone' ||
        m.observed_state === 'degraded' ||
        m.status === 'pending'
      ) {
        needsAttention.push(item)
      } else {
        activeFleet.push(item)
      }
    }

    const groups: FleetGroup[] = []
    if (needsAttention.length) groups.push({ label: 'Needs attention', machines: needsAttention })
    if (activeFleet.length) groups.push({ label: 'Active fleet', machines: activeFleet })
    if (inactive.length) groups.push({ label: 'Inactive', machines: inactive, collapsed: true })
    return groups
  })

  const selectedMachine = computed(() =>
    machines.value.find(m => m.machine.id === selectedId.value) || null
  )

  const recentlyChanged = computed(() =>
    [...machines.value]
      .sort((a, b) => (b.machine.updated_at || b.machine.created_at).localeCompare(a.machine.updated_at || a.machine.created_at))
      .slice(0, 5)
  )

  async function executeRefresh(batch: RefreshBatch) {
    if (batch.full) {
      loading.value = true
      try {
        const [machineResult, requestResult] = await Promise.allSettled([api.machines(), api.requests()])
        const failures: unknown[] = []
        if (machineResult.status === 'fulfilled') machines.value = machineResult.value || []
        else failures.push(machineResult.reason)
        if (requestResult.status === 'fulfilled') requests.value = requestResult.value || []
        else failures.push(requestResult.reason)
        if (failures.length > 0) {
          const failure = failures[0] as any
          error.value = failure?.message || 'Failed to load dashboard data'
        } else {
          error.value = ''
        }
      } finally {
        loading.value = false
      }
      return
    }

    if (batch.machineIds.length >= fullRefreshThreshold) {
      await executeRefresh({ full: true, machineIds: [] })
      return
    }

    const updateResults = await mapSettledLimited(
      batch.machineIds,
      machineRefreshConcurrency,
      id => api.machine(id),
    )
    const { values: updates, failureCount, needsFullRefresh } = summarizeSettled(updateResults)
    for (const updated of updates) {
      const index = machines.value.findIndex(item => item.machine.id === updated.machine.id)
      if (index >= 0) machines.value[index] = updated
      else machines.value.push(updated)
    }

    if (needsFullRefresh) {
      error.value = `${failureCount} machine refresh${failureCount === 1 ? '' : 'es'} failed. Refreshing fleet...`
      void refreshCoordinator.requestFull().catch(() => {})
    }
  }

  const refreshCoordinator = createRefreshCoordinator(executeRefresh)

  function refresh() {
    return refreshCoordinator.requestFull(true)
  }

  function refreshMachine(id: string) {
    return refreshCoordinator.requestMachine(id)
  }

  function setupSSE() {
    disposeSSE?.()
    if (!api.token.value) return () => {}

    const config = useRuntimeConfig()
    const removeHandler = sse.onEvent((event, rawData) => {
      const data = rawData && typeof rawData === 'object'
        ? rawData as Record<string, any>
        : {}

      if (event.startsWith('request.') || event === 'ssh_config.changed') {
        void refreshCoordinator.requestFull()
      } else if (event.startsWith('machine.') || event.startsWith('agent.')) {
        if (typeof data.machine_id === 'string' && data.machine_id) {
          void refreshCoordinator.requestMachine(data.machine_id)
        } else {
          void refreshCoordinator.requestFull()
        }
      }

      if (event === 'request.created' && data.status === 'pending') {
        notifyPending(data)
      }
    })

    sse.start(api.token.value, config.public.apiBase as string || '/api')
    disposeSSE = () => {
      removeHandler()
      sse.stop()
      disposeSSE = null
    }
    return disposeSSE
  }

  function dispose() {
    disposeSSE?.()
    refreshCoordinator.dispose()
  }

  function notifyPending(data: Record<string, any>) {
    if (!import.meta.client || !('Notification' in window)) return
    if (Notification.permission === 'granted') {
      new Notification('Reach: New request', {
        body: `${data.name || 'Unknown'} wants to connect`,
        tag: 'reach-pending',
      })
    } else if (Notification.permission !== 'denied') {
      void Notification.requestPermission()
    }
  }

  watch(pendingCount, (count) => {
    if (import.meta.client) document.title = count > 0 ? `(${count}) Reach` : 'Reach'
  })

  return {
    machines,
    requests,
    loading,
    error,
    selectedId,
    pendingRequests,
    pendingCount,
    onlineCount,
    degradedCount,
    offlineCount,
    grouped,
    selectedMachine,
    recentlyChanged,
    refresh,
    refreshMachine,
    setupSSE,
    dispose,
  }
}
