// One shared clock instead of re-rendering the whole page on a timer. Only templates that
// call relative()/now() depend on it, so a tick re-renders the time labels and nothing else.
const now = shallowRef(Date.now())
let ticker: ReturnType<typeof setInterval> | null = null
let users = 0

const TICK_MS = 15_000

export function useNow() {
  return now
}

/** Start the shared clock for the lifetime of the calling component. */
export function useClock() {
  if (!import.meta.client) return now
  onMounted(() => {
    users++
    now.value = Date.now()
    if (!ticker) ticker = setInterval(() => { now.value = Date.now() }, TICK_MS)
  })
  onBeforeUnmount(() => {
    users--
    if (users <= 0 && ticker) {
      clearInterval(ticker)
      ticker = null
      users = 0
    }
  })
  return now
}

export function useRelativeTime() {
  function relative(dateStr?: string | null): string {
    if (!dateStr) return '—'
    const date = new Date(dateStr)
    const t = date.getTime()
    if (!Number.isFinite(t)) return '—'
    const diffMs = now.value - t

    if (diffMs < 0) return 'just now'

    const seconds = Math.floor(diffMs / 1000)
    if (seconds < 10) return 'just now'
    if (seconds < 60) return `${seconds}s ago`

    const minutes = Math.floor(seconds / 60)
    if (minutes < 60) return `${minutes}m ago`

    const hours = Math.floor(minutes / 60)
    if (hours < 24) return `${hours}h ago`

    const days = Math.floor(hours / 24)
    if (days < 365) return `${days}d ago`

    return date.toLocaleDateString()
  }

  function shortTime(dateStr?: string): string {
    if (!dateStr) return '—'
    return new Date(dateStr).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
  }

  function fullTime(dateStr?: string): string {
    if (!dateStr) return '—'
    return new Date(dateStr).toLocaleString()
  }

  return { relative, shortTime, fullTime }
}
