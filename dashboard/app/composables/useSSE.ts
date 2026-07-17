import { createSSEParser, reconnectDelay } from '~/utils/sse'

export type SSEStatus = 'connecting' | 'live' | 'reconnecting' | 'stale'
export type SSEHandler = (event: string, data: unknown, id: string) => void

const sseStatus = ref<SSEStatus>('connecting')
const handlers = new Set<SSEHandler>()

let controller: AbortController | null = null
let timer: ReturnType<typeof setTimeout> | null = null
let generation = 0
let reconnectAttempt = 0
let lastEventId = ''
const staleAfterMs = 60_000

export function useSSE() {
  function start(token: string, apiBase = '/api') {
    stopSSEConnection()
    generation++
    reconnectAttempt = 0
    lastEventId = ''
    sseStatus.value = 'connecting'
    void connect(token, apiBase, generation)
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

async function connect(token: string, apiBase: string, connectionGeneration: number) {
  if (connectionGeneration !== generation) return
  controller = new AbortController()
  const activeController = controller
  let connectedAt = 0
  armStaleWatchdog(connectionGeneration)

  try {
    const headers = new Headers({ Authorization: `Bearer ${token}` })
    if (lastEventId) headers.set('Last-Event-ID', lastEventId)
    const res = await fetch(`${apiBase}/admin/events`, {
      headers,
      signal: activeController.signal,
    })

    if (res.status === 401) {
      await handleSSEUnauthorized()
      return
    }
    if (!res.ok || !res.body) throw new Error(`Event stream failed (${res.status})`)

    sseStatus.value = 'live'
    connectedAt = Date.now()
    markActive(connectionGeneration)
    const reader = res.body.getReader()
    const decoder = new TextDecoder()
    const parser = createSSEParser((message) => {
      try {
        const parsed: unknown = JSON.parse(message.data)
        handlers.forEach(handler => handler(message.event, parsed, message.id))
      } catch {
        // Ignore malformed application payloads without losing stream framing.
      }
    }, (id) => {
      lastEventId = id
    })

    while (connectionGeneration === generation) {
      const { done, value } = await reader.read()
      if (done) break
      markActive(connectionGeneration)
      parser.push(decoder.decode(value, { stream: true }))
    }
    parser.push(decoder.decode())
    parser.finish()
  } catch (error) {
    if (connectionGeneration !== generation) return
  } finally {
    if (controller === activeController) controller = null
  }

  if (connectedAt && Date.now() - connectedAt >= 10_000) reconnectAttempt = 0
  scheduleReconnect(token, apiBase, connectionGeneration)
}

function markActive(connectionGeneration: number) {
  if (connectionGeneration !== generation) return
  sseStatus.value = 'live'
  armStaleWatchdog(connectionGeneration)
}

function armStaleWatchdog(connectionGeneration: number) {
  setTimer(() => {
    if (connectionGeneration !== generation) return
    sseStatus.value = 'stale'
    controller?.abort()
  }, staleAfterMs)
}

function stopSSEConnection() {
  generation++
  controller?.abort()
  controller = null
  clearTimer()
}

async function handleSSEUnauthorized() {
  stopSSEConnection()
  const api = useReachApi()
  api.setToken(null)
  if (import.meta.client) {
    await navigateTo({ path: '/login', query: { reason: 'session-expired' } })
  }
}

function scheduleReconnect(token: string, apiBase: string, connectionGeneration: number) {
  if (connectionGeneration !== generation) return
  sseStatus.value = 'reconnecting'
  const delay = reconnectDelay(reconnectAttempt++)
  setTimer(() => void connect(token, apiBase, connectionGeneration), delay)
}

function setTimer(callback: () => void, delay: number) {
  clearTimer()
  timer = setTimeout(() => {
    timer = null
    callback()
  }, delay)
}

function clearTimer() {
  if (timer) clearTimeout(timer)
  timer = null
}
