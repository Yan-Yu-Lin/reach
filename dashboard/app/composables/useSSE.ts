type SSEStatus = 'connecting' | 'live' | 'reconnecting' | 'stale'
type SSEHandler = (event: string, data: any) => void

const sseStatus = ref<SSEStatus>('connecting')
const lastEventAt = ref(0)

let controller: AbortController | null = null
let reconnectTimer: ReturnType<typeof setTimeout> | null = null
let staleTimer: ReturnType<typeof setInterval> | null = null
const handlers = new Set<SSEHandler>()

export function useSSE() {
  function start(token: string, apiBase = '/api') {
    stopSSEConnection()
    controller = new AbortController()
    sseStatus.value = 'connecting'
    connect(token, apiBase)
    staleTimer = setInterval(() => {
      if (lastEventAt.value && Date.now() - lastEventAt.value > 60_000) {
        sseStatus.value = 'stale'
      }
    }, 10_000)
  }

  function stop() {
    stopSSEConnection()
  }

  function onEvent(handler: SSEHandler) {
    handlers.add(handler)
    return () => handlers.delete(handler)
  }

  return { status: sseStatus, start, stop, onEvent }
}

async function connect(token: string, apiBase = '/api') {
  if (!controller || controller.signal.aborted) return
  try {
    const res = await fetch(`${apiBase}/admin/events`, {
      headers: { Authorization: `Bearer ${token}` },
      signal: controller.signal,
    })
    if (res.status === 401) {
      await handleSSEUnauthorized()
      return
    }
    if (!res.ok || !res.body) {
      scheduleReconnect(token, apiBase)
      return
    }
    sseStatus.value = 'live'
    lastEventAt.value = Date.now()
    const reader = res.body.getReader()
    const dec = new TextDecoder()
    let buf = ''
    while (true) {
      const { done, value } = await reader.read()
      if (done) break
      lastEventAt.value = Date.now()
      sseStatus.value = 'live'
      buf += dec.decode(value, { stream: true })
      const parts = buf.split('\n\n')
      buf = parts.pop() || ''
      for (const part of parts) {
        const eventMatch = part.match(/^event: (.+)$/m)
        const dataMatch = part.match(/^data: (.+)$/m)
        if (eventMatch && dataMatch) {
          lastEventAt.value = Date.now()
          sseStatus.value = 'live'
          try {
            const data = JSON.parse(dataMatch[1])
            const event = eventMatch[1]
            handlers.forEach(h => h(event, data))
          } catch {}
        }
      }
    }
  } catch (e: any) {
    if (e.name === 'AbortError') return
  }
  scheduleReconnect(token, apiBase)
}

function stopSSEConnection() {
  controller?.abort()
  controller = null
  if (reconnectTimer) clearTimeout(reconnectTimer)
  if (staleTimer) clearInterval(staleTimer)
  reconnectTimer = null
  staleTimer = null
}

async function handleSSEUnauthorized() {
  stopSSEConnection()
  const api = useReachApi()
  api.setToken(null)
  if (import.meta.client) await navigateTo({ path: '/login', query: { reason: 'session-expired' } })
}

function scheduleReconnect(token: string, apiBase = '/api') {
  if (!controller || controller.signal.aborted) return
  sseStatus.value = 'reconnecting'
  reconnectTimer = setTimeout(() => connect(token, apiBase), 2000)
}
