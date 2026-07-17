export type ParsedSSEEvent = {
  id: string
  event: string
  data: string
}

export function createSSEParser(
  onEvent: (event: ParsedSSEEvent) => void,
  onLastEventId?: (id: string) => void,
) {
  let buffer = ''
  let eventType = ''
  let eventId: string | undefined
  let data: string[] = []
  let lastEventId = ''

  function dispatch() {
    if (eventId !== undefined) {
      lastEventId = eventId
      onLastEventId?.(lastEventId)
    }
    if (data.length > 0) {
      onEvent({
        id: lastEventId,
        event: eventType || 'message',
        data: data.join('\n'),
      })
    }
    eventType = ''
    eventId = undefined
    data = []
  }

  function processLine(line: string) {
    if (line === '') {
      dispatch()
      return
    }
    if (line.startsWith(':')) return

    const separator = line.indexOf(':')
    const field = separator === -1 ? line : line.slice(0, separator)
    let value = separator === -1 ? '' : line.slice(separator + 1)
    if (value.startsWith(' ')) value = value.slice(1)

    if (field === 'event') eventType = value
    else if (field === 'data') data.push(value)
    else if (field === 'id' && !value.includes('\0')) eventId = value
  }

  return {
    push(chunk: string) {
      buffer += chunk
      while (true) {
        const newline = buffer.indexOf('\n')
        if (newline === -1) break
        let line = buffer.slice(0, newline)
        buffer = buffer.slice(newline + 1)
        if (line.endsWith('\r')) line = line.slice(0, -1)
        processLine(line)
      }
    },
    finish() {
      if (buffer) {
        processLine(buffer.endsWith('\r') ? buffer.slice(0, -1) : buffer)
        buffer = ''
      }
      dispatch()
    },
  }
}

export function reconnectDelay(
  attempt: number,
  random = Math.random,
  baseMs = 1_000,
  maxMs = 30_000,
) {
  const exponential = Math.min(maxMs, baseMs * 2 ** Math.max(0, attempt))
  return Math.round(exponential * (0.85 + random() * 0.3))
}
