import { describe, expect, it } from 'vitest'
import { createSSEParser, reconnectDelay } from '../app/utils/sse'

describe('createSSEParser', () => {
  it('parses chunked CRLF events, ids, comments, and multiline data', () => {
    const events: Array<{ id: string; event: string; data: string }> = []
    const parser = createSSEParser(event => events.push(event))

    parser.push(': heartbeat\r\nid: 41\r\nevent: machine.updated\r\ndata: {"machine_id":\r\n')
    parser.push('data: "m-1"}\r\n\r\nid: 42\ndata: plain\n\n')

    expect(events).toEqual([
      { id: '41', event: 'machine.updated', data: '{"machine_id":\n"m-1"}' },
      { id: '42', event: 'message', data: 'plain' },
    ])
  })

  it('retains the latest id, ignores invalid null ids, and reports an empty-id reset', () => {
    const events: Array<{ id: string; event: string; data: string }> = []
    const ids: string[] = []
    const parser = createSSEParser(event => events.push(event), id => ids.push(id))

    parser.push('id: 7\n\ndata: first\n\nid: bad\0id\ndata: second\n\nid:\n\ndata: reset\n\n')

    expect(events.map(event => event.id)).toEqual(['7', '7', ''])
    expect(ids).toEqual(['7', ''])
  })
})

describe('reconnectDelay', () => {
  it('uses capped exponential backoff with modest jitter', () => {
    expect(reconnectDelay(0, () => 0)).toBe(850)
    expect(reconnectDelay(3, () => 0.5)).toBe(8_000)
    expect(reconnectDelay(20, () => 1)).toBe(34_500)
  })
})
