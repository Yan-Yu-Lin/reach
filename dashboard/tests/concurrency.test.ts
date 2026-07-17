import { describe, expect, it } from 'vitest'
import { mapSettledLimited, summarizeSettled } from '../app/utils/concurrency'

describe('mapSettledLimited', () => {
  it('bounds concurrency while retaining result order and partial successes', async () => {
    let active = 0
    let peak = 0

    const results = await mapSettledLimited([1, 2, 3, 4, 5], 2, async value => {
      active++
      peak = Math.max(peak, active)
      await new Promise(resolve => setTimeout(resolve, 5))
      active--
      if (value === 3) throw new Error('failed')
      return value * 10
    })

    expect(peak).toBe(2)
    expect(results.map(result => result.status)).toEqual([
      'fulfilled', 'fulfilled', 'rejected', 'fulfilled', 'fulfilled',
    ])
    expect(results[0]).toEqual({ status: 'fulfilled', value: 10 })
    expect(results[4]).toEqual({ status: 'fulfilled', value: 50 })
  })

  it('summarizes partial successes and requests a full retry after any failure', () => {
    const summary = summarizeSettled<number>([
      { status: 'fulfilled', value: 10 },
      { status: 'rejected', reason: new Error('failed') },
      { status: 'fulfilled', value: 30 },
    ])

    expect(summary).toEqual({
      values: [10, 30],
      failureCount: 1,
      needsFullRefresh: true,
    })
  })

  it('does not request a full retry when every targeted refresh succeeds', () => {
    expect(summarizeSettled([
      { status: 'fulfilled' as const, value: 10 },
      { status: 'fulfilled' as const, value: 20 },
    ])).toEqual({ values: [10, 20], failureCount: 0, needsFullRefresh: false })
  })

  it('handles empty input without invoking the mapper', async () => {
    let calls = 0
    const results = await mapSettledLimited([], 4, async () => { calls++; return true })
    expect(results).toEqual([])
    expect(calls).toBe(0)
  })
})
