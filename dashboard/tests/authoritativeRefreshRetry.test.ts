import { describe, expect, it, vi } from 'vitest'
import { createAuthoritativeRefreshRetry } from '../app/utils/authoritativeRefreshRetry'

describe('authoritative refresh retry', () => {
  it('backs off without scheduling a retry storm and resets on success', () => {
    vi.useFakeTimers()
    const retry = vi.fn()
    const state = createAuthoritativeRefreshRetry(retry, { baseMs: 100, maxMs: 400 })

    state.failed()
    state.failed()
    expect(vi.getTimerCount()).toBe(1)
    vi.advanceTimersByTime(100)
    expect(retry).toHaveBeenCalledTimes(1)

    state.failed()
    vi.advanceTimersByTime(199)
    expect(retry).toHaveBeenCalledTimes(1)
    vi.advanceTimersByTime(1)
    expect(retry).toHaveBeenCalledTimes(2)

    state.succeeded()
    state.failed()
    vi.advanceTimersByTime(100)
    expect(retry).toHaveBeenCalledTimes(3)
    vi.useRealTimers()
  })

  it('cancels pending retries on success and dispose', () => {
    vi.useFakeTimers()
    const retry = vi.fn()
    const state = createAuthoritativeRefreshRetry(retry, { baseMs: 100 })
    state.failed()
    state.succeeded()
    vi.runAllTimers()
    expect(retry).not.toHaveBeenCalled()

    state.failed()
    state.dispose()
    vi.runAllTimers()
    expect(retry).not.toHaveBeenCalled()
    vi.useRealTimers()
  })
})
