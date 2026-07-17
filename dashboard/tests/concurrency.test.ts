import { describe, expect, it } from 'vitest'
import { mapSettledLimited } from '../app/utils/concurrency'

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

  it('handles empty input without invoking the mapper', async () => {
    let calls = 0
    const results = await mapSettledLimited([], 4, async () => { calls++; return true })
    expect(results).toEqual([])
    expect(calls).toBe(0)
  })
})
