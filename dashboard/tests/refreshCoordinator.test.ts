import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createRefreshCoordinator, type RefreshBatch } from '../app/utils/refreshCoordinator'

describe('createRefreshCoordinator', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('coalesces bursts and deduplicates machine ids', async () => {
    const batches: RefreshBatch[] = []
    const coordinator = createRefreshCoordinator(async batch => { batches.push(batch) }, 50)

    const first = coordinator.requestMachine('m-1')
    const second = coordinator.requestMachine('m-1')
    const third = coordinator.requestMachine('m-2')
    await vi.advanceTimersByTimeAsync(50)
    await Promise.all([first, second, third])

    expect(batches).toEqual([{ full: false, machineIds: ['m-1', 'm-2'] }])
  })

  it('lets a full refresh supersede queued machine refreshes', async () => {
    const batches: RefreshBatch[] = []
    const coordinator = createRefreshCoordinator(async batch => { batches.push(batch) }, 50)

    const machine = coordinator.requestMachine('m-1')
    const full = coordinator.requestFull()
    await vi.advanceTimersByTimeAsync(50)
    await Promise.all([machine, full])

    expect(batches).toEqual([{ full: true, machineIds: [] }])
  })

  it('keeps one refresh in flight and performs one trailing rerun', async () => {
    const batches: RefreshBatch[] = []
    let release: (() => void) | undefined
    const coordinator = createRefreshCoordinator(batch => {
      batches.push(batch)
      return new Promise<void>(resolve => { release = resolve })
    }, 50)

    const first = coordinator.requestMachine('m-1')
    await vi.advanceTimersByTimeAsync(50)
    const trailingOne = coordinator.requestMachine('m-2')
    const trailingTwo = coordinator.requestMachine('m-3')
    await vi.advanceTimersByTimeAsync(500)
    expect(batches).toEqual([{ full: false, machineIds: ['m-1'] }])

    release?.()
    await first
    await vi.advanceTimersByTimeAsync(50)
    expect(batches).toEqual([
      { full: false, machineIds: ['m-1'] },
      { full: false, machineIds: ['m-2', 'm-3'] },
    ])

    release?.()
    await Promise.all([trailingOne, trailingTwo])
  })

  it('allows a targeted run to schedule one trailing full refresh', async () => {
    const batches: RefreshBatch[] = []
    let coordinator: ReturnType<typeof createRefreshCoordinator>
    coordinator = createRefreshCoordinator(async batch => {
      batches.push(batch)
      if (!batch.full) void coordinator.requestFull()
    }, 50)

    const targeted = coordinator.requestMachine('m-1')
    await vi.advanceTimersByTimeAsync(50)
    await targeted
    await vi.advanceTimersByTimeAsync(50)

    expect(batches).toEqual([
      { full: false, machineIds: ['m-1'] },
      { full: true, machineIds: [] },
    ])
    await vi.advanceTimersByTimeAsync(200)
    expect(batches).toHaveLength(2)
  })

  it('cancels queued work when disposed', async () => {
    const refresh = vi.fn(async () => {})
    const coordinator = createRefreshCoordinator(refresh, 50)
    const pending = coordinator.requestFull()

    coordinator.dispose()
    await vi.advanceTimersByTimeAsync(100)
    await pending

    expect(refresh).not.toHaveBeenCalled()
  })
})
