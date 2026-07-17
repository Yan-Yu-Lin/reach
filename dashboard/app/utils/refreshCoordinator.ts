export type RefreshBatch = {
  full: boolean
  machineIds: string[]
}

export function createRefreshCoordinator(
  refresh: (batch: RefreshBatch) => Promise<void>,
  delayMs = 75,
) {
  let pendingFull = false
  const pendingMachineIds = new Set<string>()
  let timer: ReturnType<typeof setTimeout> | null = null
  let running = false
  let disposed = false
  let waiters: Array<{ resolve: () => void; reject: (error: unknown) => void }> = []

  function hasPending() {
    return pendingFull || pendingMachineIds.size > 0
  }

  function schedule(immediate: boolean) {
    if (disposed || running || timer) return
    timer = setTimeout(() => {
      timer = null
      void run()
    }, immediate ? 0 : delayMs)
  }

  async function run() {
    if (disposed || running || !hasPending()) return
    running = true
    const batch: RefreshBatch = {
      full: pendingFull,
      machineIds: pendingFull ? [] : [...pendingMachineIds],
    }
    pendingFull = false
    pendingMachineIds.clear()
    const batchWaiters = waiters
    waiters = []

    try {
      await refresh(batch)
      batchWaiters.forEach(waiter => waiter.resolve())
    } catch (error) {
      batchWaiters.forEach(waiter => waiter.reject(error))
    } finally {
      running = false
      if (hasPending()) schedule(false)
    }
  }

  function queued(immediate: boolean) {
    if (disposed) return Promise.resolve()
    const promise = new Promise<void>((resolve, reject) => waiters.push({ resolve, reject }))
    schedule(immediate)
    return promise
  }

  return {
    requestFull(immediate = false) {
      pendingFull = true
      pendingMachineIds.clear()
      return queued(immediate)
    },
    requestMachine(id: string) {
      if (!pendingFull) pendingMachineIds.add(id)
      return queued(false)
    },
    dispose() {
      disposed = true
      if (timer) clearTimeout(timer)
      timer = null
      pendingFull = false
      pendingMachineIds.clear()
      waiters.forEach(waiter => waiter.resolve())
      waiters = []
    },
  }
}
