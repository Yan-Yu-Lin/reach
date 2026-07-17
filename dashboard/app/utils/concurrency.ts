export async function mapSettledLimited<T, R>(
  values: readonly T[],
  limit: number,
  mapper: (value: T) => Promise<R>,
): Promise<PromiseSettledResult<R>[]> {
  const results = new Array<PromiseSettledResult<R>>(values.length)
  const workerCount = Math.min(values.length, Math.max(1, Math.floor(limit)))
  let nextIndex = 0

  async function worker() {
    while (nextIndex < values.length) {
      const index = nextIndex++
      try {
        results[index] = { status: 'fulfilled', value: await mapper(values[index]!) }
      } catch (reason) {
        results[index] = { status: 'rejected', reason }
      }
    }
  }

  await Promise.all(Array.from({ length: workerCount }, () => worker()))
  return results
}
