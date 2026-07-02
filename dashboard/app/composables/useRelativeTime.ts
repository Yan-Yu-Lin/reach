export function useRelativeTime() {
  function relative(dateStr?: string): string {
    if (!dateStr) return '—'
    const date = new Date(dateStr)
    const now = Date.now()
    const diffMs = now - date.getTime()

    if (diffMs < 0) return 'just now'

    const seconds = Math.floor(diffMs / 1000)
    if (seconds < 5) return 'just now'
    if (seconds < 60) return `${seconds}s ago`

    const minutes = Math.floor(seconds / 60)
    if (minutes < 60) return `${minutes}m ago`

    const hours = Math.floor(minutes / 60)
    if (hours < 24) return `${hours}h ago`

    const days = Math.floor(hours / 24)
    if (days < 30) return `${days}d ago`

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
