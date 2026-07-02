export function useFleet() {
  const api = useReachApi()
  const sse = useSSE()
  const machines = ref<MachineWithTunnels[]>([])
  const requests = ref<RequestRow[]>([])
  const loading = ref(false)
  const error = ref('')
  const selectedId = ref<string | null>(null)

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

  async function refresh() {
    error.value = ''
    loading.value = true
    try {
      const [ms, rs] = await Promise.all([api.machines(), api.requests()])
      machines.value = ms || []
      requests.value = rs || []
    } catch (e: any) {
      error.value = e.message || 'Failed to load'
    } finally {
      loading.value = false
    }
  }

  async function refreshMachine(id: string) {
    try {
      const updated = await api.machine(id)
      const idx = machines.value.findIndex(m => m.machine.id === id)
      if (idx >= 0) machines.value[idx] = updated
    } catch {}
  }

  function setupSSE() {
    if (!api.token.value) return
    const config = useRuntimeConfig()
    sse.start(api.token.value, config.public.apiBase as string || '/api')

    sse.onEvent((event, data) => {
      if (event.startsWith('request.') || event === 'ssh_config.changed') {
        refresh()
      } else if (event.startsWith('machine.')) {
        if (data.machine_id) {
          refreshMachine(data.machine_id)
        } else {
          refresh()
        }
      } else if (event.startsWith('agent.')) {
        if (data.machine_id) refreshMachine(data.machine_id)
      }

      if (event === 'request.created' && data.status === 'pending') {
        notifyPending(data)
      }
    })
  }

  function notifyPending(data: any) {
    if (!('Notification' in window)) return
    if (Notification.permission === 'granted') {
      new Notification('Reach: New request', {
        body: `${data.name || 'Unknown'} wants to connect`,
        tag: 'reach-pending',
      })
    } else if (Notification.permission !== 'denied') {
      Notification.requestPermission()
    }
  }

  watch(pendingCount, (n) => {
    document.title = n > 0 ? `(${n}) Reach` : 'Reach'
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
  }
}
